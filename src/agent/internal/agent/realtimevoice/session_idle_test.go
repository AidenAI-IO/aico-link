package realtimevoice

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// Wording observed on real hardware: Gemini Live on one board, Qwen Omni
// Realtime on another, both closing an idle session with this same sentence.
const observedIdleReason = "Your session was closed because no response was generated for 180 seconds."

func TestIsSessionIdleCloseMessage(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		idle   bool
	}{
		{"observed wording", observedIdleReason, true},
		{"different idle window", "Your session was closed because no response was generated for 600 seconds.", true},
		{"quota error", "Quota exceeded for this project", false},
		{"unrelated session close", "Session closed: internal error", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSessionIdleCloseMessage(tt.reason); got != tt.idle {
				t.Errorf("isSessionIdleCloseMessage(%q) = %t, want %t", tt.reason, got, tt.idle)
			}
		})
	}
}

// A JSON error frame is the seam for providers that report the close in-band.
func TestClassifyServerErrorMessage(t *testing.T) {
	idle := classifyServerErrorMessage(observedIdleReason)
	if !errors.Is(idle, ErrSessionIdleTimeout) {
		t.Error("idle error frame did not carry ErrSessionIdleTimeout")
	}
	if !strings.Contains(idle.Error(), observedIdleReason) {
		t.Errorf("error %q lost the provider message", idle.Error())
	}

	other := classifyServerErrorMessage("Quota exceeded")
	if errors.Is(other, ErrSessionIdleTimeout) {
		t.Error("an unrelated error frame must not be classified as idle")
	}
}

// An idle close can also arrive as a 1007 close frame, which never reaches a
// provider's translate(); the transport seam has to catch that form.
func TestClassifyTransportError(t *testing.T) {
	closeErr := &websocket.CloseError{Code: websocket.CloseInvalidFramePayloadData, Text: observedIdleReason}
	classified := classifyTransportError(closeErr)
	if !errors.Is(classified, ErrSessionIdleTimeout) {
		t.Error("1007 idle close did not carry ErrSessionIdleTimeout")
	}
	if !strings.Contains(classified.Error(), "1007") {
		t.Errorf("error %q lost the close code", classified.Error())
	}
	// Classifying must not flatten the chain: a caller still needs the close
	// frame itself, not just its text.
	var recovered *websocket.CloseError
	if !errors.As(classified, &recovered) {
		t.Fatal("classified error no longer exposes its *websocket.CloseError")
	}
	if recovered.Code != websocket.CloseInvalidFramePayloadData {
		t.Errorf("recovered close code = %d, want %d", recovered.Code, websocket.CloseInvalidFramePayloadData)
	}

	wrapped := classifyTransportError(fmt.Errorf("read: %w", closeErr))
	if !errors.Is(wrapped, ErrSessionIdleTimeout) {
		t.Error("a wrapped idle close must still be classified")
	}

	for _, err := range []error{
		nil,
		errors.New("read tcp: connection reset by peer"),
		&websocket.CloseError{Code: websocket.CloseInternalServerErr, Text: "internal error"},
	} {
		if errors.Is(classifyTransportError(err), ErrSessionIdleTimeout) {
			t.Errorf("%v must not be classified as an idle close", err)
		}
	}
}

// Rotation and idle close are distinct provider behaviors with different
// handling: rotation reconnects, an idle close is announced.
func TestIdleTimeoutAndRotationAreDistinct(t *testing.T) {
	idle := classifyServerErrorMessage(observedIdleReason)
	if errors.Is(idle, ErrSessionRotated) {
		t.Error("an idle close must not satisfy ErrSessionRotated")
	}
	rotated := fmt.Errorf("%w: gemini live ends this session in 50s", ErrSessionRotated)
	if errors.Is(rotated, ErrSessionIdleTimeout) {
		t.Error("rotation must not satisfy ErrSessionIdleTimeout")
	}
}
