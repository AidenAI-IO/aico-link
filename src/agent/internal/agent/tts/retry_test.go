package tts

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"aico-agent/internal/providerretry"
)

type retryTestProvider struct {
	opens     int
	fail      string
	permanent bool
	audio     bool
	sessions  []*retryTestSession
}

func (p *retryTestProvider) Name() string               { return "retry-test" }
func (p *retryTestProvider) Capabilities() Capabilities { return Capabilities{} }
func (p *retryTestProvider) Close() error               { return nil }
func (p *retryTestProvider) BeginStream(ctx context.Context, sink AudioSink) (StreamSession, error) {
	p.opens++
	s := &retryTestSession{provider: p, sink: sink, ctx: ctx, attempt: p.opens}
	p.sessions = append(p.sessions, s)
	if p.fail == "begin" && s.attempt == 1 {
		return nil, p.failure()
	}
	return s, nil
}
func (p *retryTestProvider) failure() error {
	status := 503
	if p.permanent {
		status = 401
	}
	return &providerretry.StatusError{Status: status}
}

type retryTestSession struct {
	provider *retryTestProvider
	sink     AudioSink
	ctx      context.Context
	attempt  int
	ops      []string
	aborts   int
}

func (s *retryTestSession) op(kind string) error {
	s.ops = append(s.ops, kind)
	if s.attempt == 1 && (s.provider.fail == kind || s.provider.fail == "always") {
		if s.provider.audio {
			_ = s.sink.WritePCM([]byte{1, 2})
		}
		return s.provider.failure()
	}
	if s.provider.fail == "always" {
		return s.provider.failure()
	}
	return nil
}
func (s *retryTestSession) WriteText(text string) error { return s.op("write:" + text) }
func (s *retryTestSession) Flush() error                { return s.op("flush") }
func (s *retryTestSession) Close() error                { return s.op("close") }
func (s *retryTestSession) Abort() error                { s.aborts++; return nil }
func (s *retryTestSession) Err() error                  { return nil }

func TestHolderRetriesPreAudioOperationsAndReplaysInput(t *testing.T) {
	for _, failure := range []string{"begin", "write:hello", "flush", "close"} {
		t.Run(failure, func(t *testing.T) {
			p := &retryTestProvider{fail: failure}
			holder := NewProviderHolder(p)
			s, err := holder.BeginStream(context.Background(), noopSink{})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.WriteText("hello"); err != nil {
				t.Fatal(err)
			}
			if err = s.Flush(); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			if p.opens != 2 {
				t.Fatalf("opens=%d", p.opens)
			}
			last := p.sessions[len(p.sessions)-1]
			if !reflect.DeepEqual(last.ops, []string{"write:hello", "flush", "close"}) {
				t.Fatalf("replay=%v", last.ops)
			}
			if failure != "begin" {
				if p.sessions[0].aborts != 1 {
					t.Fatalf("failed attempt not aborted")
				}
				if err := p.sessions[0].sink.WritePCM([]byte{9}); !errors.Is(err, context.Canceled) {
					t.Fatalf("stale output accepted: %v", err)
				}
			}
		})
	}
}

func TestHolderNeverReplaysAfterPCMOrPermanentError(t *testing.T) {
	for _, tt := range []struct {
		name             string
		audio, permanent bool
	}{{"audio", true, false}, {"auth", false, true}} {
		t.Run(tt.name, func(t *testing.T) {
			p := &retryTestProvider{fail: "write:hello", audio: tt.audio, permanent: tt.permanent}
			s, err := NewProviderHolder(p).BeginStream(context.Background(), noopSink{})
			if err != nil {
				t.Fatal(err)
			}
			err = s.WriteText("hello")
			if err == nil || p.opens != 1 {
				t.Fatalf("err=%v opens=%d", err, p.opens)
			}
			_ = CancelAndClose(s)
		})
	}
}

func TestHolderRetryBudgetAndCanceledWait(t *testing.T) {
	p := &retryTestProvider{fail: "always"}
	s, err := NewProviderHolder(p).BeginStream(context.Background(), noopSink{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.WriteText("hello"); err == nil || p.opens != 3 {
		t.Fatalf("err=%v opens=%d", err, p.opens)
	}
	if err = s.Err(); err == nil {
		t.Fatal("lost final error")
	}
	_ = s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	p = &retryTestProvider{fail: "write:hello"}
	s, err = NewProviderHolder(p).BeginStream(ctx, noopSink{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err = s.WriteText("hello"); !errors.Is(err, context.Canceled) || p.opens != 1 {
		t.Fatalf("err=%v opens=%d", err, p.opens)
	}
	_ = CancelAndClose(s)
}

func TestRetiredAttemptCannotFinalizeReplacementPlayback(t *testing.T) {
	p := &retryTestProvider{fail: "write:hello"}
	sink := &retryPlaybackSink{}
	session, err := NewProviderHolder(p).BeginStream(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if err = session.WriteText("hello"); err != nil {
		t.Fatal(err)
	}
	old, current := p.sessions[0].sink, p.sessions[1].sink
	if err = current.WritePCM([]byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	_ = old.Drain(context.Background())
	_ = old.Stop()
	if sink.drains != 0 || sink.stops != 0 {
		t.Fatalf("retired attempt finalized playback: drains=%d stops=%d", sink.drains, sink.stops)
	}
	_ = current.Drain(context.Background())
	if sink.drains != 1 {
		t.Fatal("active attempt did not drain playback")
	}
	_ = session.Close()
}

type retryPlaybackSink struct {
	noopSink
	drains, stops int
}

func (s *retryPlaybackSink) Drain(context.Context) error { s.drains++; return nil }
func (s *retryPlaybackSink) Stop() error                 { s.stops++; return nil }

func TestFinishInputContextDeadlineBoundsRetries(t *testing.T) {
	p := &retryTestProvider{fail: "flush"}
	session, err := NewProviderHolder(p).BeginStream(context.Background(), noopSink{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = FinishInputContext(ctx, session)
	if !errors.Is(err, context.DeadlineExceeded) || p.opens != 1 {
		t.Fatalf("err=%v opens=%d", err, p.opens)
	}
	_ = CancelAndClose(session)
}
