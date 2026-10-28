package configweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"aico-agent/internal/agent"
	"aico-agent/internal/backup"
	"aico-agent/internal/logging"
	"aico-agent/internal/wifiproxy"
)

const (
	maxRequestBodySize = 64 * 1024
	maxSystemEnvSize   = 64 * 1024
	maxAgentConfigSize = 1024 * 1024
)

type Server struct {
	options                  Options
	http                     *http.Server
	usbHTTP                  *http.Server
	usbRetryStop             chan struct{}
	usbRetryOnce             sync.Once
	storage                  storageController
	sttTest                  *agent.STTConfigTestAPI
	closeMu                  sync.Once
	storageMu                sync.RWMutex
	configMu                 sync.Mutex
	configServiceMu          sync.Mutex
	configSaveMu             sync.Mutex
	systemEnvMu              sync.Mutex
	configSavePending        bool
	configApplyError         string
	configApplyErrorRevision uint64
	frameApplyPending        bool
	storageApplyPending      bool
	wifiOpMu                 sync.Mutex
	wifiMu                   sync.Mutex
	wifiJob                  *wifiConnectionJob
	wifiRegionMu             sync.Mutex
	wifiRegionVotes          map[string]int
	wifiRegionVotesAt        time.Time
	wifiRegionDriver         wifiRegionDriver
	maintenance              *maintenanceController
	maintenanceSessions      *maintenanceSessionStore
	backupJobs               *backupJobStore
	restoreJobs              *restoreJobStore
	services                 serviceController
	mounts                   backup.MountController
	restoreRecovery          []backup.RecoveryResult

	restartMu               sync.Mutex
	restartCommand          *exec.Cmd
	restartDone             chan error
	restartDeferred         bool
	restartReadinessPending bool
	restartError            error
}

func NewServer(options Options) (*Server, error) {
	if strings.TrimSpace(options.USBInterface) == "" {
		options.USBInterface = "usb0"
	}
	if strings.TrimSpace(options.WiFiProxyConfigPath) == "" {
		options.WiFiProxyConfigPath = wifiproxy.DefaultConfigPath
	}
	if strings.TrimSpace(options.LocalProxyAddress) == "" {
		options.LocalProxyAddress = wifiproxy.DefaultListenAddress
	}
	if strings.TrimSpace(options.LocalProxyEnvironmentPath) == "" {
		options.LocalProxyEnvironmentPath = wifiproxy.DefaultEnvironmentPath
	}
	if strings.TrimSpace(options.WiFiProxyInitScript) == "" {
		options.WiFiProxyInitScript = "/usr/lib/aico/aico-wifi-proxy-control"
	}
	if strings.TrimSpace(options.WiFiBackend) == "" {
		options.WiFiBackend = "legacy"
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	s := &Server{
		options:          options,
		sttTest:          agent.NewSTTConfigTestAPI(options.AgentConfigPath),
		wifiRegionDriver: systemWiFiRegionDriver{},
	}
	s.services = &systemdServiceController{binary: options.SystemctlBinary}
	s.maintenance = newMaintenanceController(options.MaintenanceLockPath)
	s.maintenanceSessions = newMaintenanceSessionStore(options.USBAddress, options.USBSubnet)
	s.maintenanceSessions.busy = s.maintenance.active
	s.backupJobs = newBackupJobStore(s, options.BackupJobStateDir)
	s.restoreJobs = newRestoreJobStore(s, options.BackupJobStateDir)
	s.mounts = backup.SystemMountController{}
	if _, err := os.Stat(options.AgentConfigPath); err == nil {
		if err := s.initializeStorageManager(); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat Agent config: %w", err)
	}
	usbOnly := options.BindAddress == options.USBAddress
	s.http = configHTTPServer(options.Addr(), s, usbOnly)
	if options.BindAddress == "0.0.0.0" {
		s.usbHTTP = configHTTPServer(net.JoinHostPort(options.USBAddress, strconv.Itoa(options.Port)), s, true)
		s.usbRetryStop = make(chan struct{})
	}
	return s, nil
}

func (s *Server) ListenAndServe() error {
	if s.storage == nil {
		if err := s.initializeStorageManager(); err != nil {
			return fmt.Errorf("initialize storage manager: %w", err)
		}
	}
	s.logAgentRecoveryState()
	// A Config Web crash mid-restore leaves staging or a half-committed
	// transaction behind; finish or discard it before serving requests.
	s.recoverRestoreTransactions()
	logging.Infof("config_web", "config_web", "listening on %s", s.options.Addr())
	// A separate SO_BINDTODEVICE socket establishes the USB ingress boundary.
	// The wildcard portal remains available on Wi-Fi, but its connections are
	// never maintenance-authorized, even when addressed to the USB IP.
	errorsCh := make(chan error, 2)
	if s.usbHTTP != nil {
		// The USB gadget can come up after Config Web (a Wants= dependency);
		// keep retrying the interface-bound socket in the background so USB
		// maintenance becomes available without a service restart.
		go s.serveUSBListener(errorsCh)
	}
	device := ""
	if s.options.BindAddress == s.options.USBAddress {
		device = s.options.USBInterface
	}
	listener, err := listenConfigWeb(s.http.Addr, device)
	if err != nil {
		return err
	}
	defer listener.Close()
	go func() { errorsCh <- s.http.Serve(listener) }()
	err = <-errorsCh
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// serveUSBListener binds the SO_BINDTODEVICE maintenance socket, retrying
// with backoff until it succeeds or the server shuts down.  Failures are
// fail-closed: no USB listener means no maintenance authorization.
func (s *Server) serveUSBListener(errorsCh chan<- error) {
	listener, err := retryListen(func() (net.Listener, error) {
		return listenConfigWeb(s.usbHTTP.Addr, s.options.USBInterface)
	}, s.usbRetryStop, usbListenRetrySchedule, func(attempt int, err error) {
		if attempt == 1 {
			logging.Warnf("config_web", "config_web", "USB maintenance listener unavailable (fail closed), retrying: %v", err)
		}
	})
	if err != nil {
		return
	}
	logging.Infof("config_web", "config_web", "USB maintenance listener ready on %s (%s)", s.usbHTTP.Addr, s.options.USBInterface)
	select {
	case <-s.usbRetryStop:
		// Shutdown raced the successful bind; Serve would return immediately.
		_ = listener.Close()
		return
	default:
	}
	errorsCh <- s.usbHTTP.Serve(listener)
}

func (s *Server) stopUSBRetry() {
	if s.usbRetryStop == nil {
		return
	}
	s.usbRetryOnce.Do(func() { close(s.usbRetryStop) })
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.stopUSBRetry()
	if s.backupJobs != nil {
		s.backupJobs.cancelAll()
	}
	if s.restoreJobs != nil {
		s.restoreJobs.cancelAll()
	}
	shutdownErrors := make(chan error, 2)
	go func() { shutdownErrors <- s.http.Shutdown(ctx) }()
	count := 1
	if s.usbHTTP != nil {
		count++
		go func() { shutdownErrors <- s.usbHTTP.Shutdown(ctx) }()
	}
	var err error
	for index := 0; index < count; index++ {
		err = errors.Join(err, <-shutdownErrors)
	}
	s.closeMu.Do(func() {
		if storage := s.currentStorage(); storage != nil {
			storage.Stop()
		}
	})
	return err
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
		s.APIHandler().ServeHTTP(w, r)
		return
	}
	if s.serveStatic(w, r) {
		return
	}
	http.Error(w, "Not Found", http.StatusNotFound)
}

func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) bool {
	relative := ""
	entry := false
	switch {
	case r.URL.Path == "/":
		relative, entry = "index.html", true
	case r.URL.Path == "/llm-logs":
		relative, entry = "llm-logs.html", true
	case strings.HasPrefix(r.URL.Path, "/assets/"):
		decoded, ok := safeAssetPath(strings.TrimPrefix(r.URL.EscapedPath(), "/assets/"))
		if !ok {
			http.Error(w, "Not Found", http.StatusNotFound)
			return true
		}
		relative = filepath.Join("assets", decoded)
	case isSettingsRoute(r.URL.Path):
		// Sub-routes such as /wifi and /wifi/<ssid> belong to the client router,
		// which needs the same document the root serves.
		relative, entry = "index.html", true
	default:
		return false
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return true
	}

	file, info, err := openStaticFileNoSymlinks(s.options.WebRoot, relative)
	if err != nil {
		if entry {
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		} else {
			http.Error(w, "Not Found", http.StatusNotFound)
		}
		return true
	}
	defer file.Close()
	w.Header().Set("Cache-Control", "no-cache")
	contentType := staticContentType(relative)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprint(info.Size()))
	_, _ = io.Copy(w, file)
	return true
}

// settingsRouteSections are the first path segments of the client router's
// routes (src/config_web/web/assets/js/app/routes.js).
var settingsRouteSections = map[string]bool{
	"wifi": true, "basic": true, "conversation": true, "model": true, "voice": true,
	"memory": true, "storage": true, "advanced": true, "firmware": true,
}

// isSettingsRoute reports whether a request path belongs to the client-side
// settings router, so a settings page can be reloaded or deep-linked. It goes
// by the section, not by the look of the last segment: an SSID such as
// "Home_2.4G" contains a dot, and a mistyped path outside the settings must
// still 404 rather than quietly return HTML.
func isSettingsRoute(path string) bool {
	if path == "" || strings.HasSuffix(path, "/") {
		return false
	}
	section := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)[0]
	return settingsRouteSections[section]
}

func staticContentType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "application/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	default:
		return "application/octet-stream"
	}
}

func safeAssetPath(escaped string) (string, bool) {
	if escaped == "" {
		return "", false
	}
	parts := strings.Split(escaped, "/")
	decoded := make([]string, 0, len(parts))
	for _, part := range parts {
		value, err := url.PathUnescape(part)
		if err != nil || value == "" || value == "." || value == ".." ||
			strings.ContainsAny(value, "/\\\x00") {
			return "", false
		}
		decoded = append(decoded, value)
	}
	return filepath.Join(decoded...), true
}

func openStaticFileNoSymlinks(root, relative string) (*os.File, os.FileInfo, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		if err == nil {
			err = fmt.Errorf("static root is not a directory")
		}
		return nil, nil, err
	}
	current := root
	for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, fmt.Errorf("symlink asset rejected")
		}
	}
	file, err := os.Open(current)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		if err == nil {
			err = fmt.Errorf("asset is not a regular file")
		}
		return nil, nil, err
	}
	return file, info, nil
}

func readJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(dst); err != nil {
		status := http.StatusBadRequest
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			status = http.StatusRequestEntityTooLarge
		}
		writeJSONError(w, status, "invalid JSON body")
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": message})
}
