package agent

import (
	"errors"
	"fmt"
	"testing"

	"aiden-agent/internal/agent/realtimevoice"
)

func TestTurnFailureFromError_SessionIdle(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode string
	}{
		{
			name:     "wrapped idle timeout sentinel",
			err:      fmt.Errorf("%w: Your session was closed because no response was generated for 180 seconds", realtimevoice.ErrSessionIdleTimeout),
			wantCode: TurnFailureSessionIdle,
		},
		{
			name:     "idle timeout wrapped again by the caller",
			err:      fmt.Errorf("realtime session ended: %w", fmt.Errorf("%w: idle", realtimevoice.ErrSessionIdleTimeout)),
			wantCode: TurnFailureSessionIdle,
		},
		{
			name:     "same wording without the sentinel is not reclassified",
			err:      errors.New("Your session was closed because no response was generated for 180 seconds"),
			wantCode: TurnFailureLLMUnavailable,
		},
		{
			name:     "generic error should not match",
			err:      errors.New("some other error"),
			wantCode: TurnFailureLLMUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failure := TurnFailureFromError(tt.err)
			if failure == nil {
				t.Fatal("expected failure, got nil")
			}
			if failure.Code != tt.wantCode {
				t.Errorf("Code = %q, want %q", failure.Code, tt.wantCode)
			}
		})
	}
}

func TestDefaultTurnFailureVoiceNotificationText_SessionIdle(t *testing.T) {
	tests := []struct {
		name   string
		locale string
		want   string
	}{
		{
			name:   "zh-cn",
			locale: "zh-cn",
			want:   "长时间无对话，会话已结束。",
		},
		{
			name:   "en",
			locale: "en-US",
			want:   "Session ended due to inactivity.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := defaultTurnFailureVoiceNotificationText(tt.locale, TurnFailureSessionIdle)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
