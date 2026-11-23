package tts

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"aico-agent/internal/providerretry"
)

type retryTestProvider struct {
	opens     int
	fail      string
	permanent bool
	audio     bool
	sessions  []*retryTestSession
	onBegin   func(*retryTestSession) error
	onOp      func(*retryTestSession, string) error
}

func (p *retryTestProvider) Name() string               { return "retry-test" }
func (p *retryTestProvider) Capabilities() Capabilities { return Capabilities{} }
func (p *retryTestProvider) Close() error               { return nil }
func (p *retryTestProvider) BeginStream(ctx context.Context, sink AudioSink) (StreamSession, error) {
	p.opens++
	s := &retryTestSession{provider: p, sink: sink, ctx: ctx, attempt: p.opens}
	p.sessions = append(p.sessions, s)
	if p.onBegin != nil {
		if err := p.onBegin(s); err != nil {
			return nil, err
		}
	}
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
	if s.provider.onOp != nil {
		if err := s.provider.onOp(s, kind); err != nil {
			return err
		}
	}
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

func TestPreAudioTimeoutAndPCMSerialize(t *testing.T) {
	for _, audioFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "audio-first", false: "timeout-first"}[audioFirst], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				playback := &retryPlaybackSink{}
				sink := &retryAudioSink{AudioSink: playback}
				attempt := &attemptAudioSink{retryAudioSink: sink}
				session := &retrySession{ctx: context.Background(), sink: sink, attemptCancel: cancel}
				err := session.bounded(context.Background(), 10*time.Millisecond, func() error {
					if audioFirst {
						if err := attempt.WritePCM([]byte{1}); err != nil {
							return err
						}
						// Virtual time cannot advance until this goroutine blocks,
						// so PCM is submitted before the timeout can fire.
						time.Sleep(30 * time.Millisecond)
						return ctx.Err()
					}
					<-ctx.Done()
					if err := attempt.WritePCM([]byte{1}); !errors.Is(err, context.Canceled) {
						t.Errorf("late PCM accepted: %v", err)
					}
					return ctx.Err()
				})
				if audioFirst {
					if err != nil {
						t.Fatal(err)
					}
					if sink.retirePreAudio() {
						t.Fatal("retired audible attempt")
					}
					_ = attempt.Stop()
					if playback.stops != 1 {
						t.Fatal("audible attempt lost playback cleanup")
					}
				} else if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("error=%v", err)
				}
			})
		})
	}
}

func TestPreAudioTimeoutPreservesProviderRejection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &retrySession{ctx: context.Background(), sink: &retryAudioSink{AudioSink: noopSink{}}, attemptCancel: cancel}
	rejection := &providerretry.StatusError{Status: 401}
	err := session.bounded(context.Background(), 10*time.Millisecond, func() error { <-ctx.Done(); return rejection })
	if err != rejection || providerretry.Retryable(err) {
		t.Fatalf("lost provider rejection: %v", err)
	}
}

func TestBeginStreamFailureStopsAlreadyDeliveredPlayback(t *testing.T) {
	p := &retryTestProvider{onBegin: func(s *retryTestSession) error {
		_ = s.sink.WritePCM([]byte{1})
		return &providerretry.StatusError{Status: 503}
	}}
	sink := &retryPlaybackSink{}
	_, err := NewProviderHolder(p).BeginStream(context.Background(), sink)
	if err == nil || p.opens != 1 || sink.stops != 1 {
		t.Fatalf("error=%v opens=%d stops=%d", err, p.opens, sink.stops)
	}
}

func TestFinishDeadlineBoundsReopenAndReplay(t *testing.T) {
	for _, phase := range []string{"reopen", "replay", "finish", "detach"} {
		t.Run(phase, func(t *testing.T) {
			p := &retryTestProvider{fail: "flush"}
			p.onBegin = func(s *retryTestSession) error {
				if phase == "reopen" && s.attempt == 2 {
					<-s.ctx.Done()
					return s.ctx.Err()
				}
				return nil
			}
			p.onOp = func(s *retryTestSession, op string) error {
				if s.attempt == 2 && ((phase == "replay" && op == "write:hello") || (phase == "finish" && op == "flush")) {
					<-s.ctx.Done()
					return s.ctx.Err()
				}
				return nil
			}
			session, err := NewProviderHolder(p).BeginStream(context.Background(), noopSink{})
			if err != nil {
				t.Fatal(err)
			}
			if err := session.WriteText("hello"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
			defer cancel()
			started := time.Now()
			_, err = FinishInputContext(ctx, session)
			if p.opens != 2 {
				t.Fatalf("opens=%d error=%v", p.opens, err)
			}
			if phase == "detach" {
				if err != nil {
					t.Fatal(err)
				}
				cancel()
				if err := p.sessions[1].ctx.Err(); err != nil {
					t.Fatalf("healthy replacement canceled: %v", err)
				}
			} else if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 2*time.Second {
				t.Fatalf("elapsed=%s error=%v", time.Since(started), err)
			}
			_ = CancelAndClose(session)
		})
	}
}

func TestCloseReleasesReplacementWhenReplayFails(t *testing.T) {
	p := &retryTestProvider{fail: "close", onOp: func(s *retryTestSession, op string) error {
		if s.attempt == 2 && op == "write:hello" {
			return &providerretry.StatusError{Status: 401}
		}
		return nil
	}}
	session, err := NewProviderHolder(p).BeginStream(context.Background(), noopSink{})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.WriteText("hello"); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err == nil {
		t.Fatal("lost replay failure")
	}
	if p.opens != 2 || p.sessions[1].aborts != 1 || p.sessions[1].ctx.Err() == nil {
		t.Fatal("replacement leaked after failed replay")
	}
	_ = CancelAndClose(session)
	if p.sessions[1].aborts != 1 {
		t.Fatal("replacement cleaned up twice")
	}
}
