package ekho

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"aico-agent/internal/agent/tts"
)

// Block the first PCM write until playback is released or Stop is called.
// A second completed synthesis before release proves actual overlap.
type gatedSink struct {
	testSink
	entered chan struct{}
	release chan struct{}
	stopped chan struct{}
	once    sync.Once
	stop    sync.Once
}

func (s *gatedSink) WritePCM(p []byte) error {
	s.once.Do(func() {
		close(s.entered)
		select {
		case <-s.release:
		case <-s.stopped:
		}
	})
	select {
	case <-s.stopped:
		return context.Canceled
	default:
		return s.testSink.WritePCM(p)
	}
}

func (s *gatedSink) Stop() error {
	s.stop.Do(func() { close(s.stopped) })
	return s.testSink.Stop()
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func receive(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("pipeline did not finish")
		return nil
	}
}

func TestEkhoPipelineOverlapBoundAndOrder(t *testing.T) {
	for _, mode := range []string{"finish", "cancel", "parent cancellation"} {
		t.Run(mode, func(t *testing.T) {
			cancel := mode != "finish"
			logPath, tempDir := fakeEkho(t, `
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then shift; output="$1"; fi
  shift
done
text=$(/bin/cat)
/bin/cp "$EKHO_TEST_WAV-$text" "$output"
printf '%s\n' "$text" >> "$EKHO_TEST_LOG"
`)
			var expected []byte
			for i, text := range []string{"One!", "Two!", "Three!", "Four!"} {
				pcm := []byte{byte(i + 1), 0}
				expected = append(expected, pcm...)
				if err := os.WriteFile(os.Getenv("EKHO_TEST_WAV")+"-"+text, testWAV(pcm), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			sink := &gatedSink{entered: make(chan struct{}), release: make(chan struct{}), stopped: make(chan struct{})}
			provider := fixtureProvider(t, tts.ProviderConfig{Extra: map[string]any{"pace_audio": false}})
			ctx, cancelParent := context.WithCancel(context.Background())
			defer cancelParent()
			stream, err := provider.BeginStream(ctx, sink)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { tts.Cancel(stream); stream.Close() })
			if err := stream.WriteText("One!Two!"); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool {
				log, _ := os.ReadFile(logPath)
				return string(log) == "One!\nTwo!\n"
			})
			select {
			case <-sink.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("playback did not consume the first sentence")
			}
			// Only a single text job may queue behind the two on-disk WAVs.
			if err := stream.WriteText("Three!"); err != nil {
				t.Fatal(err)
			}
			writeDone := make(chan error, 1)
			go func() { writeDone <- stream.WriteText("Four!") }()
			select {
			case err := <-writeDone:
				t.Fatalf("full pipeline did not apply backpressure: %v", err)
			case <-time.After(75 * time.Millisecond):
			}
			log, _ := os.ReadFile(logPath)
			files, _ := os.ReadDir(tempDir)
			if string(log) != "One!\nTwo!\n" || len(files) != 2 {
				t.Fatalf("lookahead exceeded: synthesized=%q WAVs=%d", log, len(files))
			}
			if cancel {
				if mode == "parent cancellation" {
					cancelParent()
				} else if err := tts.Cancel(stream); err != nil {
					t.Fatal(err)
				}
				if err := receive(t, writeDone); !errors.Is(err, context.Canceled) {
					t.Fatalf("blocked producer error = %v", err)
				}
			} else {
				close(sink.release)
				if err := receive(t, writeDone); err != nil {
					t.Fatal(err)
				}
				if err := stream.Flush(); err != nil {
					t.Fatal(err)
				}
			}
			closed := make(chan error, 1)
			go func() { closed <- stream.Close() }()
			err = receive(t, closed)
			if cancel {
				if !errors.Is(err, context.Canceled) || sink.stops != 1 || sink.drains != 0 || len(sink.pcm) != 0 {
					t.Fatalf("cancel: err=%v stops=%d drains=%d PCM=%d", err, sink.stops, sink.drains, len(sink.pcm))
				}
			} else if err != nil || sink.drains != 1 || !bytes.Equal(sink.pcm, expected) {
				t.Fatalf("finish: err=%v drains=%d PCM=%v", err, sink.drains, sink.pcm)
			}
			assertNoTempWAV(t, tempDir)
		})
	}
}

func TestEkhoWorkerCompletionHonorsCancellation(t *testing.T) {
	for _, cancellation := range []string{"none", "before watcher", "after watcher"} {
		t.Run(cancellation, func(t *testing.T) {
			// Cover both ready watcher signals and cancellation after the watcher
			// has exited but before Close has drained the sink's buffered audio.
			for i := 0; i < 32; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				sink := &testSink{}
				s := &session{
					ctx: ctx, cancel: cancel, sink: sink,
					pending: make(chan synthesisJob),
					done:    make(chan struct{}), watcherDone: make(chan struct{}),
				}
				close(s.done)
				if cancellation == "before watcher" {
					cancel()
				}
				s.watchContext()
				if cancellation == "before watcher" && (!errors.Is(s.Err(), context.Canceled) || sink.stops != 1) {
					t.Fatalf("watcher missed cancellation: Err=%v stops=%d", s.Err(), sink.stops)
				}
				if cancellation == "after watcher" {
					cancel()
				}
				err := s.Close()
				cancel()
				if cancellation != "none" {
					if !errors.Is(err, context.Canceled) || !errors.Is(s.Err(), context.Canceled) || sink.stops != 1 || sink.drains != 0 {
						t.Fatalf("canceled completion: close=%v Err=%v stops=%d drains=%d", err, s.Err(), sink.stops, sink.drains)
					}
				} else if err != nil || s.Err() != nil || sink.stops != 0 || sink.drains != 1 {
					t.Fatalf("normal completion: close=%v Err=%v stops=%d drains=%d", err, s.Err(), sink.stops, sink.drains)
				}
			}
		})
	}
}

func TestEkhoPipelinePlaybackFailure(t *testing.T) {
	_, tempDir := fakeEkho(t, `
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then shift; output="$1"; fi
  shift
done
/bin/cp "$EKHO_TEST_WAV" "$output"
`)
	failure := errors.New("device disconnected")
	sink := &testSink{err: failure}
	stream, err := fixtureProvider(t, tts.ProviderConfig{}).BeginStream(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	// Errors may arrive during enqueue or later; Close must join cleanup and
	// preserve the playback failure instead of replacing it with cancellation.
	stream.WriteText(strings.Repeat("你好。", 8))
	if err := stream.Close(); !errors.Is(err, failure) || !errors.Is(stream.Err(), failure) {
		t.Fatalf("lost async playback error: close=%v Err=%v", err, stream.Err())
	}
	if sink.stops != 1 || sink.drains != 0 {
		t.Fatalf("failure lifecycle: stops=%d drains=%d", sink.stops, sink.drains)
	}
	assertNoTempWAV(t, tempDir)
}

func TestEkhoPrefetchFailureStopsCurrentPlayback(t *testing.T) {
	_, tempDir := fakeEkho(t, `
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then shift; output="$1"; fi
  shift
done
text=$(/bin/cat)
if [ "$text" = "Two!" ]; then
  echo prefetch-failed >&2
  exit 1
fi
/bin/cp "$EKHO_TEST_WAV" "$output"
`)
	sink := &gatedSink{entered: make(chan struct{}), release: make(chan struct{}), stopped: make(chan struct{})}
	stream, err := fixtureProvider(t, tts.ProviderConfig{}).BeginStream(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tts.Cancel(stream); stream.Close() })
	if err := stream.WriteText("One!"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sink.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first playback did not start")
	}
	stream.WriteText("Two!Three!")
	closed := make(chan error, 1)
	go func() { closed <- stream.Close() }()
	if err := receive(t, closed); err == nil || !strings.Contains(err.Error(), "prefetch-failed") {
		t.Fatalf("lost prefetch failure: %v", err)
	}
	if sink.stops != 1 || sink.drains != 0 {
		t.Fatalf("prefetch failure: stops=%d drains=%d", sink.stops, sink.drains)
	}
	assertNoTempWAV(t, tempDir)
}
