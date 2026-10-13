package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"aiden-agent/internal/agent/realtimevoice"
)

type NotificationSeverity uint8

const (
	SeverityInfo NotificationSeverity = iota
	SeverityWarning
	SeverityCritical
	SeverityEmergency
)

func TurnFailureFromError(err error) *TurnFailure {
	if err == nil || errors.Is(err, context.Canceled) {
		return nil
	}
	// An idle close is the provider's own session management, so it is
	// classified before the generic timeout and network checks below.
	if errors.Is(err, realtimevoice.ErrSessionIdleTimeout) {
		return &TurnFailure{Code: TurnFailureSessionIdle}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &TurnFailure{Code: TurnFailureNetworkUnavailable}
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return &TurnFailure{Code: TurnFailureNetworkUnavailable}
	}
	var statusError interface{ HTTPStatusCode() int }
	if errors.As(err, &statusError) {
		switch statusError.HTTPStatusCode() {
		case 402:
			return &TurnFailure{Code: TurnFailureTokenInsufficient}
		case 429:
			var codeError interface{ ProviderErrorCode() string }
			if errors.As(err, &codeError) && isQuotaProviderErrorCode(codeError.ProviderErrorCode()) {
				return &TurnFailure{Code: TurnFailureTokenInsufficient}
			}
			return &TurnFailure{Code: TurnFailureNetworkUnavailable}
		}
	}

	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"insufficient balance",
		"insufficient quota",
		"insufficient_quota",
		"quota exceeded",
		"quota is exhausted",
		"exceeded your current quota",
		"credits exhausted",
		"credit balance",
		"payment required",
		"status 402",
		"status code 402",
	} {
		if strings.Contains(message, marker) {
			return &TurnFailure{Code: TurnFailureTokenInsufficient}
		}
	}
	for _, marker := range []string{
		"no such host",
		"network is unreachable",
		"connection refused",
		"connection reset",
		"dial tcp",
		"i/o timeout",
		"tls handshake timeout",
		"temporary failure in name resolution",
		"deadline exceeded",
		"rate limit",
		"rate_limit",
		"too many requests",
		"status 429",
		"status code 429",
	} {
		if strings.Contains(message, marker) {
			return &TurnFailure{Code: TurnFailureNetworkUnavailable}
		}
	}
	return &TurnFailure{Code: TurnFailureLLMUnavailable}
}

func isQuotaProviderErrorCode(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "insufficient_quota", "insufficient_credits", "billing_hard_limit_reached":
		return true
	default:
		return false
	}
}

type llmTurnFailureSource interface {
	IsLLMTurnFailureSource()
}

func isLLMTurnFailureSource(err error) bool {
	var source llmTurnFailureSource
	return errors.As(err, &source)
}

func DeliveryStatusFromError(err error) DeliveryStatus {
	if err == nil {
		return DeliveryCompleted
	}
	if errors.Is(err, context.Canceled) {
		return DeliveryCanceled
	}
	return DeliveryFailed
}

const (
	VoiceNotificationActive   = "active"
	VoiceNotificationResolved = "resolved"
)

type VoiceNotificationEvent struct {
	Code      string               `json:"code"`
	Severity  NotificationSeverity `json:"severity"`
	State     string               `json:"state"`
	DedupeKey string               `json:"dedupe_key"`
	Params    map[string]string    `json:"params,omitempty"`
}

type VoiceNotificationSink interface {
	Publish(ctx context.Context, event VoiceNotificationEvent) error
}

type TurnFailure struct {
	Code   string
	Params map[string]string
}

type TurnFailureCode = string

const (
	TurnFailureNetworkUnavailable = "network_unavailable"
	TurnFailureTokenInsufficient  = "token_insufficient"
	TurnFailureLLMUnavailable     = "llm_unavailable"
	TurnFailureSessionIdle        = "session_idle"
)

type SpokenTextInput struct {
	ResponseText   string
	TurnFailure    *TurnFailure
	TailAppendable bool
	RelatedCodes   []string
}

type SpokenTextResult struct {
	Text          string
	Mode          string
	DeliveryToken string
}

const (
	SpokenTextModeNormal       = "normal"
	SpokenTextModeReplacement  = "replacement"
	SpokenTextModeTail         = "tail"
	SpokenTextModeNotification = "notification"
)

type DeliveryStatus string

const (
	DeliveryCompleted DeliveryStatus = "completed"
	DeliveryFailed    DeliveryStatus = "failed"
	DeliveryCanceled  DeliveryStatus = "canceled"
)

type VoiceNotificationsConfig struct {
	Enabled       *bool                               `toml:"enabled,omitempty"`
	MaxPending    int                                 `toml:"max_pending,omitempty"`
	RetentionDays int                                 `toml:"retention_days,omitempty"`
	ResponseTail  VoiceNotificationResponseTailConfig `toml:"response_tail,omitempty"`
	Expiration    VoiceNotificationExpirationConfig   `toml:"expiration,omitempty"`
}

type VoiceNotificationResponseTailConfig struct {
	Enabled      *bool `toml:"enabled,omitempty"`
	MaxItems     int   `toml:"max_items,omitempty"`
	MaxTextChars int   `toml:"max_text_chars,omitempty"`
}

type VoiceNotificationExpirationConfig struct {
	DefaultTTLSeconds int            `toml:"default_ttl_seconds,omitempty"`
	CodeTTLSeconds    map[string]int `toml:"code_ttl_seconds,omitempty"`
}

func (c VoiceNotificationsConfig) EnabledOrDefault() bool {
	if c.Enabled != nil {
		return *c.Enabled
	}
	return true
}

func (c VoiceNotificationsConfig) MaxPendingOrDefault() int {
	if c.MaxPending > 0 {
		return c.MaxPending
	}
	return 8
}

func (c VoiceNotificationsConfig) RetentionDaysOrDefault() int {
	if c.RetentionDays > 0 {
		return c.RetentionDays
	}
	return defaultNotificationMemoryRetentionDays
}

func (c VoiceNotificationResponseTailConfig) EnabledOrDefault() bool {
	if c.Enabled != nil {
		return *c.Enabled
	}
	return true
}

func (c VoiceNotificationResponseTailConfig) MaxTextCharsOrDefault() int {
	if c.MaxTextChars > 0 {
		return c.MaxTextChars
	}
	return 40
}

type voiceNotificationRecord struct {
	code              string
	dedupeKey         string
	cycleID           uint64
	currentSeverity   NotificationSeverity
	deliveredSeverity *NotificationSeverity
	params            map[string]string
	deliveryState     string
	firstSeenAt       time.Time
	lastSeenAt        time.Time
	severityChangedAt time.Time
	leaseExpiresAt    time.Time
	deliveryCount     uint64
	lastDeliveredAt   time.Time
}

type voiceNotificationDelivery struct {
	dedupeKey        string
	cycleID          uint64
	severitySnapshot NotificationSeverity
}

type voiceNotificationPolicyKey struct {
	code     string
	severity NotificationSeverity
	locale   string
}

type turnFailurePolicyKey struct {
	code   string
	locale string
}

type VoiceNotificationManager struct {
	mu               sync.Mutex
	now              func() time.Time
	config           VoiceNotificationsConfig
	locale           string
	records          map[string]*voiceNotificationRecord
	deliveries       map[string]voiceNotificationDelivery
	persistentTexts  map[voiceNotificationPolicyKey]string
	turnFailureTexts map[turnFailurePolicyKey]string
	nextCycle        uint64
	nextToken        uint64
}

type VoiceNotificationManagerOption func(*VoiceNotificationManager)

func WithVoiceNotificationClock(now func() time.Time) VoiceNotificationManagerOption {
	return func(manager *VoiceNotificationManager) {
		if now != nil {
			manager.now = now
		}
	}
}

func WithVoiceNotificationLocale(locale string) VoiceNotificationManagerOption {
	return func(manager *VoiceNotificationManager) {
		if locale = normalizeVoiceNotificationLocale(locale); locale != "" {
			manager.locale = locale
		}
	}
}

// resolvedVoiceNotificationLocale is the single config seam for built-in
// notification text, punctuation, and prerecorded fallback selection.
func resolvedVoiceNotificationLocale(cfg Config) string {
	return normalizeVoiceNotificationLocale(cfg.LocaleOrDefault())
}

func NewVoiceNotificationManager(config VoiceNotificationsConfig, opts ...VoiceNotificationManagerOption) *VoiceNotificationManager {
	manager := &VoiceNotificationManager{
		now:              time.Now,
		config:           config,
		locale:           normalizeVoiceNotificationLocale(defaultLocale),
		records:          make(map[string]*voiceNotificationRecord),
		deliveries:       make(map[string]voiceNotificationDelivery),
		persistentTexts:  make(map[voiceNotificationPolicyKey]string),
		turnFailureTexts: make(map[turnFailurePolicyKey]string),
	}
	for _, opt := range opts {
		opt(manager)
	}
	return manager
}

func (m *VoiceNotificationManager) RegisterPersistentText(code string, severity NotificationSeverity, locale, text string) error {
	if m == nil {
		return nil
	}
	code = strings.TrimSpace(code)
	locale = normalizeVoiceNotificationLocale(locale)
	text = strings.TrimSpace(text)
	if code == "" || locale == "" || text == "" {
		return fmt.Errorf("voice notification persistent text requires code, locale, and text")
	}
	if severity > SeverityEmergency {
		return fmt.Errorf("invalid voice notification severity %d", severity)
	}
	m.mu.Lock()
	m.persistentTexts[voiceNotificationPolicyKey{code: code, severity: severity, locale: locale}] = text
	m.mu.Unlock()
	return nil
}

func (m *VoiceNotificationManager) RegisterTurnFailureText(code, locale, text string) error {
	if m == nil {
		return nil
	}
	code = strings.TrimSpace(code)
	locale = normalizeVoiceNotificationLocale(locale)
	text = strings.TrimSpace(text)
	if code == "" || locale == "" || text == "" {
		return fmt.Errorf("voice notification turn failure text requires code, locale, and text")
	}
	m.mu.Lock()
	m.turnFailureTexts[turnFailurePolicyKey{code: code, locale: locale}] = text
	m.mu.Unlock()
	return nil
}

func (r *Runtime) VoiceNotifications() *VoiceNotificationManager {
	if r == nil {
		return nil
	}
	return r.voiceNotifications
}

func (r *Runtime) VoiceNotificationSink() VoiceNotificationSink {
	if r == nil || r.voiceNotifications == nil {
		return nil
	}
	return r.voiceNotifications
}

func (r *Runtime) PrepareSpokenText(ctx context.Context, input SpokenTextInput) SpokenTextResult {
	result := SpokenTextResult{Text: input.ResponseText, Mode: SpokenTextModeNormal}
	if r == nil || r.voiceNotifications == nil {
		return result
	}
	return r.voiceNotifications.PrepareSpokenText(ctx, input)
}

func (r *Runtime) ReportSpokenTextDelivery(token string, err error) {
	if r == nil || r.voiceNotifications == nil || token == "" {
		return
	}
	r.voiceNotifications.ReportDelivery(token, DeliveryStatusFromError(err))
}

// PrepareNotification claims one pending persistent notification for a
// standalone speech response. Unlike PrepareSpokenText, it does not append the
// notification to an existing assistant response.
func (m *VoiceNotificationManager) PrepareNotification(_ context.Context) SpokenTextResult {
	result := SpokenTextResult{Mode: SpokenTextModeNormal}
	if m == nil {
		return result
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.config.EnabledOrDefault() {
		return result
	}
	m.pruneExpiredLocked(m.now())
	text, token := m.claimPendingNotificationLocked()
	if text == "" || token == "" {
		return result
	}
	result.Text = text
	result.Mode = SpokenTextModeNotification
	result.DeliveryToken = token
	return result
}

func (m *VoiceNotificationManager) pendingNotificationRecordsLocked(relatedCodesInput []string) []*voiceNotificationRecord {
	pending := make([]*voiceNotificationRecord, 0, len(m.records))
	for _, record := range m.records {
		if record.deliveryState == "pending" {
			pending = append(pending, record)
		}
	}
	relatedCodes := make(map[string]struct{}, len(relatedCodesInput))
	for _, code := range relatedCodesInput {
		if code = strings.TrimSpace(code); code != "" {
			relatedCodes[code] = struct{}{}
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		_, iRelated := relatedCodes[pending[i].code]
		_, jRelated := relatedCodes[pending[j].code]
		if iRelated != jRelated {
			return iRelated
		}
		if pending[i].currentSeverity != pending[j].currentSeverity {
			return pending[i].currentSeverity > pending[j].currentSeverity
		}
		if !pending[i].severityChangedAt.Equal(pending[j].severityChangedAt) {
			return pending[i].severityChangedAt.After(pending[j].severityChangedAt)
		}
		if !pending[i].firstSeenAt.Equal(pending[j].firstSeenAt) {
			return pending[i].firstSeenAt.Before(pending[j].firstSeenAt)
		}
		return pending[i].dedupeKey < pending[j].dedupeKey
	})
	return pending
}

func (m *VoiceNotificationManager) claimPendingNotificationLocked() (string, string) {
	for _, record := range m.pendingNotificationRecordsLocked(nil) {
		text := m.persistentTextLocked(record)
		if text == "" {
			continue
		}
		text = limitVoiceNotificationText(text, m.config.ResponseTail.MaxTextCharsOrDefault())
		m.nextToken++
		token := fmt.Sprintf("voice-notification-%d", m.nextToken)
		m.deliveries[token] = voiceNotificationDelivery{
			dedupeKey:        record.dedupeKey,
			cycleID:          record.cycleID,
			severitySnapshot: record.currentSeverity,
		}
		record.deliveryState = "in_flight"
		return text, token
	}
	return "", ""
}

// PrepareVoiceNotification is the runtime-level entry point used by realtime
// and other foreground speech consumers.
func (r *Runtime) PrepareVoiceNotification(ctx context.Context) SpokenTextResult {
	if r == nil || r.voiceNotifications == nil {
		return SpokenTextResult{Mode: SpokenTextModeNormal}
	}
	return r.voiceNotifications.PrepareNotification(ctx)
}

func (m *VoiceNotificationManager) Publish(_ context.Context, event VoiceNotificationEvent) error {
	if m == nil {
		return nil
	}
	event.Code = strings.TrimSpace(event.Code)
	event.State = strings.TrimSpace(event.State)
	event.DedupeKey = strings.TrimSpace(event.DedupeKey)
	if err := validateVoiceNotificationEvent(event); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.config.EnabledOrDefault() {
		return nil
	}
	m.pruneExpiredLocked(m.now())

	if event.State == VoiceNotificationResolved {
		delete(m.records, event.DedupeKey)
		for token, delivery := range m.deliveries {
			if delivery.dedupeKey == event.DedupeKey {
				delete(m.deliveries, token)
			}
		}
		return nil
	}

	now := m.now()
	record := m.records[event.DedupeKey]
	if record == nil {
		if len(m.records) >= m.config.MaxPendingOrDefault() {
			return fmt.Errorf("voice notification capacity reached: max_pending=%d", m.config.MaxPendingOrDefault())
		}
		m.nextCycle++
		record = &voiceNotificationRecord{
			code:              event.Code,
			dedupeKey:         event.DedupeKey,
			cycleID:           m.nextCycle,
			currentSeverity:   event.Severity,
			params:            cloneVoiceNotificationParams(event.Params),
			deliveryState:     "pending",
			firstSeenAt:       now,
			lastSeenAt:        now,
			severityChangedAt: now,
			leaseExpiresAt:    m.leaseExpiration(event.Code, now),
		}
		m.records[event.DedupeKey] = record
		return nil
	}
	if record.code != event.Code {
		return fmt.Errorf("voice notification dedupe key %q belongs to code %q, not %q", event.DedupeKey, record.code, event.Code)
	}
	previousSeverity := record.currentSeverity
	record.currentSeverity = event.Severity
	record.params = cloneVoiceNotificationParams(event.Params)
	record.lastSeenAt = now
	record.leaseExpiresAt = m.leaseExpiration(event.Code, now)
	if event.Severity > previousSeverity {
		record.severityChangedAt = now
	} else if event.Severity < previousSeverity {
		record.severityChangedAt = time.Time{}
	}
	if event.Severity != previousSeverity {
		m.refreshDeliveryStateLocked(record)
	}
	return nil
}

func validateVoiceNotificationEvent(event VoiceNotificationEvent) error {
	if event.Code == "" {
		return fmt.Errorf("voice notification code is required")
	}
	if event.State != VoiceNotificationActive && event.State != VoiceNotificationResolved {
		return fmt.Errorf("invalid voice notification state %q", event.State)
	}
	prefix, scope, ok := strings.Cut(event.DedupeKey, ":")
	if !ok || prefix == "" || strings.TrimSpace(scope) == "" {
		return fmt.Errorf("voice notification dedupe key %q must use <Code>:<ScopeID>", event.DedupeKey)
	}
	if prefix != event.Code {
		return fmt.Errorf("voice notification dedupe key %q does not match code %q", event.DedupeKey, event.Code)
	}
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "info", "warning", "critical", "emergency":
		return fmt.Errorf("voice notification dedupe key %q must not contain severity as its scope", event.DedupeKey)
	}
	if event.State == VoiceNotificationActive && event.Severity > SeverityEmergency {
		return fmt.Errorf("invalid voice notification severity %d", event.Severity)
	}
	return nil
}

func (m *VoiceNotificationManager) PrepareSpokenText(_ context.Context, input SpokenTextInput) SpokenTextResult {
	result := SpokenTextResult{Text: input.ResponseText, Mode: SpokenTextModeNormal}
	if m == nil {
		return result
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.config.EnabledOrDefault() {
		return result
	}
	if input.TurnFailure != nil {
		result.Text = m.turnFailureTextLocked(input.TurnFailure)
		result.Mode = SpokenTextModeReplacement
		return result
	}
	if !m.config.ResponseTail.EnabledOrDefault() || m.config.ResponseTail.MaxItems == 0 || !input.TailAppendable || strings.TrimSpace(input.ResponseText) == "" {
		return result
	}

	m.pruneExpiredLocked(m.now())
	pending := make([]*voiceNotificationRecord, 0, len(m.records))
	for _, record := range m.records {
		if record.deliveryState != "pending" {
			continue
		}
		pending = append(pending, record)
	}
	relatedCodes := make(map[string]struct{}, len(input.RelatedCodes))
	for _, code := range input.RelatedCodes {
		if code = strings.TrimSpace(code); code != "" {
			relatedCodes[code] = struct{}{}
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		_, iRelated := relatedCodes[pending[i].code]
		_, jRelated := relatedCodes[pending[j].code]
		if iRelated != jRelated {
			return iRelated
		}
		if pending[i].currentSeverity != pending[j].currentSeverity {
			return pending[i].currentSeverity > pending[j].currentSeverity
		}
		if !pending[i].severityChangedAt.Equal(pending[j].severityChangedAt) {
			return pending[i].severityChangedAt.After(pending[j].severityChangedAt)
		}
		if !pending[i].firstSeenAt.Equal(pending[j].firstSeenAt) {
			return pending[i].firstSeenAt.Before(pending[j].firstSeenAt)
		}
		return pending[i].dedupeKey < pending[j].dedupeKey
	})
	for _, record := range pending {
		tail := m.persistentTextLocked(record)
		if tail == "" {
			continue
		}
		tail = limitVoiceNotificationText(tail, m.config.ResponseTail.MaxTextCharsOrDefault())
		m.nextToken++
		token := fmt.Sprintf("voice-notification-%d", m.nextToken)
		m.deliveries[token] = voiceNotificationDelivery{
			dedupeKey:        record.dedupeKey,
			cycleID:          record.cycleID,
			severitySnapshot: record.currentSeverity,
		}
		record.deliveryState = "in_flight"
		result.Text = appendVoiceNotificationTail(input.ResponseText, tail, m.locale)
		result.Mode = SpokenTextModeTail
		result.DeliveryToken = token
		return result
	}
	return result
}

func (m *VoiceNotificationManager) leaseExpiration(code string, now time.Time) time.Time {
	seconds := m.config.Expiration.DefaultTTLSeconds
	if configured, ok := m.config.Expiration.CodeTTLSeconds[code]; ok {
		seconds = configured
	}
	if seconds <= 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(seconds) * time.Second)
}

func (m *VoiceNotificationManager) pruneExpiredLocked(now time.Time) {
	for dedupeKey, record := range m.records {
		if record.leaseExpiresAt.IsZero() || now.Before(record.leaseExpiresAt) {
			continue
		}
		delete(m.records, dedupeKey)
		for token, delivery := range m.deliveries {
			if delivery.dedupeKey == dedupeKey && delivery.cycleID == record.cycleID {
				delete(m.deliveries, token)
			}
		}
	}
}

func defaultTurnFailureVoiceNotificationText(locale, code string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(locale)), "en") {
		switch strings.TrimSpace(code) {
		case TurnFailureNetworkUnavailable:
			return "The network is unavailable, so I cannot complete this request right now."
		case TurnFailureTokenInsufficient:
			return "The service quota is exhausted, so I cannot complete this request right now."
		case TurnFailureSessionIdle:
			return "Session ended due to inactivity."
		default:
			return "The assistant service is temporarily unavailable. Please try again later."
		}
	}
	switch strings.TrimSpace(code) {
	case TurnFailureNetworkUnavailable:
		return "当前网络不可用，暂时无法完成这个请求。"
	case TurnFailureTokenInsufficient:
		return "当前服务额度不足，暂时无法完成这个请求。"
	case TurnFailureSessionIdle:
		return "长时间无对话，会话已结束。"
	default:
		return "当前智能服务暂时不可用，请稍后再试。"
	}
}

func (m *VoiceNotificationManager) turnFailureTextLocked(failure *TurnFailure) string {
	if failure == nil {
		return ""
	}
	locale := m.locale
	for _, candidate := range voiceNotificationLocaleFallbacks(locale) {
		if text := m.turnFailureTexts[turnFailurePolicyKey{code: strings.TrimSpace(failure.Code), locale: candidate}]; text != "" {
			return renderVoiceNotificationText(text, failure.Params)
		}
	}
	return renderVoiceNotificationText(defaultTurnFailureVoiceNotificationText(locale, failure.Code), failure.Params)
}

func (m *VoiceNotificationManager) ReportDelivery(token string, status DeliveryStatus) {
	if m == nil || token == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneExpiredLocked(m.now())
	delivery, ok := m.deliveries[token]
	if !ok {
		return
	}
	delete(m.deliveries, token)
	record := m.records[delivery.dedupeKey]
	if record == nil || record.cycleID != delivery.cycleID {
		return
	}
	if status == DeliveryCompleted {
		record.deliveryCount++
		record.lastDeliveredAt = m.now()
		if record.deliveredSeverity == nil || delivery.severitySnapshot > *record.deliveredSeverity {
			severity := delivery.severitySnapshot
			record.deliveredSeverity = &severity
		}
	}
	m.refreshDeliveryStateLocked(record)
}

func (m *VoiceNotificationManager) refreshDeliveryStateLocked(record *voiceNotificationRecord) {
	if record == nil {
		return
	}
	for _, delivery := range m.deliveries {
		if delivery.dedupeKey == record.dedupeKey &&
			delivery.cycleID == record.cycleID &&
			delivery.severitySnapshot >= record.currentSeverity {
			record.deliveryState = "in_flight"
			return
		}
	}
	if record.deliveredSeverity == nil || record.currentSeverity > *record.deliveredSeverity {
		record.deliveryState = "pending"
		return
	}
	record.deliveryState = "delivered"
}

func defaultPersistentVoiceNotificationText(locale, code string, severity NotificationSeverity) string {
	if code != "storage" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(locale)), "en") {
		switch severity {
		case SeverityWarning:
			return "Also, your device is running low on storage."
		case SeverityCritical:
			return "Also, your device is critically low on storage. Please clean it up soon."
		case SeverityEmergency:
			return "Also, your device storage is almost full. Please clean it up now."
		default:
			return ""
		}
	}
	switch severity {
	case SeverityWarning:
		return "另外提醒一下，设备存储空间不足。"
	case SeverityCritical:
		return "另外提醒一下，设备存储空间严重不足，请尽快清理。"
	case SeverityEmergency:
		return "另外提醒一下，设备存储空间即将耗尽，请立即清理。"
	default:
		return ""
	}
}

func (m *VoiceNotificationManager) persistentTextLocked(record *voiceNotificationRecord) string {
	if record == nil {
		return ""
	}
	locale := m.locale
	for _, candidate := range voiceNotificationLocaleFallbacks(locale) {
		key := voiceNotificationPolicyKey{code: record.code, severity: record.currentSeverity, locale: candidate}
		if text := m.persistentTexts[key]; text != "" {
			return renderVoiceNotificationText(text, record.params)
		}
	}
	return renderVoiceNotificationText(defaultPersistentVoiceNotificationText(locale, record.code, record.currentSeverity), record.params)
}

func normalizeVoiceNotificationLocale(locale string) string {
	locale = strings.ToLower(strings.TrimSpace(locale))
	locale = strings.ReplaceAll(locale, "_", "-")
	return locale
}

func voiceNotificationLocaleFallbacks(locale string) []string {
	if locale == "" {
		locale = "zh-cn"
	}
	if locale == "zh-cn" {
		return []string{"zh-cn"}
	}
	return []string{locale, "zh-cn"}
}

func renderVoiceNotificationText(text string, params map[string]string) string {
	for key, value := range params {
		text = strings.ReplaceAll(text, "{{"+key+"}}", value)
		text = strings.ReplaceAll(text, "{"+key+"}", value)
	}
	return strings.TrimSpace(text)
}

func appendVoiceNotificationTail(response, tail, locale string) string {
	response = strings.TrimSpace(response)
	tail = strings.TrimSpace(tail)
	if response == "" {
		return tail
	}
	responseRunes := []rune(response)
	last := string(responseRunes[len(responseRunes)-1])
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(locale)), "en") {
		if !strings.ContainsAny(last, ".!?") {
			response += "."
		}
		return response + " " + tail
	}
	if !strings.ContainsAny(last, "。！？.!?") {
		response += "。"
	}
	return response + tail
}

func limitVoiceNotificationText(text string, maxChars int) string {
	if maxChars <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	return string(runes[:maxChars])
}

func cloneVoiceNotificationParams(params map[string]string) map[string]string {
	if len(params) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(params))
	for key, value := range params {
		cloned[key] = value
	}
	return cloned
}

func (m *VoiceNotificationManager) Reconfigure(cfg VoiceNotificationsConfig, locale string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.config = cfg
	m.locale = locale
}
