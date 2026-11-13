package agent

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHandleUserFiles_ServesExistingReport(t *testing.T) {
	dir := t.TempDir()
	rpt := filepath.Join(dir, "files_report.html")
	os.WriteFile(rpt, []byte("<html><body>cached text mentions /user_files?refresh=1</body></html>"), 0o644)
	s := &Server{userFilesReportPath: rpt}
	req := httptest.NewRequest(http.MethodGet, "/user_files", nil)
	rec := httptest.NewRecorder()
	s.handleUserFiles(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "cached") {
		t.Errorf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), userFilesRefreshControlMarker) {
		t.Errorf("legacy report response is missing refresh control: %q", rec.Body.String())
	}
}

func TestHandleUserFiles_GeneratesIfMissing(t *testing.T) {
	tools := t.TempDir()
	rpt := filepath.Join(t.TempDir(), "report.html")
	script := filepath.Join(tools, "view_agent_files.sh")
	body := "#!/bin/sh\necho '<html>generated</html>' > \"$1\"\n"
	os.WriteFile(script, []byte(body), 0o755)
	s := &Server{userFilesReportPath: rpt, userFilesToolsDir: tools}
	req := httptest.NewRequest(http.MethodGet, "/user_files", nil)
	rec := httptest.NewRecorder()
	s.handleUserFiles(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "generated") {
		t.Errorf("code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestHandleUserFiles_GeneratesConfiguredData(t *testing.T) {
	tools := t.TempDir()
	for _, name := range []string{"view_agent_files.sh", "generate_agent_files_report.py", "agent_files_template.html"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if name == "view_agent_files.sh" {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(tools, name), data, mode); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(t.TempDir(), "agent's data")
	for _, kind := range []string{"memory", "skills", "skill-state"} {
		root := filepath.Join(dir, kind)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "sample.md"), []byte("configured-"+kind+"-content"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{userFilesReportPath: filepath.Join(dir, "files_report.html"), userFilesToolsDir: tools}
	rec := httptest.NewRecorder()
	s.handleUserFiles(rec, httptest.NewRequest(http.MethodGet, "/user_files", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	for _, kind := range []string{"memory", "skills", "skill-state"} {
		if !strings.Contains(rec.Body.String(), "configured-"+kind+"-content") {
			t.Errorf("report is missing %s from the configured data directory", kind)
		}
	}
}

func TestHandleUserFiles_RefreshQueryRegeneratesReport(t *testing.T) {
	tools := t.TempDir()
	rpt := filepath.Join(t.TempDir(), "report.html")
	if err := os.WriteFile(rpt, []byte("<html>stale</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(tools, "view_agent_files.sh")
	body := "#!/bin/sh\necho '<html>fresh</html>' > \"$1\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	s := &Server{userFilesReportPath: rpt, userFilesToolsDir: tools}
	req := httptest.NewRequest(http.MethodGet, "/user_files?refresh=1", nil)
	rec := httptest.NewRecorder()
	s.handleUserFiles(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/user_files" {
		t.Fatalf("Location = %q, want /user_files", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/user_files", nil)
	rec = httptest.NewRecorder()
	s.handleUserFiles(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "fresh") {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestHandleUserFiles_ConcurrentRefreshCoalescesGeneration(t *testing.T) {
	tools := t.TempDir()
	rpt := filepath.Join(t.TempDir(), "report.html")
	marker := filepath.Join(t.TempDir(), "runs")
	if err := os.WriteFile(rpt, []byte("<html>stale</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(tools, "view_agent_files.sh")
	body := "#!/bin/sh\necho run >> " + marker + "\nsleep 0.2\necho '<html>fresh</html>' > \"$1\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	s := &Server{userFilesReportPath: rpt, userFilesToolsDir: tools}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodGet, "/user_files?refresh=1", nil)
			rec := httptest.NewRecorder()
			s.handleUserFiles(rec, req)
			if rec.Code != http.StatusSeeOther {
				t.Errorf("code=%d body=%q", rec.Code, rec.Body.String())
			}
		}()
	}
	close(start)
	wg.Wait()

	runs, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(runs), "run\n"); got != 1 {
		t.Fatalf("generation runs = %d, want 1", got)
	}
}

func TestHandleUserFiles_SequentialRefreshRunsAgain(t *testing.T) {
	tools := t.TempDir()
	rpt := filepath.Join(t.TempDir(), "report.html")
	marker := filepath.Join(t.TempDir(), "runs")
	if err := os.WriteFile(rpt, []byte("<html>stale</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(tools, "view_agent_files.sh")
	body := "#!/bin/sh\necho run >> " + marker + "\necho '<html>fresh</html>' > \"$1\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	s := &Server{userFilesReportPath: rpt, userFilesToolsDir: tools}
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/user_files?refresh=1", nil)
		rec := httptest.NewRecorder()
		s.handleUserFiles(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("refresh %d: code=%d body=%q", i+1, rec.Code, rec.Body.String())
		}
	}

	runs, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(runs), "run\n"); got != 2 {
		t.Fatalf("generation runs = %d, want 2", got)
	}
}

func TestHandleUserFiles_ServesCompleteReportDuringRefresh(t *testing.T) {
	tools := t.TempDir()
	rpt := filepath.Join(t.TempDir(), "report.html")
	marker := filepath.Join(t.TempDir(), "writing")
	release := filepath.Join(t.TempDir(), "release")
	if err := os.WriteFile(rpt, []byte("<html>stale</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(tools, "view_agent_files.sh")
	body := "#!/bin/sh\nprintf '<html>partial' > \"$1\"\ntouch \"" + marker + "\"\nwhile [ ! -e \"" + release + "\" ]; do sleep 0.01; done\nprintf '<html>fresh</html>' > \"$1\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	s := &Server{userFilesReportPath: rpt, userFilesToolsDir: tools}
	refreshCode := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/user_files?refresh=1", nil)
		rec := httptest.NewRecorder()
		s.handleUserFiles(rec, req)
		refreshCode <- rec.Code
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("generator did not begin writing temporary report")
		}
		time.Sleep(10 * time.Millisecond)
	}

	req := httptest.NewRequest(http.MethodGet, "/user_files", nil)
	rec := httptest.NewRecorder()
	s.handleUserFiles(rec, req)
	if err := os.WriteFile(release, []byte("continue"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "stale") || strings.Contains(rec.Body.String(), "partial") {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	select {
	case code := <-refreshCode:
		if code != http.StatusSeeOther {
			t.Fatalf("refresh code=%d, want %d", code, http.StatusSeeOther)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("refresh did not finish after generator release")
	}

	req = httptest.NewRequest(http.MethodGet, "/user_files", nil)
	rec = httptest.NewRecorder()
	s.handleUserFiles(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "fresh") {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestHandleUserFiles_ConcurrentRefreshSharesGenerationFailure(t *testing.T) {
	tools := t.TempDir()
	rpt := filepath.Join(t.TempDir(), "report.html")
	marker := filepath.Join(t.TempDir(), "runs")
	if err := os.WriteFile(rpt, []byte("<html>stale</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(tools, "view_agent_files.sh")
	body := "#!/bin/sh\necho run >> " + marker + "\nsleep 0.2\necho generation-failed\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	s := &Server{userFilesReportPath: rpt, userFilesToolsDir: tools}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodGet, "/user_files?refresh=1", nil)
			rec := httptest.NewRecorder()
			s.handleUserFiles(rec, req)
			if rec.Code != http.StatusInternalServerError {
				t.Errorf("code=%d body=%q", rec.Code, rec.Body.String())
			}
		}()
	}
	close(start)
	wg.Wait()

	runs, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(runs), "run\n"); got != 1 {
		t.Fatalf("generation runs = %d, want 1", got)
	}
}

func TestHandleUserFilesRegenerate_RunsAsync(t *testing.T) {
	tools := t.TempDir()
	rpt := filepath.Join(t.TempDir(), "report.html")
	marker := filepath.Join(t.TempDir(), "marker")
	script := filepath.Join(tools, "view_agent_files.sh")
	// 200ms sleep is long enough that a synchronous handler would not yet
	// have created the marker by the time we check immediately afterwards.
	body := "#!/bin/sh\nsleep 0.2\ntouch " + marker + "\necho '<html>regen</html>' > \"$1\"\n"
	os.WriteFile(script, []byte(body), 0o755)
	s := &Server{userFilesReportPath: rpt, userFilesToolsDir: tools}
	req := httptest.NewRequest(http.MethodPost, "/user_files/regenerate", nil)
	rec := httptest.NewRecorder()
	s.handleUserFilesRegenerate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	// Handler must return before the script finishes; marker must not exist yet.
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("handler waited for script completion; expected async fire-and-forget")
	}
	for i := 0; i < 30; i++ {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("script did not run within timeout")
}

func TestHandleUserFilesPreview_ServesImage(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "screens", "frame.png")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte("\x89PNG\r\n\x1a\npreview")
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{userFilesMemoryDir: root}
	req := httptest.NewRequest(http.MethodGet, "/user_files/preview?type=memory&path=screens/frame.png", nil)
	rec := httptest.NewRecorder()
	s.handleUserFilesPreview(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "image/png") {
		t.Fatalf("Content-Type = %q, want image/png", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") {
		t.Fatalf("Content-Security-Policy = %q, want restrictive policy", got)
	}
	if got := rec.Body.Bytes(); string(got) != string(want) {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestHandleUserFilesPreview_RejectsNonImage(t *testing.T) {
	root := t.TempDir()
	for name, data := range map[string][]byte{
		"note.txt": []byte("hello"),
		"icon.svg": []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
	} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, path := range []string{"note.txt", "icon.svg"} {
		s := &Server{userFilesMemoryDir: root}
		req := httptest.NewRequest(http.MethodGet, "/user_files/preview?type=memory&path="+path, nil)
		rec := httptest.NewRecorder()
		s.handleUserFilesPreview(rec, req)

		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("%s: status %d body=%q", path, rec.Code, rec.Body.String())
		}
	}
}

func TestHandleUserFilesPreview_RejectsInvalidRequests(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "screens"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "screens", "frame.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		url  string
	}{
		{name: "path traversal", url: "/user_files/preview?type=memory&path=../frame.png"},
		{name: "absolute path", url: "/user_files/preview?type=memory&path=/tmp/frame.png"},
		{name: "unknown file type", url: "/user_files/preview?type=unknown&path=screens/frame.png"},
		{name: "missing path", url: "/user_files/preview?type=memory&path=screens/missing.png"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{userFilesMemoryDir: root}
			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			rec := httptest.NewRecorder()
			s.handleUserFilesPreview(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d body=%q", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") {
				t.Fatalf("Content-Security-Policy = %q, want restrictive policy", got)
			}
		})
	}
}

func TestHandleUserFilesPreview_RejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.png")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.png")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	s := &Server{userFilesMemoryDir: root}
	req := httptest.NewRequest(http.MethodGet, "/user_files/preview?type=memory&path=link.png", nil)
	rec := httptest.NewRecorder()
	s.handleUserFilesPreview(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d body=%q", rec.Code, rec.Body.String())
	}
}
