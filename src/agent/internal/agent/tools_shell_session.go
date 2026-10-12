package agent

import (
	"aiden-agent/internal/logging"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	gopty "github.com/aymanbagabas/go-pty"
)

const (
	shellSessionIdleTimeout = 30 * time.Minute
	shellPTYDrainTimeout    = 2 * time.Second
)

type shellSession struct {
	id           string
	command      string
	workdir      string
	usePTY       bool
	cmd          *exec.Cmd
	ptyCmd       *gopty.Cmd
	stdin        io.WriteCloser
	pty          gopty.Pty
	output       *shellRingBuffer
	captureDone  chan struct{}
	done         chan struct{}
	cancel       context.CancelFunc
	startedAt    time.Time
	lastActivity time.Time
	finishedAt   time.Time
	exitErr      error
	exitCode     *int

	mu     sync.Mutex
	closed bool
	reaped bool
}

type shellRingBuffer struct {
	mu        sync.Mutex
	buf       []byte
	readPos   int
	maxBytes  int
	truncated bool
}

// shellSessionOutputWriter lets os/exec own the output-copy lifecycle. Cmd.Wait
// waits for writers like this one before closing the session's done channel.
type shellSessionOutputWriter struct {
	output *shellRingBuffer
}

func (w shellSessionOutputWriter) Write(chunk []byte) (int, error) {
	w.output.write(chunk)
	return len(chunk), nil
}

type shellSessionManager struct {
	mu          sync.Mutex
	sessions    map[string]*shellSession
	nextID      uint64
	cleanupOnce sync.Once
}

var globalShellSessionManager = &shellSessionManager{
	sessions: map[string]*shellSession{},
}

func newShellRingBuffer(maxBytes int) *shellRingBuffer {
	return &shellRingBuffer{
		buf:      make([]byte, 0, maxBytes),
		maxBytes: maxBytes,
	}
}

func (b *shellRingBuffer) write(chunk []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buf = append(b.buf, chunk...)
	if len(b.buf) <= b.maxBytes {
		return
	}

	overflow := len(b.buf) - b.maxBytes
	b.buf = append([]byte{}, b.buf[overflow:]...)
	b.truncated = true
	if b.readPos >= overflow {
		b.readPos -= overflow
	} else {
		b.readPos = 0
	}
}

func (b *shellRingBuffer) readUnread(limit int) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.readPos >= len(b.buf) {
		return "", false
	}
	end := len(b.buf)
	truncated := false
	if limit > 0 && b.readPos+limit < end {
		end = b.readPos + limit
		truncated = true
	}
	text := string(b.buf[b.readPos:end])
	b.readPos = end
	return text, truncated || b.truncated
}

func (b *shellRingBuffer) hasUnread() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.readPos < len(b.buf)
}

// release drops the buffered bytes once every produced byte has been read. A
// finished session keeps answering polls, but it no longer needs the output.
// Callers must only release after the process has exited, because a running
// process can still append; at that point nothing is lost.
func (b *shellRingBuffer) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = nil
	b.readPos = 0
	b.truncated = false
}

func (s *shellSession) capture(r io.Reader) {
	buffer := make([]byte, 4096)
	for {
		n, err := r.Read(buffer)
		if n > 0 {
			s.output.write(buffer[:n])
		}
		if err != nil {
			return
		}
	}
}

func (s *shellSession) wait() {
	defer close(s.done)
	var exitErr error
	var exitCode *int
	if s.usePTY && s.ptyCmd != nil {
		exitErr = s.ptyCmd.Wait()
		s.markReaped()
		if s.ptyCmd.ProcessState != nil {
			code := s.ptyCmd.ProcessState.ExitCode()
			exitCode = &code
		}
		// Let capture drain the PTY before closing the master: closing it
		// first discards output still buffered in the kernel. Once every
		// slave fd is closed the read returns EIO and capture ends on its own;
		// the timeout covers a background child that keeps the slave open.
		if s.captureDone != nil {
			select {
			case <-s.captureDone:
			case <-time.After(shellPTYDrainTimeout):
			}
		}
		s.closeInput()
		if s.captureDone != nil {
			<-s.captureDone
		}
		s.setExitState(exitErr, exitCode)
		return
	}

	exitErr = s.cmd.Wait()
	s.markReaped()
	if s.cmd.ProcessState != nil {
		code := s.cmd.ProcessState.ExitCode()
		exitCode = &code
	}
	s.setExitState(exitErr, exitCode)
	s.closeInput()
}

func (s *shellSession) setExitState(exitErr error, exitCode *int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exitErr = exitErr
	s.exitCode = exitCode
	s.finishedAt = time.Now()
}

// markReaped records that Wait() has returned, which is the point from which the
// OS may reuse the PID. It runs in the waiting goroutine immediately after Wait,
// and stop() reads it under the same lock, so a session whose process is already
// gone is never signalled. isRunning() is not a substitute: done closes only
// after the PTY capture goroutine has drained, which can be long after the
// process itself was reaped.
func (s *shellSession) markReaped() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reaped = true
}

func (s *shellSession) isRunning() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// finishedAtOr reports when the session's process exited, or the zero time
// while it is still running.
func (s *shellSession) finishedAtOr() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishedAt
}

func (s *shellSession) processID() int {
	if s.usePTY && s.ptyCmd != nil && s.ptyCmd.Process != nil {
		return s.ptyCmd.Process.Pid
	}
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

func (s *shellSession) exitCodeValue() interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exitCode == nil {
		return nil
	}
	return *s.exitCode
}

func (s *shellSession) exitErrorText() interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exitErr == nil {
		return nil
	}
	return s.exitErr.Error()
}

func (s *shellSession) stop() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true

	if s.cancel != nil {
		s.cancel()
	}
	s.closeInputLocked()
	done := s.done
	var process *os.Process
	// Decide whether to signal under the same lock Wait() takes to record the
	// reap. Signalling a reaped PID is what the process-group kill below must
	// never do: the PID may already belong to something else.
	if !s.reaped {
		if s.usePTY && s.ptyCmd != nil {
			process = s.ptyCmd.Process
		} else if s.cmd != nil {
			process = s.cmd.Process
		}
	}
	s.mu.Unlock()

	if process == nil {
		return
	}
	if err := process.Signal(os.Interrupt); err != nil {
		// Wait() may have reaped the process since the check above; Go reports
		// that as ErrProcessDone. Escalating to a process-group kill here would
		// target a PID that may already be reused.
		if errors.Is(err, os.ErrProcessDone) {
			return
		}
		if killErr := shellKillProcessGroup(process); killErr != nil {
			logging.Warnf("agent", "tools_shell_session", "shell: kill after failed interrupt: %v", killErr)
		}
		return
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		if killErr := shellKillProcessGroup(process); killErr != nil {
			logging.Warnf("agent", "tools_shell_session", "shell: kill after interrupt timeout: %v", killErr)
		}
	}
}

func (s *shellSession) closeInput() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeInputLocked()
}

func (s *shellSession) closeInputLocked() {
	if s.pty != nil {
		_ = s.pty.Close()
		s.pty = nil
		s.stdin = nil
		return
	}
	if s.stdin != nil {
		_ = s.stdin.Close()
		s.stdin = nil
	}
}

func (s *shellSession) writeString(input string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.stdin == nil {
		return 0, fmt.Errorf("shell session is no longer running")
	}
	return io.WriteString(s.stdin, input)
}

func (m *shellSessionManager) nextSessionID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	id := m.nextID
	return "shell-" + strconv.FormatUint(id, 10)
}

func (m *shellSessionManager) put(session *shellSession) {
	m.ensureCleanup()
	m.mu.Lock()
	defer m.mu.Unlock()
	session.lastActivity = time.Now()
	m.sessions[session.id] = session
}

func (m *shellSessionManager) get(id string) *shellSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[id]
	if session != nil {
		session.lastActivity = time.Now()
	}
	return session
}

func (m *shellSessionManager) delete(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

func (m *shellSessionManager) ensureCleanup() {
	m.cleanupOnce.Do(func() {
		go m.cleanupLoop()
	})
}

func (m *shellSessionManager) cleanupLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		m.evictIdle()
	}
}

func (m *shellSessionManager) evictIdle() {
	m.mu.Lock()
	var expired []*shellSession
	for _, session := range m.sessions {
		if time.Since(session.lastActivity) > shellSessionIdleTimeout {
			expired = append(expired, session)
		}
	}
	for _, session := range expired {
		delete(m.sessions, session.id)
	}
	m.mu.Unlock()

	for _, session := range expired {
		session.stop()
	}
}

func shellPlatformShell() string {
	if runtime.GOOS == "windows" {
		return "cmd.exe"
	}
	return "sh"
}

func shellPlatformShellArg() string {
	if runtime.GOOS == "windows" {
		return "/C"
	}
	return "-c"
}

func shellCommandEnv(usePTY bool, execution shellExecutionConfig) []string {
	env := os.Environ()
	env = shellApplyProxyEnv(env, execution.proxy)
	// PYTHONUSERBASE and pip flags are configured globally by /etc/profile.d/aiden-python.sh
	// and inherited from the parent environment. TMPDIR is injected command-scoped
	// to use /userdata/tmp for agent shell commands, avoiding the small /tmp tmpfs (73 MB)
	// while preventing storage wear from a global TMPDIR override.
	if execution.temporaryDirectory != "" {
		env = shellEnsureEnv(env, "TMPDIR", execution.temporaryDirectory)
	}
	if usePTY {
		env = shellEnsureEnv(env, "TERM", "dumb")
		env = shellEnsureEnv(env, "NO_COLOR", "1")
		env = shellEnsureEnv(env, "CLICOLOR", "0")
		env = shellEnsureEnv(env, "COLORTERM", "")
		env = shellEnsureEnv(env, "TERM_PROGRAM", "")
		env = shellEnsureEnv(env, "TERMINAL_EMULATOR", "")
		env = shellEnsureEnv(env, "PAGER", "cat")
	}
	return env
}

func shellApplyProxyEnv(env []string, proxy ProxyConfig) []string {
	proxy = proxy.WithDefaults()
	if !proxy.HasProxyURL() {
		return env
	}

	for _, key := range []string{
		"http_proxy", "https_proxy", "all_proxy", "no_proxy",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	} {
		env = shellRemoveEnv(env, key)
	}

	if value := strings.TrimSpace(proxy.HTTPProxy); value != "" {
		env = shellEnsureEnv(env, "http_proxy", value)
		env = shellEnsureEnv(env, "HTTP_PROXY", value)
	}
	if value := strings.TrimSpace(proxy.HTTPSProxy); value != "" {
		env = shellEnsureEnv(env, "https_proxy", value)
		env = shellEnsureEnv(env, "HTTPS_PROXY", value)
	}
	if value := strings.TrimSpace(proxy.AllProxy); value != "" {
		env = shellEnsureEnv(env, "all_proxy", value)
		env = shellEnsureEnv(env, "ALL_PROXY", value)
	}
	if value := strings.TrimSpace(proxy.NoProxy); value != "" {
		env = shellEnsureEnv(env, "no_proxy", value)
		env = shellEnsureEnv(env, "NO_PROXY", value)
	}
	return env
}

func shellRemoveEnv(env []string, key string) []string {
	prefix := key + "="
	filtered := env[:0]
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func shellEnsureEnv(env []string, key string, value string) []string {
	prefix := key + "="
	for i, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func shellKeySequence(key string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "enter", "return":
		return "\n", nil
	case "tab":
		return "\t", nil
	case "backspace":
		return "\b", nil
	case "escape", "esc":
		return "\x1b", nil
	case "space":
		return " ", nil
	case "ctrl+c":
		return "\x03", nil
	case "ctrl+d":
		return "\x04", nil
	case "ctrl+z":
		return "\x1a", nil
	case "up":
		return "\x1b[A", nil
	case "down":
		return "\x1b[B", nil
	case "right":
		return "\x1b[C", nil
	case "left":
		return "\x1b[D", nil
	default:
		return "", fmt.Errorf("unsupported key sequence: %s", key)
	}
}
