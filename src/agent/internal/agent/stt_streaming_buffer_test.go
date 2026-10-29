package agent

import (
	"bytes"
	"reflect"
	"testing"
)

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

	queued, overflow, installed := dialog.installRecordSTT(42, &streamingSTTSession{uploader: uploader})
	if !installed || overflow {
		t.Fatalf("install result installed=%v overflow=%v", installed, overflow)
	}
	for _, pcm := range queued {
		if err := dialog.currentRecordSTT().UploadPCM(pcm); err != nil {
			t.Fatalf("replay UploadPCM() error = %v", err)
		}
	}
	if !reflect.DeepEqual(uploader.writes, [][]byte{{1, 2}}) {
		t.Fatalf("replayed writes = %#v, want [[1 2]]", uploader.writes)
	}
	if bytes.Equal(uploader.writes[0], first) {
		t.Fatal("replay unexpectedly observed caller buffer mutation")
	}
}

func TestAudioDialogInstallPreReadyRejectsStoppedSession(t *testing.T) {
	dialog := &AudioDialog{}
	dialog.recordSTTMu.Lock()
	dialog.recordSTTSessionID = 42
	dialog.recordSTTPreReady = newSTTPCMPreReadyBuffer(16000, 1, 16)
	dialog.recordSTTMu.Unlock()

	if got := dialog.takeRecordSTT(); got != nil {
		t.Fatalf("takeRecordSTT() = %#v, want nil", got)
	}
	if _, _, installed := dialog.installRecordSTT(42, &streamingSTTSession{uploader: &stubSTTStreamUploader{}}); installed {
		t.Fatal("stale provider session installed after recording stopped")
	}
}
