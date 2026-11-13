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
	Enabled         *bool  `json:"enabled"`
	Password        string `json:"password"`
	CurrentPassword string `json:"current_password"`
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

// securityManager reports whether r may change the security settings and
// manage MCP access tokens: a USB client, or, while a device password is set,
// a client the guard admitted with a session.
//
// While no device password is set, being admitted proves nothing about
// ownership: with LAN access on, every LAN client is admitted. These settings
// decide who owns the board (the first password becomes the aico login, and
// MCP tokens outlive any later password), so until a password exists only the
// cable holder may change them.
func (s *Server) securityManager(r *http.Request) bool {
	usb := s.lanAccessRequestFromUSB(r) || lanaccess.IsUSB(r)
	if s.lanAccess.PasswordSet() {
		return usb || lanaccess.IsAdmitted(r)
	}
	return usb
}

// handleLANAccessStatus reports the switch and whether a password exists. It
// never returns the hash.
func (s *Server) handleLANAccessStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.lanAccessStatus(r))
}

// handleLANAccessUpdate turns LAN access on or off and sets the device
// password; the two are independent. Any admitted client may change them.
// See passwordOwner for when the current password is needed.
func (s *Server) handleLANAccessUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.securityManager(r) {
		writeJSONError(w, http.StatusForbidden, "sign in to change LAN access")
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
	if request.Password != "" && !s.passwordOwner(w, r, request.CurrentPassword) {
		return
	}
	if request.Password != "" {
		if err := s.lanAccess.SetPassword(request.Password); err != nil {
			logging.Errorf("config_web", "lan_access", "store device password: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "failed to store the device password")
			return
		}
		logging.Infof("config_web", "lan_access", "device password updated")
	}
	if request.Enabled != nil {
		if err := s.lanAccess.SetEnabled(*request.Enabled); err != nil {
			logging.Errorf("config_web", "lan_access", "set LAN access: %v", err)
			if syncErr := s.syncLANAccess(r.Context()); syncErr != nil {
				logging.Warnf("config_web", "lan_access", "apply LAN access after a failed change: %v", syncErr)
			}
			writeJSONError(w, http.StatusInternalServerError, "failed to change LAN access")
			return
		}
		logging.Infof("config_web", "lan_access", "LAN access turned %s", map[bool]string{true: "on", false: "off"}[*request.Enabled])
	}
	s.writeLANAccessResult(w, r)
}

// handleClearPassword removes the device password, as if none had ever been
// set: every session is revoked and the aico login is locked again. The LAN
// access switch and MCP access tokens are kept. It needs the current
// password. A forgotten one is removed on the board itself (delete the
// password file and run aico-lan-access-sync), for example by the Agent.
func (s *Server) handleClearPassword(w http.ResponseWriter, r *http.Request) {
	if !s.securityManager(r) {
		writeJSONError(w, http.StatusForbidden, "sign in to clear the device password")
		return
	}
	if !sameOrigin(r) {
		writeJSONError(w, http.StatusForbidden, "cross-origin request rejected")
		return
	}
	var request struct {
		CurrentPassword string `json:"current_password"`
	}
	if !readJSONBody(w, r, &request) {
		return
	}
	s.lanAccessMu.Lock()
	defer s.lanAccessMu.Unlock()
	if !s.passwordOwner(w, r, request.CurrentPassword) {
		return
	}
	if err := s.lanAccess.ClearPassword(); err != nil {
		logging.Errorf("config_web", "lan_access", "clear device password: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to clear the device password")
		return
	}
	logging.Infof("config_web", "lan_access", "device password cleared")
	s.writeLANAccessResult(w, r)
}

// passwordOwner reports whether the current password allows replacing or
// clearing the device password, and writes the refusal when it does not.
// Every client needs it, the app included, so a stolen session cannot lock
// the owner out or remove the password; wrong guesses share sign-in's
// per-client lockout. With no password set there is nothing to protect.
func (s *Server) passwordOwner(w http.ResponseWriter, r *http.Request, current string) bool {
	ok, err := s.lanAccess.VerifyCurrentPassword(r, current)
	var limited lanaccess.ErrTooManyAttempts
	switch {
	case errors.As(err, &limited):
		seconds := max(int(limited.RetryAfter.Round(time.Second)/time.Second), 1)
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": "too_many_attempts", "retry_after": seconds})
		return false
	case !ok:
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "current_password_invalid"})
		return false
	}
	return true
}

// writeLANAccessResult applies the stored state to SSH and the login account
// and reports it.
func (s *Server) writeLANAccessResult(w http.ResponseWriter, r *http.Request) {
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
// only through the Agent's /webtty/ on 8080. Both are behind the guard: while
// a device password is set, a USB browser without a session is sent to
// /login, which pairs it silently and comes straight back.
func newUSBTerminalServer(options Options, state *lanaccess.State) (*http.Server, error) {
	target, err := url.Parse(strings.TrimSpace(options.TerminalTarget))
	if err != nil || target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("invalid terminal target %q", options.TerminalTarget)
	}
	var handler http.Handler = agent.NewTerminalProxy(target)
	if options.LANGuard {
		handler = lanaccess.Guard(state, handler)
	} else {
		handler = crossSiteGuard(handler)
	}
	return &http.Server{
		Addr:              net.JoinHostPort(options.USBAddress, strconv.Itoa(options.USBTerminalPort)),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		// The listener is bound to the USB interface, so every connection
		// is a USB one.
		ConnContext: func(ctx context.Context, _ net.Conn) context.Context { return lanaccess.WithUSB(ctx, true) },
	}, nil
}

// serveUSBTerminal binds the USB terminal socket, retrying until the USB
// interface appears or the server shuts down, then serves the ttyd proxy.
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

// crossSiteGuard refuses browser requests made on behalf of another site. It
// covers the USB terminal on a board without the guard, which serves it
// without credentials, so a page open on the connected phone or computer
// must not be able to drive it.
func crossSiteGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reason := lanaccess.CrossSiteReason(r); reason != "" {
			writeJSONError(w, http.StatusForbidden, "cross-site request rejected: "+reason)
			return
		}
		next.ServeHTTP(w, r)
	})
}
