package agent

import (
	"aiden-agent/internal/logging"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	gopty "github.com/aymanbagabas/go-pty"
)

const (
	shellDefaultTimeoutSeconds = 30.0
	shellMaxTimeoutSeconds     = 300.0
	shellDefaultPollBytes      = 12000
	shellMaxPollBytes          = 100000
	shellDefaultPTYRows        = 40
	shellDefaultPTYCols        = 120
)

type ShellTool struct {
	execution shellExecutionConfig
}

type shellExecutionConfig struct {
	proxy              ProxyConfig
	temporaryDirectory string
}

func (t *ShellTool) Name() string { return "shell" }

func (t *ShellTool) Description() string {
	return `Execute a shell command or manage a running shell session on the Aiden hardware controller. ` +
		`For one-shot commands, pass "command" and read the returned output. Shell can also inspect the controller's precise clock or timezone and perform deterministic calculations with available command-line utilities; these results describe the controller, not the target device shown in screenshots. ` +
		`Original phone-notification records are stored as date-sharded JSONL files under "/userdata/agent/memory/notifications/events/". Each UTC "YYYY-MM-DD.jsonl" file contains one record per line; inspect these files with normal read-only shell utilities and do not modify them. ` +
		`For interactive or long-running programs, set "background" (or an "action") to start a session that returns a session_id, then drive it with follow-up "action" calls ("poll", "write", "submit", "send_keys", "resize", "stop") carrying that session_id.`
}

func (t *ShellTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"command":    stringArgSchema("Shell command to run for foreground execution or background start. Required unless driving an existing session."),
		"timeout":    numberArgSchema("Execution timeout in seconds (default 30, max 300)."),
		"workdir":    stringArgSchema("Working directory for the command (default: current directory)."),
		"background": boolArgSchema("Start a long-running shell session and return a session_id immediately."),
		"pty":        boolArgSchema("Run the command in a pseudo-terminal."),
		"action":     stringEnumArgSchema("Shell session lifecycle action.", "start", "poll", "write", "submit", "send_keys", "resize", "stop"),
		"session_id": stringArgSchema("Running shell session id returned by a background start."),
		"input":      stringArgSchema("Text to write for write or submit actions."),
		"keys":       stringArrayArgSchema(`Key sequence names for send_keys, e.g. "enter", "ctrl+c", or "tab".`),
		"rows":       minIntegerArgSchema("PTY row count for start or resize.", 1),
		"cols":       minIntegerArgSchema("PTY column count for start or resize.", 1),
		"bytes":      rangedIntegerArgSchema("Maximum output bytes to return for poll (default 12000).", 1, shellMaxPollBytes),
	})
}

func (t *ShellTool) Call(ctx context.Context, input string) (string, error) {
	arguments := map[string]interface{}{}
	trimmed := strings.TrimSpace(input)
	if trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &arguments); err != nil {
			return toolErrorResultf(ctx, CodeInvalidArguments, "invalid input: %v", err), nil
		}
	}

	_, hasAction := arguments["action"]
	if shellBoolArg(arguments, "background") || hasAction {
		out, err := shellExecuteBackground(ctx, arguments, t.execution)
		if err != nil {
			return out, err
		}
		return out, nil
	}

	command, ok := arguments["command"].(string)
	if !ok || strings.TrimSpace(command) == "" {
		return toolErrorResultString(ctx, CodeInvalidArguments, "Missing required parameter: command"), nil
	}

	timeoutSecs := shellDefaultTimeoutSeconds
	if v, ok := arguments["timeout"].(float64); ok && v > 0 {
		timeoutSecs = v
		if timeoutSecs > shellMaxTimeoutSeconds {
			timeoutSecs = shellMaxTimeoutSeconds
		}
	}

	workdir := shellStringArg(arguments, "workdir", "")
	usePTY := shellBoolArg(arguments, "pty")

	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSecs*float64(time.Second)))
	defer cancel()

	result, runErr := shellRunForeground(execCtx, command, workdir, usePTY, t.execution)
	if contextErr := contextError(execCtx, runErr); errors.Is(contextErr, context.Canceled) {
		return "", contextErr
	}
	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		return toolErrorResultf(ctx, CodeDeadlineExceeded, "Error: command timed out after %.0f seconds", timeoutSecs), nil
	}
	if runErr != nil {
		return toolErrorResultString(ctx, CodeToolExecutionFailed, result), nil
	}
	return result, nil
}

func shellExecuteBackground(ctx context.Context, arguments map[string]interface{}, execution shellExecutionConfig) (string, error) {
	action := shellStringArg(arguments, "action", "start")
	switch action {
	case "start":
		return shellStartBackground(ctx, arguments, execution)
	case "poll":
		return shellPollBackground(ctx, arguments)
	case "write":
		return shellWriteBackground(ctx, arguments)
	case "submit":
		return shellSubmitBackground(ctx, arguments)
	case "send_keys":
		return shellSendKeysBackground(ctx, arguments)
	case "resize":
		return shellResizeBackground(ctx, arguments)
	case "stop":
		return shellStopBackground(ctx, arguments)
	default:
		return toolErrorResultf(ctx, CodeInvalidArguments, "invalid action: %s", action), nil
	}
}

func shellStartBackground(ctx context.Context, arguments map[string]interface{}, execution shellExecutionConfig) (string, error) {
	command := shellStringArg(arguments, "command", "")
	if command == "" {
		return toolErrorResultString(ctx, CodeInvalidArguments, "Missing required parameter: command"), nil
	}

	workdir := shellStringArg(arguments, "workdir", "")
	usePTY := shellBoolArg(arguments, "pty")

	var sessionCtx context.Context
	var cancel context.CancelFunc
	if v, ok := arguments["timeout"].(float64); ok && v > 0 {
		timeoutSecs := v
		if timeoutSecs > shellMaxTimeoutSeconds {
			timeoutSecs = shellMaxTimeoutSeconds
		}
		sessionCtx, cancel = context.WithTimeout(context.Background(), time.Duration(timeoutSecs*float64(time.Second)))
	} else {
		sessionCtx, cancel = context.WithCancel(context.Background())
	}

	cmd, err := shellBuildCommand(sessionCtx, command, execution)
	if err != nil {
		cancel()
		return toolErrorResultString(ctx, CodeToolExecutionFailed, err.Error()), nil
	}

	session := &shellSession{
		id:        globalShellSessionManager.nextSessionID(),
		command:   command,
		workdir:   workdir,
		usePTY:    usePTY,
		cmd:       cmd,
		output:    newShellRingBuffer(shellMaxPollBytes * 2),
		done:      make(chan struct{}),
		cancel:    cancel,
		startedAt: time.Now(),
	}

	if usePTY {
		if err = shellStartPTYBackground(sessionCtx, session, arguments, execution); err != nil {
			cancel()
			return toolErrorResultString(ctx, CodeToolExecutionFailed, err.Error()), nil
		}
	} else {
		if workdir != "" {
			cmd.Dir = workdir
		}
		stdin, pipeErr := cmd.StdinPipe()
		if pipeErr != nil {
			cancel()
			return toolErrorResultString(ctx, CodeToolExecutionFailed, pipeErr.Error()), nil
		}
		// Give os/exec writers instead of StdoutPipe/StderrPipe. When a command
		// exits, Cmd.Wait waits for these internal copy goroutines to finish,
		// so the session cannot be reported as done before all output reaches
		// the ring buffer.
		cmd.Stdout = shellSessionOutputWriter{output: session.output}
		cmd.Stderr = shellSessionOutputWriter{output: session.output}

		if pipeErr = cmd.Start(); pipeErr != nil {
			cancel()
			_ = stdin.Close()
			return toolErrorResultString(ctx, CodeToolExecutionFailed, pipeErr.Error()), nil
		}

		session.stdin = stdin
	}

	go session.wait()
	globalShellSessionManager.put(session)

	return shellJSONString(map[string]interface{}{
		"session_id": session.id,
		"running":    true,
		"completed":  false,
		"pid":        session.processID(),
		"command":    session.command,
		"workdir":    session.workdir,
		"pty":        session.usePTY,
		"started_at": session.startedAt.Format(time.RFC3339),
		"message":    "Background shell session started. Use action=poll to read output, action=write to send input, and action=stop to terminate it.",
	}), nil
}

func shellPollBackground(ctx context.Context, arguments map[string]interface{}) (string, error) {
	sessionID := shellStringArg(arguments, "session_id", "")
	if sessionID == "" {
		return toolErrorResultString(ctx, CodeInvalidArguments, "Missing required parameter: session_id"), nil
	}
	session := globalShellSessionManager.get(sessionID)
	if session == nil {
		return toolErrorResultf(ctx, CodeInvalidArguments, "shell session not found: %s", sessionID), nil
	}

	maxBytes := shellIntArg(arguments, "bytes", shellDefaultPollBytes)
	if maxBytes <= 0 {
		maxBytes = shellDefaultPollBytes
	}
	if maxBytes > shellMaxPollBytes {
		maxBytes = shellMaxPollBytes
	}

	output, truncated := session.output.readUnread(maxBytes)
	running := session.isRunning()
	payload := map[string]interface{}{
		"session_id":       session.id,
		"running":          running,
		"completed":        !running,
		"pid":              session.processID(),
		"command":          session.command,
		"workdir":          session.workdir,
		"pty":              session.usePTY,
		"output":           output,
		"output_truncated": truncated,
		"started_at":       session.startedAt.Format(time.RFC3339),
		"exit_code":        session.exitCodeValue(),
		"exit_error":       session.exitErrorText(),
	}
	if finished := session.finishedAtOr(); !finished.IsZero() {
		payload["finished_at"] = finished.Format(time.RFC3339)
	}

	if !running && !session.output.hasUnread() {
		// Keep the session so a repeated poll answers with this terminal payload
		// instead of "session not found". Drop the bytes instead: every produced
		// byte has been delivered, so nothing is lost, and the record stays small
		// until evictIdle reclaims it on a fixed clock.
		session.output.release()
	}
	return shellJSONString(payload), nil
}

func shellWriteBackground(ctx context.Context, arguments map[string]interface{}) (string, error) {
	sessionID := shellStringArg(arguments, "session_id", "")
	if sessionID == "" {
		return toolErrorResultString(ctx, CodeInvalidArguments, "Missing required parameter: session_id"), nil
	}
	session := globalShellSessionManager.get(sessionID)
	if session == nil {
		return toolErrorResultf(ctx, CodeInvalidArguments, "shell session not found: %s", sessionID), nil
	}
	if !session.isRunning() {
		return toolErrorResultString(ctx, CodeToolExecutionFailed, "shell session is no longer running"), nil
	}

	input := shellStringArg(arguments, "input", "")
	if input == "" {
		return toolErrorResultString(ctx, CodeInvalidArguments, "Missing required parameter: input"), nil
	}

	n, err := session.writeString(input)
	if err != nil {
		return toolErrorResultString(ctx, CodeToolExecutionFailed, err.Error()), nil
	}
	return shellJSONString(map[string]interface{}{
		"session_id":    session.id,
		"written_bytes": n,
		"message":       "Input written to shell session.",
	}), nil
}

func shellSubmitBackground(ctx context.Context, arguments map[string]interface{}) (string, error) {
	input := shellStringArg(arguments, "input", "")
	if input == "" {
		return shellWriteBackground(ctx, map[string]interface{}{
			"session_id": arguments["session_id"],
			"input":      "\n",
		})
	}
	return shellWriteBackground(ctx, map[string]interface{}{
		"session_id": arguments["session_id"],
		"input":      input + "\n",
	})
}

func shellSendKeysBackground(ctx context.Context, arguments map[string]interface{}) (string, error) {
	sessionID := shellStringArg(arguments, "session_id", "")
	if sessionID == "" {
		return toolErrorResultString(ctx, CodeInvalidArguments, "Missing required parameter: session_id"), nil
	}
	session := globalShellSessionManager.get(sessionID)
	if session == nil {
		return toolErrorResultf(ctx, CodeInvalidArguments, "shell session not found: %s", sessionID), nil
	}
	if !session.isRunning() {
		return toolErrorResultString(ctx, CodeToolExecutionFailed, "shell session is no longer running"), nil
	}

	keys := shellStringSliceArg(arguments, "keys")
	if len(keys) == 0 {
		return toolErrorResultString(ctx, CodeInvalidArguments, "Missing required parameter: keys"), nil
	}

	var seq strings.Builder
	for _, key := range keys {
		part, err := shellKeySequence(key)
		if err != nil {
			return toolErrorResultString(ctx, CodeInvalidArguments, err.Error()), nil
		}
		seq.WriteString(part)
	}

	n, err := session.writeString(seq.String())
	if err != nil {
		return toolErrorResultString(ctx, CodeToolExecutionFailed, err.Error()), nil
	}
	return shellJSONString(map[string]interface{}{
		"session_id":    session.id,
		"written_bytes": n,
		"message":       "Key sequence written to shell session.",
	}), nil
}

func shellResizeBackground(ctx context.Context, arguments map[string]interface{}) (string, error) {
	sessionID := shellStringArg(arguments, "session_id", "")
	if sessionID == "" {
		return toolErrorResultString(ctx, CodeInvalidArguments, "Missing required parameter: session_id"), nil
	}
	session := globalShellSessionManager.get(sessionID)
	if session == nil {
		return toolErrorResultf(ctx, CodeInvalidArguments, "shell session not found: %s", sessionID), nil
	}
	if !session.usePTY || session.pty == nil {
		return toolErrorResultString(ctx, CodeInvalidArguments, "shell session is not running in pty mode"), nil
	}

	cols := shellPTYCols(arguments)
	rows := shellPTYRows(arguments)
	if err := shellResizePTY(session, cols, rows); err != nil {
		return toolErrorResultString(ctx, CodeToolExecutionFailed, err.Error()), nil
	}
	return shellJSONString(map[string]interface{}{
		"session_id": session.id,
		"pty":        true,
		"cols":       cols,
		"rows":       rows,
		"message":    "Shell PTY resized.",
	}), nil
}

func shellStopBackground(ctx context.Context, arguments map[string]interface{}) (string, error) {
	sessionID := shellStringArg(arguments, "session_id", "")
	if sessionID == "" {
		return toolErrorResultString(ctx, CodeInvalidArguments, "Missing required parameter: session_id"), nil
	}
	session := globalShellSessionManager.get(sessionID)
	if session == nil {
		return toolErrorResultf(ctx, CodeInvalidArguments, "shell session not found: %s", sessionID), nil
	}

	session.stop()
	globalShellSessionManager.delete(session.id)

	return shellJSONString(map[string]interface{}{
		"session_id": session.id,
		"stopped":    true,
		"running":    false,
		"message":    "Shell session stopped.",
	}), nil
}

func shellBuildCommand(ctx context.Context, command string, execution shellExecutionConfig) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, shellPlatformShell(), shellPlatformShellArg(), command)
	cmd.Env = shellCommandEnv(false, execution)
	shellSetProcessGroup(cmd)
	return cmd, nil
}

func shellBuildPtyCommand(ctx context.Context, command string, execution shellExecutionConfig) (gopty.Pty, *gopty.Cmd, error) {
	ptmx, err := gopty.New()
	if err != nil {
		return nil, nil, err
	}
	cmd := ptmx.CommandContext(ctx, shellPlatformShell(), shellPlatformShellArg(), command)
	cmd.Env = shellCommandEnv(true, execution)
	// go-pty starts Unix commands with Setsid and Setctty. Do not also request
	// Setpgid: a session leader cannot move process groups, so exec would fail
	// with EPERM. Setsid already gives the PTY command its own process group for
	// shellKillProcessGroup.
	return ptmx, cmd, nil
}

func shellRunForeground(ctx context.Context, command string, workdir string, usePTY bool, execution shellExecutionConfig) (string, error) {
	if !usePTY {
		return shellRunForegroundCmd(ctx, command, workdir, execution)
	}
	return shellRunForegroundPTY(ctx, command, workdir, execution)
}

func shellRunForegroundCmd(ctx context.Context, command string, workdir string, execution shellExecutionConfig) (string, error) {
	cmd := exec.Command(shellPlatformShell(), shellPlatformShellArg(), command)
	cmd.Env = shellCommandEnv(false, execution)
	shellSetProcessGroup(cmd)
	if workdir != "" {
		cmd.Dir = workdir
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if startErr := cmd.Start(); startErr != nil {
		return fmt.Sprintf("Error: %s", startErr.Error()), startErr
	}

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	select {
	case runErr := <-waitDone:
		stdoutStr := stdout.String()
		stderrStr := stderr.String()
		if runErr != nil {
			var parts []string
			parts = append(parts, fmt.Sprintf("Error: %s", runErr.Error()))
			if stderrStr != "" {
				parts = append(parts, fmt.Sprintf("Stderr:\n%s", stderrStr))
			}
			if stdoutStr != "" {
				parts = append(parts, fmt.Sprintf("Stdout:\n%s", stdoutStr))
			}
			return strings.Join(parts, "\n"), runErr
		}
		result := stdoutStr
		if stderrStr != "" {
			if result != "" {
				result = fmt.Sprintf("%s\nStderr:\n%s", result, stderrStr)
			} else {
				result = fmt.Sprintf("Stderr:\n%s", stderrStr)
			}
		}
		if result == "" {
			result = "(no output)"
		}
		return result, nil

	case <-ctx.Done():
		if killErr := shellKillProcessGroup(cmd.Process); killErr != nil {
			logging.Warnf("agent", "tools_shell", "shell: kill process group failed: %v", killErr)
		}
		<-waitDone
		return "", ctx.Err()
	}
}

func shellRunForegroundPTY(ctx context.Context, command string, workdir string, execution shellExecutionConfig) (string, error) {
	ptmx, ptyCmd, err := shellBuildPtyCommand(ctx, command, execution)
	if err != nil {
		return fmt.Sprintf("Error: %s", err.Error()), err
	}
	if workdir != "" {
		ptyCmd.Dir = workdir
	}
	if sizeErr := shellResizePtyRaw(ptmx, int(shellDefaultPTYCols), int(shellDefaultPTYRows)); sizeErr != nil {
		_ = ptmx.Close()
		return fmt.Sprintf("Error: %s", sizeErr.Error()), sizeErr
	}
	if err = ptyCmd.Start(); err != nil {
		_ = ptmx.Close()
		return fmt.Sprintf("Error: %s", err.Error()), err
	}

	var output bytes.Buffer
	copyDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(&output, ptmx)
		copyDone <- shellPTYCopyError(copyErr)
	}()

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- ptyCmd.Wait()
	}()

	select {
	case runErr := <-waitDone:
		_ = ptmx.Close()
		copyErr := <-copyDone
		if copyErr != nil && runErr == nil {
			runErr = copyErr
		}
		text := output.String()
		if text == "" {
			text = "(no output)"
		}
		if runErr != nil {
			return fmt.Sprintf("Error: %s\nOutput:\n%s", runErr.Error(), text), runErr
		}
		return text, nil

	case <-ctx.Done():
		if killErr := shellKillProcessGroup(ptyCmd.Process); killErr != nil {
			logging.Warnf("agent", "tools_shell", "shell: kill pty process group failed: %v", killErr)
		}
		_ = ptmx.Close()
		<-waitDone
		<-copyDone
		return "", ctx.Err()
	}
}

func shellStartPTYBackground(ctx context.Context, session *shellSession, arguments map[string]interface{}, execution shellExecutionConfig) error {
	ptmx, ptyCmd, err := shellBuildPtyCommand(ctx, session.command, execution)
	if err != nil {
		return err
	}
	if session.workdir != "" {
		ptyCmd.Dir = session.workdir
	}
	if sizeErr := shellResizePtyRaw(ptmx, int(shellPTYCols(arguments)), int(shellPTYRows(arguments))); sizeErr != nil {
		_ = ptmx.Close()
		return sizeErr
	}
	if err = ptyCmd.Start(); err != nil {
		_ = ptmx.Close()
		return err
	}
	// The child holds its own slave fds now. Drop ours so the master reports
	// EIO once the child exits instead of blocking until the drain timeout.
	if unixPty, ok := ptmx.(gopty.UnixPty); ok {
		_ = unixPty.Slave().Close()
	}

	session.pty = ptmx
	session.ptyCmd = ptyCmd
	session.stdin = ptmx
	session.captureDone = make(chan struct{})
	go func() {
		defer close(session.captureDone)
		session.capture(ptmx)
	}()
	return nil
}

func shellResizePTY(session *shellSession, cols uint16, rows uint16) error {
	if session.pty == nil {
		return fmt.Errorf("pty is not available for this shell session")
	}
	return shellResizePtyRaw(session.pty, int(cols), int(rows))
}

func shellResizePtyRaw(ptmx gopty.Pty, cols int, rows int) error {
	return ptmx.Resize(cols, rows)
}

func shellStringArg(arguments map[string]interface{}, key string, defaultValue string) string {
	value, ok := arguments[key].(string)
	if !ok {
		return defaultValue
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultValue
	}
	return value
}

func shellBoolArg(arguments map[string]interface{}, key string) bool {
	value, ok := arguments[key].(bool)
	return ok && value
}

func shellIntArg(arguments map[string]interface{}, key string, defaultValue int) int {
	value, ok := arguments[key]
	if !ok {
		return defaultValue
	}
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return defaultValue
	}
}

func shellHasAction(arguments map[string]interface{}) bool {
	action, ok := arguments["action"].(string)
	if !ok {
		return false
	}
	switch strings.TrimSpace(action) {
	case "start", "poll", "write", "submit", "send_keys", "resize", "stop":
		return true
	default:
		return false
	}
}

func shellStringSliceArg(arguments map[string]interface{}, key string) []string {
	value, ok := arguments[key]
	if !ok {
		return nil
	}
	raw, ok := value.([]interface{})
	if !ok {
		return nil
	}
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		text, ok := item.(string)
		if !ok {
			continue
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		result = append(result, text)
	}
	return result
}

func shellPTYRows(arguments map[string]interface{}) uint16 {
	rows := shellIntArg(arguments, "rows", shellDefaultPTYRows)
	if rows <= 0 {
		rows = shellDefaultPTYRows
	}
	return uint16(rows)
}

func shellPTYCols(arguments map[string]interface{}) uint16 {
	cols := shellIntArg(arguments, "cols", shellDefaultPTYCols)
	if cols <= 0 {
		cols = shellDefaultPTYCols
	}
	return uint16(cols)
}

func shellJSONString(v interface{}) string {
	bs, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("json marshal failed: %v", err)
	}
	return string(bs)
}
