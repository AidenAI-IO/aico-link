package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aico-agent/internal/agent/messages"
	"aico-agent/internal/lanaccess"
)

func postSharedFile(t *testing.T, handler http.Handler, name, content string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(part, content)
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/api/files", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestSharedFilesUploadDownloadAndHistory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AICO_SHARE_DIR", dir)
	handler := (&Server{}).sharedFilesHandler()
	name, content := "报告 (final) #1.txt", "shared file bytes\n"
	response := postSharedFile(t, handler, name, content)
	if response.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", response.Code, response.Body)
	}
	var uploaded sharedFile
	if err := json.Unmarshal(response.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	if uploaded.Path != filepath.Join(dir, name) || uploaded.Size != int64(len(content)) {
		t.Fatalf("metadata: %+v", uploaded)
	}
	duplicate := postSharedFile(t, handler, name, "second upload")
	var second sharedFile
	_ = json.Unmarshal(duplicate.Body.Bytes(), &second)
	if duplicate.Code != http.StatusCreated || second.Path == uploaded.Path {
		t.Fatalf("collision not preserved: %s", duplicate.Body)
	}
	original, _ := os.ReadFile(uploaded.Path)
	if string(original) != content {
		t.Fatal("original file overwritten")
	}

	longName := strings.Repeat("报", 78) + ".txt"
	if postSharedFile(t, handler, longName, "one").Code != 201 {
		t.Fatal("long filename rejected")
	}
	longDuplicate := postSharedFile(t, handler, longName, "two")
	var longFile sharedFile
	_ = json.Unmarshal(longDuplicate.Body.Bytes(), &longFile)
	if longDuplicate.Code != 201 {
		t.Fatal("long duplicate failed")
	}
	if _, err := sharedFileMetadata(longFile.Path); err != nil {
		t.Fatalf("renamed long filename unavailable: %v", err)
	}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		request := httptest.NewRequest(method, uploaded.DownloadURL, nil)
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 200 || !strings.HasPrefix(response.Header().Get("Content-Disposition"), "attachment;") {
			t.Fatalf("download: %d %v", response.Code, response.Header())
		}
		if method == http.MethodGet && response.Body.String() != content {
			t.Fatal("download content differs")
		}
		if method == http.MethodHead && response.Body.Len() != 0 {
			t.Fatal("HEAD returned a body")
		}
	}
	request := httptest.NewRequest(http.MethodGet, uploaded.DownloadURL, nil)
	request.Header.Set("Range", "bytes=0-5")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusPartialContent || response.Body.String() != content[:6] {
		t.Fatalf("range: %d %s", response.Code, response.Body)
	}

	input, history, err := (&Server{}).resolveRequestInput(ChatRequest{Attachments: []MessageAttachment{{Kind: "file", Path: uploaded.Path, Name: "forged", MIMEType: "image/png", Size: 999}}})
	if err != nil {
		t.Fatal(err)
	}
	if history[0].Name != name || history[0].DownloadURL != uploaded.DownloadURL || input.Attachments[0].Path != uploaded.Path || len(input.Attachments[0].Data) != 0 {
		t.Fatalf("file input: %+v %+v", input, history)
	}
	stored := userMessageFromInput(nil, input.InputText, input.Attachments)
	if !strings.Contains(stored.Content, uploaded.Path) || len(stored.Attachments) != 0 {
		t.Fatalf("provider input: %+v", stored)
	}
	encoded, _ := json.Marshal(stored)
	var restored messages.Message
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	projected, ok := webMessageFromContextMessage(restored, "backend")
	if !ok || len(projected.Attachments) != 1 || projected.Attachments[0].DownloadURL != uploaded.DownloadURL {
		t.Fatalf("history lost shared file: %+v", projected)
	}
}

func TestSharedFilesExternalOutputsAndConfinement(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AICO_SHARE_DIR", dir)
	if err := os.Mkdir(filepath.Join(dir, "outputs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "outputs", "report.pdf"), []byte("generated"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "private.txt")
	_ = os.WriteFile(outside, []byte("secret"), 0600)
	if err := os.Symlink(outside, filepath.Join(dir, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, ".upload-incomplete"), []byte("partial"), 0600)
	handler := (&Server{}).sharedFilesHandler()
	for _, route := range []string{"/share/outputs/report.pdf", "/api/files?path=outputs", "/share/outputs/"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, route, nil))
		if response.Code != 200 {
			t.Fatalf("external output route %s: %d", route, response.Code)
		}
	}
	for _, rel := range []string{"escape.txt", ".upload-incomplete", "outputs/../../private.txt", "outputs\\report.pdf"} {
		if _, err := sharedFileMetadata(dir + "/" + rel); err == nil {
			t.Fatalf("accepted invalid attachment %q", rel)
		}
	}
	for _, route := range []string{"/share/escape.txt", "/share/.upload-incomplete", "/share/%2e%2e/private.txt", "/share/outputs%2f..%2f..%2fprivate.txt"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, route, nil))
		if response.Code == 200 || strings.Contains(response.Body.String(), "secret") {
			t.Fatalf("exposed %s: %d", route, response.Code)
		}
	}
	for _, name := range []string{"../escape.txt", ".hidden", "folder/file.txt", "folder\\file.txt"} {
		if response := postSharedFile(t, handler, name, "bad"); response.Code != http.StatusBadRequest {
			t.Fatalf("accepted filename %q: %d", name, response.Code)
		}
	}
	entries, err := listSharedFiles("")
	if err != nil || len(entries) != 1 || entries[0].Kind != "directory" {
		t.Fatalf("listing exposed hidden files or links: %+v %v", entries, err)
	}
}

func TestSharedFilesRejectIncompleteAndOversizedUploads(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AICO_SHARE_DIR", dir)
	// Exercise the same streaming handler with a small limit so this regression
	// test does not need a 100 MiB disk allocation.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { uploadSharedFileWithLimit(w, r, 16) })
	response := postSharedFile(t, handler, "large.txt", strings.Repeat("x", 17))
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: %d", response.Code)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "partial.txt")
	_, _ = part.Write([]byte("partial"))
	request := httptest.NewRequest(http.MethodPost, "/api/files", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("incomplete: %d", response.Code)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("failed upload left files: %v", entries)
	}
}

func TestSharedFilesRejectCrossOriginBrowserUploads(t *testing.T) {
	t.Setenv("AICO_SHARE_DIR", t.TempDir())
	handler := (&Server{}).sharedFilesHandler()
	for _, origin := range []string{"https://untrusted.example", "null"} {
		request := httptest.NewRequest(http.MethodPost, "http://board:8080/api/files", strings.NewReader("ignored"))
		request.Header.Set("Origin", origin)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden || response.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("cross-origin upload permitted: %d", response.Code)
		}
	}
}

func TestSharedFilesListenerFailureKeepsMainServerAvailable(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	t.Setenv("AICO_SHARE_DIR", t.TempDir())
	t.Setenv("AICO_SHARE_ADDR", occupied.Addr().String())
	server := &Server{addr: "127.0.0.1:0", eventBroadcaster: NewEventBroadcaster()}
	defer server.Close()
	done := make(chan error, 1)
	go func() { done <- server.Start() }()
	var address string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		server.httpMu.Lock()
		if server.httpListener != nil {
			address = server.httpListener.Addr().String()
		}
		server.httpMu.Unlock()
		if address != "" {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Agent exited on share bind failure: %v", err)
		default:
		}
		time.Sleep(time.Millisecond)
	}
	if address == "" {
		t.Fatal("main listener never started")
	}
	response, err := http.Get("http://" + address + "/api/files")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("main file routes unavailable: %d", response.StatusCode)
	}
	server.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown timed out")
	}
}

func TestSharedFilesListenerHonorsLANGuard(t *testing.T) {
	shareDir := t.TempDir()
	t.Setenv("AICO_SHARE_DIR", shareDir)
	t.Setenv("AICO_SHARE_ADDR", "127.0.0.1:0")
	if err := os.WriteFile(filepath.Join(shareDir, "report.txt"), []byte("report"), 0600); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	state := lanaccess.NewState(filepath.Join(stateDir, "enabled"), filepath.Join(stateDir, "password"), filepath.Join(stateDir, "secret"))
	server := &Server{addr: "127.0.0.1:0", eventBroadcaster: NewEventBroadcaster()}
	server.EnableLANGuard(state, USBIngress{})
	defer server.Close()
	done := make(chan error, 1)
	go func() { done <- server.Start() }()
	var handler http.Handler
	var mainHandler http.Handler
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		server.httpMu.Lock()
		if server.shareServer != nil {
			handler = server.shareServer.Handler
			mainHandler = server.httpServer.Handler
		}
		server.httpMu.Unlock()
		if handler != nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Agent exited before shared listener started: %v", err)
		default:
		}
		time.Sleep(time.Millisecond)
	}
	if handler == nil {
		t.Fatal("shared listener never started")
	}
	request := httptest.NewRequest(http.MethodGet, "/share/report.txt", nil)
	request.RemoteAddr = "192.168.1.2:12345"
	request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("192.168.1.1"), Port: 8000}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("LAN request bypassed guard: %d", response.Code)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request.WithContext(lanaccess.WithTrusted(request.Context(), true)))
	if response.Code != http.StatusOK || response.Body.String() != "report" {
		t.Fatalf("USB request rejected: %d %s", response.Code, response.Body.String())
	}
	if err := state.SetPassword("shared-files-test-password"); err != nil {
		t.Fatal(err)
	}
	if err := state.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated LAN request accepted: %d", response.Code)
	}
	login := httptest.NewRequest(http.MethodPost, lanaccess.LoginAPI, strings.NewReader(`{"password":"shared-files-test-password"}`)).WithContext(request.Context())
	login.RemoteAddr = request.RemoteAddr
	login.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	mainHandler.ServeHTTP(response, login)
	var session struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &session); err != nil || response.Code != http.StatusOK || session.Token == "" {
		t.Fatalf("login failed: status=%d err=%v", response.Code, err)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("login did not set the session cookie")
	}
	// A login on 8080 must authorize every file route on both ports. Invalid
	// credentials must be rejected before serving bytes or persisting uploads.
	for port, guarded := range map[string]http.Handler{"8080": mainHandler, "8000": handler} {
		for _, route := range []struct {
			method, path string
			status       int
		}{{http.MethodGet, "/api/files", 200}, {http.MethodGet, "/share/", 200}, {http.MethodGet, "/share/report.txt", 200}, {http.MethodHead, "/share/report.txt", 200}, {http.MethodPost, "/api/files", 201}} {
			for _, credential := range []string{"none", "invalid", "cookie", "bearer"} {
				t.Run(port+"/"+route.method+route.path+"/"+credential, func(t *testing.T) {
					var body bytes.Buffer
					writer := multipart.NewWriter(&body)
					if route.method == http.MethodPost {
						part, _ := writer.CreateFormFile("file", "upload.txt")
						_, _ = part.Write([]byte("uploaded"))
						_ = writer.Close()
					}
					r := httptest.NewRequest(route.method, route.path, &body).WithContext(request.Context())
					r.RemoteAddr = request.RemoteAddr
					r.Header.Set("Content-Type", writer.FormDataContentType())
					switch credential {
					case "invalid":
						r.Header.Set("Authorization", "Bearer invalid-session")
					case "cookie":
						r.AddCookie(cookies[0])
					case "bearer":
						r.Header.Set("Authorization", "Bearer "+session.Token)
					}
					w := httptest.NewRecorder()
					guarded.ServeHTTP(w, r)
					want := route.status
					if credential == "none" || credential == "invalid" {
						want = http.StatusUnauthorized
					}
					if w.Code != want {
						t.Fatalf("status=%d, want %d", w.Code, want)
					}
				})
			}
		}
	}
	if err := state.SetPassword("replacement-test-password"); err != nil {
		t.Fatal(err)
	}
	for port, guarded := range map[string]http.Handler{"8080": mainHandler, "8000": handler} {
		for _, credential := range []string{"cookie", "bearer"} {
			t.Run(port+"/revoked-"+credential, func(t *testing.T) {
				r := httptest.NewRequest(http.MethodGet, "/share/report.txt", nil).WithContext(request.Context())
				r.RemoteAddr = request.RemoteAddr
				if credential == "cookie" {
					r.AddCookie(cookies[0])
				} else {
					r.Header.Set("Authorization", "Bearer "+session.Token)
				}
				w := httptest.NewRecorder()
				guarded.ServeHTTP(w, r)
				if w.Code != http.StatusUnauthorized {
					t.Fatalf("revoked session accepted: %d", w.Code)
				}
			})
		}
	}
	server.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown timed out")
	}
}
