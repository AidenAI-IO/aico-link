package agent

import (
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const maxSharedFileBytes int64 = 100 << 20

// The device mounts persistent userdata here; tests and desktop runs may override it.
func sharedFilesDir() string {
	if dir := os.Getenv("AICO_SHARE_DIR"); filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	return "/share"
}

type sharedFile struct {
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	MIMEType    string    `json:"mime_type,omitempty"`
	DownloadURL string    `json:"download_url,omitempty"`
	Modified    time.Time `json:"modified"`
}

func validSharedName(name string) bool {
	return name != "" && !strings.HasPrefix(name, ".") && len(name) <= 240 &&
		!strings.ContainsAny(name, "/\\") && !strings.ContainsFunc(name, unicode.IsControl)
}

func validSharedRelative(name string) bool {
	if name == "" {
		return true
	}
	for _, part := range strings.Split(name, "/") {
		if !validSharedName(part) {
			return false
		}
	}
	return true
}

func sharedRelativePath(filename string) (string, error) {
	prefix := sharedFilesDir() + string(filepath.Separator)
	if !strings.HasPrefix(filename, prefix) {
		return "", fmt.Errorf("file must be inside %s", sharedFilesDir())
	}
	rel := strings.TrimPrefix(filename, prefix)
	if rel == "" || !validSharedRelative(rel) {
		return "", fmt.Errorf("invalid shared file path")
	}
	return rel, nil
}

func sharedDownloadURL(filename string) string {
	rel, err := sharedRelativePath(filename)
	if err != nil {
		return ""
	}
	return sharedURLPath(rel)
}

func sharedURLPath(rel string) string {
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return "/share/" + strings.Join(parts, "/")
}

func sharedCollisionName(name string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	// Keep the result inside both the upload and filesystem filename limits.
	if len(ext) > 32 {
		ext = ""
		stem = name
	}
	maxStem := 240 - 9 - len(ext)
	if len(stem) > maxStem {
		stem = stem[:maxStem]
		for !utf8.ValidString(stem) {
			stem = stem[:len(stem)-1]
		}
	}
	return stem + "-" + uuid.NewString()[:8] + ext
}

func sharedFileInfo(rel string, info os.FileInfo) sharedFile {
	item := sharedFile{Kind: "file", Name: info.Name(), Path: filepath.Join(sharedFilesDir(), rel), Size: info.Size(), Modified: info.ModTime()}
	if info.IsDir() {
		item.Kind = "directory"
		return item
	}
	item.MIMEType = mime.TypeByExtension(filepath.Ext(info.Name()))
	if item.MIMEType == "" {
		item.MIMEType = "application/octet-stream"
	}
	item.DownloadURL = sharedDownloadURL(item.Path)
	return item
}

// os.Root confines resolution even if another process swaps a path for a symlink.
// Reject symlinks as well so only explicitly shared regular files are exposed.
func openSharedFile(root *os.Root, rel string) (*os.File, error) {
	if rel == "" {
		return root.Open(".")
	}
	if !validSharedRelative(rel) {
		return nil, os.ErrNotExist
	}
	parts := strings.Split(rel, "/")
	for i := range parts {
		info, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return nil, os.ErrNotExist
		}
	}
	return root.Open(rel)
}

func sharedFileMetadata(filename string) (sharedFile, error) {
	rel, err := sharedRelativePath(filename)
	if err != nil {
		return sharedFile{}, err
	}
	root, err := os.OpenRoot(sharedFilesDir())
	if err != nil {
		return sharedFile{}, err
	}
	defer root.Close()
	file, err := openSharedFile(root, rel)
	if err != nil {
		return sharedFile{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return sharedFile{}, err
	}
	if !info.Mode().IsRegular() {
		return sharedFile{}, fmt.Errorf("attachment must be a regular file")
	}
	return sharedFileInfo(rel, info), nil
}

func (s *Server) sharedFilesHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/files", s.handleSharedFiles)
	mux.HandleFunc("/share/", s.handleSharedDownload)
	return mux
}

func sharedFileHeaders(w http.ResponseWriter, r *http.Request) bool {
	// Web transfers use the current origin; native clients do not send Origin.
	// Reject cross-origin browser writes: multipart POST can skip preflight,
	// and DELETE gets the same check rather than relying on CORS alone.
	if r.Method == http.MethodPost || r.Method == http.MethodDelete {
		origin := r.Header.Get("Origin")
		parsed, err := url.Parse(origin)
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" || (origin != "" && (err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host != r.Host)) {
			http.Error(w, "Cross-origin writes are not allowed", http.StatusForbidden)
			return true
		}
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

func (s *Server) handleSharedFiles(w http.ResponseWriter, r *http.Request) {
	if sharedFileHeaders(w, r) {
		return
	}
	switch r.Method {
	case http.MethodPost:
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Minute))
		uploadSharedFile(w, r)
	case http.MethodGet:
		entries, err := listSharedFiles(r.URL.Query().Get("path"))
		if err != nil {
			http.Error(w, "Shared directory unavailable", http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"files": entries, "max_upload_bytes": maxSharedFileBytes})
	case http.MethodDelete:
		deleteSharedFile(w, r, r.URL.Query().Get("path"))
	default:
		w.Header().Set("Allow", "GET, POST, DELETE, OPTIONS")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func uploadSharedFile(w http.ResponseWriter, r *http.Request) {
	uploadSharedFileWithLimit(w, r, maxSharedFileBytes)
}

func uploadSharedFileWithLimit(w http.ResponseWriter, r *http.Request, limit int64) {
	r.Body = http.MaxBytesReader(w, r.Body, limit+(1<<20))
	reader, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "Expected multipart/form-data with one file field", http.StatusBadRequest)
		return
	}
	part, err := reader.NextPart()
	if err != nil {
		http.Error(w, "Missing file", http.StatusBadRequest)
		return
	}
	// FileName() strips directory components, so validate the original header too.
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	name := params["filename"]
	if err != nil || part.FormName() != "file" || !validSharedName(name) {
		http.Error(w, "Invalid file name or field (expected file)", http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(sharedFilesDir(), 0755); err != nil {
		http.Error(w, "Shared storage unavailable", http.StatusServiceUnavailable)
		return
	}
	root, err := os.OpenRoot(sharedFilesDir())
	if err != nil {
		http.Error(w, "Shared storage unavailable", http.StatusServiceUnavailable)
		return
	}
	defer root.Close()
	temp := ".upload-" + uuid.NewString()
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		http.Error(w, "Cannot create upload", http.StatusInsufficientStorage)
		return
	}
	defer root.Remove(temp)
	defer file.Close()
	size, err := io.Copy(file, io.LimitReader(part, limit+1))
	var limitErr *http.MaxBytesError
	if size > limit || errors.As(err, &limitErr) {
		http.Error(w, "File exceeds 100 MiB upload limit", http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		http.Error(w, "Upload failed", http.StatusBadRequest)
		return
	}
	if _, err := reader.NextPart(); err != io.EOF {
		http.Error(w, "Upload exactly one file per request", http.StatusBadRequest)
		return
	}
	if err := file.Sync(); err != nil {
		http.Error(w, "Cannot persist upload", http.StatusInsufficientStorage)
		return
	}
	if err := file.Close(); err != nil {
		http.Error(w, "Cannot persist upload", http.StatusInsufficientStorage)
		return
	}
	// Link publishes the completed file atomically and never replaces an existing file.
	published := name
	for attempts := 0; ; attempts++ {
		err = root.Link(temp, published)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) || attempts >= 8 {
			http.Error(w, "Cannot publish upload", http.StatusInsufficientStorage)
			return
		}
		published = sharedCollisionName(name)
	}
	info, err := root.Stat(published)
	if err != nil {
		http.Error(w, "Cannot inspect upload", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, sharedFileInfo(published, info))
}

// deleteSharedFile removes one regular file or one empty folder below the
// shared root. Like downloads, it never follows or removes symbolic links,
// hidden entries, or special files; those answer 404 as if absent. A folder
// that still has entries answers 409, so a single request never removes a
// whole tree.
func deleteSharedFile(w http.ResponseWriter, r *http.Request, rel string) {
	rel = strings.Trim(rel, "/")
	if rel == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	if !validSharedRelative(rel) {
		http.NotFound(w, r)
		return
	}
	root, err := os.OpenRoot(sharedFilesDir())
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer root.Close()
	if err := removeSharedEntry(root, rel); err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			http.NotFound(w, r)
		case errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST):
			http.Error(w, "Folder is not empty", http.StatusConflict)
		default:
			http.Error(w, "Cannot delete file", http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sharedRemoveHook runs between validating the parent folder and removing the
// entry; tests use it to swap folders in that window.
var sharedRemoveHook func()

// removeSharedEntry removes rel without following symbolic links. It opens each
// parent folder in turn and checks that the open handle is the folder Lstat saw,
// then removes the last name inside that handle. Swapping a parent for a link
// afterwards cannot redirect the removal to another folder.
func removeSharedEntry(root *os.Root, rel string) error {
	parts := strings.Split(rel, "/")
	dir := root
	for _, name := range parts[:len(parts)-1] {
		info, err := dir.Lstat(name)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return os.ErrNotExist
		}
		next, err := dir.OpenRoot(name)
		if err != nil {
			return err
		}
		if dir != root {
			dir.Close()
		}
		dir = next
		opened, err := dir.Stat(".")
		if err != nil {
			dir.Close()
			return err
		}
		if !os.SameFile(info, opened) {
			dir.Close()
			return os.ErrNotExist
		}
	}
	if dir != root {
		defer dir.Close()
	}
	name := parts[len(parts)-1]
	info, err := dir.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return os.ErrNotExist
	}
	if sharedRemoveHook != nil {
		sharedRemoveHook()
	}
	return dir.Remove(name)
}

func listSharedFiles(rel string) ([]sharedFile, error) {
	if !validSharedRelative(rel) {
		return nil, os.ErrNotExist
	}
	root, err := os.OpenRoot(sharedFilesDir())
	if errors.Is(err, os.ErrNotExist) && rel == "" {
		return []sharedFile{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dir, err := openSharedFile(root, rel)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	result := make([]sharedFile, 0, len(entries))
	for _, entry := range entries {
		if !validSharedName(entry.Name()) || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
			continue
		}
		result = append(result, sharedFileInfo(path.Join(rel, entry.Name()), info))
	}
	return result, nil
}

var sharePage = template.Must(template.New("share").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Shared files</title><style>body{font:16px system-ui;max-width:900px;margin:32px auto;padding:0 20px}li{margin:18px 0;overflow-wrap:anywhere}a{color:#315bdd}small{color:#666}</style><h1>Shared files</h1><p>Uploaded files and Agent outputs. Select a file to download.</p><a href="/share/">Root</a><ul>{{range .}}<li>{{if eq .Kind "directory"}}<a href="{{.URL}}">{{.Name}}/</a>{{else}}<a href="{{.URL}}" download>{{.Name}}</a> <small>{{.Size}} bytes</small>{{end}}</li>{{else}}<li>No files yet.</li>{{end}}</ul></html>`))

func (s *Server) handleSharedDownload(w http.ResponseWriter, r *http.Request) {
	if sharedFileHeaders(w, r) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/share/"), "/")
	if !validSharedRelative(rel) {
		http.NotFound(w, r)
		return
	}
	root, err := os.OpenRoot(sharedFilesDir())
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer root.Close()
	file, err := openSharedFile(root, rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if info.IsDir() {
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, (&url.URL{Path: r.URL.Path + "/"}).String(), http.StatusMovedPermanently)
			return
		}
		entries, err := listSharedFiles(rel)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		type row struct {
			Name, Kind, URL string
			Size            int64
		}
		rows := make([]row, 0, len(entries))
		for _, entry := range entries {
			link := sharedURLPath(path.Join(rel, entry.Name))
			if entry.Kind == "directory" {
				link += "/"
			}
			rows = append(rows, row{entry.Name, entry.Kind, link, entry.Size})
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		if r.Method != http.MethodHead {
			_ = sharePage.Execute(w, rows)
		}
		return
	}
	if !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": info.Name()}))
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}
