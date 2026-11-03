package langfuse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Langfuse ingests traces through its OpenTelemetry (OTLP) endpoint:
//
//	POST {base_url}/api/public/otel/v1/traces
//
// The legacy /api/public/ingestion event API this client used before is
// deprecated and stops accepting trace and observation events on 2026-11-16,
// so spans are sent as OTLP/HTTP JSON payloads with Langfuse's `langfuse.*`
// attribute conventions.
// See https://langfuse.com/integrations/native/opentelemetry for the mapping.
//
// The payload is written directly instead of through the OpenTelemetry SDK:
// spans are synthesised from a committed episode after the run, so no live
// context propagation, batching, or sampling is involved, and the agent stays
// free of an SDK dependency tree.
const (
	tracesPath = "/api/public/otel/v1/traces"
	// ingestionVersionV4 makes Langfuse apply the observations-first v4 data
	// model and expose the spans in real time instead of the legacy pipeline.
	ingestionVersionHeader = "x-langfuse-ingestion-version"
	ingestionVersionV4     = "4"
	scopeName              = "aico.agent.telemetry"
	serviceName            = "aico-agent"
)

// Observation types recognized by Langfuse. An unrecognized type is not
// rejected, but it is rewritten, so only these values are used.
const (
	TypeSpan       = "span"
	TypeGeneration = "generation"
	TypeEvent      = "event"
	TypeTool       = "tool"
	TypeAgent      = "agent"
	TypeRetriever  = "retriever"
	TypeChain      = "chain"
	TypeEmbedding  = "embedding"
	TypeEvaluator  = "evaluator"
	TypeGuardrail  = "guardrail"
)

// Observation levels recognized by Langfuse.
const (
	LevelDefault = "DEFAULT"
	LevelWarning = "WARNING"
	LevelError   = "ERROR"
)

// TraceContext is trace-wide context that Langfuse queries per observation.
// Every exported span carries it, otherwise filtering and aggregation by user,
// session, tags, or trace metadata only works on the root observation.
// See https://langfuse.com/integrations/native/opentelemetry#propagating-attributes.
type TraceContext struct {
	Name        string
	UserID      string
	SessionID   string
	Release     string
	Version     string
	Environment string
	Tags        []string
	Metadata    map[string]interface{}
}

// Span is one observation in a trace. Input, Output, Metadata, ModelParameters,
// Usage, and Cost accept nested structures; they are JSON-encoded as Langfuse
// requires for structured values.
type Span struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	Name         string
	Type         string
	StartTime    time.Time
	EndTime      time.Time

	Input     interface{}
	Output    interface{}
	Metadata  map[string]interface{}
	Level     string
	StatusMsg string
	Trace     TraceContext

	// Generation-only fields.
	Model               string
	ModelParameters     map[string]interface{}
	Usage               map[string]int
	Cost                map[string]float64
	CompletionStartTime time.Time
}

// RejectedSpansError reports spans that Langfuse accepted the request for but
// did not ingest. Re-sending the batch would duplicate the spans that were
// ingested, so this error must not be retried.
type RejectedSpansError struct {
	Count   int
	Message string
}

func (e *RejectedSpansError) Error() string {
	if strings.TrimSpace(e.Message) == "" {
		return fmt.Sprintf("langfuse rejected %d span(s)", e.Count)
	}
	return fmt.Sprintf("langfuse rejected %d span(s): %s", e.Count, e.Message)
}

// ExportSpans sends one batch of complete spans. Spans are immutable once
// ingested, so callers must set final timings and attributes before exporting
// and must not re-export the same span ID.
func (c *Client) ExportSpans(ctx context.Context, spans []Span) error {
	if len(spans) == 0 {
		return nil
	}
	if !c.Configured() {
		return fmt.Errorf("langfuse client is not configured")
	}
	payload, err := json.Marshal(otlpRequest{ResourceSpans: []otlpResourceSpans{{
		Resource: otlpResource{Attributes: []otlpAttribute{{
			Key:   "service.name",
			Value: otlpValue{StringValue: serviceName},
		}}},
		ScopeSpans: []otlpScopeSpans{{
			Scope: otlpScope{Name: scopeName},
			Spans: encodeSpans(spans),
		}},
	}}})
	if err != nil {
		return fmt.Errorf("marshal otlp spans: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+tracesPath, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create otlp request: %w", err)
	}
	req.SetBasicAuth(c.publicKey, c.secretKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(ingestionVersionHeader, ingestionVersionV4)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("otlp request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("otlp HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed otlpResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil // Accepted; the OTLP response body is optional.
	}
	if rejected := otlpRejectedSpans(parsed.PartialSuccess.RejectedSpans); rejected > 0 {
		return &RejectedSpansError{Count: rejected, Message: parsed.PartialSuccess.ErrorMessage}
	}
	return nil
}

// otlpRejectedSpans reads the rejected-span count of an OTLP partial-success
// response, which encodes int64 fields as either numbers or strings.
func otlpRejectedSpans(value interface{}) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err == nil {
			return parsed
		}
	}
	return 0
}

func encodeSpans(spans []Span) []otlpSpan {
	encoded := make([]otlpSpan, 0, len(spans))
	for _, span := range spans {
		attributes := traceAttributes(span.Trace)
		attributes = append(attributes, observationAttributes(span)...)
		item := otlpSpan{
			TraceID:           span.TraceID,
			SpanID:            span.SpanID,
			ParentSpanID:      span.ParentSpanID,
			Name:              span.Name,
			Kind:              1, // SPAN_KIND_INTERNAL
			StartTimeUnixNano: unixNano(span.StartTime),
			EndTimeUnixNano:   unixNano(span.EndTime),
			Attributes:        attributes,
		}
		if span.Level == LevelError {
			item.Status = &otlpStatus{Code: 2, Message: span.StatusMsg}
		}
		encoded = append(encoded, item)
	}
	return encoded
}

func traceAttributes(trace TraceContext) []otlpAttribute {
	attributes := []otlpAttribute{}
	appendString := func(key, value string) {
		if strings.TrimSpace(value) != "" {
			attributes = append(attributes, stringAttribute(key, value))
		}
	}
	appendString("langfuse.trace.name", trace.Name)
	appendString("langfuse.user.id", trace.UserID)
	appendString("langfuse.session.id", trace.SessionID)
	appendString("langfuse.release", trace.Release)
	appendString("langfuse.version", trace.Version)
	appendString("langfuse.environment", trace.Environment)
	if tags := nonEmptyStrings(trace.Tags); len(tags) > 0 {
		values := make([]otlpValue, 0, len(tags))
		for _, tag := range tags {
			values = append(values, otlpValue{StringValue: tag})
		}
		attributes = append(attributes, otlpAttribute{Key: "langfuse.trace.tags", Value: otlpValue{ArrayValue: &otlpArray{Values: values}}})
	}
	return append(attributes, metadataAttributes("langfuse.trace.metadata.", trace.Metadata)...)
}

func observationAttributes(span Span) []otlpAttribute {
	attributes := []otlpAttribute{}
	if span.Type != "" {
		attributes = append(attributes, stringAttribute("langfuse.observation.type", span.Type))
	}
	if span.Level != "" && span.Level != LevelDefault {
		attributes = append(attributes, stringAttribute("langfuse.observation.level", span.Level))
	}
	if strings.TrimSpace(span.StatusMsg) != "" {
		attributes = append(attributes, stringAttribute("langfuse.observation.status_message", span.StatusMsg))
	}
	if span.Input != nil {
		if encoded, ok := jsonAttribute("langfuse.observation.input", span.Input); ok {
			attributes = append(attributes, encoded)
		}
	}
	if span.Output != nil {
		if encoded, ok := jsonAttribute("langfuse.observation.output", span.Output); ok {
			attributes = append(attributes, encoded)
		}
	}
	if span.Model != "" {
		attributes = append(attributes, stringAttribute("langfuse.observation.model.name", span.Model))
	}
	if !span.CompletionStartTime.IsZero() {
		attributes = append(attributes, stringAttribute("langfuse.observation.completion_start_time", RFC3339(span.CompletionStartTime)))
	}
	for _, field := range []struct {
		key   string
		value interface{}
	}{
		{"langfuse.observation.model.parameters", span.ModelParameters},
		{"langfuse.observation.usage_details", span.Usage},
		{"langfuse.observation.cost_details", span.Cost},
	} {
		if isEmptyStructured(field.value) {
			continue
		}
		if encoded, ok := jsonAttribute(field.key, field.value); ok {
			attributes = append(attributes, encoded)
		}
	}
	return append(attributes, metadataAttributes("langfuse.observation.metadata.", span.Metadata)...)
}

// metadataAttributes maps a metadata map to filterable Langfuse attributes.
// Scalars keep their type; nested values are JSON-encoded because OTLP
// attribute values are limited to scalars and arrays of scalars.
func metadataAttributes(prefix string, metadata map[string]interface{}) []otlpAttribute {
	attributes := []otlpAttribute{}
	for _, key := range sortedKeys(metadata) {
		if strings.TrimSpace(key) == "" {
			continue
		}
		value, ok := attributeValue(metadata[key])
		if !ok {
			continue
		}
		attributes = append(attributes, otlpAttribute{Key: prefix + key, Value: value})
	}
	return attributes
}

func attributeValue(value interface{}) (otlpValue, bool) {
	switch typed := value.(type) {
	case nil:
		return otlpValue{}, false
	case string:
		if typed == "" {
			return otlpValue{}, false
		}
		return otlpValue{StringValue: typed}, true
	case bool:
		return otlpValue{BoolValue: &typed}, true
	case int:
		return intAttributeValue(int64(typed)), true
	case int8:
		return intAttributeValue(int64(typed)), true
	case int16:
		return intAttributeValue(int64(typed)), true
	case int32:
		return intAttributeValue(int64(typed)), true
	case int64:
		return intAttributeValue(typed), true
	case uint:
		return intAttributeValue(int64(typed)), true
	case uint8:
		return intAttributeValue(int64(typed)), true
	case uint16:
		return intAttributeValue(int64(typed)), true
	case uint32:
		return intAttributeValue(int64(typed)), true
	case uint64:
		if typed > uint64(1<<63-1) {
			return otlpValue{}, false
		}
		return intAttributeValue(int64(typed)), true
	case float32:
		converted := float64(typed)
		return otlpValue{DoubleValue: &converted}, true
	case float64:
		return otlpValue{DoubleValue: &typed}, true
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return otlpValue{}, false
		}
		return otlpValue{StringValue: string(encoded)}, true
	}
}

// intAttributeValue encodes integers as JSON numbers. The OTLP JSON mapping
// writes int64 fields as strings, but Langfuse's parser only reads numeric
// intValue payloads and drops observations whose attributes fail validation.
func intAttributeValue(value int64) otlpValue {
	return otlpValue{IntValue: &value}
}

func jsonAttribute(key string, value interface{}) (otlpAttribute, bool) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return otlpAttribute{}, false
	}
	return stringAttribute(key, string(encoded)), true
}

func stringAttribute(key, value string) otlpAttribute {
	return otlpAttribute{Key: key, Value: otlpValue{StringValue: value}}
}

func isEmptyStructured(value interface{}) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case map[string]interface{}:
		return len(typed) == 0
	case map[string]int:
		return len(typed) == 0
	case map[string]float64:
		return len(typed) == 0
	default:
		return false
	}
}

func nonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func sortedKeys(metadata map[string]interface{}) []string {
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func unixNano(ts time.Time) string {
	return fmt.Sprintf("%d", ts.UTC().UnixNano())
}

type otlpRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpAttribute `json:"attributes"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name string `json:"name"`
}

type otlpSpan struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	ParentSpanID      string          `json:"parentSpanId,omitempty"`
	Name              string          `json:"name"`
	Kind              int             `json:"kind"`
	StartTimeUnixNano string          `json:"startTimeUnixNano"`
	EndTimeUnixNano   string          `json:"endTimeUnixNano"`
	Attributes        []otlpAttribute `json:"attributes"`
	Status            *otlpStatus     `json:"status,omitempty"`
}

type otlpStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

type otlpAttribute struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpValue struct {
	StringValue string     `json:"stringValue,omitempty"`
	BoolValue   *bool      `json:"boolValue,omitempty"`
	IntValue    *int64     `json:"intValue,omitempty"`
	DoubleValue *float64   `json:"doubleValue,omitempty"`
	ArrayValue  *otlpArray `json:"arrayValue,omitempty"`
}

type otlpArray struct {
	Values []otlpValue `json:"values"`
}

type otlpResponse struct {
	PartialSuccess struct {
		RejectedSpans interface{} `json:"rejectedSpans"`
		ErrorMessage  string      `json:"errorMessage"`
	} `json:"partialSuccess"`
}
