package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLookSavesScreenshotLocally(t *testing.T) {
	var frame bytes.Buffer
	if err := jpeg.Encode(&frame, image.NewRGBA(image.Rect(0, 0, 200, 100)), nil); err != nil {
		t.Fatal(err)
	}
	output, err := json.Marshal(map[string]string{"data": base64.StdEncoding.EncodeToString(frame.Bytes())})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		scoped bool
		region []string
		size   image.Point
	}{
		{"direct full frame", false, nil, image.Pt(200, 100)},
		{"scoped cropped frame", true, []string{"--region", "250", "200", "750", "600"}, image.Pt(100, 40)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := "/api/tools/screenshot"
				if tc.scoped {
					want = "/command"
				}
				if r.Method != http.MethodPost || r.URL.Path != want {
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"output": string(output)})
			}))
			defer server.Close()
			t.Setenv("AICO_DEVICE_CLI_URL", server.URL)
			token := ""
			if tc.scoped {
				token = "test-token"
			}
			t.Setenv("AICO_DEVICE_CLI_TOKEN", token)
			dir := t.TempDir()
			if err := run(server.URL, append([]string{"look", "--dir", dir}, tc.region...)); err != nil {
				t.Fatal(err)
			}
			files, err := filepath.Glob(filepath.Join(dir, "*.jpg"))
			if err != nil || len(files) != 1 {
				t.Fatalf("screenshot files=%v, error=%v", files, err)
			}
			saved, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			img, err := jpeg.Decode(bytes.NewReader(saved))
			if err != nil {
				t.Fatal(err)
			}
			if img.Bounds().Size() != tc.size {
				t.Fatalf("saved %v, want %v", img.Bounds().Size(), tc.size)
			}
			if tc.region == nil && !bytes.Equal(saved, frame.Bytes()) {
				t.Fatal("full screenshot was re-encoded")
			}
		})
	}
}

func TestRunRejectsNotDispatchedResponseWithScreenshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output":   `{"data":"aW1hZ2U=","action_output":"not sent: screen turned"}`,
			"is_error": true,
			"error":    "not sent: screen turned",
		})
	}))
	defer server.Close()
	t.Setenv("AICO_DEVICE_CLI_TOKEN", "")
	if err := run(server.URL, []string{"tap", "100", "250"}); err == nil || err.Error() != "not sent: screen turned" {
		t.Fatalf("expected the rejected tap to fail even with a screenshot, got %v", err)
	}
}
