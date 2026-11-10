package configweb

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func callMCPTokens(t *testing.T, server *Server, method, path, body string, usb bool) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, "http://127.0.0.1"+path, bytes.NewBufferString(body))
	req.RemoteAddr = "127.0.0.2:1234"
	req = req.WithContext(context.WithValue(req.Context(), usbIngressContextKey{}, usb))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, req)
	decoded := map[string]any{}
	_ = json.Unmarshal(response.Body.Bytes(), &decoded)
	return response.Code, decoded
}

func TestMCPTokensLifecycleOverUSB(t *testing.T) {
	server, _ := newLANAccessServer(t)
	const api = "/api/security/mcp-tokens"
	code, body := callMCPTokens(t, server, http.MethodPost, api, `{"name":"MacBook"}`, true)
	if code != http.StatusCreated || body["secret"] == "" {
		t.Fatalf("create: %d %v", code, body)
	}
	token := body["token"].(map[string]any)
	id, _ := token["id"].(string)
	if _, leaked := token["hash"]; leaked && token["hash"] != "" {
		t.Fatalf("create leaked the hash: %v", token)
	}
	if !server.lanAccess.VerifyMCPToken(body["secret"].(string)) {
		t.Fatal("created secret does not verify")
	}

	code, body = callMCPTokens(t, server, http.MethodGet, api, "", true)
	tokens, _ := body["tokens"].([]any)
	if code != http.StatusOK || len(tokens) != 1 || tokens[0].(map[string]any)["name"] != "MacBook" {
		t.Fatalf("list: %d %v", code, body)
	}

	code, _ = callMCPTokens(t, server, http.MethodDelete, api+"/"+id, "", true)
	if code != http.StatusOK {
		t.Fatalf("revoke: %d", code)
	}
	code, _ = callMCPTokens(t, server, http.MethodDelete, api+"/"+id, "", true)
	if code != http.StatusNotFound {
		t.Fatalf("revoke twice: %d", code)
	}
}

func TestMCPTokensRefuseUnauthenticatedLANClients(t *testing.T) {
	server, _ := newLANAccessServer(t)
	const api = "/api/security/mcp-tokens"
	for _, call := range []struct{ method, path, body string }{
		{http.MethodGet, api, ""},
		{http.MethodPost, api, `{"name":"x"}`},
		{http.MethodDelete, api + "/abc", ""},
	} {
		if code, _ := callMCPTokens(t, server, call.method, call.path, call.body, false); code != http.StatusForbidden {
			t.Fatalf("%s %s from LAN: %d", call.method, call.path, code)
		}
	}
}
