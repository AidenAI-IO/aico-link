package lanaccess

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func enabledState(t *testing.T) *State {
	t.Helper()
	state := newState(t)
	if err := state.SetPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if err := state.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	return state
}

func lanRequest(method, target, body string) *http.Request {
	r := newRequest("192.168.50.20:5000", "192.168.50.76", false)
	r.Method = method
	r.URL, _ = url.Parse(target)
	r.RequestURI = target
	r.Host = "192.168.50.76"
	if body != "" {
		r.Body = httpBody(body)
		r.ContentLength = int64(len(body))
	}
	return r
}

func httpBody(body string) *readCloser { return &readCloser{strings.NewReader(body)} }

type readCloser struct{ *strings.Reader }

func (readCloser) Close() error { return nil }

func record(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func jsonLogin(password string) *http.Request {
	r := lanRequest(http.MethodPost, LoginAPI, `{"password":`+mustJSON(password)+`}`)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func mustJSON(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func TestLoginIssuesTokenAndCookie(t *testing.T) {
	state := enabledState(t)
	h := guarded(state)

	if w := record(h, lanRequest(http.MethodGet, "/api/tools", "")); w.Code != http.StatusUnauthorized {
		t.Fatalf("no session: %d", w.Code)
	}
	if w := record(h, jsonLogin("wrong password")); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", w.Code)
	}
	w := record(h, jsonLogin("correct horse"))
	var body struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || !body.OK || body.Token == "" {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != SessionCookie || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie: %+v", cookies)
	}

	withCookie := lanRequest(http.MethodGet, "/api/tools", "")
	withCookie.AddCookie(cookies[0])
	if w := record(h, withCookie); w.Code != http.StatusNoContent {
		t.Fatalf("cookie session: %d", w.Code)
	}
	withBearer := lanRequest(http.MethodGet, "/api/tools", "")
	withBearer.Header.Set("Authorization", "Bearer "+body.Token)
	if w := record(h, withBearer); w.Code != http.StatusNoContent {
		t.Fatalf("bearer session: %d", w.Code)
	}
}

func TestBrowserIsSentToLoginAndBack(t *testing.T) {
	state := enabledState(t)
	h := guarded(state)

	page := lanRequest(http.MethodGet, "/webtty/?a=1", "")
	page.Header.Set("Accept", "text/html")
	w := record(h, page)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login?next="+url.QueryEscape("/webtty/?a=1") {
		t.Fatalf("redirect to login: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := record(h, lanRequest(http.MethodGet, "/login?next=%2Fwebtty%2F", "")); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `name="next" value="/webtty/"`) {
		t.Fatalf("login page: %d %s", w.Code, w.Body.String())
	}

	form := lanRequest(http.MethodPost, LoginAPI, "password=correct+horse&next=%2Fwebtty%2F")
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	form.Header.Set("Origin", "http://192.168.50.76")
	w = record(h, form)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/webtty/" || len(w.Result().Cookies()) != 1 {
		t.Fatalf("form login: %d %q", w.Code, w.Header().Get("Location"))
	}

	bad := lanRequest(http.MethodPost, LoginAPI, "password=nope&next=%2F")
	bad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if w := record(h, bad); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "Incorrect password") {
		t.Fatalf("form wrong password: %d", w.Code)
	}
}

func TestLoginRejectsCrossOriginAndOpenRedirect(t *testing.T) {
	state := enabledState(t)
	h := guarded(state)

	cross := jsonLogin("correct horse")
	cross.Header.Set("Origin", "http://evil.example")
	if w := record(h, cross); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login: %d", w.Code)
	}
	for _, next := range []string{"//evil.example/", "https://evil.example/", "/\\evil.example", "evil"} {
		if got := safeNext(next); got != "/" {
			t.Fatalf("safeNext(%q) = %q", next, got)
		}
	}
	if got := safeNext("/advanced/lan?x=1"); got != "/advanced/lan?x=1" {
		t.Fatalf("safeNext kept path: %q", got)
	}
}

func TestPasswordChangeAndSwitchOffSignOut(t *testing.T) {
	state := enabledState(t)
	token, _, err := state.IssueToken(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.VerifyToken(token, time.Now()); err != nil {
		t.Fatalf("fresh token: %v", err)
	}
	if err := state.SetPassword("another horse"); err != nil {
		t.Fatal(err)
	}
	if _, err := state.VerifyToken(token, time.Now()); err == nil {
		t.Fatal("token survived a password change")
	}

	token, _, _ = state.IssueToken(time.Now())
	if err := state.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	if err := state.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, err := state.VerifyToken(token, time.Now()); err == nil {
		t.Fatal("token survived turning LAN access off")
	}
}

func TestTokenExpiresAndRejectsTampering(t *testing.T) {
	state := enabledState(t)
	now := time.Now()
	token, _, err := state.IssueToken(now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.VerifyToken(token, now.Add(SessionTTL+time.Second)); err == nil {
		t.Fatal("expired token accepted")
	}
	parts := strings.Split(token, ".")
	parts[1] = "99999999999"
	if _, err := state.VerifyToken(strings.Join(parts, "."), now); err == nil {
		t.Fatal("tampered expiry accepted")
	}
	if _, err := state.VerifyToken("", now); err == nil {
		t.Fatal("empty token accepted")
	}
}

func TestSessionCookieIsRenewedAfterADay(t *testing.T) {
	state := enabledState(t)
	auth := &authHandler{state: state, limiter: newLoginLimiter(), now: time.Now}
	issued := time.Now().Add(-renewAfter - time.Minute)
	token, _, err := state.IssueToken(issued)
	if err != nil {
		t.Fatal(err)
	}
	r := lanRequest(http.MethodGet, "/api/tools", "")
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	w := httptest.NewRecorder()
	if !sessionValid(state, w, r, auth.now()) {
		t.Fatal("day-old session rejected")
	}
	if cookies := w.Result().Cookies(); len(cookies) != 1 || cookies[0].Value == token {
		t.Fatalf("day-old cookie not renewed: %+v", cookies)
	}
}

func TestLoginLockoutAfterRepeatedFailures(t *testing.T) {
	state := enabledState(t)
	h := guarded(state)
	for i := 0; i < loginFreeFailures+1; i++ {
		if w := record(h, jsonLogin("wrong password")); w.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i+1, w.Code)
		}
	}
	w := record(h, jsonLogin("correct horse"))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("locked out client: %d", w.Code)
	}

	limiter := newLoginLimiter()
	now := time.Now()
	for i := 0; i < loginFreeFailures+1; i++ {
		limiter.fail("a", now)
	}
	if wait := limiter.retryAfter("a", now); wait != loginBaseLockout {
		t.Fatalf("first lockout = %v", wait)
	}
	if limiter.retryAfter("a", now.Add(loginBaseLockout)) != 0 {
		t.Fatal("lockout did not end")
	}
	limiter.fail("a", now.Add(loginBaseLockout))
	if wait := limiter.retryAfter("a", now.Add(loginBaseLockout)); wait != 2*loginBaseLockout {
		t.Fatalf("second lockout = %v", wait)
	}
	limiter.succeed("a")
	if limiter.retryAfter("a", now.Add(loginBaseLockout)) != 0 {
		t.Fatal("success did not clear the lockout")
	}
}

func TestAuthEndpointsWhileOffAndOverUSB(t *testing.T) {
	state := newState(t)
	h := guarded(state)
	if w := record(h, jsonLogin("correct horse")); w.Code != http.StatusForbidden {
		t.Fatalf("login while LAN access is off: %d", w.Code)
	}
	usb := newRequest("192.168.42.100:5000", "192.168.42.1", true)
	usb.URL, _ = url.Parse(SessionAPI)
	w := record(h, usb)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"authenticated":true`) {
		t.Fatalf("USB session status: %d %s", w.Code, w.Body.String())
	}
}

func TestLogoutClearsCookie(t *testing.T) {
	state := enabledState(t)
	h := guarded(state)
	w := record(h, lanRequest(http.MethodPost, LogoutAPI, ""))
	cookies := w.Result().Cookies()
	if w.Code != http.StatusOK || len(cookies) != 1 || cookies[0].MaxAge >= 0 {
		t.Fatalf("logout: %d %+v", w.Code, cookies)
	}
}

func TestCookieSessionCountsWhenBearerIsAnotherToken(t *testing.T) {
	state := enabledState(t)
	h := guarded(state)
	token, _, err := state.IssueToken(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Backup and restore put their own maintenance/job tokens in the bearer.
	r := lanRequest(http.MethodGet, "/api/backup/jobs/1", "")
	r.Header.Set("Authorization", "Bearer maintenance-token")
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	if w := record(h, r); w.Code != http.StatusNoContent {
		t.Fatalf("cookie session with a non-session bearer: %d", w.Code)
	}

	r = lanRequest(http.MethodGet, "/api/backup/jobs/1", "")
	r.Header.Set("Authorization", "Bearer maintenance-token")
	if w := record(h, r); w.Code != http.StatusUnauthorized {
		t.Fatalf("non-session bearer alone: %d", w.Code)
	}
}
