package agent

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestAudioRequestUsesTopLevelFormatFields(t *testing.T) {
	req := audioRequest{
		Op:         "start_playback",
		SampleRate: 32000,
		Channels:   1,
		BitWidth:   16,
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	if decoded["sample_rate"] != float64(32000) {
		t.Fatalf("sample_rate = %v, want 32000", decoded["sample_rate"])
	}
	if decoded["channels"] != float64(1) {
		t.Fatalf("channels = %v, want 1", decoded["channels"])
	}
	if decoded["bit_width"] != float64(16) {
		t.Fatalf("bit_width = %v, want 16", decoded["bit_width"])
	}
	if _, ok := decoded["format"]; ok {
		t.Fatalf("unexpected nested format field in request: %s", string(data))
	}
}

func TestAudioRequestVolumeField(t *testing.T) {
	req := audioRequest{
		Op:     "set_playback_volume",
		Volume: 65,
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	if decoded["volume"] != float64(65) {
		t.Fatalf("volume = %v, want 65", decoded["volume"])
	}
}

// serveReapedRecordChunks answers exactly one chunk per connection and then
// closes it, reproducing how audio_service reaps an idle client connection.
func serveReapedRecordChunks(t *testing.T, socketPath string, chunks ...string) {
	t.Helper()
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for _, chunk := range chunks {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if _, err := readUdsMessage(conn); err != nil {
				_ = conn.Close()
				return
			}
			_ = writeUdsMessage(conn, udsMessage{
				HeaderJSON: `{"status":"OK"}`,
				Payload:    []byte(chunk),
			})
			// The service closes the connection; the reader's next write hits a
			// socket the server has already discarded.
			_ = conn.Close()
		}
	}()
}

func audioTestSocketPath(t *testing.T) string {
	t.Helper()
	// Unix socket paths are limited to ~104 bytes on macOS, so avoid t.TempDir().
	dir, err := os.MkdirTemp("", "as")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "a.sock")
}

func TestRecordChunkStreamFallsBackWhenServiceClosesConnection(t *testing.T) {
	socketPath := audioTestSocketPath(t)
	serveReapedRecordChunks(t, socketPath, "first", "second", "third")

	stream := NewAudioServiceClient(socketPath).OpenRecordChunkStream(7)
	defer stream.Close()

	// The first read uses the persistent connection; the service closes it
	// afterwards. The stream must keep reading through the per-request path
	// instead of failing the realtime session.
	for _, want := range []string{"first", "second", "third"} {
		chunk, err := stream.Read(200)
		if err != nil {
			t.Fatalf("read %q after the service closed the connection: %v", want, err)
		}
		if string(chunk.PCM) != want {
			t.Fatalf("chunk = %q, want %q", chunk.PCM, want)
		}
	}
}

func TestRecordChunkStreamReportsServiceUnavailable(t *testing.T) {
	socketPath := audioTestSocketPath(t)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	stream := NewAudioServiceClient(socketPath).OpenRecordChunkStream(7)
	defer stream.Close()

	accepted, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	// The service goes away entirely: the fallback must surface an error
	// rather than hide the failure.
	_ = accepted.Close()
	_ = listener.Close()

	if _, err := stream.Read(200); err == nil {
		t.Fatal("read after the audio service disappeared = nil, want an error")
	}
}

func TestSetPlaybackVolumeRejectsOutOfRange(t *testing.T) {
	client := NewAudioServiceClient("/tmp/does-not-matter.sock")

	for _, volume := range []int{-1, 101} {
		if err := client.SetPlaybackVolume(volume); err == nil {
			t.Fatalf("expected error for volume %d", volume)
		}
	}
}
