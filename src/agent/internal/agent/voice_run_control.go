package agent

import (
	"aico-agent/internal/logging"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

type VoiceTurnContext struct {
	RuntimeContext string
}

type queuedVoiceSteer struct {
	requestID     string
	contentLength int
	interrupted   bool
}

type voiceRunControl struct {
	mu              sync.Mutex
	activeRequestID string
	acceptingSteer  bool
	pendingSteer    RunSteerMessage
	hasPendingSteer bool
	interrupt       voiceSteerInterruptState
	inactive        chan struct{}
}

type voiceSteerInterruptState struct {
	signal  chan struct{}
	ready   chan struct{}
	active  bool
	done    bool
	steer   RunSteerMessage
	hasText bool
}

func normalizeVoiceTurnContext(turnContext VoiceTurnContext) VoiceTurnContext {
	return VoiceTurnContext{
		RuntimeContext: strings.TrimSpace(turnContext.RuntimeContext),
	}
}

func createVoiceRequestID() string {
	return fmt.Sprintf("voice-%x", time.Now().UnixNano())
}

func (c *voiceRunControl) begin(requestID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activeRequestID != "" {
		return false
	}
	c.inactive = make(chan struct{})
	c.activeRequestID = requestID
	c.acceptingSteer = true
	c.clearPendingLocked()
	c.interrupt = voiceSteerInterruptState{signal: make(chan struct{})}
	return true
}

func (c *voiceRunControl) end(requestID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activeRequestID != requestID {
		return
	}
	c.resolveInterruptLocked(RunSteerMessage{}, false)
	c.activeRequestID = ""
	c.acceptingSteer = false
	c.clearPendingLocked()
	c.interrupt = voiceSteerInterruptState{}
	if c.inactive != nil {
		close(c.inactive)
		c.inactive = nil
	}
}

func (c *voiceRunControl) forceReset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resolveInterruptLocked(RunSteerMessage{}, false)
	c.activeRequestID = ""
	c.acceptingSteer = false
	c.clearPendingLocked()
	c.interrupt = voiceSteerInterruptState{}
	if c.inactive != nil {
		close(c.inactive)
		c.inactive = nil
	}
}

func (c *voiceRunControl) waitUntilInactive(ctx context.Context) bool {
	if ctx == nil {
		ctx = context.Background()
	}

	c.mu.Lock()
	inactive := c.inactive
	active := c.activeRequestID != ""
	c.mu.Unlock()
	if !active || inactive == nil {
		return true
	}

	select {
	case <-ctx.Done():
		return false
	case <-inactive:
		return true
	}
}

func (c *voiceRunControl) queueSteer(content string) (queuedVoiceSteer, bool) {
	content = strings.TrimSpace(content)
	if content == "" {
		return queuedVoiceSteer{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activeRequestID == "" || !c.acceptingSteer {
		return queuedVoiceSteer{}, false
	}
	steer := RunSteerMessage{
		ID:        createSteerID(),
		RequestID: c.activeRequestID,
		Content:   content,
		Timestamp: time.Now(),
	}
	queued := queuedVoiceSteer{
		requestID:     c.activeRequestID,
		contentLength: len(content),
	}
	if c.interrupt.active && !c.interrupt.done {
		c.resolveInterruptLocked(steer, true)
		queued.interrupted = true
		return queued, true
	}
	c.pendingSteer = steer
	c.hasPendingSteer = true
	return queued, true
}

func (c *voiceRunControl) beginInterrupt() (string, bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activeRequestID == "" || !c.acceptingSteer {
		return "", false, false
	}
	if c.interrupt.active {
		return c.activeRequestID, false, true
	}
	if c.interrupt.signal == nil {
		c.interrupt.signal = make(chan struct{})
	}
	c.interrupt.ready = make(chan struct{})
	c.interrupt.active = true
	c.interrupt.done = false
	c.interrupt.steer = RunSteerMessage{}
	c.interrupt.hasText = false
	close(c.interrupt.signal)
	return c.activeRequestID, true, true
}

func (c *voiceRunControl) resumeInterrupt() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activeRequestID == "" || !c.acceptingSteer || !c.interrupt.active {
		return "", false
	}
	requestID := c.activeRequestID
	c.resolveInterruptLocked(RunSteerMessage{}, false)
	return requestID, true
}

func (c *voiceRunControl) consumePending(requestID string) (RunSteerMessage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if requestID == "" {
		return RunSteerMessage{}, false
	}
	if c.activeRequestID != requestID {
		return RunSteerMessage{}, false
	}

	// Check interrupt steer first (interruption scenario)
	if c.interrupt.active && c.interrupt.done && c.interrupt.hasText {
		steer := c.interrupt.steer
		c.resetInterruptLocked()
		// Clear any stale pending steer to prevent old instruction from executing after new one
		c.clearPendingLocked()
		logging.Infof("agent", "steer", "consumePending: interrupt steer consumed (length=%d)", len(steer.Content))
		return steer, true
	}

	// Check pending steer next (normal scenario)
	if !c.hasPendingSteer {
		return RunSteerMessage{}, false
	}
	logging.Infof("agent", "steer", "consumePending: pending steer consumed (length=%d)", len(c.pendingSteer.Content))
	return c.consumePendingLocked()
}

func (c *voiceRunControl) consumeFinalPending(requestID string) (RunSteerMessage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if requestID == "" || c.activeRequestID != requestID || !c.acceptingSteer {
		return RunSteerMessage{}, false
	}
	if c.hasPendingSteer {
		return c.consumePendingLocked()
	}
	c.closeAcceptanceLocked()
	return RunSteerMessage{}, false
}

func (c *voiceRunControl) stopAccepting(requestID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if requestID == "" || c.activeRequestID != requestID {
		return
	}
	c.closeAcceptanceLocked()
}

func (c *voiceRunControl) interruptChannel(requestID string) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if requestID == "" || c.activeRequestID != requestID || !c.acceptingSteer {
		return nil
	}
	return c.interrupt.signal
}

func (c *voiceRunControl) waitForInterrupt(ctx context.Context, requestID string) (RunSteerMessage, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if requestID == "" || c.activeRequestID != requestID || !c.acceptingSteer || !c.interrupt.active {
		c.mu.Unlock()
		return RunSteerMessage{}, false, nil
	}
	ready := c.interrupt.ready
	c.mu.Unlock()
	if ready == nil {
		return RunSteerMessage{}, false, nil
	}

	select {
	case <-ctx.Done():
		return RunSteerMessage{}, false, ctx.Err()
	case <-ready:
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if requestID == "" || c.activeRequestID != requestID || !c.acceptingSteer {
		return RunSteerMessage{}, false, nil
	}
	steer := c.interrupt.steer
	hasText := c.interrupt.hasText
	c.resetInterruptLocked()
	return steer, hasText, nil
}

func (c *voiceRunControl) consumePendingLocked() (RunSteerMessage, bool) {
	steer := c.pendingSteer
	c.clearPendingLocked()
	return steer, true
}

func (c *voiceRunControl) isActiveRequest(requestID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeRequestID != "" && c.activeRequestID == requestID
}

func (c *voiceRunControl) closeAcceptanceLocked() {
	c.acceptingSteer = false
	c.clearPendingLocked()
	c.resolveInterruptLocked(RunSteerMessage{}, false)
}

func (c *voiceRunControl) clearPendingLocked() {
	c.pendingSteer = RunSteerMessage{}
	c.hasPendingSteer = false
}

func (c *voiceRunControl) resolveInterruptLocked(steer RunSteerMessage, hasText bool) {
	if !c.interrupt.active || c.interrupt.done {
		return
	}
	c.interrupt.steer = steer
	c.interrupt.hasText = hasText
	c.interrupt.done = true
	if c.interrupt.ready != nil {
		close(c.interrupt.ready)
	}
}

func (c *voiceRunControl) resetInterruptLocked() {
	c.interrupt = voiceSteerInterruptState{signal: make(chan struct{})}
}

func steerContentFromTurnInput(input TurnInput) string {
	input = normalizeTurnInput(input)
	if content := strings.TrimSpace(input.InputText); content != "" {
		if content == voiceAudioInputPlaceholder && strings.TrimSpace(input.Transcript) == "" {
			return ""
		}
		return content
	}
	return strings.TrimSpace(input.Transcript)
}
