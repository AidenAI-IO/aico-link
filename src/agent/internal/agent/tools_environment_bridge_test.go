package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aiden-agent/internal/agent/mnk"
	"aiden-agent/internal/agent/screenprovider"
)

type recordingBridgeTextProvider struct {
	text string
	err  error
}

func (p *recordingBridgeTextProvider) KeyboardText(_ context.Context, text string) error {
	p.text = text
	return p.err
}

func TestEnvironmentBridgeRejectsOwnAgentListener(t *testing.T) {
	listener := &net.TCPAddr{IP: net.IPv4zero, Port: 8080}
	for _, tc := range []struct {
		endpoint string
		reject   bool
	}{
		{endpoint: "http://127.0.0.1:8080", reject: true},
		{endpoint: "http://127.0.0.1:08080", reject: true},
		{endpoint: "http://localhost:8080", reject: true},
		{endpoint: "http://0.0.0.0:8080", reject: true},
		{endpoint: "http://[::1]:8080", reject: true},
		{endpoint: "http://127.0.0.1:9090", reject: false},
		{endpoint: "http://127.0.0.1", reject: false},
		{endpoint: "http://192.0.2.5:8080", reject: false},
		{endpoint: "http://host.docker.internal:8080", reject: false},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			err := validateBridgeEndpointNotSelf(tc.endpoint, listener)
			if (err != nil) != tc.reject {
				t.Fatalf("validateBridgeEndpointNotSelf(%q) = %v; reject=%v", tc.endpoint, err, tc.reject)
			}
		})
	}
}

func TestEnvironmentBridgeRejectsLocalInterfaceAddressOnWildcardListener(t *testing.T) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("list interface addresses: %v", err)
	}
	var local net.IP
	for _, addr := range addrs {
		if network, ok := addr.(*net.IPNet); ok && !network.IP.IsLoopback() && network.IP.To4() != nil {
			local = network.IP
			break
		}
	}
	if local == nil {
		t.Skip("no non-loopback IPv4 interface address")
	}
	endpoint := "http://" + net.JoinHostPort(local.String(), "8080")
	if err := validateBridgeEndpointNotSelf(endpoint, &net.TCPAddr{IP: net.IPv4zero, Port: 8080}); err == nil {
		t.Fatalf("validateBridgeEndpointNotSelf(%q) on wildcard listener = nil; want rejection", endpoint)
	}
	// A listener bound to loopback only does not answer on interface addresses.
	if err := validateBridgeEndpointNotSelf(endpoint, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}); err != nil {
		t.Fatalf("validateBridgeEndpointNotSelf(%q) on loopback listener = %v; want nil", endpoint, err)
	}
}

func TestServerStartRejectsBridgeEndpointPointingAtItself(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve HTTP address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release HTTP address: %v", err)
	}
	cfg := Config{EnvironmentBridge: EnvironmentBridgeConfig{Enabled: true, Endpoint: "http://" + addr}}
	server := &Server{
		addr:             addr,
		runtime:          &Runtime{config: cfg},
		eventBroadcaster: NewEventBroadcaster(),
		activeRuns:       make(map[string]context.CancelFunc),
	}
	err = server.Start()
	if err == nil || !strings.Contains(err.Error(), "points to the agent listener") {
		t.Fatalf("Server.Start() = %v; want self-endpoint rejection", err)
	}
	// The rejected listener must be released so the port is reusable.
	again, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listener was not closed after rejection: %v", err)
	}
	_ = again.Close()
}

func TestGoEnvironmentBridgeServesInternalKeyboardText(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			sink := &recordingBridgeTextProvider{}
			if fail {
				sink.err = fmt.Errorf("keyboard disconnected")
			}
			tools := &ToolSet{
				mnkProvider: mnk.NewMockProvider(),
				textInputHW: &textInputHardwareDeps{keyboardText: &KeyboardTextTool{textProvider: sink}},
			}
			server := &Server{runtime: &Runtime{tools: tools}}
			endpoint := httptest.NewServer(http.HandlerFunc(server.handleProviderMNK))
			defer endpoint.Close()
			client := mnk.NewHTTPProvider(mnk.HTTPProviderConfig{BaseURL: endpoint.URL})
			err := client.KeyboardText(context.Background(), "Aiden 2026!")
			if fail {
				if err == nil || !strings.Contains(err.Error(), "keyboard disconnected") {
					t.Fatalf("lost keyboard failure: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if sink.text != "Aiden 2026!" {
				t.Fatalf("typed %q", sink.text)
			}
		})
	}
}

func TestBuiltinToolSetFromConfigUsesEnvironmentBridgeProviders(t *testing.T) {
	tools := NewBuiltinToolSetFromConfig(Config{
		EnvironmentBridge: EnvironmentBridgeConfig{
			Enabled:         true,
			Endpoint:        "http://bridge.example",
			BenchmarkTaskID: "suite:task-1",
		},
	}, ProxyConfig{})

	if _, ok := tools.ScreenProvider().(*screenprovider.HTTP); !ok {
		t.Fatalf("screen provider = %T, want *screenprovider.HTTP", tools.ScreenProvider())
	}
	if _, ok := tools.MNKProvider().(*mnk.HTTPProvider); !ok {
		t.Fatalf("MNK provider = %T, want *mnk.HTTPProvider", tools.MNKProvider())
	}
	if !tools.iosKeyboardIsolationOptional {
		t.Fatal("environment bridge tool set must allow iOS text input without local isolation")
	}
}

// Covers the wiring, not just the flag: enter_text is only ever built by
// RegisterEnterTextTool, so a bypass that the ToolSet knows about but never
// hands to the tool still fails every iOS bridge run.
func TestRegisterEnterTextToolPropagatesBridgeIsolationBypass(t *testing.T) {
	bridgeTools := NewBuiltinToolSetFromConfig(Config{
		Device: DeviceConfig{DeviceType: "iOS"},
		EnvironmentBridge: EnvironmentBridgeConfig{
			Enabled:         true,
			Endpoint:        "http://vphone-bridge:8899",
			BenchmarkTaskID: "vphone-regression",
		},
	}, ProxyConfig{})
	bridgeTools.RegisterEnterTextTool(&testModelResolver{model: &scriptedModel{}}, func() string { return "iOS" })
	if !registeredEnterTextTool(t, bridgeTools).allowIOSKeyboardIsolationBypass {
		t.Fatal("registered enter_text must bypass local isolation when an environment bridge drives the device")
	}

	localTools := NewBuiltinToolSetFromConfig(Config{Device: DeviceConfig{DeviceType: "iOS"}}, ProxyConfig{})
	localTools.RegisterEnterTextTool(&testModelResolver{model: &scriptedModel{}}, func() string { return "iOS" })
	if registeredEnterTextTool(t, localTools).allowIOSKeyboardIsolationBypass {
		t.Fatal("local iOS hardware must keep requiring USB keyboard isolation for enter_text")
	}
}

func registeredEnterTextTool(t *testing.T, tools *ToolSet) *EnterTextTool {
	t.Helper()
	wrapped, ok := tools.Get("enter_text")
	if !ok {
		t.Fatal("enter_text is not registered")
	}
	post, ok := wrapped.(*postActionScreenshotTool)
	if !ok {
		t.Fatalf("enter_text type = %T, want *postActionScreenshotTool", wrapped)
	}
	entry, ok := post.inner.(*EnterTextTool)
	if !ok {
		t.Fatalf("enter_text inner type = %T, want *EnterTextTool", post.inner)
	}
	return entry
}

func TestIOSKeyboardIsolationIsDisabledForActiveEnvironmentBridge(t *testing.T) {
	bridge := Config{EnvironmentBridge: EnvironmentBridgeConfig{
		Enabled:  true,
		Endpoint: "http://vphone-bridge:8899",
	}}
	if shouldEnableIOSKeyboardIsolation(bridge) {
		t.Fatal("environment bridge must not enable local iOS USB keyboard isolation")
	}

	localIOS := Config{Device: DeviceConfig{DeviceType: "iOS"}}
	if !shouldEnableIOSKeyboardIsolation(localIOS) {
		t.Fatal("local iOS hardware must keep USB keyboard isolation enabled")
	}
}

func TestEnvironmentBridgeIOSQuickActionUsesRemoteMNKWithoutIsolation(t *testing.T) {
	var gotKeys []string
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/providers/mnk" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		var payload struct {
			Operation string `json:"operation"`
			Keypress  struct {
				Keys []string `json:"keys"`
			} `json:"keypress"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gotKeys = append([]string(nil), payload.Keypress.Keys...)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer bridge.Close()

	tools := NewBuiltinToolSetFromConfig(Config{
		Device: DeviceConfig{DeviceType: "iOS"},
		EnvironmentBridge: EnvironmentBridgeConfig{
			Enabled:         true,
			Endpoint:        " \t" + bridge.URL + "\n ",
			BenchmarkTaskID: "vphone-regression",
		},
	}, ProxyConfig{})
	tools.SetRuntimeDeviceTypeFn(func() string { return "iOS" })
	wrapped, ok := tools.Get("quick_action")
	if !ok {
		t.Fatal("quick_action is not registered")
	}
	quick, ok := wrapped.(*postActionScreenshotTool)
	if !ok {
		t.Fatalf("quick_action type = %T, want postActionScreenshotTool", wrapped)
	}
	inner, ok := quick.inner.(*QuickActionTool)
	if !ok {
		t.Fatalf("quick_action inner type = %T, want QuickActionTool", quick.inner)
	}
	if inner.iosKeyboardIsolation != nil {
		t.Fatal("bridge quick_action must not have a local iOS isolation controller")
	}

	if _, err := inner.Call(context.Background(), `{"action":"select_all"}`); err != nil {
		t.Fatalf("quick_action Call() error = %v", err)
	}
	if want := []string{"meta", "a"}; !equalStrings(gotKeys, want) {
		t.Fatalf("remote keypress = %v, want %v", gotKeys, want)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestWhitespaceOnlyEnvironmentBridgeEndpointKeepsLocalProviders(t *testing.T) {
	tools := NewBuiltinToolSetFromConfig(Config{EnvironmentBridge: EnvironmentBridgeConfig{Enabled: true, Endpoint: " \t\n "}}, ProxyConfig{})
	if _, ok := tools.ScreenProvider().(*screenprovider.HTTP); ok {
		t.Fatal("blank endpoint selected HTTP screen provider")
	}
	if _, ok := tools.MNKProvider().(*mnk.HTTPProvider); ok {
		t.Fatal("blank endpoint selected HTTP MNK provider")
	}
	if tools.iosKeyboardIsolationOptional {
		t.Fatal("blank endpoint disabled local isolation requirement")
	}
}

func TestEnvironmentBridgeTextInputUsesRemoteKeyboard(t *testing.T) {
	var gotText string
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/providers/mnk" || r.Header.Get(mnk.BenchmarkTaskIDHeader) != "typing-task" {
			t.Errorf("unexpected keyboard request: %s task=%q", r.URL.Path, r.Header.Get(mnk.BenchmarkTaskIDHeader))
		}
		var payload struct {
			Operation    string `json:"operation"`
			KeyboardText struct {
				Text string `json:"text"`
			} `json:"keyboard_text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.Operation != "keyboard_text" {
			t.Errorf("operation = %q", payload.Operation)
		}
		gotText = payload.KeyboardText.Text
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer bridge.Close()
	tools := NewBuiltinToolSetFromConfig(Config{EnvironmentBridge: EnvironmentBridgeConfig{
		Enabled: true, Endpoint: bridge.URL, BenchmarkTaskID: "typing-task",
	}}, ProxyConfig{})
	// Exercise the exact internal dependency used by enter_text's probe and chunks.
	out, err := tools.textInputHW.keyboardText.Call(context.Background(), `{"text":"Aiden test-2026!"}`)
	if err != nil || out != "ok" || gotText != "Aiden test-2026!" {
		t.Fatalf("remote typing: output=%q err=%v text=%q", out, err, gotText)
	}
}
