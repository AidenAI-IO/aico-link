package agent

import (
	"bytes"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

type blockingReplaySTTUploader struct {
	firstStarted chan struct{}
	releaseFirst chan struct{}
	writes       [][]byte
	finalized    atomic.Bool
}

func (u *blockingReplaySTTUploader) UploadPCM(pcm []byte) error {
	u.writes = append(u.writes, append([]byte(nil), pcm...))
	if len(u.writes) == 1 {
		close(u.firstStarted)
		<-u.releaseFirst
	}
	return nil
}

func (u *blockingReplaySTTUploader) Finalize() (string, error) {
	u.finalized.Store(true)
	return "", nil
}

func (u *blockingReplaySTTUploader) Close() error { return nil }

func waitForReplay(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for buffered PCM upload")
	}
}

func TestSTTPCMPreReadyBufferCopiesAndDrainsInOrder(t *testing.T) {
	b := newSTTPCMPreReadyBuffer(16000, 1, 16)
	first := []byte{1, 2}
	second := []byte{3, 4}
	if !b.append(first) || !b.append(second) {
		t.Fatal("append unexpectedly rejected a bounded chunk")
	}
	first[0] = 9
	got, overflow := b.drain()
	if overflow {
		t.Fatal("drain reported overflow")
	}
	want := [][]byte{{1, 2}, {3, 4}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("drained chunks = %#v, want %#v", got, want)
	}
	if _, overflow := b.drain(); overflow {
		t.Fatal("second drain reported stale overflow")
	}
}

func TestSTTPCMPreReadyBufferDropsPrefixOnOverflow(t *testing.T) {
	b := newSTTPCMPreReadyBuffer(1, 1, 8)
	if !b.append([]byte{1, 2}) {
		t.Fatal("first chunk should fit")
	}
	if b.append([]byte{3}) {
		t.Fatal("chunk exceeding the bound should be rejected")
	}
	got, overflow := b.drain()
	if !overflow {
		t.Fatal("drain did not report overflow")
	}
	if len(got) != 0 {
		t.Fatalf("overflow retained %#v, want no replayable prefix", got)
	}
}

func TestAudioDialogReplaysPreReadySTTChunksWhenReady(t *testing.T) {
	uploader := &stubSTTStreamUploader{}
	dialog := &AudioDialog{}
	dialog.recordSTTMu.Lock()
	dialog.recordSTTSessionID = 42
	dialog.recordSTTPreReady = newSTTPCMPreReadyBuffer(16000, 1, 16)
	dialog.recordSTTMu.Unlock()

	first := []byte{1, 2}
	dialog.uploadRecordChunkToStreamingSTT(42, &AudioChunkResult{PCM: first})
	first[0] = 9
	dialog.uploadRecordChunkToStreamingSTT(7, &AudioChunkResult{PCM: []byte{8, 8}})

	overflow, installed, err := dialog.installRecordSTT(42, &streamingSTTSession{uploader: uploader}, dialog.recordSTTPreReady)
	if err != nil || !installed || overflow {
		t.Fatalf("install result installed=%v overflow=%v err=%v", installed, overflow, err)
	}
	if !reflect.DeepEqual(uploader.writes, [][]byte{{1, 2}}) {
		t.Fatalf("replayed writes = %#v, want [[1 2]]", uploader.writes)
	}
	if bytes.Equal(uploader.writes[0], first) {
		t.Fatal("replay unexpectedly observed caller buffer mutation")
	}
}

func TestAudioDialogReplaysBufferedSTTBeforeLiveChunks(t *testing.T) {
	uploader := &blockingReplaySTTUploader{firstStarted: make(chan struct{}), releaseFirst: make(chan struct{})}
	dialog := &AudioDialog{recordSTTSessionID: 42, recordSTTPreReady: newSTTPCMPreReadyBuffer(16000, 1, 16)}
	dialog.uploadRecordChunkToStreamingSTT(42, &AudioChunkResult{PCM: []byte{1}})
	preReady := dialog.recordSTTPreReady

	type installResult struct {
		overflow, installed bool
		err                 error
	}
	done := make(chan installResult, 1)
	go func() {
		overflow, installed, err := dialog.installRecordSTT(42, &streamingSTTSession{uploader: uploader}, preReady)
		done <- installResult{overflow, installed, err}
	}()
	waitForReplay(t, uploader.firstStarted)
	dialog.uploadRecordChunkToStreamingSTT(42, &AudioChunkResult{PCM: []byte{2}})
	if got := dialog.currentRecordSTT(); got != nil {
		t.Fatal("streaming session became visible before buffered replay completed")
	}
	close(uploader.releaseFirst)
	select {
	case result := <-done:
		if result.err != nil || !result.installed || result.overflow {
			t.Fatalf("install result = %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for buffered replay")
	}
	if want := [][]byte{{1}, {2}}; !reflect.DeepEqual(uploader.writes, want) {
		t.Fatalf("uploaded PCM = %#v, want %#v", uploader.writes, want)
	}
}

func TestAudioDialogStopDuringBufferedSTTReplaySkipsFinalize(t *testing.T) {
	uploader := &blockingReplaySTTUploader{firstStarted: make(chan struct{}), releaseFirst: make(chan struct{})}
	dialog := &AudioDialog{
		recordBackend:      warmupRecordingBackend{},
		recordActive:       true,
		sessionID:          42,
		recordSTTSessionID: 42,
		recordSTTPreReady:  newSTTPCMPreReadyBuffer(16000, 1, 16),
	}
	dialog.uploadRecordChunkToStreamingSTT(42, &AudioChunkResult{PCM: []byte{1}})
	preReady := dialog.recordSTTPreReady
	type installResult struct {
		installed bool
		err       error
	}
	done := make(chan installResult, 1)
	go func() {
		_, installed, err := dialog.installRecordSTT(42, &streamingSTTSession{uploader: uploader}, preReady)
		done <- installResult{installed, err}
	}()
	waitForReplay(t, uploader.firstStarted)
	dialog.uploadRecordChunkToStreamingSTT(42, &AudioChunkResult{PCM: []byte{2}})
	if err := dialog.StopRecording(); err != nil {
		t.Fatalf("StopRecording() error = %v", err)
	}
	if uploader.finalized.Load() {
		t.Fatal("stopped recording finalized a session with incomplete replay")
	}
	close(uploader.releaseFirst)
	select {
	case result := <-done:
		if result.installed || result.err != nil {
			t.Fatalf("install result after stop = %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stopped replay")
	}
	if got := dialog.currentRecordSTT(); got != nil {
		t.Fatal("stopped recording retained streaming STT session")
	}
}

func TestAudioDialogInstallPreReadyRejectsStoppedSession(t *testing.T) {
	dialog := &AudioDialog{}
	dialog.recordSTTMu.Lock()
	dialog.recordSTTSessionID = 42
	dialog.recordSTTPreReady = newSTTPCMPreReadyBuffer(16000, 1, 16)
	dialog.recordSTTMu.Unlock()
	preReady := dialog.recordSTTPreReady

	if got := dialog.takeRecordSTT(); got != nil {
		t.Fatalf("takeRecordSTT() = %#v, want nil", got)
	}
	if _, installed, _ := dialog.installRecordSTT(42, &streamingSTTSession{uploader: &stubSTTStreamUploader{}}, preReady); installed {
		t.Fatal("stale provider session installed after recording stopped")
	}
}
