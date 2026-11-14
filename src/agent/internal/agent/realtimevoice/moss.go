package realtimevoice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"aico-agent/internal/logging"

	"github.com/gorilla/websocket"
)

const (
	DefaultMossLiveEndpoint = "wss://api.mosi.cn/v1/realtime"
	DefaultMossLiveModel    = "moss-live-1.0"
	mossInputSampleRate     = 16000
	mossOutputSampleRate    = 48000
)

// MossProvider is the MOSS-LIVE realtime voice adapter.
// Wire contract: JSON control frames + raw 16 kHz PCM uplink / 48 kHz PCM downlink.
// Playback acknowledgements are required; this adapter sends immediate (fake) acks.
type MossProvider struct {
	Endpoint    string
	Dialer      *websocket.Dialer
	EventBuffer int
}

func (p MossProvider) Open(ctx context.Context, cfg SessionConfig) (Session, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("moss live: APIKey is required")
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = DefaultMossLiveModel
	}
	endpoint, err := p.endpoint(model)
	if err != nil {
		return nil, err
	}
	d := p.Dialer
	if d == nil {
		d = websocket.DefaultDialer
	}
	header := http.Header{}
	header.Set("Authorization", bearer(cfg.APIKey))
	conn, resp, err := d.DialContext(ctx, endpoint, header)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("moss live websocket connect: %w", err)
	}

	inputRate := cfg.InputSampleRate
	if inputRate <= 0 {
		inputRate = mossInputSampleRate
	}
	outputRate := cfg.OutputSampleRate
	if outputRate <= 0 {
		outputRate = mossOutputSampleRate
	}
	buffer := p.EventBuffer
	if buffer <= 0 {
		buffer = 64
	}
	writeGate := make(chan struct{}, 1)
	writeGate <- struct{}{}
	// Build the session.update payload and alias map before readLoop so tool
	// call frames cannot observe a nil toolNameLocal without synchronization.
	update, aliases := buildMossSessionUpdate(cfg)
	s := &mossSession{
		conn:          conn,
		label:         "moss live",
		events:        make(chan Event, buffer),
		errs:          make(chan error, 1),
		done:          make(chan struct{}),
		writeGate:     writeGate,
		ready:         make(chan struct{}),
		updated:       make(chan struct{}),
		toolNameLocal: aliases,
		info: newPCM16SessionInfo(cfg.SessionID, inputRate, outputRate, Capabilities{
			EmitsSpeechEvents:                true,
			ServerAuthoritativeInterruption:  true,
			ServerAuthoritativeTurnDetection: true,
			// Sync client tools continue inside the same run: the adapter sends
			// response.create itself once every result is acknowledged, so the
			// daemon must not wait for a terminal event before continuing.
		}),
	}
	go s.readLoop()

	attach := map[string]any{"action": "attach_session"}
	// Public API accepts client tools only with client delegation.
	if len(cfg.Tools) > 0 {
		attach["delegation"] = map[string]any{"type": "client"}
	}
	if err := s.writeJSON(ctx, attach); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.waitReady(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.writeJSON(ctx, update); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.waitUpdated(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.writeJSON(ctx, map[string]any{"action": "mic_state", "enabled": true}); err != nil {
		_ = s.Close()
		return nil, err
	}
	logging.Infof("agent", "realtime", "Moss Live session ready: session_id=%s model=%s", s.Info().ID, model)
	return s, nil
}

func (p MossProvider) endpoint(model string) (string, error) {
	raw := strings.TrimSpace(p.Endpoint)
	if raw == "" {
		raw = DefaultMossLiveEndpoint
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("moss live: parse endpoint: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("moss live: endpoint must use ws:// or https://, got %q", u.Scheme)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1/realtime"
	}
	q := u.Query()
	q.Set("model", model)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func buildMossSessionUpdate(cfg SessionConfig) (map[string]any, map[string]string) {
	session := map[string]any{}
	// Default persona is intentional for v1; only forward daemon instructions.
	if instructions := strings.TrimSpace(cfg.Instructions); instructions != "" {
		session["instructions"] = instructions
	}
	tools, aliases := buildMossTools(cfg.Tools)
	if len(tools) > 0 {
		session["tools"] = tools
	}
	return map[string]any{"action": "session.update", "session": session}, aliases
}

// mossMaxClientTools is the public MOSS-LIVE limit for client tool
// registrations. session.update returns "invalid session configuration" above
// this count even when each individual tool is otherwise valid.
const mossMaxClientTools = 32

// mossReservedClientToolNames are rejected by the public MOSS-LIVE API when
// registered as client tools (built-in / reserved identifiers). Export them
// under a stable alias and map call names back to the local catalog.
var mossReservedClientToolNames = map[string]string{
	"web_search": "aico_web_search",
	"web-search": "aico_web_search",
}

// mossLowPriorityClientTools are dropped first when the local catalog exceeds
// mossMaxClientTools. Keep voice-core and device-action tools ahead of admin /
// housekeeping entries.
var mossLowPriorityClientTools = []string{
	"skill_mark_used",
	"skill_manage",
	"config_update",
	"config_get",
	"inspect_episode",
	"web_scraper",
	"wikipedia",
	"wait_for_stable_screen",
	"mouse_scroll",
	"mouse_move",
}

func mossWireToolName(local string) string {
	if wire, ok := mossReservedClientToolNames[local]; ok {
		return wire
	}
	return local
}

func buildMossTools(tools []Tool) ([]map[string]any, map[string]string) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]map[string]any, 0, len(tools))
	aliases := map[string]string{}
	for _, tool := range tools {
		local := strings.TrimSpace(tool.Name)
		if local == "" {
			continue
		}
		wire := mossWireToolName(local)
		if wire != local {
			aliases[wire] = local
		}
		entry := map[string]any{
			"type": "function",
			"name": wire,
		}
		if desc := strings.TrimSpace(tool.Description); desc != "" {
			entry["description"] = desc
		}
		if len(tool.Parameters) > 0 {
			entry["parameters"] = json.RawMessage(tool.Parameters)
		}
		out = append(out, entry)
	}
	if trimmed := trimMossClientTools(out); len(trimmed) != len(out) {
		logging.Infof("agent", "realtime", "Moss Live trimmed client tools from %d to %d (API limit %d)", len(out), len(trimmed), mossMaxClientTools)
		out = trimmed
		aliases = filterMossToolAliases(aliases, out)
	}
	if len(aliases) == 0 {
		aliases = nil
	}
	return out, aliases
}

func trimMossClientTools(tools []map[string]any) []map[string]any {
	if len(tools) <= mossMaxClientTools {
		return tools
	}
	drop := make(map[string]struct{}, len(mossLowPriorityClientTools))
	for _, name := range mossLowPriorityClientTools {
		drop[name] = struct{}{}
	}
	kept := make([]map[string]any, 0, mossMaxClientTools)
	deferred := make([]map[string]any, 0)
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		if _, low := drop[name]; low {
			deferred = append(deferred, tool)
			continue
		}
		kept = append(kept, tool)
	}
	if len(kept) > mossMaxClientTools {
		return kept[:mossMaxClientTools]
	}
	for _, tool := range deferred {
		if len(kept) >= mossMaxClientTools {
			break
		}
		kept = append(kept, tool)
	}
	return kept
}

func filterMossToolAliases(aliases map[string]string, tools []map[string]any) map[string]string {
	if len(aliases) == 0 {
		return nil
	}
	present := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if name, _ := tool["name"].(string); name != "" {
			present[name] = struct{}{}
		}
	}
	out := make(map[string]string, len(aliases))
	for wire, local := range aliases {
		if _, ok := present[wire]; ok {
			out[wire] = local
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

type mossChunkRef struct {
	ChunkID string
	RunID   string
	TurnID  string
}

type mossSession struct {
	conn   *websocket.Conn
	label  string
	events chan Event
	errs   chan error
	done   chan struct{}

	infoMu sync.RWMutex
	info   SessionInfo

	writeGate chan struct{}
	closeOnce sync.Once
	closeErr  error

	ready       chan struct{}
	readyOnce   sync.Once
	updated     chan struct{}
	updatedOnce sync.Once
	updateErr   string

	ctxMu                 sync.Mutex
	audioContexts         []mossChunkRef
	runID                 string
	turnID                string
	lastInputText         string
	lastOutputText        string
	responseStarted       bool
	audioDone             bool
	playbackCompletedSent bool
	// suppressChunkAcks is set after playback.stopped so a concurrent write
	// cannot emit a late playback.chunk.completed for the interrupted turn.
	suppressChunkAcks bool
	// interruptedRunID is the run that issued playback.stopped. Late frames
	// from that run must not clear suppressChunkAcks via beginNewResponse.
	interruptedRunID string
	// toolCalls tracks the current run's client tool calls by call_id. A sync
	// tool keeps the run open until response.create names every call whose
	// result the server acknowledged with conversation.item.created. Entries
	// stay until the run ends so a late response.done cannot replay a call.
	toolCalls map[string]mossToolCallState
	// toolNameLocal maps Moss wire tool names back to the local catalog
	// when a reserved identifier had to be aliased for session.update.
	toolNameLocal map[string]string
}

func (s *mossSession) Info() SessionInfo {
	s.infoMu.RLock()
	defer s.infoMu.RUnlock()
	return s.info
}

func (s *mossSession) Events() <-chan Event  { return s.events }
func (s *mossSession) Errors() <-chan error  { return s.errs }
func (s *mossSession) Done() <-chan struct{} { return s.done }

func (s *mossSession) SendAudio(ctx context.Context, pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}
	return s.writeBinary(ctx, pcm)
}

// SendToolResult returns a client tool result. Probed wire shape matches the
// OpenAI-style conversation.item.create / function_call_output envelope,
// wrapped in Moss's action field.
func (s *mossSession) SendToolResult(ctx context.Context, id, output string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%s: empty tool call id", s.label)
	}
	return s.writeJSON(ctx, map[string]any{
		"action": "conversation.item.create",
		"item": map[string]any{
			"type":    "function_call_output",
			"call_id": id,
			"output":  output,
		},
	})
}

type mossToolCallState uint8

const (
	// mossToolCallPending waits for its acknowledged result.
	mossToolCallPending mossToolCallState = iota
	// mossToolCallAcknowledged has a result confirmed by conversation.item.created.
	mossToolCallAcknowledged
	// mossToolCallContinued was already named in a response.create.
	mossToolCallContinued
)

// continueAfterToolResult records the server's acknowledgement of a tool
// result. When no call of the current run is still waiting for its result, it
// returns the call IDs for response.create, which resumes the same run.
func (s *mossSession) continueAfterToolResult(callID string) []string {
	s.ctxMu.Lock()
	defer s.ctxMu.Unlock()
	if state, known := s.toolCalls[callID]; !known || state != mossToolCallPending {
		return nil
	}
	s.toolCalls[callID] = mossToolCallAcknowledged
	callIDs := make([]string, 0, len(s.toolCalls))
	for id, state := range s.toolCalls {
		switch state {
		case mossToolCallPending:
			return nil
		case mossToolCallAcknowledged:
			callIDs = append(callIDs, id)
		}
	}
	for _, id := range callIDs {
		s.toolCalls[id] = mossToolCallContinued
	}
	sort.Strings(callIDs)
	return callIDs
}

func (s *mossSession) sendResponseCreate(callIDs []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.writeJSON(ctx, map[string]any{
		"action":   "response.create",
		"response": map[string]any{"call_ids": callIDs},
	}); err != nil {
		logging.Warnf("agent", "realtime", "Moss Live response.create failed: call_ids=%v err=%v", callIDs, err)
	}
}

// toolCallEvent registers a tool call once per call_id. Moss announces a call
// in response.function_call_arguments.done and may repeat it in response.done.
func (s *mossSession) toolCallEvent(responseID, itemID, callID, name, arguments string) (Event, bool) {
	if callID == "" {
		return Event{}, false
	}
	s.ctxMu.Lock()
	if _, seen := s.toolCalls[callID]; seen {
		s.ctxMu.Unlock()
		return Event{}, false
	}
	if s.toolCalls == nil {
		s.toolCalls = make(map[string]mossToolCallState)
	}
	s.toolCalls[callID] = mossToolCallPending
	s.ctxMu.Unlock()
	if local, ok := s.toolNameLocal[name]; ok {
		name = local
	}
	return Event{
		Kind:       EventToolCall,
		ResponseID: responseID,
		ItemID:     itemID,
		CallID:     callID,
		Name:       name,
		Arguments:  arguments,
	}, true
}

func (s *mossSession) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		s.closeErr = s.conn.Close()
	})
	return s.closeErr
}

func (s *mossSession) ensureChannels() {
	if s.ready == nil {
		s.ready = make(chan struct{})
	}
	if s.updated == nil {
		s.updated = make(chan struct{})
	}
	if s.writeGate == nil {
		s.writeGate = make(chan struct{}, 1)
		s.writeGate <- struct{}{}
	}
}

func (s *mossSession) waitReady(ctx context.Context) error {
	s.ensureChannels()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("%s: timed out waiting for session_attached", s.label)
	case err := <-s.errs:
		if err != nil {
			return err
		}
		return fmt.Errorf("%s: event stream closed while waiting for session_attached", s.label)
	case <-s.ready:
		return nil
	case <-s.done:
		return fmt.Errorf("%s: session closed while waiting for session_attached", s.label)
	}
}

func (s *mossSession) waitUpdated(ctx context.Context) error {
	s.ensureChannels()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("%s: timed out waiting for session.updated", s.label)
	case err := <-s.errs:
		if err != nil {
			return err
		}
		return fmt.Errorf("%s: event stream closed while waiting for session.updated", s.label)
	case <-s.updated:
		if strings.TrimSpace(s.updateErr) != "" {
			return fmt.Errorf("%s: session.updated error: %s", s.label, s.updateErr)
		}
		return nil
	case <-s.done:
		return fmt.Errorf("%s: session closed while waiting for session.updated", s.label)
	}
}

func (s *mossSession) writeJSON(ctx context.Context, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%s: marshal event: %w", s.label, err)
	}
	return s.writeMessage(ctx, websocket.TextMessage, body)
}

func (s *mossSession) writeBinary(ctx context.Context, pcm []byte) error {
	return s.writeMessage(ctx, websocket.BinaryMessage, pcm)
}

func (s *mossSession) writeMessage(ctx context.Context, messageType int, payload []byte) error {
	if ctx == nil {
		return fmt.Errorf("%s: nil context", s.label)
	}
	s.ensureChannels()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return fmt.Errorf("%s: session is closed", s.label)
	case <-s.writeGate:
	}
	defer func() { s.writeGate <- struct{}{} }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return fmt.Errorf("%s: session is closed", s.label)
	default:
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = s.conn.Close() })
	defer stopCancel()
	deadline := time.Now().Add(10 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = s.conn.SetWriteDeadline(deadline)
	defer s.conn.SetWriteDeadline(time.Time{})
	if err := s.conn.WriteMessage(messageType, payload); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("%s websocket write: %w", s.label, err)
	}
	return nil
}

func (s *mossSession) readLoop() {
	defer close(s.events)
	defer close(s.errs)
	defer s.closeOnce.Do(func() { close(s.done); _ = s.conn.Close() })
	for {
		messageType, body, err := s.conn.ReadMessage()
		if err != nil {
			if !normalClose(err) && !errors.Is(err, context.Canceled) {
				select {
				case s.errs <- classifyTransportError(err):
				default:
				}
			}
			return
		}
		var events []Event
		switch messageType {
		case websocket.BinaryMessage:
			events = s.onBinaryAudio(body)
		case websocket.TextMessage:
			events = s.onJSON(body)
		default:
			continue
		}
		for _, event := range events {
			select {
			case s.events <- event:
			case <-s.done:
				return
			}
		}
	}
}

func (s *mossSession) onBinaryAudio(body []byte) []Event {
	// Voice-only sessions use untagged downlink PCM S16LE at 48 kHz.
	if len(body) == 0 {
		return nil
	}
	ref := s.takeAudioContext()
	s.ctxMu.Lock()
	drop := ref.RunID != "" && ref.RunID == s.interruptedRunID
	s.ctxMu.Unlock()
	if drop {
		// Late PCM for a barged-in run: do not restart playback or ack as played.
		return nil
	}
	events := make([]Event, 0, 2)
	if !s.responseStarted {
		if !s.beginNewResponse(ref.RunID) {
			return nil
		}
		events = append(events, Event{Kind: EventResponseStarted, ResponseID: ref.RunID})
	}
	events = append(events, Event{
		Kind:       EventAudio,
		ResponseID: ref.RunID,
		ItemID:     ref.ChunkID,
		PCM:        body,
	})
	// Immediate (fake) ack on the reader goroutine so every chunk is acknowledged
	// before the next server frame is processed.
	s.sendChunkAck(ref)
	s.maybeSendPlaybackCompleted()
	return events
}

func (s *mossSession) takeAudioContext() mossChunkRef {
	s.ctxMu.Lock()
	defer s.ctxMu.Unlock()
	if len(s.audioContexts) == 0 {
		return mossChunkRef{RunID: s.runID, TurnID: s.turnID, ChunkID: fmt.Sprintf("orphan-%d", time.Now().UnixNano())}
	}
	ref := s.audioContexts[0]
	s.audioContexts = s.audioContexts[1:]
	return ref
}

func (s *mossSession) sendChunkAck(ref mossChunkRef) {
	s.ctxMu.Lock()
	suppress := s.suppressChunkAcks || (ref.RunID != "" && ref.RunID == s.interruptedRunID)
	s.ctxMu.Unlock()
	if suppress {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := s.writeJSON(ctx, map[string]any{
		"action":   "playback.chunk.completed",
		"chunk_id": ref.ChunkID,
		"run_id":   ref.RunID,
		"turn_id":  ref.TurnID,
	})
	cancel()
	if err != nil {
		logging.Warnf("agent", "realtime", "Moss Live playback chunk ack failed: chunk_id=%s err=%v", ref.ChunkID, err)
	}
}

func (s *mossSession) sendPlaybackStopped(runID, turnID string, playedAudioMs int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := s.writeJSON(ctx, map[string]any{
		"action":          "playback.stopped",
		"run_id":          firstNonEmpty(runID, s.runID),
		"turn_id":         firstNonEmpty(turnID, s.turnID),
		"played_audio_ms": playedAudioMs,
	})
	cancel()
	if err != nil {
		logging.Warnf("agent", "realtime", "Moss Live playback.stopped failed: err=%v", err)
	}
}

func (s *mossSession) maybeSendPlaybackCompleted() {
	s.ctxMu.Lock()
	done := s.audioDone && len(s.audioContexts) == 0 && !s.playbackCompletedSent && !s.suppressChunkAcks
	runID := s.runID
	turnID := s.turnID
	if done {
		s.playbackCompletedSent = true
	}
	s.ctxMu.Unlock()
	if !done || runID == "" || turnID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.writeJSON(ctx, map[string]any{
		"action":  "playback.completed",
		"run_id":  runID,
		"turn_id": turnID,
	}); err != nil {
		logging.Warnf("agent", "realtime", "Moss Live playback.completed failed: err=%v", err)
	}
}

func (s *mossSession) onJSON(body []byte) []Event {
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		return []Event{{Kind: EventError, Error: fmt.Errorf("moss live: decode event: %w", err)}}
	}
	action, _ := msg["action"].(string)
	data, _ := msg["data"].(map[string]any)
	if data == nil {
		data = map[string]any{}
	}
	switch action {
	case "session_attached":
		sessionID, _ := data["session_id"].(string)
		if sessionID != "" {
			s.infoMu.Lock()
			s.info.ID = sessionID
			s.infoMu.Unlock()
		}
		s.ensureChannels()
		s.readyOnce.Do(func() { close(s.ready) })
		// Handshake is consumed by waitReady; do not emit EventReady into the daemon stream.
		return nil
	case "session.updated":
		if errVal, ok := data["error"]; ok {
			s.updateErr = fmt.Sprint(errVal)
			if s.updateErr == "<nil>" {
				s.updateErr = ""
			}
		}
		s.ensureChannels()
		s.updatedOnce.Do(func() { close(s.updated) })
		if strings.TrimSpace(s.updateErr) != "" {
			return []Event{{Kind: EventError, Error: fmt.Errorf("moss live: session.updated: %s", s.updateErr)}}
		}
		return nil
	case "input.speech.started":
		s.lastInputText = ""
		return []Event{{Kind: EventSpeechStarted}}
	case "input.speech.stopped":
		return []Event{{Kind: EventSpeechStopped}}
	case "response.audio.started":
		runID, _ := data["run_id"].(string)
		turnID, _ := data["turn_id"].(string)
		s.rememberRun(runID, turnID)
		events := make([]Event, 0, 1)
		if !s.responseStarted && s.beginNewResponse(runID) {
			events = append(events, Event{Kind: EventResponseStarted, ResponseID: runID})
		}
		return events
	case "input.transcript.updated":
		text, _ := data["text"].(string)
		delta := mossTextDelta(s.lastInputText, text)
		s.lastInputText = text
		if delta == "" {
			return nil
		}
		return []Event{{Kind: EventTranscriptDelta, Role: "user", Text: delta, TextSource: "audio"}}
	case "input.transcript.done":
		text, _ := data["text"].(string)
		s.lastInputText = text
		return []Event{{Kind: EventTranscriptFinal, Role: "user", Text: text, TextSource: "audio", Final: true}}
	case "output.transcript.updated":
		text, _ := data["text"].(string)
		runID, _ := data["run_id"].(string)
		turnID, _ := data["turn_id"].(string)
		s.rememberRun(runID, turnID)
		delta := mossTextDelta(s.lastOutputText, text)
		s.lastOutputText = text
		events := make([]Event, 0, 2)
		if !s.responseStarted && s.beginNewResponse(runID) {
			events = append(events, Event{Kind: EventResponseStarted, ResponseID: runID})
		}
		if delta != "" {
			events = append(events, Event{Kind: EventTranscriptDelta, ResponseID: runID, Role: "assistant", Text: delta, TextSource: "audio"})
		}
		return events
	case "output.transcript.done":
		text, _ := data["text"].(string)
		runID, _ := data["run_id"].(string)
		turnID, _ := data["turn_id"].(string)
		s.rememberRun(runID, turnID)
		s.lastOutputText = text
		events := make([]Event, 0, 2)
		if !s.responseStarted && s.beginNewResponse(runID) {
			events = append(events, Event{Kind: EventResponseStarted, ResponseID: runID})
		}
		events = append(events, Event{Kind: EventTranscriptFinal, ResponseID: runID, Role: "assistant", Text: text, TextSource: "audio", Final: true})
		return events
	case "response.audio.context":
		chunkID, _ := data["chunk_id"].(string)
		runID, _ := data["run_id"].(string)
		turnID, _ := data["turn_id"].(string)
		s.rememberRun(runID, turnID)
		events := make([]Event, 0, 1)
		if !s.responseStarted && s.beginNewResponse(runID) {
			events = append(events, Event{Kind: EventResponseStarted, ResponseID: runID})
		}
		s.ctxMu.Lock()
		// Keep dropping the barged-in run even after a newer response has
		// cleared suppressChunkAcks, otherwise late frames re-ack it.
		drop := runID != "" && runID == s.interruptedRunID
		if !drop {
			s.audioContexts = append(s.audioContexts, mossChunkRef{ChunkID: chunkID, RunID: runID, TurnID: turnID})
		}
		s.ctxMu.Unlock()
		return events
	case "response.audio.done":
		runID, _ := data["run_id"].(string)
		turnID, _ := data["turn_id"].(string)
		s.rememberRun(runID, turnID)
		s.ctxMu.Lock()
		s.audioDone = true
		s.ctxMu.Unlock()
		s.maybeSendPlaybackCompleted()
		return nil
	case "response.audio.stop", "response.interrupted":
		// Both stop playback on the client. MOSS requires playback.stopped so
		// the server does not keep treating the turn as still playing.
		runID, _ := data["run_id"].(string)
		turnID, _ := data["turn_id"].(string)
		s.rememberRun(runID, turnID)
		s.ctxMu.Lock()
		s.suppressChunkAcks = true
		s.interruptedRunID = firstNonEmpty(runID, s.runID)
		s.ctxMu.Unlock()
		s.sendPlaybackStopped(runID, turnID, 0)
		s.resetResponseTurn()
		return []Event{{Kind: EventInterruption, ResponseID: runID, At: "assistant"}}
	case "response.function_call_arguments.done":
		callID, _ := data["call_id"].(string)
		name, _ := data["name"].(string)
		arguments, _ := data["arguments"].(string)
		responseID, _ := data["response_id"].(string)
		itemID, _ := data["item_id"].(string)
		s.rememberRun(responseID, "")
		events := make([]Event, 0, 2)
		if !s.responseStarted && s.beginNewResponse(responseID) {
			events = append(events, Event{Kind: EventResponseStarted, ResponseID: responseID})
		}
		if event, ok := s.toolCallEvent(responseID, itemID, callID, name, arguments); ok {
			events = append(events, event)
		}
		return events
	case "response.done":
		// A summary of the run's function calls, not a terminal event: the run
		// stays open until response.ended. Only calls not already announced by
		// response.function_call_arguments.done are new.
		response, _ := data["response"].(map[string]any)
		responseID, _ := response["id"].(string)
		output, _ := response["output"].([]any)
		var events []Event
		for _, raw := range output {
			item, _ := raw.(map[string]any)
			if itemType, _ := item["type"].(string); itemType != "function_call" {
				continue
			}
			callID, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			arguments, _ := item["arguments"].(string)
			itemID, _ := item["item_id"].(string)
			event, ok := s.toolCallEvent(responseID, itemID, callID, name, arguments)
			if !ok {
				continue
			}
			// A call first announced here still opens its response, as in
			// response.function_call_arguments.done, so the daemon admits
			// the continued answer and its terminal event.
			s.rememberRun(responseID, "")
			if !s.responseStarted && s.beginNewResponse(responseID) {
				events = append(events, Event{Kind: EventResponseStarted, ResponseID: responseID})
			}
			events = append(events, event)
		}
		return events
	case "conversation.item.created":
		item, _ := data["item"].(map[string]any)
		if itemType, _ := item["type"].(string); itemType != "function_call_output" {
			return nil
		}
		callID, _ := item["call_id"].(string)
		if callIDs := s.continueAfterToolResult(callID); len(callIDs) > 0 {
			s.sendResponseCreate(callIDs)
		}
		return nil
	case "response.ended":
		status, _ := data["status"].(string)
		runID, _ := data["run_id"].(string)
		turnID, _ := data["turn_id"].(string)
		s.rememberRun(runID, turnID)
		s.ctxMu.Lock()
		s.audioDone = true
		s.ctxMu.Unlock()
		s.maybeSendPlaybackCompleted()
		s.resetResponseTurn()
		return []Event{terminalResponseEvent("moss", runID, normalizeMossResponseStatus(status), Usage{})}
	case "error":
		code := digString(data, "error", "code")
		message := digString(data, "error", "message")
		if message == "" {
			message = "moss live error"
		}
		if code != "" {
			message = code + ": " + message
		}
		return []Event{{Kind: EventError, Error: errors.New(message)}}
	default:
		logging.Debugf("agent", "realtime", "Moss Live ignored action=%s", action)
		return nil
	}
}

func (s *mossSession) rememberRun(runID, turnID string) {
	s.ctxMu.Lock()
	defer s.ctxMu.Unlock()
	if runID != "" {
		s.runID = runID
	}
	if turnID != "" {
		s.turnID = turnID
	}
}

func (s *mossSession) resetResponseTurn() {
	s.ctxMu.Lock()
	s.responseStarted = false
	s.lastOutputText = ""
	s.audioContexts = nil
	s.audioDone = false
	s.playbackCompletedSent = false
	// Results for an ended or interrupted run must not continue it.
	s.toolCalls = nil
	// Keep suppressChunkAcks until the next response starts so trailing binary
	// frames after barge-in cannot emit late playback.chunk.completed.
	s.ctxMu.Unlock()
}

// beginNewResponse arms a new assistant turn. It returns false when the frame
// still belongs to an interrupted run, so late audio/transcript events cannot
// clear suppressChunkAcks and re-ack after playback.stopped.
func (s *mossSession) beginNewResponse(runID string) bool {
	s.ctxMu.Lock()
	defer s.ctxMu.Unlock()
	interrupted := s.interruptedRunID
	if runID != "" && runID == interrupted {
		return false
	}
	if s.suppressChunkAcks && runID == "" {
		return false
	}
	s.suppressChunkAcks = false
	// Keep interruptedRunID so late frames from the barged-in run stay dropped
	// after the next turn starts.
	s.playbackCompletedSent = false
	s.audioDone = false
	s.responseStarted = true
	if runID != "" {
		s.runID = runID
	}
	return true
}

func mossTextDelta(prev, next string) string {
	if next == "" {
		return ""
	}
	if strings.HasPrefix(next, prev) {
		return strings.TrimPrefix(next, prev)
	}
	// Non-prefix replacement: emit whole text as a fresh delta chunk.
	if !utf8.ValidString(next) {
		return next
	}
	return next
}

func digString(m map[string]any, keys ...string) string {
	cur := any(m)
	for _, key := range keys {
		obj, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = obj[key]
	}
	s, _ := cur.(string)
	return s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// normalizeMossResponseStatus maps Moss turn statuses onto the shared
// terminal-event vocabulary. Moss uses aborted/failed for finished turns that
// should not tear down the WebSocket session.
func normalizeMossResponseStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "aborted", "failed":
		return "cancelled"
	default:
		return status
	}
}
