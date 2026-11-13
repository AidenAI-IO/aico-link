package agent

import (
	"bytes"
	"fmt"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const userFilesGenerateTimeout = 30 * time.Second

const userFilesRefreshHref = "/user_files?refresh=1"

const userFilesRefreshControlMarker = `id="user-files-refresh"`

type userFilesReportGeneration struct {
	done chan struct{}
	err  error
}

// runUserFilesScript runs view_agent_files.sh under a fixed timeout, killing
// the process if it exceeds the deadline. Output is collected for diagnostics.
// Used by both ensureUserFilesReport (synchronous) and handleUserFilesRegenerate
// (async) so a hung script can never leak a long-lived child.
func (s *Server) runUserFilesScript(stdoutSink string) error {
	reportDir := filepath.Dir(s.userFilesReportPath)
	tempReport, err := os.CreateTemp(reportDir, ".files_report-*.html")
	if err != nil {
		return fmt.Errorf("create temporary user_files report: %w", err)
	}
	tempReportPath := tempReport.Name()
	if err := tempReport.Close(); err != nil {
		os.Remove(tempReportPath)
		return fmt.Errorf("close temporary user_files report: %w", err)
	}
	defer os.Remove(tempReportPath)

	script := "cd " + shellQuote(s.userFilesToolsDir) + " && ./view_agent_files.sh " + shellQuote(tempReportPath) + " " + shellQuote(reportDir)
	if stdoutSink != "" {
		script += " > " + shellQuote(stdoutSink) + " 2>&1"
	} else {
		script += " 2>&1"
	}
	cmd := exec.Command("sh", "-c", script)
	timer := time.AfterFunc(userFilesGenerateTimeout, func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	})
	defer timer.Stop()
	if stdoutSink != "" {
		// Caller redirected stdout to a file; we only need exit status.
		err = cmd.Run()
	} else {
		var out []byte
		out, err = cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("user_files generation failed: %s", string(out))
		}
	}
	if err != nil {
		return err
	}
	info, err := os.Stat(tempReportPath)
	if err != nil || info.Size() == 0 {
		return fmt.Errorf("script ran but report not produced")
	}
	if err := os.Chmod(tempReportPath, 0o644); err != nil {
		return fmt.Errorf("set user_files report permissions: %w", err)
	}
	if err := os.Rename(tempReportPath, s.userFilesReportPath); err != nil {
		return fmt.Errorf("publish user_files report: %w", err)
	}
	return nil
}

func (s *Server) ensureUserFilesReport(forceRefresh bool, stdoutSink string) error {
	if _, err := os.Stat(s.userFilesReportPath); err == nil && !forceRefresh {
		return nil
	}

	s.userFilesGenerateMu.Lock()
	if generation := s.userFilesGeneration; generation != nil {
		s.userFilesGenerateMu.Unlock()
		<-generation.done
		return generation.err
	}

	_, reportErr := os.Stat(s.userFilesReportPath)
	if reportErr == nil && !forceRefresh {
		s.userFilesGenerateMu.Unlock()
		return nil
	}
	generation := &userFilesReportGeneration{done: make(chan struct{})}
	s.userFilesGeneration = generation
	s.userFilesGenerateMu.Unlock()

	err := s.runUserFilesScript(stdoutSink)
	if err == nil {
		if _, statErr := os.Stat(s.userFilesReportPath); statErr != nil {
			err = fmt.Errorf("script ran but report not produced")
		}
	}

	s.userFilesGenerateMu.Lock()
	generation.err = err
	s.userFilesGeneration = nil
	close(generation.done)
	s.userFilesGenerateMu.Unlock()
	return err
}

func (s *Server) handleUserFiles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	forceRefresh := r.URL.Query().Get("refresh") == "1"
	if err := s.ensureUserFilesReport(forceRefresh, ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if forceRefresh {
		http.Redirect(w, r, "/user_files", http.StatusSeeOther)
		return
	}
	data, err := os.ReadFile(s.userFilesReportPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data = userFilesReportWithRefreshControl(data)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func userFilesReportWithRefreshControl(data []byte) []byte {
	if bytes.Contains(data, []byte(userFilesRefreshControlMarker)) {
		return data
	}
	control := fmt.Appendf(nil, `<a id="user-files-refresh" href="%s" style="position:fixed;top:16px;right:16px;z-index:100;padding:8px 12px;border:1px solid #ddd;border-radius:8px;background:#fff;color:#222;text-decoration:none;font:600 12px system-ui,sans-serif">Refresh data</a>`, userFilesRefreshHref)
	if index := bytes.LastIndex(data, []byte("</body>")); index >= 0 {
		result := make([]byte, 0, len(data)+len(control))
		result = append(result, data[:index]...)
		result = append(result, control...)
		result = append(result, data[index:]...)
		return result
	}
	return append(append([]byte(nil), data...), control...)
}

func (s *Server) handleUserFilesPreview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'none'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; sandbox")
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	root := s.userFilesRoot(r.URL.Query().Get("type"))
	if root == "" {
		http.Error(w, "invalid file type", http.StatusBadRequest)
		return
	}

	filePath, err := safeUserFilesPath(root, r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	info, err := os.Stat(filePath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if info.IsDir() {
		http.Error(w, "path is a directory", http.StatusBadRequest)
		return
	}

	contentType := userFilesImageContentType(filePath)
	if contentType == "" {
		http.Error(w, "unsupported preview type", http.StatusUnsupportedMediaType)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, filePath)
}

func (s *Server) handleUserFilesRegenerate(w http.ResponseWriter, r *http.Request) {
	go func() {
		// Reuse the same timeout budget so a hung script cannot pile up
		// long-lived background generators on repeated calls.
		_ = s.ensureUserFilesReport(true, "/tmp/user_files_regenerate.log")
	}()
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true,"regenerating":true}`))
}

func (s *Server) userFilesRoot(kind string) string {
	switch strings.TrimSpace(kind) {
	case "memory":
		if s.userFilesMemoryDir != "" {
			return s.userFilesMemoryDir
		}
		return userFilesRootFromReportPath(s.userFilesReportPath, "memory")
	case "skills":
		if s.userFilesSkillsDir != "" {
			return s.userFilesSkillsDir
		}
		return userFilesRootFromReportPath(s.userFilesReportPath, "skills")
	case "skill-state":
		if s.userFilesSkillStateDir != "" {
			return s.userFilesSkillStateDir
		}
		return userFilesRootFromReportPath(s.userFilesReportPath, "skill-state")
	default:
		return ""
	}
}

func userFilesRootFromReportPath(reportPath, name string) string {
	base := strings.TrimSpace(filepath.Dir(reportPath))
	if base == "" || base == "." {
		return ""
	}
	return filepath.Join(base, name)
}

func safeUserFilesPath(root, rel string) (string, error) {
	root = strings.TrimSpace(root)
	rel = strings.TrimSpace(rel)
	if root == "" || rel == "" {
		return "", fmt.Errorf("empty path")
	}

	rel = filepath.FromSlash(rel)
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute path")
	}
	cleanRel := filepath.Clean(rel)
	if cleanRel == "." || cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes root")
	}

	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	resolvedPath, err := filepath.EvalSymlinks(filepath.Join(resolvedRoot, cleanRel))
	if err != nil {
		return "", err
	}

	relToRoot, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil {
		return "", err
	}
	if relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) || filepath.IsAbs(relToRoot) {
		return "", fmt.Errorf("path escapes root")
	}
	return resolvedPath, nil
}

func userFilesImageContentType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".svg" {
		return ""
	}
	if contentType := mime.TypeByExtension(ext); strings.HasPrefix(strings.ToLower(contentType), "image/") {
		return contentType
	}
	switch ext {
	case ".avif":
		return "image/avif"
	case ".bmp":
		return "image/bmp"
	case ".gif":
		return "image/gif"
	case ".heic":
		return "image/heic"
	case ".heif":
		return "image/heif"
	case ".ico":
		return "image/x-icon"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".tif", ".tiff":
		return "image/tiff"
	case ".webp":
		return "image/webp"
	default:
		return ""
	}
}
