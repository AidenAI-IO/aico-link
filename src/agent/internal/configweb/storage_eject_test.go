package configweb

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aico-agent/internal/agent"
)

func TestStorageEjectDistinguishesFlushWarnings(t *testing.T) {
	const warning = "Card was detached, but pending writes may not have been saved"
	for _, tc := range []struct {
		name        string
		err         error
		lastWarning string
		wantStatus  int
		wantWarning bool
	}{
		{name: "clean eject", wantStatus: http.StatusOK},
		{name: "historical warning", lastWarning: warning, wantStatus: http.StatusOK},
		{name: "detached after failed flush", err: fmt.Errorf("pending writes: %w", agent.ErrStorageFlush), lastWarning: warning, wantStatus: http.StatusOK, wantWarning: true},
		{name: "busy mount", err: errors.New("cannot eject: target busy"), wantStatus: http.StatusConflict},
		{name: "busy mount with historical warning", err: errors.New("cannot eject: target busy"), lastWarning: warning, wantStatus: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage := &fakeStorageController{
				ejectError: tc.err,
				status: agent.StorageStatus{
					EffectiveMode: agent.StorageModeDual,
					Card:          agent.StorageCardStatus{Present: true, Mounted: true},
					LastWarning:   tc.lastWarning,
				},
			}
			server := &Server{options: testOptions(t), storage: storage}
			response := httptest.NewRecorder()
			server.APIHandler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/storage/eject", strings.NewReader("{}")))
			if response.Code != tc.wantStatus {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var payload struct {
				agent.StorageStatus
				WarningCode string `json:"warning_code"`
				Error       string `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if tc.wantStatus == http.StatusConflict {
				if payload.Error != tc.err.Error() || !storage.status.Card.Mounted || payload.WarningCode != "" {
					t.Fatalf("failed eject was treated as completed: %+v", payload)
				}
				return
			}
			if payload.Error != "" || payload.Card.Mounted || !payload.Card.Present || payload.EffectiveMode != agent.StorageModeEMMCOnly || payload.LastWarning != tc.lastWarning {
				t.Fatalf("completed eject response lost state or warning: %+v", payload)
			}
			if tc.wantWarning {
				if payload.WarningCode != "storage_flush_failed" {
					t.Fatalf("flush warning code=%q", payload.WarningCode)
				}
			} else if payload.WarningCode != "" {
				t.Fatalf("historical warning was reported as a new eject warning: %q", payload.WarningCode)
			}
		})
	}
}
