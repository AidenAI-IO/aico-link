package agent

import (
	"reflect"
	"testing"
	"time"
)

type blockingFinalizeSTTUploader struct {
	started chan struct{}
	release chan struct{}
}

func (u *blockingFinalizeSTTUploader) UploadPCM([]byte) error { return nil }
func (u *blockingFinalizeSTTUploader) Finalize() (string, error) {
	close(u.started)
	<-u.release
	return "previous turn", nil
}
func (u *blockingFinalizeSTTUploader) Close() error { return nil }

func TestAudioDialogRotateSTTBuffersNewTurnBeforeOldFinalize(t *testing.T) {
	previous := &blockingFinalizeSTTUploader{started: make(chan struct{}), release: make(chan struct{})}
	next := &stubSTTStreamUploader{}
	oldBuffer := newSTTPCMPreReadyBuffer(16000, 1, 16)
	dialog := &AudioDialog{
		sttClient:          &stubSTTClient{supportsStreaming: true, streamUploader: next},
		recordActive:       true,
		sessionID:          42,
		recordSTT:          &streamingSTTSession{uploader: previous},
		recordSTTSessionID: 42,
		recordSTTPreReady:  oldBuffer,
	}
	done := make(chan error, 1)
	go func() { done <- dialog.FinalizeAndRotateSTT() }()
	select {
	case <-previous.started:
	case <-time.After(time.Second):
		t.Fatal("old STT finalization did not start")
	}
	dialog.recordSTTMu.Lock()
	newBuffer := dialog.recordSTTPreReady
	dialog.recordSTTMu.Unlock()
	if newBuffer == nil || newBuffer == oldBuffer {
		t.Fatal("new turn did not get a fresh pre-ready PCM buffer before finalization")
	}
	dialog.uploadRecordChunkToStreamingSTT(42, &AudioChunkResult{PCM: []byte{7, 8}})
	if _, installed, err := dialog.installRecordSTT(42,
		&streamingSTTSession{uploader: &stubSTTStreamUploader{}}, oldBuffer); err != nil || installed {
		t.Fatalf("stale STT session installed into new turn: installed=%v err=%v", installed, err)
	}
	close(previous.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("FinalizeAndRotateSTT() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("STT rotation did not complete")
	}
	deadline := time.Now().Add(time.Second)
	for dialog.currentRecordSTT() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if dialog.currentRecordSTT() == nil {
		t.Fatal("next STT session was not installed")
	}
	if want := [][]byte{{7, 8}}; !reflect.DeepEqual(next.writes, want) {
		t.Fatalf("next STT received PCM %#v, want %#v", next.writes, want)
	}
}
