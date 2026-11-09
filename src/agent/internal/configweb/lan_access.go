package configweb

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"aico-agent/internal/agent"
	"aico-agent/internal/lanaccess"
	"aico-agent/internal/logging"
)

type lanAccessRequest struct {
	Enabled  *bool  `json:"enabled"`
	Password string `json:"password"`
}

func (s *Server) lanAccessStatus(r *http.Request) map[string]any {
	return map[string]any{
		"ok":           true,
		"enabled":      s.lanAccess.Enabled(),
		"password_set": s.lanAccess.PasswordSet(),
		"via_usb":      s.lanAccessRequestFromUSB(r),
		"min_length":   lanaccess.MinPasswordLength,
	}
}

// handleLANAccessStatus reports the switch and whether a password exists. It
// never returns the hash.
func (s *Server) handleLANAccessStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.lanAccessStatus(r))
}

// handleLANAccessUpdate turns LAN access on or off and sets the device
// password. It is accepted only over the USB link: whoever holds the cable
// owns the board, so no current password is asked for, and a forgotten
// password is recovered by setting a new one over USB.
func (s *Server) handleLANAccessUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.lanAccessRequestFromUSB(r) {
		writeJSONError(w, http.StatusForbidden, "LAN access can only be changed over the USB connection")
		return
	}
	if !sameOrigin(r) {
		writeJSONError(w, http.StatusForbidden, "cross-origin request rejected")
		return
	}
	var request lanAccessRequest
	if !readJSONBody(w, r, &request) {
		return
	}
	if request.Enabled == nil && request.Password == "" {
		writeJSONError(w, http.StatusBadRequest, "nothing to change")
		return
	}
	if request.Password != "" {
		if err := lanaccess.ValidatePassword(request.Password); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	s.lanAccessMu.Lock()
	defer s.lanAccessMu.Unlock()
	if request.Enabled != nil && *request.Enabled && request.Password == "" && !s.lanAccess.PasswordSet() {
		writeJSONError(w, http.StatusBadRequest, lanaccess.ErrPasswordRequired.Error())
		return
	}
	if request.Password != "" {
		if err := s.lanAccess.SetPassword(request.Password); err != nil {
			logging.Errorf("config_web", "lan_access", "store device password: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "failed to store the device password")
			return
		}
		logging.Infof("config_web", "lan_access", "device password updated over USB")
	}
	if request.Enabled != nil {
		if err := s.lanAccess.SetEnabled(*request.Enabled); err != nil {
			logging.Errorf("config_web", "lan_access", "set LAN access: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "failed to change LAN access")
			return
		}
		logging.Infof("config_web", "lan_access", "LAN access turned %s over USB", map[bool]string{true: "on", false: "off"}[*request.Enabled])
	}
	systemSynced := true
	if err := s.syncLANAccess(r.Context()); err != nil {
		// The state is stored and the HTTP services already follow it. The
		// helper keeps SSH restricted when it cannot apply the password; the
		// boot unit retries on the next start.
		systemSynced = false
		logging.Warnf("config_web", "lan_access", "apply LAN access to SSH and the login account: %v", err)
	}
	status := s.lanAccessStatus(r)
	status["system_synced"] = systemSynced
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) lanAccessRequestFromUSB(r *http.Request) bool {
	return s.maintenanceSessions != nil && isUSBRequest(r, s.maintenanceSessions.address, s.maintenanceSessions.network)
}

// syncLANAccess applies the switch to sshd and the stored hash to the aico
// login account through the same helper the boot unit runs. The helper is
// detached from the caller's cancellation: once the state is stored, a client
// that disconnects must not stop it half-way and leave SSH out of step with
// the switch. Only the timeout bounds it.
func (s *Server) syncLANAccess(ctx context.Context) error {
	helper := strings.TrimSpace(s.options.LANAccessSync)
	if helper == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, helper).CombinedOutput()
	if err != nil {
		if message := strings.TrimSpace(string(output)); message != "" {
			return fmt.Errorf("%w: %s", err, message)
		}
		return err
	}
	return nil
}

// newUSBTerminalServer fronts ttyd, which listens on loopback only, with a
// listener bound to the USB interface. The app keeps opening
// http://192.168.42.1:3000/webtty/ over USB; LAN clients reach the terminal
// only through the Agent's /webtty/ on 8080, which the LAN guard covers.
func newUSBTerminalServer(options Options) (*http.Server, error) {
	target, err := url.Parse(strings.TrimSpace(options.TerminalTarget))
	if err != nil || target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("invalid terminal target %q", options.TerminalTarget)
	}
	return &http.Server{
		Addr:              net.JoinHostPort(options.USBAddress, strconv.Itoa(options.USBTerminalPort)),
		Handler:           agent.NewTerminalProxy(target),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}, nil
}

func (s *Server) serveUSBTerminal() {
	listener, err := lanaccess.RetryListen(func() (net.Listener, error) {
		return lanaccess.ListenOnDevice("tcp4", s.usbTerminal.Addr, s.options.USBInterface)
	}, s.usbRetryStop, func(attempt int, err error) {
		if attempt == 1 {
			logging.Warnf("config_web", "terminal", "USB terminal listener unavailable, retrying: %v", err)
		}
	})
	if err != nil {
		return
	}
	select {
	case <-s.usbRetryStop:
		_ = listener.Close()
		return
	default:
	}
	logging.Infof("config_web", "terminal", "USB terminal listener ready on %s (%s)", s.usbTerminal.Addr, s.options.USBInterface)
	if err := s.usbTerminal.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logging.Warnf("config_web", "terminal", "USB terminal listener stopped: %v", err)
	}
}
