package mnk

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func newTapSequenceTestAdapter(mock *MockProvider) (*TouchGestureToolAdapter, *[]int) {
	adapter := NewTouchGestureToolAdapter(mock)
	sleeps := &[]int{}
	adapter.sleep = func(_ context.Context, ms int) error {
		*sleeps = append(*sleeps, ms)
		return nil
	}
	return adapter, sleeps
}

func decodeTapSequenceResult(t *testing.T, output string) TapSequenceResult {
	t.Helper()
	var result TapSequenceResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("output %q is not a tap_sequence result: %v", output, err)
	}
	return result
}

func TestTapSequenceTapsPointsInOrderWithInterval(t *testing.T) {
	mock := NewMockProvider()
	adapter, sleeps := newTapSequenceTestAdapter(mock)
	var checks []int
	adapter.WithTapSequenceGuard(func(_ context.Context, executed int) (bool, string, error) {
		checks = append(checks, executed)
		return false, "", nil
	})

	output, err := adapter.Call(context.Background(), `{"type":"tap_sequence","points":[{"x":100,"y":200},{"x":300,"y":400},{"x":300,"y":400}],"interval_ms":120}`)
	if err != nil {
		t.Fatal(err)
	}
	clicks := mock.ClickCalls()
	want := []MockClick{{X: 100, Y: 200, Button: ButtonLeft}, {X: 300, Y: 400, Button: ButtonLeft}, {X: 300, Y: 400, Button: ButtonLeft}}
	if len(clicks) != len(want) {
		t.Fatalf("clicks = %#v, want %#v", clicks, want)
	}
	for i := range want {
		if clicks[i] != want[i] {
			t.Fatalf("click %d = %#v, want %#v", i, clicks[i], want[i])
		}
	}
	if got := *sleeps; len(got) != 2 || got[0] != 120 || got[1] != 120 {
		t.Fatalf("sleeps = %v, want two 120ms intervals", got)
	}
	if len(checks) != 2 || checks[0] != 1 || checks[1] != 2 {
		t.Fatalf("guard checks = %v, want after taps 1 and 2", checks)
	}
	result := decodeTapSequenceResult(t, output)
	if result != (TapSequenceResult{Requested: 3, Executed: 3}) {
		t.Fatalf("result = %#v", result)
	}
}

func TestTapSequenceDefaultInterval(t *testing.T) {
	mock := NewMockProvider()
	adapter, sleeps := newTapSequenceTestAdapter(mock)
	if _, err := adapter.Call(context.Background(), `{"type":"tap_sequence","points":[{"x":1,"y":1},{"x":2,"y":2}]}`); err != nil {
		t.Fatal(err)
	}
	if got := *sleeps; len(got) != 1 || got[0] != DefaultTapSequenceIntervalMs {
		t.Fatalf("sleeps = %v, want [%d]", got, DefaultTapSequenceIntervalMs)
	}
}

func TestTapSequenceGuardStopsRemainingTaps(t *testing.T) {
	for _, tc := range []struct {
		name       string
		guard      TapSequenceGuard
		wantReason string
	}{
		{
			name: "layout change",
			guard: func(_ context.Context, executed int) (bool, string, error) {
				return executed == 2, "layout changed", nil
			},
			wantReason: "layout changed",
		},
		{
			name: "check failure",
			guard: func(_ context.Context, executed int) (bool, string, error) {
				if executed == 2 {
					return false, "", errors.New("frame unavailable")
				}
				return false, "", nil
			},
			wantReason: "screen check failed: frame unavailable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := NewMockProvider()
			adapter, _ := newTapSequenceTestAdapter(mock)
			adapter.WithTapSequenceGuard(tc.guard)
			output, err := adapter.Call(context.Background(), `{"type":"tap_sequence","points":[{"x":1,"y":1},{"x":2,"y":2},{"x":3,"y":3},{"x":4,"y":4}]}`)
			if err != nil {
				t.Fatal(err)
			}
			if clicks := mock.ClickCalls(); len(clicks) != 2 || clicks[1].X != 2 {
				t.Fatalf("clicks = %#v, want the first two taps only", clicks)
			}
			result := decodeTapSequenceResult(t, output)
			if result.Requested != 4 || result.Executed != 2 || !result.StoppedEarly || result.StopReason != tc.wantReason {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

// The screen check cannot be switched off from the request; an unknown
// field asking for that is ignored like any other.
func TestTapSequenceGuardIsNotOptional(t *testing.T) {
	mock := NewMockProvider()
	adapter, _ := newTapSequenceTestAdapter(mock)
	checks := 0
	adapter.WithTapSequenceGuard(func(context.Context, int) (bool, string, error) {
		checks++
		return false, "", nil
	})
	if _, err := adapter.Call(context.Background(), `{"type":"tap_sequence","points":[{"x":1,"y":1},{"x":2,"y":2}],"stop_on_screen_change":false}`); err != nil {
		t.Fatal(err)
	}
	if len(mock.ClickCalls()) != 2 || checks != 1 {
		t.Fatalf("clicks = %#v checks=%d, want the guard to run", mock.ClickCalls(), checks)
	}
}

func TestTapSequenceRejectsInvalidInputWithoutTapping(t *testing.T) {
	many := make([]string, MaxTapSequencePoints+1)
	for i := range many {
		many[i] = `{"x":1,"y":1}`
	}
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{"single point", `{"type":"tap_sequence","points":[{"x":1,"y":1}]}`, "requires 2 to 32 points"},
		{"too many points", `{"type":"tap_sequence","points":[` + strings.Join(many, ",") + `]}`, "requires 2 to 32 points"},
		{"missing points", `{"type":"tap_sequence"}`, "requires 2 to 32 points"},
		{"null point", `{"type":"tap_sequence","points":[{"x":1,"y":1},null]}`, "points[1] must be an object"},
		{"out of range", `{"type":"tap_sequence","points":[{"x":1,"y":1},{"x":1001,"y":1}]}`, "points[1] must use the normalized 0-1000 scale"},
		{"interval too short", `{"type":"tap_sequence","points":[{"x":1,"y":1},{"x":2,"y":2}],"interval_ms":10}`, "interval_ms must be between 80 and 2000"},
		{"interval too long", `{"type":"tap_sequence","points":[{"x":1,"y":1},{"x":2,"y":2}],"interval_ms":5000}`, "interval_ms must be between 80 and 2000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := NewMockProvider()
			adapter, _ := newTapSequenceTestAdapter(mock)
			_, err := adapter.Call(context.Background(), tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if mnkErr := AsError(err); mnkErr == nil || mnkErr.Kind != ErrInvalidArguments {
				t.Fatalf("err = %T %v, want invalid arguments", err, err)
			}
			if len(mock.ClickCalls()) != 0 {
				t.Fatalf("invalid input tapped: %#v", mock.ClickCalls())
			}
		})
	}
}
