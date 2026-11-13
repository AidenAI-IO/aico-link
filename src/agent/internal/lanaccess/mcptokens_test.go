package lanaccess

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mcpRequest(path, bearer string) *http.Request {
	r := lanRequest(http.MethodPost, path, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

func TestMCPTokenReachesOnlyMCP(t *testing.T) {
	state := enabledState(t)
	token, secret, err := state.CreateMCPToken("MacBook Claude Code", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if token.ID == "" || token.Hash != "" || !strings.HasPrefix(secret, mcpTokenPrefix+token.ID+"_") {
		t.Fatalf("created token: %+v %q", token, secret)
	}
	h := guarded(state)
	if code := serve(h, mcpRequest(MCPPath, secret)); code != http.StatusNoContent {
		t.Fatalf("MCP with token: %d", code)
	}
	for _, path := range []string{"/api/tools/shell", "/api/chat", "/webtty/", "/api/security/lan-access", "/mcp/"} {
		if code := serve(h, mcpRequest(path, secret)); code != http.StatusUnauthorized {
			t.Fatalf("%s with an MCP token: %d", path, code)
		}
	}
	if code := serve(h, mcpRequest(MCPPath, secret+"x")); code != http.StatusUnauthorized {
		t.Fatalf("MCP with a tampered token: %d", code)
	}

	// MCP does not depend on LAN access; everything else is closed again.
	if err := state.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	if code := serve(h, mcpRequest(MCPPath, secret)); code != http.StatusNoContent {
		t.Fatalf("MCP token while LAN access is off: %d", code)
	}
	if code := serve(h, mcpRequest("/api/chat", secret)); code != http.StatusForbidden {
		t.Fatalf("other routes while LAN access is off: %d", code)
	}
	if code := serve(h, mcpRequest(MCPPath, "")); code != http.StatusUnauthorized {
		t.Fatalf("MCP without a token while LAN access is off: %d", code)
	}
}

func TestMCPTokenSurvivesSignOutButNotRevocation(t *testing.T) {
	state := enabledState(t)
	token, secret, err := state.CreateMCPToken("ci", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetPassword("another horse"); err != nil {
		t.Fatal(err)
	}
	h := guarded(state)
	if code := serve(h, mcpRequest(MCPPath, secret)); code != http.StatusNoContent {
		t.Fatalf("MCP token after a password change: %d", code)
	}
	if err := state.RevokeMCPToken(token.ID); err != nil {
		t.Fatal(err)
	}
	if code := serve(h, mcpRequest(MCPPath, secret)); code != http.StatusUnauthorized {
		t.Fatalf("revoked MCP token: %d", code)
	}
	if err := state.RevokeMCPToken(token.ID); !errors.Is(err, ErrMCPTokenNotFound) {
		t.Fatalf("revoking twice: %v", err)
	}
}

func TestMCPTokenStoreKeepsOnlyHashes(t *testing.T) {
	state := newState(t)
	_, secret, err := state.CreateMCPToken("one", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(state.MCPTokensPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) || !strings.Contains(string(data), hashMCPToken(secret)) {
		t.Fatalf("token file: %s", data)
	}
	if info, err := os.Stat(state.MCPTokensPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode: %v %v", info.Mode(), err)
	}
	listed, err := state.MCPTokens()
	if err != nil || len(listed) != 1 || listed[0].Hash != "" || listed[0].Name != "one" {
		t.Fatalf("listed: %+v %v", listed, err)
	}
}

func TestMCPTokenValidation(t *testing.T) {
	state := newState(t)
	for _, name := range []string{"", "   ", strings.Repeat("x", MaxMCPTokenNameLen+1), "a\nb"} {
		if _, _, err := state.CreateMCPToken(name, time.Now()); !errors.Is(err, ErrMCPTokenName) {
			t.Fatalf("name %q: %v", name, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(state.MCPTokensPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.MCPTokensPath, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if state.VerifyMCPToken(mcpTokenPrefix + "abc_def") {
		t.Fatal("a damaged token file must accept nothing")
	}
}

func TestMCPNeedsATokenFromEveryClient(t *testing.T) {
	state := newState(t)
	_, secret, err := state.CreateMCPToken("direct", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	h := guarded(state)
	usbMCP := func(remote, local string, usb bool, bearer string) *http.Request {
		r := newRequest(remote, local, usb)
		r.Method = http.MethodPost
		r.URL.Path = MCPPath
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		return r
	}
	for name, r := range map[string]*http.Request{
		"USB":      usbMCP("192.168.42.100:5000", "192.168.42.1", true, ""),
		"loopback": usbMCP("127.0.0.1:5000", "127.0.0.1", false, ""),
	} {
		if code := serve(h, r); code != http.StatusUnauthorized {
			t.Fatalf("%s /mcp without a token: %d", name, code)
		}
	}
	// LAN access stays off throughout: MCP does not need it on either link.
	if code := serve(h, usbMCP("192.168.42.100:5000", "192.168.42.1", true, secret)); code != http.StatusNoContent {
		t.Fatalf("USB /mcp with a token: %d", code)
	}
	if code := serve(h, mcpRequest(MCPPath, secret)); code != http.StatusNoContent {
		t.Fatalf("LAN /mcp with a token while LAN access is off: %d", code)
	}
	// Without a device password, other USB routes need no credentials.
	other := newRequest("192.168.42.100:5000", "192.168.42.1", true)
	if code := serve(h, other); code != http.StatusNoContent {
		t.Fatalf("USB request to another route: %d", code)
	}
}

func TestMCPRefusesSessionTokens(t *testing.T) {
	state := enabledState(t)
	session, _, err := state.IssueToken(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if code := serve(guarded(state), mcpRequest(MCPPath, session)); code != http.StatusUnauthorized {
		t.Fatalf("/mcp with a session token: %d", code)
	}
}
