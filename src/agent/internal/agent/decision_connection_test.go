package agent

import (
	"context"
	"strings"
	"testing"
)

func TestRunDecisionModelTestUsesSavedKeyAndChecksTheAnswer(t *testing.T) {
	server := newDecisionServer(t, `{"answers":{"check":{"type":"noul","noul":0.98}}}`)
	saved := Config{Decision: DecisionModelConfig{APIKey: "ts-saved"}}

	result, err := RunDecisionModelTest(context.Background(), saved, DecisionModelConfig{BaseURL: server.URL})
	if err != nil || result.Provider != DecisionProviderTypeSafe || result.Model != defaultTypeSafeModel {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if server.requestCount() != 1 {
		t.Fatalf("requests = %d", server.requestCount())
	}

	// A reply without the asked answer is a failed test, not a pass.
	server = newDecisionServer(t, `{"answers":{}}`)
	if _, err := RunDecisionModelTest(context.Background(), saved, DecisionModelConfig{BaseURL: server.URL}); err == nil {
		t.Fatal("missing answer passed the test")
	}

	if _, err := RunDecisionModelTest(context.Background(), Config{}, DecisionModelConfig{}); err == nil || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("missing key error = %v", err)
	}
}
