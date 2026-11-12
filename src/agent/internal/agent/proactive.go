package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"aico-agent/internal/agent/agentpath"
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/stream"
)

// messageTypeProactive is the public history type of an assistant message the
// Agent started on its own, outside any user turn.
const messageTypeProactive = "proactive"

// Proactive delivery outcomes. Every proactive message is first written to the
// conversation history; delivery only decides how the user is told about it.
const (
	// ProactiveDeliveryChat: the companion App is in the foreground, so the
	// message appears in the chat page without a notification.
	ProactiveDeliveryChat = "chat"
	// ProactiveDeliveryNotification: the App is not in the foreground and a
	// local notification carrying the message was posted on the phone.
	ProactiveDeliveryNotification = "notification"
	// ProactiveDeliveryDeferred: no notification route is available. The
	// message waits in the history until the user next opens the chat.
	ProactiveDeliveryDeferred = "deferred"
)

const (
	proactiveMaxContentBytes        = 4000
	proactiveNotificationTitle      = "Aiden"
	proactiveNotificationBodyRunes  = 200
	proactiveRunLockTimeout         = 2 * time.Minute
	proactiveNotificationSendBudget = 15 * time.Second
)

var errProactiveRealtimeUnsupported = errors.New("proactive messages are not supported while input_mode=realtime")

// ProactiveResult reports where a proactive message was stored and how it was
// delivered.
type ProactiveResult struct {
	Delivery string  `json:"delivery"`
	Message  Message `json:"message"`
	// Error explains why a notification attempt fell back to deferred.
	Error string `json:"error,omitempty"`
}

// AppendProactiveMessage appends an Agent-initiated assistant message to the
// backend conversation. It waits for a running task to finish so the message
// never splits a tool call from its result, and it stays an ordinary assistant
// message for the model so a later reply from the user has its context.
func (r *Runtime) AppendProactiveMessage(ctx context.Context, content string) (Message, error) {
	if r == nil {
		return Message{}, fmt.Errorf("runtime is unavailable")
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return Message{}, fmt.Errorf("content is required")
	}
	if len(content) > proactiveMaxContentBytes {
		return Message{}, fmt.Errorf("content exceeds %d bytes", proactiveMaxContentBytes)
	}
	unlock, err := r.lockRun(ctx)
	if err != nil {
		return Message{}, fmt.Errorf("wait for running task: %w", err)
	}
	defer unlock()

	// Read the mode under the run gate: config apply holds it while it
	// writes the config, so the mode cannot change before the append.
	cfg := r.ConfigSnapshot()
	// Realtime mode displays the user context, which the live voice session
	// owns and writes through its own manager.
	if cfg.InputModeOrDefault() == "realtime" {
		return Message{}, errProactiveRealtimeUnsupported
	}

	if r.contextManager == nil {
		folder := agentpath.ContextManagerSessionFolder(cfg.ConfigDir)
		recovered, err := recoverPendingBackendRunManager(folder, nil)
		if err != nil {
			return Message{}, err
		}
		if recovered != nil {
			recovered.AddAppendMessageHook(r.getStateHook())
			r.contextManager = recovered
		} else {
			manager, err := r.initializeContextManager(cfg, r.getSystemPrompt())
			if err != nil {
				return Message{}, err
			}
			r.contextManager = manager
		}
	}

	appended, err := r.contextManager.AppendMessagesWithResult([]messages.Message{{
		Role:      messages.MessageRoleAssistant,
		Content:   content,
		Proactive: true,
	}})
	if err != nil {
		return Message{}, fmt.Errorf("append proactive message: %w", err)
	}
	for i, item := range appended.Messages {
		if !item.Proactive {
			continue
		}
		message, ok := webMessageFromContextMessage(item, "backend")
		if !ok {
			break
		}
		message.RequestID = fmt.Sprintf("context-backend-%d", appended.Start+i)
		return message, nil
	}
	return Message{}, fmt.Errorf("append proactive message: message was not stored")
}

// DeliverProactiveMessage stores content in the conversation history, tells
// open chat views to reload, and posts a phone notification when the companion
// App is not in the foreground.
func (s *Server) DeliverProactiveMessage(ctx context.Context, content string) (ProactiveResult, error) {
	if s == nil || s.runtime == nil {
		return ProactiveResult{}, fmt.Errorf("server is unavailable")
	}
	message, err := s.runtime.AppendProactiveMessage(ctx, content)
	if err != nil {
		return ProactiveResult{}, err
	}
	s.announceProactiveMessage(message)

	result := ProactiveResult{Message: message}
	bridge := s.runtime.PhoneBridge()
	if bridge == nil {
		result.Delivery = ProactiveDeliveryDeferred
		return result, nil
	}
	status := bridge.getStatus()
	switch {
	case proactiveAppInForeground(status):
		result.Delivery = ProactiveDeliveryChat
	case proactiveNotificationRouteAvailable(ctx, bridge, status):
		if err := sendProactiveNotification(ctx, bridge, message); err != nil {
			result.Delivery = ProactiveDeliveryDeferred
			result.Error = err.Error()
		} else {
			result.Delivery = ProactiveDeliveryNotification
		}
	default:
		result.Delivery = ProactiveDeliveryDeferred
	}
	if s.logger != nil {
		s.logger.Info("proactive message %s delivered via %s", message.RequestID, result.Delivery)
	}
	return result, nil
}

// announceProactiveMessage tells both event streams that the history gained a
// message outside a run, so a visible chat view shows it immediately.
func (s *Server) announceProactiveMessage(message Message) {
	s.BroadcastMessage(message)
	if s.semanticEvents == nil {
		return
	}
	event := stream.NewEvent(stream.EventIdentity{ItemID: message.RequestID}, stream.EventMessageAppended, message)
	event.Visibility = stream.VisibilityUser
	if err := s.semanticEvents.Publish(event); err != nil && s.logger != nil {
		s.logger.Warn("publish proactive message event: %v", err)
	}
}

// proactiveAppInForeground reports whether the user is in the App, where the
// chat page itself shows the message.
func proactiveAppInForeground(status PhoneBridgeStatus) bool {
	return status.Connected && strings.EqualFold(strings.TrimSpace(status.AppState), "active")
}

// proactiveNotificationRouteAvailable reports whether notification_send can
// reach the phone without bringing the App to the foreground. Restoring the
// App through its Dynamic Island entry would interrupt the user, which is the
// opposite of a notification.
func proactiveNotificationRouteAvailable(ctx context.Context, bridge *PhoneBridge, status PhoneBridgeStatus) bool {
	const commandType = "notification_send"
	if phoneBridgeReadyForCommand(status, commandType) ||
		phoneBridgeCanUsePiPBackground(status, commandType) ||
		phoneBridgeCanUseFGSBackground(status, commandType) {
		return true
	}
	return phoneBridgeCanUseBLEBackground(status, commandType) && bridge.bleWakeAvailable(ctx)
}

func sendProactiveNotification(ctx context.Context, bridge *PhoneBridge, message Message) error {
	payload, err := json.Marshal(map[string]any{
		"title": proactiveNotificationTitle,
		"body":  truncateRunes(message.Content, proactiveNotificationBodyRunes),
		"sound": true,
	})
	if err != nil {
		return err
	}
	sendCtx, cancel := context.WithTimeout(ctx, proactiveNotificationSendBudget)
	defer cancel()
	// No restorer: the route check above already excluded restoring the App.
	resp, _, err := sendRoutedBridgeCommand(sendCtx, bridge, nil, BridgeCommand{
		ID:        nextBridgeCmdID("proactive"),
		Type:      "notification_send",
		Payload:   payload,
		TimeoutMs: 5000,
	})
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	return nil
}

func truncateRunes(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit-1]) + "…"
}

type proactiveRequest struct {
	Content string `json:"content"`
}

// handleProactive accepts POST /api/proactive {"content": "..."}.
func (s *Server) handleProactive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req proactiveRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	if err := decoder.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), proactiveRunLockTimeout)
	defer cancel()
	result, err := s.DeliverProactiveMessage(ctx, req.Content)
	if err != nil {
		code := http.StatusInternalServerError
		switch {
		case errors.Is(err, errProactiveRealtimeUnsupported):
			code = http.StatusConflict
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			code = http.StatusServiceUnavailable
		case strings.TrimSpace(req.Content) == "" || len(strings.TrimSpace(req.Content)) > proactiveMaxContentBytes:
			code = http.StatusBadRequest
		}
		http.Error(w, err.Error(), code)
		return
	}
	writeJSON(w, result)
}
