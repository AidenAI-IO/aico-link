package agent

import (
	"aico-agent/internal/agent/langfuse"
	"encoding/hex"
	"strings"
	"time"

	"github.com/google/uuid"
)

// telemetrySpanBuilder accumulates the observations of one episode trace.
// Every span it produces carries the trace context, so user, session, tags,
// release, and trace metadata stay filterable on child observations instead of
// only on the root.
type telemetrySpanBuilder struct {
	traceID string
	context langfuse.TraceContext
	spans   []langfuse.Span
	index   map[string]int
}

func newTelemetrySpanBuilder(traceID string, traceContext langfuse.TraceContext) *telemetrySpanBuilder {
	return &telemetrySpanBuilder{
		traceID: traceID,
		context: traceContext,
		index:   map[string]int{},
	}
}

// add appends a span and returns its observation ID. The caller only supplies
// the span's own fields; trace identity is filled in here.
func (b *telemetrySpanBuilder) add(span langfuse.Span) string {
	if b == nil {
		return ""
	}
	if strings.TrimSpace(span.SpanID) == "" {
		span.SpanID = telemetryObservationID()
	}
	span.TraceID = b.traceID
	span.Trace = b.context
	if span.EndTime.Before(span.StartTime) {
		span.EndTime = span.StartTime
	}
	b.index[span.SpanID] = len(b.spans)
	b.spans = append(b.spans, span)
	return span.SpanID
}

// update applies a change to an already added span, used by observations that
// only learn their output once later events have been processed. It reports
// whether the span was found.
func (b *telemetrySpanBuilder) update(spanID string, apply func(span *langfuse.Span)) bool {
	if b == nil || apply == nil {
		return false
	}
	index, ok := b.index[spanID]
	if !ok {
		return false
	}
	apply(&b.spans[index])
	return true
}

// end moves a span's end time, used by spans that only learn their duration
// once later events have been processed.
func (b *telemetrySpanBuilder) end(spanID string, endTime time.Time) {
	b.update(spanID, func(span *langfuse.Span) {
		span.EndTime = endTime
	})
}

func (b *telemetrySpanBuilder) spansList() []langfuse.Span {
	if b == nil {
		return nil
	}
	return b.spans
}

// telemetryObservationID returns an OTLP SpanId: 8 random bytes encoded as
// 16 lowercase hexadecimal characters. OpenTelemetry TraceIds are 16 bytes;
// SpanIds are half that size.
func telemetryObservationID() string {
	id := uuid.New()
	return hex.EncodeToString(id[:8])
}

// telemetryTraceID returns the OTLP trace ID (32 hex characters) for an
// episode. Episode IDs are UUIDs; anything else is hashed into the same space.
func telemetryTraceID(episodeID string) string {
	if _, err := uuid.Parse(episodeID); err == nil {
		return strings.ReplaceAll(episodeID, "-", "")
	}
	return strings.ReplaceAll(uuid.NewSHA1(uuid.NameSpaceURL, []byte(episodeID)).String(), "-", "")
}

// telemetryEventMetadata starts observation metadata from an episode event and
// layers extra fields on top. Values that are not scalars are JSON-encoded
// when the span is exported.
func telemetryEventMetadata(event TaskEpisodeEvent, extra map[string]interface{}) map[string]interface{} {
	metadata := map[string]interface{}{}
	if event.EventID != "" {
		metadata["event_id"] = event.EventID
	}
	if event.Role != "" {
		metadata["role"] = event.Role
	}
	for key, value := range event.Metadata {
		metadata[key] = value
	}
	for key, value := range extra {
		metadata[key] = value
	}
	return metadata
}
