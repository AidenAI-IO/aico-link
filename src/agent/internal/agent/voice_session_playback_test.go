package agent

import (
	"sync"
	"testing"

	"aico-agent/internal/agent/tts"
)

// scriptedRecording returns the queued chunks, then empty reads.
type scriptedRecording struct {
	mu      sync.Mutex
	chunks  [][]byte
	started int
}

func (r *scriptedRecording) StartRecording(AudioFormat) (*RecordStartResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started++
	return &RecordStartResult{SessionID: 1}, nil
}

func (r *scriptedRecording) ReadRecordChunk(uint64, uint32) (*AudioChunkResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.chunks) == 0 {
		return &AudioChunkResult{}, nil
	}
	chunk := r.chunks[0]
	r.chunks = r.chunks[1:]
	return &AudioChunkResult{PCM: chunk}, nil
}

func (r *scriptedRecording) StopRecording(uint64) error { return nil }

type recordedPlayback struct {
	mu     sync.Mutex
	writes [][]byte
}

func (p *recordedPlayback) StartPlayback(tts.AudioFormat) (uint64, error) { return 1, nil }
func (p *recordedPlayback) WritePlayChunk(_ uint64, data []byte, _ bool) error {
	p.mu.Lock()
	p.writes = append(p.writes, append([]byte(nil), data...))
	p.mu.Unlock()
	return nil
}
func (p *recordedPlayback) StopPlayback(uint64) error { return nil }

type scriptedWebAudio struct {
	recording *scriptedRecording
	playback  *recordedPlayback
}

func (scriptedWebAudio) Route() RealtimeAudioRoute           { return RealtimeAudioRouteWeb }
func (a scriptedWebAudio) Recording() AudioRecordingBackend  { return a.recording }
func (a scriptedWebAudio) Playback() tts.AudioServiceBackend { return a.playback }
func (scriptedWebAudio) Close(string)                        {}

// A Classic dialog serving a browser conversation captures and plays through
// the browser, never the device backend, and applies the conversation's mute
// and End.
func TestClassicDialogUsesBrowserConversationAudio(t *testing.T) {
	device := &scriptedRecording{}
	devicePlayback := &recordedPlayback{}
	web := scriptedWebAudio{
		recording: &scriptedRecording{chunks: [][]byte{{1, 2}, {3, 4}}},
		playback:  &recordedPlayback{},
	}
	dialog := &AudioDialog{recordBackend: device, ttsPlaybackBackend: devicePlayback}
	voice := NewRealtimeVoiceSession(web)
	dialog.SetVoiceSession(voice)

	if err := dialog.StartRecording(); err != nil {
		t.Fatal(err)
	}
	if web.recording.started != 1 || device.started != 0 {
		t.Fatalf("recording starts: browser=%d device=%d, want only the browser", web.recording.started, device.started)
	}
	chunk, err := dialog.ReadRecordChunk(10)
	if err != nil || string(chunk.PCM) != string([]byte{1, 2}) {
		t.Fatalf("chunk = %+v, %v; want the browser's audio", chunk, err)
	}
	voice.muted.Store(true)
	chunk, err = dialog.ReadRecordChunk(10)
	if err != nil || string(chunk.PCM) != string([]byte{0, 0}) {
		t.Fatalf("muted chunk = %+v, %v; want silence", chunk, err)
	}

	playback := dialog.currentTTSPlaybackBackend()
	voice.silenced.Store(true)
	if err := playback.WritePlayChunk(1, []byte{9, 9}, false); err != nil {
		t.Fatal(err)
	}
	if len(devicePlayback.writes) != 0 || len(web.playback.writes) != 1 || string(web.playback.writes[0]) != string([]byte{0, 0}) {
		t.Fatalf("playback: device=%v browser=%v; want silence in the browser only", devicePlayback.writes, web.playback.writes)
	}

	voice.RequestStop()
	chunk, err = dialog.ReadRecordChunk(10)
	if err != nil || !chunk.EndOfStream {
		t.Fatalf("chunk after End = %+v, %v; want the end of the stream", chunk, err)
	}
	if err := dialog.StopRecording(); err != nil {
		t.Fatal(err)
	}
	dialog.SetVoiceSession(nil)
	if got := dialog.currentTTSPlaybackBackend(); got != devicePlayback {
		t.Fatal("playback did not return to the device once the conversation ended")
	}
}
