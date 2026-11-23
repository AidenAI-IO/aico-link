package ekho

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"aico-agent/internal/agent/tts"
)

type testSink struct {
	mu     sync.Mutex
	pcm    []byte
	drains int
	stops  int
	err    error
}

func (*testSink) Format() tts.AudioFormat {
	return tts.AudioFormat{SampleRate: sampleRate, Channels: 1, BitWidth: 16}
}
func (s *testSink) WritePCM(p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pcm = append(s.pcm, p...)
	return s.err
}
func (s *testSink) Drain(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drains++
	return nil
}
func (s *testSink) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops++
	return nil
}

func testWAV(pcm []byte) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(46+len(pcm)))
	b.WriteString("WAVEJUNK")
	binary.Write(&b, binary.LittleEndian, uint32(1))
	b.Write([]byte{7, 0}) // Odd metadata chunk with RIFF padding.
	b.WriteString("fmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	for _, v := range []any{uint16(1), uint16(1), uint32(sampleRate), uint32(sampleRate * 2), uint16(2), uint16(16)} {
		binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}

func fakeEkho(t *testing.T, body string) (logPath, tempDir string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "input")
	tempDir = t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("TMPDIR", tempDir)
	t.Setenv("EKHO_TEST_LOG", logPath)
	fixture := filepath.Join(dir, "fixture.wav")
	t.Setenv("EKHO_TEST_WAV", fixture)
	if err := os.WriteFile(fixture, testWAV([]byte{1, 0, 2, 0}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ekho"), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return logPath, tempDir
}

func fixtureProvider(t *testing.T, cfg tts.ProviderConfig) tts.TTSProvider {
	t.Helper()
	provider, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	provider.(*Adapter).lookPath = func(name string) (string, error) {
		if name != "ekho" {
			return "", exec.ErrNotFound
		}
		return exec.LookPath(name)
	}
	return provider
}

func TestEkhoStreamSentencesAndTail(t *testing.T) {
	logPath, tempDir := fakeEkho(t, `
printf '%s\n' "$@" > "$EKHO_TEST_LOG.args"
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then shift; output="$1"; fi
  shift
done
/bin/cat >> "$EKHO_TEST_LOG"
printf '\n' >> "$EKHO_TEST_LOG"
/bin/cp "$EKHO_TEST_WAV" "$output"
`)
	provider := fixtureProvider(t, tts.ProviderConfig{SpeedRatio: 1})
	sink := &testSink{}
	stream, err := provider.BeginStream(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	// A fragmented decimal and UTF-8 text must retain both text and order.
	for _, text := range []string{"你好", "。Price is 3.", "14 dollars.", " Next", " tail"} {
		if err := stream.WriteText(text); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil || sink.drains != 1 {
		t.Fatalf("close must be idempotent: err=%v drains=%d", err, sink.drains)
	}
	input, _ := os.ReadFile(logPath)
	if string(input) != "你好。\nPrice is 3.14 dollars.\nNext tail\n" {
		t.Fatalf("synthesized input = %q", input)
	}
	args, _ := os.ReadFile(logPath + ".args")
	if !strings.HasPrefix(string(args), "-v\nEnglish\n-s\n20\n--english-speed\n-10\n-a\n0\n--samplerate\n22050\n-f\n-\n-o\n") {
		t.Fatalf("unexpected command args: %q", args)
	}
	if !bytes.Equal(sink.pcm, bytes.Repeat([]byte{1, 0, 2, 0}, 3)) {
		t.Fatalf("PCM includes a header or lost sentence order: %v", sink.pcm)
	}
	assertNoTempWAV(t, tempDir)
	if !errors.Is(stream.WriteText("late"), tts.ErrSessionClosed) {
		t.Fatal("write after close must fail")
	}
}

func TestEkhoCancellationKillsProcessAndCleansWAV(t *testing.T) {
	logPath, tempDir := fakeEkho(t, `
printf started > "$EKHO_TEST_LOG"
exec /bin/sleep 30
`)
	provider := fixtureProvider(t, tts.ProviderConfig{})
	sink := &testSink{}
	stream, err := provider.BeginStream(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.WriteText("取消这一句。"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- stream.Close() }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(logPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tts.Cancel(stream); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("close error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not kill synthesis")
	}
	if err := stream.Close(); !errors.Is(err, context.Canceled) || sink.drains != 0 || sink.stops != 1 {
		t.Fatalf("cancelled close: err=%v drains=%d stops=%d", err, sink.drains, sink.stops)
	}
	assertNoTempWAV(t, tempDir)
}

func TestEkhoFailureAndReset(t *testing.T) {
	_, tempDir := fakeEkho(t, "echo synthesis-failed >&2\nexit 1\n")
	provider := fixtureProvider(t, tts.ProviderConfig{})
	stream, _ := provider.BeginStream(context.Background(), &testSink{})
	if err := stream.WriteText("discard me"); err != nil {
		t.Fatal(err)
	}
	stream.(*session).ResetBuffer()
	if err := stream.Flush(); err != nil {
		t.Fatalf("reset retained text: %v", err)
	}
	if err := stream.WriteText("失败。"); err != nil && !strings.Contains(err.Error(), "synthesis-failed") {
		t.Fatal(err)
	}
	if err := stream.Flush(); err == nil || !strings.Contains(err.Error(), "synthesis-failed") {
		t.Fatalf("subprocess failure not surfaced: %v", err)
	}
	if stream.Close() == nil || stream.Err() == nil {
		t.Fatal("synthesis failure was lost on close")
	}
	assertNoTempWAV(t, tempDir)
}

func TestEkhoNumberLanguageRouting(t *testing.T) {
	logPath, tempDir := fakeEkho(t, `
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then shift; output="$1"; fi
  if [ "$1" = "-v" ]; then shift; voice="$1"; fi
  shift
done
printf '%s|' "$voice" >> "$EKHO_TEST_LOG"
/bin/cat >> "$EKHO_TEST_LOG"
printf '\n' >> "$EKHO_TEST_LOG"
/bin/cp "$EKHO_TEST_WAV" "$output"
`)
	for _, tc := range []struct {
		name      string
		fragments []string
		reset     bool
		want      string
	}{
		{"decimal fragments", []string{"You have 12 messages, total $3.", "14 and 50%."}, false,
			"English|You have 12 messages, total $3.14 and 50%.\n"},
		{"dates and units", []string{"On 2026-11-20 at 10:30, set 22.5 degrees and save 1,234 files."}, false,
			"English|On 2026-11-20 at 10:30, set 22.5 degrees and save 1,234 files.\n"},
		{"Unicode punctuation", []string{"“There are 12 files”—50% are ready。"}, false,
			"English|“There are 12 files”—50% are ready。\n"},
		{"Chinese", []string{"共有12条消息，金额3.14元。"}, false,
			"Mandarin|共有12条消息，金额3.14元。\n"},
		{"mixed", []string{"请 read 12 messages，金额3.14元。"}, false,
			"Mandarin|请 read 12 messages，金额3.14元。\n"},
		{"adjacent Chinese sentence", []string{"You have 12 messages.共有3条。"}, false,
			"English|You have 12 messages.\nMandarin|共有3条。\n"},
		{"adjacent after decimal", []string{"The total is 3.14.共有3条。"}, false,
			"English|The total is 3.14.\nMandarin|共有3条。\n"},
		{"single digit before period", []string{"There are 3.", "中文。"}, false,
			"English|There are 3.\nMandarin|中文。\n"},
		{"single digit before space", []string{"There are 3. Next are 12."}, false,
			"English|There are 3.\nEnglish|Next are 12.\n"},
		{"mixed decimal remains intact", []string{"金额为3.", "14元。"}, false,
			"Mandarin|金额为3.14元。\n"},
		{"numeric default", []string{"12345"}, false, "Mandarin|12345\n"},
		{"English continuation", []string{"There are 12 files!", "12345"}, false,
			"English|There are 12 files!\nEnglish|12345\n"},
		{"language switch", []string{"There are 12 files!共有3个。", "456"}, false,
			"English|There are 12 files!\nMandarin|共有3个。\nMandarin|456\n"},
		{"reset language", []string{"There are 12 files!", "12345"}, true,
			"English|There are 12 files!\nMandarin|12345\n"},
		{"long numeric continuation", []string{"ID " + strings.Repeat("1", maxChunkRunes+10)}, false,
			"English|ID\nEnglish|" + strings.Repeat("1", maxChunkRunes) + "\nEnglish|" + strings.Repeat("1", 10) + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(logPath, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			provider := fixtureProvider(t, tts.ProviderConfig{Extra: map[string]any{"pace_audio": false}})
			stream, err := provider.BeginStream(context.Background(), &testSink{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { tts.Cancel(stream); stream.Close() })
			for i, fragment := range tc.fragments {
				if tc.reset && i > 0 {
					stream.(*session).ResetBuffer()
				}
				if err := stream.WriteText(fragment); err != nil {
					t.Fatal(err)
				}
			}
			if err := stream.Close(); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(logPath)
			if err != nil || string(got) != tc.want {
				t.Fatalf("subprocess input = %q, err=%v; want %q", got, err, tc.want)
			}
			assertNoTempWAV(t, tempDir)
		})
	}
}

func TestEkhoSubprocessStderrIsBounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stderr boundedStderr
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "printf '%8192s' '' >&2")
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if got := len(stderr.String()); got != 4096 {
		t.Fatalf("retained %d stderr bytes, want 4096", got)
	}
}

func assertNoTempWAV(t *testing.T, dir string) {
	t.Helper()
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary files leaked: %v, %v", files, err)
	}
}

func TestEkhoChunkBoundaries(t *testing.T) {
	for _, input := range []string{
		"短句。English!\n最后一句", "Dr. Smith paid 3.14 dollars. Next sentence? tail",
		strings.Repeat("没有标点", 140), strings.Repeat("long English text ", 45),
		"你好！ --text; echo 'a'。", "fragmented UTF-8 中文。尾句",
	} {
		t.Run(input[:min(len(input), 12)], func(t *testing.T) {
			buffer, restored := "", ""
			// Byte-by-byte writes exercise punctuation and split UTF-8 sequences.
			for i := 0; i < len(input); i++ {
				buffer += input[i : i+1]
				for n := nextBoundary(buffer, false); n > 0; n = nextBoundary(buffer, false) {
					chunk := buffer[:n]
					if !utf8.ValidString(chunk) || utf8.RuneCountInString(chunk) > maxChunkRunes {
						t.Fatalf("invalid/big chunk: %q", chunk)
					}
					restored += chunk
					buffer = buffer[n:]
				}
			}
			restored += buffer[:nextBoundary(buffer, true)]
			if restored != input {
				t.Fatalf("lost/duplicated text: %q", restored)
			}
		})
	}
	for _, input := range []string{"3.14", "Dr. Smith", "e.g. apples"} {
		if n := nextBoundary(input, false); n != 0 {
			t.Fatalf("split decimal/abbreviation %q at %d", input, n)
		}
	}
	for _, prefixSize := range []int{maxChunkRunes - 2, maxChunkRunes - 1} {
		prefix := strings.Repeat("中", prefixSize)
		for _, fragments := range [][]string{{prefix + "3.14元。"}, {prefix + "3.", "14元。"}, {prefix + "3", ".14元。"}} {
			var buffer string
			var chunks []string
			for _, fragment := range fragments {
				buffer += fragment
				for n := nextBoundary(buffer, false); n > 0; n = nextBoundary(buffer, false) {
					chunks = append(chunks, buffer[:n])
					buffer = buffer[n:]
				}
			}
			if len(chunks) != 2 || chunks[0] != prefix || chunks[1] != "3.14元。" || buffer != "" {
				t.Fatalf("decimal split for prefix %d, fragments %q: chunks %q, tail %q", prefixSize, fragments, chunks, buffer)
			}
		}
	}
}

func TestEkhoWAVValidation(t *testing.T) {
	pcm := bytes.Repeat([]byte{2, 0}, 10000)
	valid := testWAV(pcm)
	sink := &testSink{}
	if err := writeWAVPCM(context.Background(), bytes.NewReader(valid), sink); err != nil || !bytes.Equal(sink.pcm, pcm) {
		t.Fatalf("stream padded WAV: err=%v, bytes=%d", err, len(sink.pcm))
	}
	wrongRate := bytes.Clone(valid)
	binary.LittleEndian.PutUint32(wrongRate[34:38], 16000)
	for name, input := range map[string][]byte{
		"truncated header": valid[:8], "truncated audio": valid[:len(valid)-1],
		"wrong rate": wrongRate, "odd PCM": testWAV([]byte{1}), "empty PCM": testWAV(nil),
	} {
		t.Run(name, func(t *testing.T) {
			if err := writeWAVPCM(context.Background(), bytes.NewReader(input), &testSink{}); err == nil {
				t.Fatal("invalid WAV accepted")
			}
		})
	}
	failure := errors.New("playback failed")
	if err := writeWAVPCM(context.Background(), bytes.NewReader(valid), &testSink{err: failure}); !errors.Is(err, failure) {
		t.Fatalf("sink failure = %v", err)
	}
}

func TestEkhoPacingHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target := &testSink{}
	sink := &pacedSink{AudioSink: target, ctx: ctx}
	// The next write must wait for this audio's playback budget, and must be
	// cancellable without sending it to the device or waiting for that budget.
	first := make([]byte, sampleRate*2)
	if err := sink.WritePCM(first); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- sink.WritePCM([]byte{0, 0}) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pacing wait error = %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("pacing wait did not cancel promptly")
	}
	if len(target.pcm) != len(first) {
		t.Fatal("cancelled audio reached playback")
	}
}

func TestEkhoPacingAfterIdleGap(t *testing.T) {
	sink := &pacedSink{AudioSink: &testSink{}, ctx: context.Background(), next: time.Now().Add(-time.Minute)}
	start := time.Now()
	for i := 0; i < 4; i++ {
		if err := sink.WritePCM(make([]byte, sampleRate/10*2)); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("audio flooded the queue after an idle gap: %s", elapsed)
	}
}
