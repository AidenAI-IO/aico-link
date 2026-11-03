package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"aico-agent/internal/agent/contextmanager"
)

func TestShellToolDescriptionMentionsNotificationHistory(t *testing.T) {
	description := (&ShellTool{}).Description()
	for _, want := range []string{"/userdata/agent/memory/notifications/events/", "YYYY-MM-DD.jsonl", "one record per line", "read-only"} {
		if !strings.Contains(description, want) {
			t.Fatalf("shell description missing %q: %s", want, description)
		}
	}
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test relies on POSIX sh")
	}
}

func TestShellToolForegroundEcho(t *testing.T) {
	skipOnWindows(t)
	tool := &ShellTool{}

	out, err := tool.Call(context.Background(), `{"command":"echo hello"}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if strings.TrimRight(out, "\n") != "hello" {
		t.Fatalf("Call output = %q, want %q", out, "hello\n")
	}
}

func TestShellToolReadsStoredToolResultFileByPath(t *testing.T) {
	skipOnWindows(t)
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	stored, err := manager.StoreArtifact("text/plain", []byte("prefix\nBOARD_TAIL\nsuffix\n"), contextmanager.ArtifactMetadata{})
	if err != nil {
		t.Fatalf("StoreArtifact() error = %v", err)
	}
	input, err := json.Marshal(map[string]any{
		"command": fmt.Sprintf("grep -F BOARD_TAIL %q", stored.Path),
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	out, err := (&ShellTool{}).Call(context.Background(), string(input))
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if strings.TrimSpace(out) != "BOARD_TAIL" {
		t.Fatalf("Call() output = %q", out)
	}
}

func TestShellToolForegroundInjectsProxyEnv(t *testing.T) {
	skipOnWindows(t)
	tool := &ShellTool{execution: shellExecutionConfig{proxy: ProxyConfig{
		HTTPProxy:  "http://proxy.example:18080",
		HTTPSProxy: "http://proxy.example:18443",
		AllProxy:   "socks5://proxy.example:18081",
		NoProxy:    "localhost,127.0.0.1",
	}}}

	out, err := tool.Call(context.Background(), `{"command":"printf '%s|%s|%s|%s|%s|%s|%s|%s' \"$http_proxy\" \"$HTTP_PROXY\" \"$https_proxy\" \"$HTTPS_PROXY\" \"$all_proxy\" \"$ALL_PROXY\" \"$no_proxy\" \"$NO_PROXY\""}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	want := strings.Join([]string{
		"http://proxy.example:18080",
		"http://proxy.example:18080",
		"http://proxy.example:18443",
		"http://proxy.example:18443",
		"socks5://proxy.example:18081",
		"socks5://proxy.example:18081",
		"localhost,127.0.0.1",
		"localhost,127.0.0.1",
	}, "|")
	if out != want {
		t.Fatalf("Call output = %q, want %q", out, want)
	}
}

func TestShellToolInjectsTemporaryDirectoryWithoutChangingPythonEnvironment(t *testing.T) {
	skipOnWindows(t)
	// PYTHONUSERBASE and pip flags are inherited from the parent environment.
	// TMPDIR is injected command-scoped by the shell tool.
	t.Setenv("PYTHONUSERBASE", "inherited-userbase")
	t.Setenv("PIP_USER", "inherited-pip-user")
	t.Setenv("TMPDIR", "inherited-tmp")
	t.Setenv("PATH", "/bin:/usr/bin")

	tool := &ShellTool{execution: shellExecutionConfig{temporaryDirectory: "/userdata/tmp"}}
	out, err := tool.Call(context.Background(), `{"command":"printf '%s|%s|%s|%s' \"$PYTHONUSERBASE\" \"$PIP_USER\" \"$TMPDIR\" \"$PATH\""}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	want := "inherited-userbase|inherited-pip-user|/userdata/tmp|/bin:/usr/bin"
	if out != want {
		t.Fatalf("Call output = %q, want %q", out, want)
	}
}

func TestBuiltinToolSetWiresTemporaryDirectoryIntoShell(t *testing.T) {
	skipOnWindows(t)
	// PYTHONUSERBASE is inherited from the parent environment.
	t.Setenv("PYTHONUSERBASE", "/userdata/agent/python")

	toolSet := NewBuiltinToolSet(
		HIDConfig{},
		AudioConfig{},
		SearchConfig{},
		ProxyConfig{},
		WithShellTemporaryDirectory("/userdata/tmp"),
	)
	tool, ok := toolSet.Get("shell")
	if !ok {
		t.Fatal("shell tool is not registered")
	}

	out, err := tool.Call(context.Background(), `{"command":"printf '%s|%s' \"$PYTHONUSERBASE\" \"$TMPDIR\""}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if want := "/userdata/agent/python|/userdata/tmp"; out != want {
		t.Fatalf("Call output = %q, want %q", out, want)
	}
}

func TestShellCommandEnvAppliesTemporaryDirectoryInPTYAndNonPTYModes(t *testing.T) {
	// PYTHONUSERBASE is now configured globally and inherited from the
	// parent environment. TMPDIR is injected command-scoped to avoid storage
	// wear from a global override.
	t.Setenv("PYTHONUSERBASE", "/userdata/agent/python")

	execution := shellExecutionConfig{temporaryDirectory: "/userdata/tmp"}
	for _, usePTY := range []bool{false, true} {
		env := shellCommandEnv(usePTY, execution)
		values := make(map[string]string)
		for _, entry := range env {
			parts := strings.SplitN(entry, "=", 2)
			if len(parts) == 2 {
				values[parts[0]] = parts[1]
			}
		}
		if got := values["PYTHONUSERBASE"]; got != "/userdata/agent/python" {
			t.Errorf("usePTY=%v PYTHONUSERBASE = %q, want %q (should inherit from parent env)", usePTY, got, "/userdata/agent/python")
		}
		if got := values["TMPDIR"]; got != execution.temporaryDirectory {
			t.Errorf("usePTY=%v TMPDIR = %q, want %q (should be injected command-scoped)", usePTY, got, execution.temporaryDirectory)
		}
	}
}

func TestShellToolTemporaryDirectoryReachesForegroundPTY(t *testing.T) {
	skipOnWindows(t)
	// PYTHONUSERBASE is inherited from parent environment.
	t.Setenv("PYTHONUSERBASE", "/userdata/agent/python")

	tool := &ShellTool{execution: shellExecutionConfig{temporaryDirectory: "/userdata/tmp"}}
	out, err := tool.Call(context.Background(), `{"command":"printf '%s|%s' \"$PYTHONUSERBASE\" \"$TMPDIR\"","pty":true}`)
	if err != nil {
		t.Fatalf("PTY Call returned error: %v", err)
	}
	want := "/userdata/agent/python|/userdata/tmp"
	if !strings.Contains(out, want) {
		t.Fatalf("PTY Call output = %q, want it to contain %q", out, want)
	}
}

func TestShellToolTemporaryDirectoryReachesBackgroundCommands(t *testing.T) {
	skipOnWindows(t)
	// PYTHONUSERBASE is inherited from parent environment.
	t.Setenv("PYTHONUSERBASE", "/userdata/agent/python")

	tool := &ShellTool{execution: shellExecutionConfig{temporaryDirectory: "/userdata/tmp"}}
	want := "/userdata/agent/python|/userdata/tmp"

	startOut, err := tool.Call(context.Background(), `{"action":"start","command":"printf '%s|%s' \"$PYTHONUSERBASE\" \"$TMPDIR\"; sleep 1"}`)
	if err != nil {
		t.Fatalf("background start returned error: %v", err)
	}
	var started struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(startOut), &started); err != nil {
		t.Fatalf("background start output = %q, unmarshal error = %v", startOut, err)
	}
	defer func() {
		stopArgs, _ := json.Marshal(map[string]interface{}{
			"action":     "stop",
			"session_id": started.SessionID,
		})
		_, _ = tool.Call(context.Background(), string(stopArgs))
	}()

	pollArgs, _ := json.Marshal(map[string]interface{}{
		"action":     "poll",
		"session_id": started.SessionID,
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pollOut, pollErr := tool.Call(context.Background(), string(pollArgs))
		if pollErr != nil {
			t.Fatalf("background poll returned error: %v", pollErr)
		}
		var poll struct {
			Output string `json:"output"`
		}
		if err := json.Unmarshal([]byte(pollOut), &poll); err != nil {
			t.Fatalf("background poll output = %q, unmarshal error = %v", pollOut, err)
		}
		if strings.Contains(poll.Output, want) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("background command did not receive managed Python hints")
}

func TestShellApplyProxyEnvReplacesInheritedProxy(t *testing.T) {
	env := shellApplyProxyEnv([]string{
		"PATH=/bin",
		"http_proxy=http://old-http",
		"HTTP_PROXY=http://old-http",
		"https_proxy=http://old-https",
		"HTTPS_PROXY=http://old-https",
		"NO_PROXY=old.local",
	}, ProxyConfig{
		HTTPSProxy: " http://new-https ",
		NoProxy:    " localhost,127.0.0.1 ",
	})

	got := map[string]string{}
	for _, entry := range env {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) == 2 {
			got[parts[0]] = parts[1]
		}
	}

	if got["PATH"] != "/bin" {
		t.Fatalf("PATH = %q, want preserved /bin", got["PATH"])
	}
	if _, ok := got["http_proxy"]; ok {
		t.Fatalf("http_proxy was preserved from inherited env: %q", got["http_proxy"])
	}
	if _, ok := got["HTTP_PROXY"]; ok {
		t.Fatalf("HTTP_PROXY was preserved from inherited env: %q", got["HTTP_PROXY"])
	}
	if got["https_proxy"] != "http://new-https" || got["HTTPS_PROXY"] != "http://new-https" {
		t.Fatalf("https proxy env not replaced: https_proxy=%q HTTPS_PROXY=%q", got["https_proxy"], got["HTTPS_PROXY"])
	}
	if got["no_proxy"] != "localhost,127.0.0.1" || got["NO_PROXY"] != "localhost,127.0.0.1" {
		t.Fatalf("no_proxy env not replaced: no_proxy=%q NO_PROXY=%q", got["no_proxy"], got["NO_PROXY"])
	}
}

func TestShellApplyProxyEnvDefaultsNoProxy(t *testing.T) {
	env := shellApplyProxyEnv([]string{"PATH=/bin"}, ProxyConfig{
		HTTPSProxy: "http://proxy.example:18443",
	})

	got := map[string]string{}
	for _, entry := range env {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) == 2 {
			got[parts[0]] = parts[1]
		}
	}

	if got["no_proxy"] != DefaultNoProxy || got["NO_PROXY"] != DefaultNoProxy {
		t.Fatalf("default no_proxy not injected: no_proxy=%q NO_PROXY=%q", got["no_proxy"], got["NO_PROXY"])
	}
}

func TestShellApplyProxyEnvKeepsEnvironmentWhenNoProxyURLConfigured(t *testing.T) {
	env := shellApplyProxyEnv([]string{
		"PATH=/bin",
		"HTTPS_PROXY=http://inherited-proxy",
		"NO_PROXY=internal.example",
	}, ProxyConfig{
		NoProxy: DefaultNoProxy,
	})

	got := map[string]string{}
	for _, entry := range env {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) == 2 {
			got[parts[0]] = parts[1]
		}
	}

	if got["HTTPS_PROXY"] != "http://inherited-proxy" {
		t.Fatalf("HTTPS_PROXY = %q, want inherited proxy preserved", got["HTTPS_PROXY"])
	}
	if _, ok := got["no_proxy"]; ok {
		t.Fatalf("no_proxy = %q, want unset", got["no_proxy"])
	}
	if got["NO_PROXY"] != "internal.example" {
		t.Fatalf("NO_PROXY = %q, want inherited value", got["NO_PROXY"])
	}
}

func TestShellToolForegroundWorkdir(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()
	tool := &ShellTool{}

	payload := map[string]interface{}{
		"command": "pwd",
		"workdir": dir,
	}
	bs, _ := json.Marshal(payload)

	out, err := tool.Call(context.Background(), string(bs))
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}

	got := strings.TrimRight(out, "\n")
	// macOS resolves /var to /private/var; accept both.
	if got != dir && got != "/private"+dir {
		t.Fatalf("Call output = %q, want %q", got, dir)
	}
}

func TestShellToolForegroundFailureSurfacesStderr(t *testing.T) {
	skipOnWindows(t)
	tool := &ShellTool{}
	ctx, _ := WithToolError(context.Background())

	out, err := tool.Call(ctx, `{"command":"sh -c 'echo boom 1>&2; exit 7'"}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !strings.HasPrefix(out, "Error: ") {
		t.Fatalf("expected error message, got %q", out)
	}
	if !strings.Contains(out, "Stderr:\nboom") {
		t.Fatalf("expected stderr in output, got %q", out)
	}
	if got := ToolErrorFromContext(ctx); got == nil || got.Code != CodeToolExecutionFailed || got.Message != out {
		t.Fatalf("ToolError = %+v, want tool_execution_failed with output message", got)
	}
}

func TestShellToolForegroundTimeout(t *testing.T) {
	skipOnWindows(t)
	tool := &ShellTool{}

	out, err := tool.Call(context.Background(), `{"command":"sleep 2","timeout":0.2}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !strings.Contains(out, "timed out") {
		t.Fatalf("expected timeout message, got %q", out)
	}
}

func TestShellToolForegroundCancellationReturnsContextError(t *testing.T) {
	skipOnWindows(t)
	tool := &ShellTool{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tool.Call(ctx, `{"command":"sleep 1"}`)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Call error = %v, want context.Canceled", err)
	}
}

func TestShellToolMissingCommand(t *testing.T) {
	tool := &ShellTool{}

	out, err := tool.Call(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !strings.Contains(out, "Missing required parameter: command") {
		t.Fatalf("expected missing command error, got %q", out)
	}
}

func TestShellToolBackgroundLifecycle(t *testing.T) {
	skipOnWindows(t)
	tool := &ShellTool{}

	startOut, err := tool.Call(context.Background(), `{"action":"start","command":"sh -c 'for i in 1 2 3; do echo line-$i; done; sleep 5'"}`)
	if err != nil {
		t.Fatalf("start returned error: %v", err)
	}

	var started struct {
		SessionID string `json:"session_id"`
		Running   bool   `json:"running"`
		PID       int    `json:"pid"`
	}
	if err := json.Unmarshal([]byte(startOut), &started); err != nil {
		t.Fatalf("start output = %q, unmarshal err = %v", startOut, err)
	}
	if started.SessionID == "" {
		t.Fatalf("start output missing session_id: %q", startOut)
	}
	if !started.Running {
		t.Fatalf("expected running=true, got %q", startOut)
	}

	defer func() {
		stopArgs, _ := json.Marshal(map[string]interface{}{
			"action":     "stop",
			"session_id": started.SessionID,
		})
		_, _ = tool.Call(context.Background(), string(stopArgs))
	}()

	// Poll a few times until we see all three lines or run out of attempts.
	var combined string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pollArgs, _ := json.Marshal(map[string]interface{}{
			"action":     "poll",
			"session_id": started.SessionID,
		})
		pollOut, err := tool.Call(context.Background(), string(pollArgs))
		if err != nil {
			t.Fatalf("poll returned error: %v", err)
		}

		var poll struct {
			SessionID string `json:"session_id"`
			Running   bool   `json:"running"`
			Output    string `json:"output"`
		}
		if err := json.Unmarshal([]byte(pollOut), &poll); err != nil {
			t.Fatalf("poll output = %q, unmarshal err = %v", pollOut, err)
		}
		if poll.SessionID != started.SessionID {
			t.Fatalf("session_id mismatch: got %q want %q", poll.SessionID, started.SessionID)
		}
		combined += poll.Output
		if strings.Contains(combined, "line-1") &&
			strings.Contains(combined, "line-2") &&
			strings.Contains(combined, "line-3") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !strings.Contains(combined, "line-1") ||
		!strings.Contains(combined, "line-2") ||
		!strings.Contains(combined, "line-3") {
		t.Fatalf("expected all three lines, got %q", combined)
	}

	stopArgs, _ := json.Marshal(map[string]interface{}{
		"action":     "stop",
		"session_id": started.SessionID,
	})
	stopOut, err := tool.Call(context.Background(), string(stopArgs))
	if err != nil {
		t.Fatalf("stop returned error: %v", err)
	}

	var stopped struct {
		Stopped bool `json:"stopped"`
	}
	if err := json.Unmarshal([]byte(stopOut), &stopped); err != nil {
		t.Fatalf("stop output = %q, unmarshal err = %v", stopOut, err)
	}
	if !stopped.Stopped {
		t.Fatalf("stop output not marked stopped: %q", stopOut)
	}

	// Polling a stopped session should report it gone.
	pollArgs, _ := json.Marshal(map[string]interface{}{
		"action":     "poll",
		"session_id": started.SessionID,
	})
	missingOut, err := tool.Call(context.Background(), string(pollArgs))
	if err != nil {
		t.Fatalf("post-stop poll returned error: %v", err)
	}
	if !strings.Contains(missingOut, "shell session not found") {
		t.Fatalf("expected session-not-found error, got %q", missingOut)
	}
}

func TestShellToolBackgroundSubmitInput(t *testing.T) {
	skipOnWindows(t)
	tool := &ShellTool{}

	startOut, err := tool.Call(context.Background(), `{"action":"start","command":"cat"}`)
	if err != nil {
		t.Fatalf("start returned error: %v", err)
	}
	var started struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(startOut), &started); err != nil {
		t.Fatalf("start unmarshal err: %v", err)
	}
	defer func() {
		stopArgs, _ := json.Marshal(map[string]interface{}{
			"action":     "stop",
			"session_id": started.SessionID,
		})
		_, _ = tool.Call(context.Background(), string(stopArgs))
	}()

	submitArgs, _ := json.Marshal(map[string]interface{}{
		"action":     "submit",
		"session_id": started.SessionID,
		"input":      "ping",
	})
	if _, err := tool.Call(context.Background(), string(submitArgs)); err != nil {
		t.Fatalf("submit returned error: %v", err)
	}

	pollArgs, _ := json.Marshal(map[string]interface{}{
		"action":     "poll",
		"session_id": started.SessionID,
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pollOut, err := tool.Call(context.Background(), string(pollArgs))
		if err != nil {
			t.Fatalf("poll returned error: %v", err)
		}
		var poll struct {
			Output string `json:"output"`
		}
		if err := json.Unmarshal([]byte(pollOut), &poll); err != nil {
			t.Fatalf("poll unmarshal err: %v", err)
		}
		if strings.Contains(poll.Output, "ping") {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("did not see %q echoed back from cat session", "ping")
}

func TestShellToolUnknownAction(t *testing.T) {
	tool := &ShellTool{}
	ctx, _ := WithToolError(context.Background())

	out, err := tool.Call(ctx, `{"action":"bogus"}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !strings.Contains(out, "invalid action: bogus") {
		t.Fatalf("expected invalid-action error, got %q", out)
	}
	if got := ToolErrorFromContext(ctx); got == nil || got.Code != CodeInvalidArguments || got.Message != out {
		t.Fatalf("ToolError = %+v, want invalid_arguments with output message", got)
	}
}

func TestShellRingBufferTruncates(t *testing.T) {
	rb := newShellRingBuffer(8)
	rb.write([]byte("abcdefgh"))
	rb.write([]byte("ij"))

	out, truncated := rb.readUnread(100)
	if out != "cdefghij" {
		t.Fatalf("readUnread = %q, want %q", out, "cdefghij")
	}
	if !truncated {
		t.Fatalf("expected truncated=true after overflow")
	}
}

func TestShellRingBufferHasUnread(t *testing.T) {
	rb := newShellRingBuffer(16)
	if rb.hasUnread() {
		t.Fatalf("hasUnread() = true on empty buffer")
	}
	rb.write([]byte("abcdef"))
	if !rb.hasUnread() {
		t.Fatalf("hasUnread() = false after write")
	}
	if _, _ = rb.readUnread(3); !rb.hasUnread() {
		t.Fatalf("hasUnread() = false with bytes still pending")
	}
	if _, _ = rb.readUnread(100); rb.hasUnread() {
		t.Fatalf("hasUnread() = true after full drain")
	}
}

func TestShellToolBackgroundPollDrainsAndKeepsSession(t *testing.T) {
	skipOnWindows(t)
	for _, usePTY := range []bool{false, true} {
		t.Run(fmt.Sprintf("pty=%t", usePTY), func(t *testing.T) {
			tool := &ShellTool{}
			startArgs, _ := json.Marshal(map[string]interface{}{
				"action":  "start",
				"command": "sh -c 'printf %50000s X; printf stdout-final; printf stderr-final >&2'",
				"pty":     usePTY,
			})
			startOut, err := tool.Call(context.Background(), string(startArgs))
			if err != nil {
				t.Fatalf("start returned error: %v", err)
			}
			var started struct {
				SessionID string `json:"session_id"`
				Completed bool   `json:"completed"`
			}
			if err := json.Unmarshal([]byte(startOut), &started); err != nil {
				t.Fatalf("start unmarshal err: %v", err)
			}
			if started.Completed {
				t.Fatalf("start reported completed=true: %s", startOut)
			}
			defer globalShellSessionManager.delete(started.SessionID)

			session := globalShellSessionManager.get(started.SessionID)
			if session == nil {
				t.Fatalf("session %q not found after start", started.SessionID)
			}
			select {
			case <-session.done:
			case <-time.After(5 * time.Second):
				t.Fatal("background command did not finish")
			}
			// wait() records the reap the moment Wait returns, before the PTY
			// capture goroutine drains and done closes. stop() depends on it to
			// avoid signalling a PID the OS may already have reused.
			if !session.reaped {
				t.Fatal("wait() did not record the process reap")
			}

			pollArgs, _ := json.Marshal(map[string]interface{}{
				"action":     "poll",
				"session_id": started.SessionID,
			})
			type shellPoll struct {
				SessionID       string `json:"session_id"`
				Running         bool   `json:"running"`
				Completed       bool   `json:"completed"`
				FinishedAt      string `json:"finished_at"`
				Output          string `json:"output"`
				OutputTruncated bool   `json:"output_truncated"`
			}
			var collected strings.Builder
			var last shellPoll
			firstPoll := true
			firstPollSawTruncation := false
			for i := 0; i < 12; i++ {
				pollOut, err := tool.Call(context.Background(), string(pollArgs))
				if err != nil {
					t.Fatalf("poll returned error: %v", err)
				}
				if err := json.Unmarshal([]byte(pollOut), &last); err != nil {
					t.Fatalf("poll unmarshal err: %v (raw=%q)", err, pollOut)
				}
				if last.Running {
					t.Fatalf("session still running after done closed: %s", pollOut)
				}
				if firstPoll {
					firstPollSawTruncation = last.OutputTruncated
					firstPoll = false
				}
				collected.WriteString(last.Output)
				if last.Output == "" {
					break
				}
			}

			if !firstPollSawTruncation {
				t.Fatalf("expected first post-exit poll to be truncated")
			}
			if collected.Len() < 50000 {
				t.Fatalf("collected %d bytes, want at least 50000", collected.Len())
			}
			if !strings.Contains(collected.String(), "stdout-final") || !strings.Contains(collected.String(), "stderr-final") {
				t.Fatalf("final stdout/stderr missing from %d captured bytes", collected.Len())
			}
			// Draining the last bytes must not drop the session: the follow-up poll
			// reports the terminal state instead of "session not found".
			if !last.Completed || last.FinishedAt == "" {
				t.Fatalf("drained poll did not report completion: %+v", last)
			}
			if last.Output != "" {
				t.Fatalf("drained poll repeated buffered output: %q", last.Output)
			}
			if globalShellSessionManager.get(started.SessionID) == nil {
				t.Fatalf("session %q dropped after drain", started.SessionID)
			}
		})
	}
}

// A finished, fully drained session used to be deleted, so the next poll answered
// "shell session not found" — indistinguishable from a bogus session id, and it
// invited the model to start a replacement session. The session now stays
// pollable and reports its terminal state instead.
func TestShellToolPollAfterCompletionIsIdempotent(t *testing.T) {
	skipOnWindows(t)
	tool := &ShellTool{}
	startArgs, _ := json.Marshal(map[string]interface{}{
		"action":  "start",
		"command": `printf '23 24 25\nsum: 72\n'`,
	})
	startOut, err := tool.Call(context.Background(), string(startArgs))
	if err != nil {
		t.Fatalf("start returned error: %v", err)
	}
	var started struct {
		SessionID string `json:"session_id"`
		Completed bool   `json:"completed"`
	}
	if err := json.Unmarshal([]byte(startOut), &started); err != nil {
		t.Fatalf("start unmarshal err: %v", err)
	}
	if started.SessionID == "" {
		t.Fatalf("start returned no session_id: %s", startOut)
	}
	if started.Completed {
		t.Fatalf("start reported completed=true: %s", startOut)
	}
	defer globalShellSessionManager.delete(started.SessionID)

	pollArgs, _ := json.Marshal(map[string]interface{}{
		"action":     "poll",
		"session_id": started.SessionID,
	})
	type shellPoll struct {
		SessionID  string `json:"session_id"`
		Running    bool   `json:"running"`
		Completed  bool   `json:"completed"`
		FinishedAt string `json:"finished_at"`
		ExitCode   *int   `json:"exit_code"`
		Output     string `json:"output"`
	}

	var first shellPoll
	deadline := time.Now().Add(5 * time.Second)
	for {
		pollOut, err := tool.Call(context.Background(), string(pollArgs))
		if err != nil {
			t.Fatalf("poll returned error: %v", err)
		}
		if err := json.Unmarshal([]byte(pollOut), &first); err != nil {
			t.Fatalf("poll unmarshal err: %v (raw=%q)", err, pollOut)
		}
		if first.Completed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background command never reported completion: %+v", first)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if first.ExitCode == nil || *first.ExitCode != 0 {
		t.Fatalf("poll exit_code = %v, want 0", first.ExitCode)
	}
	if !strings.Contains(first.Output, "sum: 72") {
		t.Fatalf("poll output = %q, want the command result", first.Output)
	}
	if first.FinishedAt == "" {
		t.Fatalf("completed poll reported no finished_at: %+v", first)
	}

	// Repeating the poll must keep answering, not report the session gone.
	pollOut, err := tool.Call(context.Background(), string(pollArgs))
	if err != nil {
		t.Fatalf("repeat poll returned error: %v", err)
	}
	var second shellPoll
	if err := json.Unmarshal([]byte(pollOut), &second); err != nil {
		t.Fatalf("repeat poll is not a session payload: %q", pollOut)
	}
	if !second.Completed || second.Running {
		t.Fatalf("repeat poll lost the terminal state: %+v", second)
	}
	if second.SessionID != started.SessionID {
		t.Fatalf("repeat poll session_id = %q, want %q", second.SessionID, started.SessionID)
	}
	if second.Output != "" {
		t.Fatalf("repeat poll repeated already-delivered output: %q", second.Output)
	}
}

// stop() must not signal a process that Wait() has already reaped: the OS may
// have reused the PID, and the escalation path is a raw process-group kill.
//
// done stays open here, so a stop() that signals the process cannot return until
// its interrupt timeout expires. A prompt return is therefore what distinguishes
// "skipped the reap-aware check" from "signalled anyway"; asserting on the helper
// still being alive does not work, because an unreaped child is a zombie and
// signalling a zombie succeeds.
func TestShellSessionStopSkipsReapedProcess(t *testing.T) {
	skipOnWindows(t)
	cmd := exec.Command("sleep", "30")
	shellSetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	session := &shellSession{
		id:     "shell-reaped",
		cmd:    cmd,
		done:   make(chan struct{}),
		reaped: true,
	}
	started := time.Now()
	session.stop()
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stop() took %v on a reaped session; it must not signal one", elapsed)
	}
}

func TestShellKeySequence(t *testing.T) {
	cases := map[string]string{
		"enter":  "\n",
		"tab":    "\t",
		"ctrl+c": "\x03",
		"up":     "\x1b[A",
	}
	for key, want := range cases {
		got, err := shellKeySequence(key)
		if err != nil {
			t.Fatalf("shellKeySequence(%q) error: %v", key, err)
		}
		if got != want {
			t.Fatalf("shellKeySequence(%q) = %q, want %q", key, got, want)
		}
	}
	if _, err := shellKeySequence("nope"); err == nil {
		t.Fatalf("expected error for unknown key")
	}
}
