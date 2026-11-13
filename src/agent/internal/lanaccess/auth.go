package lanaccess

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// While a device password is set, clients need a session token. A LAN client
// signs in with the password; a USB client pairs without it, since only the
// owner holds the cable (the app does this silently and then sends the token
// as a bearer over USB and Wi-Fi alike). Both get the same kind of token.
// Tokens are stateless: an HMAC over the expiry and issue time, keyed by a
// secret on userdata, so the Agent and Config Web verify the same token
// without sharing memory. Setting, changing, or clearing the password
// replaces the secret, which signs every client out.
const (
	DefaultSecretPath = "/userdata/system/lan-session-secret"

	SessionCookie = "aico_session"
	// SessionTTL is how long a token stays valid without use. A browser that
	// keeps using the board gets a fresh cookie once a day (see renewAfter),
	// so it stays signed in.
	SessionTTL = 30 * 24 * time.Hour
	renewAfter = 24 * time.Hour

	LoginPath  = "/login"
	LoginAPI   = "/api/auth/login"
	PairAPI    = "/api/auth/pair"
	LogoutAPI  = "/api/auth/logout"
	SessionAPI = "/api/auth/session"

	secretBytes   = 32
	tokenVersion  = "v1"
	maxLoginBody  = 4 << 10
	maxNextLength = 2048
)

var errInvalidToken = errors.New("invalid session token")

// secret reads the signing key. It returns an error when none exists yet.
func (s *State) secret() ([]byte, error) {
	data, err := os.ReadFile(s.SecretPath)
	if err != nil {
		return nil, err
	}
	key, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(key) < secretBytes {
		return nil, errInvalidToken
	}
	return key, nil
}

// ensureSecret returns the signing key, creating it on first use.
func (s *State) ensureSecret() ([]byte, error) {
	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	if key, err := s.secret(); err == nil {
		return key, nil
	}
	return s.writeNewSecret()
}

// RotateSecret replaces the signing key, invalidating every issued token.
func (s *State) RotateSecret() error {
	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	_, err := s.writeNewSecret()
	return err
}

// revokeSessions invalidates every issued token. It replaces the signing key,
// and if that fails it deletes the key, which also invalidates every token
// (the next sign-in creates a new one). It fails only when the old key is
// still on disk.
func (s *State) revokeSessions() error {
	err := s.RotateSecret()
	if err == nil {
		return nil
	}
	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	_ = os.Remove(s.SecretPath)
	if _, statErr := os.Lstat(s.SecretPath); statErr == nil {
		return fmt.Errorf("revoke LAN sessions: %w", err)
	}
	return nil
}

// readRandom fills new signing keys; tests replace it to make rotation fail.
var readRandom = rand.Read

func (s *State) writeNewSecret() ([]byte, error) {
	key := make([]byte, secretBytes)
	if _, err := readRandom(key); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(s.SecretPath, []byte(base64.RawStdEncoding.EncodeToString(key)+"\n")); err != nil {
		return nil, err
	}
	return key, nil
}

// CheckPassword reports whether password matches the stored device password.
func (s *State) CheckPassword(password string) bool {
	data, err := os.ReadFile(s.PasswordPath)
	if err != nil {
		return false
	}
	hash := strings.TrimSpace(string(data))
	if hash == "" || len(password) > MaxPasswordLength {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// IssueToken signs a session token valid for SessionTTL from now.
func (s *State) IssueToken(now time.Time) (string, time.Time, error) {
	key, err := s.ensureSecret()
	if err != nil {
		return "", time.Time{}, err
	}
	expires := now.Add(SessionTTL).UTC().Truncate(time.Second)
	payload := fmt.Sprintf("%s.%d.%d", tokenVersion, expires.Unix(), now.Unix())
	return payload + "." + sign(key, payload), expires, nil
}

// VerifyToken checks a token's signature and expiry and returns its issue time.
func (s *State) VerifyToken(token string, now time.Time) (time.Time, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 4 || parts[0] != tokenVersion {
		return time.Time{}, errInvalidToken
	}
	key, err := s.secret()
	if err != nil {
		return time.Time{}, errInvalidToken
	}
	payload := strings.Join(parts[:3], ".")
	if subtle.ConstantTimeCompare([]byte(sign(key, payload)), []byte(parts[3])) != 1 {
		return time.Time{}, errInvalidToken
	}
	expires, err1 := strconv.ParseInt(parts[1], 10, 64)
	issued, err2 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || !now.Before(time.Unix(expires, 0)) {
		return time.Time{}, errInvalidToken
	}
	return time.Unix(issued, 0), nil
}

func sign(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

type requestCredential struct {
	token      string
	fromCookie bool
}

// requestCredentials lists the bearer token, then the session cookie. The
// bearer is not always a LAN session token: backup and restore send their
// own maintenance and job tokens there, so a signed-in browser's cookie must
// still count when the bearer is something else.
func requestCredentials(r *http.Request) []requestCredential {
	var credentials []requestCredential
	if value := strings.TrimSpace(r.Header.Get("Authorization")); len(value) > 7 && strings.EqualFold(value[:7], "Bearer ") {
		credentials = append(credentials, requestCredential{token: strings.TrimSpace(value[7:])})
	}
	if cookie, err := r.Cookie(SessionCookie); err == nil && cookie.Value != "" {
		credentials = append(credentials, requestCredential{token: cookie.Value, fromCookie: true})
	}
	return credentials
}

func setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// loginLimiter slows password guessing per client address: after
// loginFreeFailures failures each further failure doubles the lockout, up to
// loginMaxLockout. A success clears the address.
type loginLimiter struct {
	mu      sync.Mutex
	clients map[string]*loginFailures
}

type loginFailures struct {
	count       int
	lockedUntil time.Time
	last        time.Time
}

const (
	loginFreeFailures = 5
	loginBaseLockout  = 30 * time.Second
	loginMaxLockout   = 15 * time.Minute
	loginMaxClients   = 256
)

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{clients: make(map[string]*loginFailures)}
}

func (l *loginLimiter) retryAfter(client string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if entry := l.clients[client]; entry != nil && now.Before(entry.lockedUntil) {
		return entry.lockedUntil.Sub(now)
	}
	return 0
}

func (l *loginLimiter) fail(client string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.clients) >= loginMaxClients {
		for key, entry := range l.clients {
			if now.Sub(entry.last) > loginMaxLockout && now.After(entry.lockedUntil) {
				delete(l.clients, key)
			}
		}
	}
	entry := l.clients[client]
	if entry == nil {
		if len(l.clients) >= loginMaxClients {
			// Full of active entries: lock the newcomer rather than evict
			// a client that is mid-lockout.
			l.clients[client] = &loginFailures{count: loginFreeFailures, lockedUntil: now.Add(loginBaseLockout), last: now}
			return
		}
		entry = &loginFailures{}
		l.clients[client] = entry
	}
	entry.count++
	entry.last = now
	if extra := entry.count - loginFreeFailures; extra > 0 {
		lockout := loginBaseLockout << min(extra-1, 10)
		entry.lockedUntil = now.Add(min(lockout, loginMaxLockout))
	}
}

func (l *loginLimiter) succeed(client string) {
	l.mu.Lock()
	delete(l.clients, client)
	l.mu.Unlock()
}

func clientKey(r *http.Request) string {
	if ip, ok := addrIP(r.RemoteAddr); ok {
		return ip.String()
	}
	return r.RemoteAddr
}

// sessionValid reports whether r carries a valid token, renewing an aging
// session cookie so a browser in use stays signed in.
func sessionValid(state *State, w http.ResponseWriter, r *http.Request, now time.Time) bool {
	for _, credential := range requestCredentials(r) {
		issued, err := state.VerifyToken(credential.token, now)
		if err != nil {
			continue
		}
		if credential.fromCookie && now.Sub(issued) >= renewAfter {
			if fresh, expires, err := state.IssueToken(now); err == nil {
				setSessionCookie(w, fresh, expires)
			}
		}
		return true
	}
	return false
}

// safeNext keeps post-login redirects on this host.
func safeNext(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxNextLength || !strings.HasPrefix(value, "/") ||
		strings.HasPrefix(value, "//") || strings.HasPrefix(value, "/\\") || strings.ContainsAny(value, "\r\n") {
		return "/"
	}
	if parsed, err := url.Parse(value); err != nil || parsed.Host != "" || parsed.Scheme != "" {
		return "/"
	}
	return value
}

func sameOriginRequest(r *http.Request) bool {
	value := strings.TrimSpace(r.Header.Get("Origin"))
	if value == "" || value == "null" {
		return value == ""
	}
	parsed, err := url.Parse(value)
	return err == nil && strings.EqualFold(parsed.Host, r.Host)
}

type authHandler struct {
	state   *State
	limiter *loginLimiter
	now     func() time.Time
}

func (a *authHandler) serveLogin(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == LoginPath:
		next := safeNext(r.URL.Query().Get("next"))
		if !a.state.PasswordSet() || IsLocal(r) || sessionValid(a.state, w, r, a.now()) {
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		if IsUSB(r) {
			// A browser on the USB link, such as the app's WebView or the
			// computer at the other end of the cable, pairs instead of
			// asking for the password.
			if a.issueSession(w) {
				writePairedPage(w, next)
				return
			}
			writeLoginPage(w, http.StatusInternalServerError, next, "badRequest")
			return
		}
		writeLoginPage(w, http.StatusOK, next, "")
	case r.Method == http.MethodPost && r.URL.Path == LoginAPI:
		a.login(w, r)
	case r.Method == http.MethodPost && r.URL.Path == PairAPI:
		a.pair(w, r)
	case r.Method == http.MethodPost && r.URL.Path == LogoutAPI:
		if !sameOriginRequest(r) {
			writeAuthJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "cross_origin_rejected"})
			return
		}
		clearSessionCookie(w)
		writeAuthJSON(w, http.StatusOK, map[string]any{"ok": true})
	case r.Method == http.MethodGet && r.URL.Path == SessionAPI:
		passwordSet := a.state.PasswordSet()
		writeAuthJSON(w, http.StatusOK, map[string]any{
			"ok":            true,
			"via_usb":       IsUSB(r),
			"password_set":  passwordSet,
			"authenticated": !passwordSet || IsLocal(r) || sessionValid(a.state, w, r, a.now()),
		})
	default:
		w.Header().Set("Allow", map[string]string{LoginPath: "GET", LoginAPI: "POST", PairAPI: "POST", LogoutAPI: "POST", SessionAPI: "GET"}[r.URL.Path])
		writeAuthJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method_not_allowed"})
	}
}

func (a *authHandler) login(w http.ResponseWriter, r *http.Request) {
	form := !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json")
	var password, next string
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBody)
	if form {
		if err := r.ParseForm(); err != nil {
			writeLoginPage(w, http.StatusBadRequest, "/", "badRequest")
			return
		}
		password, next = r.PostForm.Get("password"), r.PostForm.Get("next")
	} else {
		var body struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAuthJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_request"})
			return
		}
		password = body.Password
	}
	next = safeNext(next)
	fail := func(status int, code, page string, extra map[string]any) {
		if form {
			writeLoginPage(w, status, next, page)
			return
		}
		body := map[string]any{"ok": false, "error": code}
		for key, value := range extra {
			body[key] = value
		}
		writeAuthJSON(w, status, body)
	}
	if !sameOriginRequest(r) {
		fail(http.StatusForbidden, "cross_origin_rejected", "badRequest", nil)
		return
	}
	client := clientKey(r)
	now := a.now()
	if wait := a.limiter.retryAfter(client, now); wait > 0 {
		seconds := int(wait.Round(time.Second) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(max(seconds, 1)))
		fail(http.StatusTooManyRequests, "too_many_attempts", "tooMany", map[string]any{"retry_after": max(seconds, 1)})
		return
	}
	if !a.state.PasswordSet() || !a.state.CheckPassword(password) {
		a.limiter.fail(client, now)
		fail(http.StatusUnauthorized, "invalid_password", "invalid", nil)
		return
	}
	a.limiter.succeed(client)
	token, expires, err := a.state.IssueToken(now)
	if err != nil {
		fail(http.StatusInternalServerError, "session_unavailable", "badRequest", nil)
		return
	}
	setSessionCookie(w, token, expires)
	if form {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	writeAuthJSON(w, http.StatusOK, map[string]any{"ok": true, "token": token, "expires_at": expires.Format(time.RFC3339)})
}

// pair issues a session to a USB client without the password: holding the
// cable proves ownership. The app calls it silently, stores the token, and
// sends it as a bearer from then on, over USB and Wi-Fi alike. It pairs
// again whenever the token stops working (the password changed or was
// cleared, or the token expired) while it is on USB. A browser page on the
// connected computer cannot pair on behalf of another site: Guard refuses
// cross-site requests, and the Origin must be this host.
func (a *authHandler) pair(w http.ResponseWriter, r *http.Request) {
	if !IsUSB(r) {
		writeAuthJSON(w, http.StatusForbidden, map[string]any{
			"ok":      false,
			"error":   "usb_required",
			"message": "Pairing works only over the USB connection. On the LAN, sign in with the device password at " + LoginPath + ".",
		})
		return
	}
	if !sameOriginRequest(r) {
		writeAuthJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "cross_origin_rejected"})
		return
	}
	token, expires, err := a.state.IssueToken(a.now())
	if err != nil {
		writeAuthJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "session_unavailable"})
		return
	}
	setSessionCookie(w, token, expires)
	writeAuthJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"token":        token,
		"expires_at":   expires.Format(time.RFC3339),
		"password_set": a.state.PasswordSet(),
	})
}

// writePairedPage continues to next from a page on this host rather than
// with a redirect. A browser keeps treating a redirect chain that another
// site started (a link to the board followed from a chat page) as
// cross-site, so it would withhold the SameSite=Strict cookie just set and
// land back on /login in a loop. A refresh from this page is a same-site
// navigation and carries the cookie.
func writePairedPage(w http.ResponseWriter, next string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	w.WriteHeader(http.StatusOK)
	_ = pairedPage.Execute(w, next)
}

var pairedPage = template.Must(template.New("paired").Parse(`<!doctype html><html><head><meta charset="utf-8">` +
	`<meta http-equiv="refresh" content="0;url={{.}}"><title>Aico Link</title></head>` +
	`<body><a href="{{.}}">Continue</a></body></html>`))

// issueSession sets a fresh session cookie.
func (a *authHandler) issueSession(w http.ResponseWriter) bool {
	token, expires, err := a.state.IssueToken(a.now())
	if err != nil {
		return false
	}
	setSessionCookie(w, token, expires)
	return true
}

func isAuthPath(path string) bool {
	return path == LoginPath || path == LoginAPI || path == PairAPI || path == LogoutAPI || path == SessionAPI
}

// rejectUnauthenticated sends browsers to the login page and API clients a 401.
func rejectUnauthenticated(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, LoginPath+"?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="aico"`)
	writeAuthJSON(w, http.StatusUnauthorized, map[string]any{
		"ok":      false,
		"error":   "login_required",
		"message": "Sign in with the device password at " + LoginPath + ", or pair over USB at " + PairAPI + ".",
	})
}

func writeAuthJSON(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

var loginErrors = map[string]string{
	"invalid":    "密码错误 · Incorrect password",
	"tooMany":    "尝试次数过多，请稍后再试 · Too many attempts, try again later",
	"badRequest": "请求无法处理 · The request could not be processed",
}

//go:embed login.html
var loginPageSource string

var loginPage = template.Must(template.New("login").Parse(loginPageSource))

func writeLoginPage(w http.ResponseWriter, status int, next, errorKey string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_ = loginPage.Execute(w, struct{ Action, Next, Error string }{LoginAPI, next, loginErrors[errorKey]})
}
