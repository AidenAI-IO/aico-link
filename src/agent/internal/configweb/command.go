package configweb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"aiden-agent/internal/logging"
)

type commandResult struct {
	Output        []byte
	ExitCode      int
	TimedOut      bool
	FailureReason string
}

func runCommand(timeout time.Duration, env []string, input []byte, name string, args ...string) commandResult {
	return runCommandContext(context.Background(), timeout, env, input, name, args...)
}

func runCommandContext(parent context.Context, timeout time.Duration, env []string, input []byte, name string, args ...string) commandResult {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.CombinedOutput()
	result := commandResult{Output: output, ExitCode: 0}
	if ctx.Err() != nil {
		result.TimedOut = true
		result.ExitCode = -1
		return result
	}
	if err == nil {
		return result
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
	} else {
		result.ExitCode = 127
		if len(output) == 0 {
			result.Output = []byte(err.Error())
		}
	}
	return result
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func (s *Server) scheduleAgentRestart() error {
	s.restartMu.Lock()
	defer s.restartMu.Unlock()
	s.reapRestartLocked()
	if s.maintenance != nil && s.maintenance.active() {
		s.restartDeferred = true
		s.restartReadinessPending = true
		return nil
	}
	if s.restartCommand != nil {
		s.restartDeferred = true
		s.restartReadinessPending = true
		return nil
	}
	err := s.launchRestartLocked()
	if err != nil {
		s.restartError = err
	}
	return err
}

func (s *Server) restartWiFiProxy() error {
	path := strings.TrimSpace(s.options.WiFiProxyInitScript)
	if path == "" {
		return errors.New("Wi-Fi proxy restart script path is empty")
	}
	result := runCommand(15*time.Second, nil, nil, path, "restart")
	if result.ExitCode == 0 {
		return nil
	}
	if result.ExitCode == 127 {
		result = runCommand(15*time.Second, nil, nil, "/bin/sh", path, "restart")
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("restart Wi-Fi proxy: %s", strings.TrimSpace(string(result.Output)))
	}
	return nil
}

func (s *Server) startDeferredRestartIfIdle() {
	if s.maintenance != nil && s.maintenance.active() {
		return
	}
	s.restartMu.Lock()
	defer s.restartMu.Unlock()
	s.reapRestartLocked()
	if s.restartDeferred && s.restartCommand == nil {
		s.restartDeferred = false
		s.restartReadinessPending = true
		if err := s.launchRestartLocked(); err != nil {
			s.restartReadinessPending = false
			s.restartError = err
		}
	}
}

func (s *Server) waitForAgentRestart(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		s.restartMu.Lock()
		s.reapRestartLocked()
		pending := s.restartCommand != nil
		restartErr := s.restartError
		s.restartMu.Unlock()
		if restartErr != nil {
			return restartErr
		}
		if !pending {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for agent restart")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (s *Server) agentRestartPending() bool {
	s.restartMu.Lock()
	defer s.restartMu.Unlock()
	s.reapRestartLocked()
	return s.restartReadinessPending || s.restartCommand != nil || s.restartDeferred || s.restartError != nil
}

func (s *Server) launchRestartLocked() error {
	path := strings.TrimSpace(s.options.AgentInitScript)
	if path == "" {
		return errors.New("agent restart script path is empty")
	}
	cmd := exec.Command(path, "restart")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		cmd = exec.Command("/bin/sh", path, "restart")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if shellErr := cmd.Start(); shellErr != nil {
			return fmt.Errorf("launch agent restart: %w", shellErr)
		}
	}
	s.restartError = nil
	s.restartCommand = cmd
	s.restartDone = make(chan error, 1)
	s.restartReadinessPending = true
	done := s.restartDone
	go func() { done <- cmd.Wait() }()
	return nil
}

func (s *Server) reapRestartLocked() {
	if s.restartCommand == nil || s.restartDone == nil {
		return
	}
	select {
	case err := <-s.restartDone:
		s.restartCommand = nil
		s.restartDone = nil
		if err != nil {
			s.restartReadinessPending = false
			s.restartError = fmt.Errorf("agent restart failed: %w", err)
		}
	default:
	}
}

func logConfigWebError(message string) {
	logging.Errorf("config_web", "config_web", "%s", message)
}

func mergeEnvironment(base []string, assignments []EnvAssignment) []string {
	values := make(map[string]string, len(base)+len(assignments))
	order := make([]string, 0, len(base)+len(assignments))
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = item
	}
	for _, assignment := range assignments {
		if _, exists := values[assignment.Key]; !exists {
			order = append(order, assignment.Key)
		}
		values[assignment.Key] = assignment.Key + "=" + assignment.Value
	}
	result := make([]string, 0, len(order))
	for _, key := range order {
		result = append(result, values[key])
	}
	return result
}
