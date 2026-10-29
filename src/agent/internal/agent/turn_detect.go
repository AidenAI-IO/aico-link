package agent

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"aico-agent/internal/logging"
)

const (
	defaultTurnDetectionModelPath  = "/usr/lib/aico/models/smart-turn-v3.2-dyt-4s-rv1106-int8-mmse.rknn"
	defaultTurnDetectionHelperPath = "/usr/lib/aico/smart_turn_helper"
	defaultTurnDetectionThreshold  = 0.5
	defaultTurnAnalyzeSilenceMs    = 600
	defaultTurnSilenceFallbackMs   = 3000
	turnHelperReadinessTimeout     = 30 * time.Second
	turnHelperProtocolTimeout      = 3 * time.Second
	turnHelperShutdownTimeout      = 2 * time.Second
	smartTurnFailCommitAfter       = 2
)

// TurnDetector predicts whether the current user turn is complete.
type TurnDetector interface {
	Predict(pcm []int16) (prob float64, err error)
	Close() error
}

type helperTurnDetector struct {
	modelPath  string
	helperPath string

	mu            sync.Mutex
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	stdout        *bufio.Reader
	waitErr       chan error
	windowSamples int
}

func DefaultTurnDetectionModelPath() string {
	return defaultTurnDetectionModelPath
}

func DefaultTurnDetectionHelperPath() string {
	return defaultTurnDetectionHelperPath
}

func DefaultTurnDetectionThreshold() float64 {
	return defaultTurnDetectionThreshold
}

func DefaultTurnAnalyzeSilenceMs() int {
	return defaultTurnAnalyzeSilenceMs
}

func DefaultTurnSilenceFallbackMs() int {
	return defaultTurnSilenceFallbackMs
}

func NewHelperTurnDetector(modelPath, helperPath string) *helperTurnDetector {
	modelPath = strings.TrimSpace(modelPath)
	if modelPath == "" {
		modelPath = defaultTurnDetectionModelPath
	}
	helperPath = strings.TrimSpace(helperPath)
	if helperPath == "" {
		helperPath = defaultTurnDetectionHelperPath
	}
	return &helperTurnDetector{
		modelPath:  modelPath,
		helperPath: helperPath,
	}
}

func (d *helperTurnDetector) Predict(pcm []int16) (float64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.ensureStarted(); err != nil {
		return 0, err
	}
	if len(pcm) == 0 {
		return 0, errors.New("empty turn audio")
	}
	if len(pcm) > 16000*120 {
		return 0, fmt.Errorf("turn too long: %d samples", len(pcm))
	}

	header := make([]byte, 1+4)
	header[0] = 'T'
	binary.LittleEndian.PutUint32(header[1:], uint32(len(pcm)))
	if _, err := d.stdin.Write(header); err != nil {
		d.stopAfterProtocolError()
		return 0, fmt.Errorf("write turn helper header: %w", err)
	}
	payload := make([]byte, len(pcm)*2)
	for i, sample := range pcm {
		binary.LittleEndian.PutUint16(payload[i*2:], uint16(sample))
	}
	if _, err := d.stdin.Write(payload); err != nil {
		d.stopAfterProtocolError()
		return 0, fmt.Errorf("write turn helper pcm: %w", err)
	}

	line, err := d.readLineWithTimeout("read turn helper probability")
	if err != nil {
		d.stopAfterProtocolError()
		return 0, fmt.Errorf("read turn helper probability: %w", err)
	}
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "ERR ") {
		// NPU submit failures leave the helper in a bad state; recycle it so
		// the next Predict starts clean instead of reusing a dead context.
		d.stopAfterProtocolError()
		return 0, errors.New(strings.TrimPrefix(line, "ERR "))
	}
	if !strings.HasPrefix(line, "P ") {
		d.stopAfterProtocolError()
		return 0, fmt.Errorf("unexpected turn helper response %q", line)
	}
	fields := strings.Fields(strings.TrimPrefix(line, "P "))
	if len(fields) < 1 {
		d.stopAfterProtocolError()
		return 0, fmt.Errorf("empty turn helper probability in %q", line)
	}
	prob, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		d.stopAfterProtocolError()
		return 0, fmt.Errorf("parse turn helper probability %q: %w", line, err)
	}
	return prob, nil
}

func (d *helperTurnDetector) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closeLocked()
}

func (d *helperTurnDetector) WindowSamples() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = d.ensureStarted()
	return d.windowSamples
}

func (d *helperTurnDetector) ensureStarted() error {
	if d.cmd != nil {
		return nil
	}
	cmd := exec.Command(d.helperPath, "--model", d.modelPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open turn helper stdin: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("open turn helper stdout: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("open turn helper stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("start turn helper %q: %w", d.helperPath, err)
	}
	go logTurnHelperStderr(stderrPipe)

	reader := bufio.NewReader(stdoutPipe)
	line, err := readHelperLineWithTimeout(cmd, reader, "wait for turn helper readiness", turnHelperReadinessTimeout)
	if err != nil {
		_ = terminateCommand(cmd)
		return fmt.Errorf("wait for turn helper readiness: %w", err)
	}
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "ERR ") {
		_ = terminateCommand(cmd)
		return errors.New(strings.TrimPrefix(line, "ERR "))
	}
	fields := strings.Fields(line)
	if len(fields) < 1 || fields[0] != "READY" {
		_ = terminateCommand(cmd)
		return fmt.Errorf("unexpected turn helper readiness response %q", line)
	}
	if len(fields) >= 2 {
		if n, err := strconv.Atoi(fields[1]); err == nil && n > 0 {
			d.windowSamples = n
		}
	}

	d.cmd = cmd
	d.stdin = stdin
	d.stdout = reader
	d.waitErr = make(chan error, 1)
	go func() {
		d.waitErr <- cmd.Wait()
	}()
	return nil
}

func (d *helperTurnDetector) readLineWithTimeout(operation string) (string, error) {
	return readHelperLineWithTimeout(d.cmd, d.stdout, operation, turnHelperProtocolTimeout)
}

func (d *helperTurnDetector) stopAfterProtocolError() {
	_ = d.closeLocked()
}

func (d *helperTurnDetector) closeLocked() error {
	if d.cmd == nil {
		return nil
	}
	var writeErr error
	if d.stdin != nil {
		_, writeErr = d.stdin.Write([]byte{'Q'})
		_ = d.stdin.Close()
	}
	done := d.waitErr
	cmd := d.cmd
	d.cmd = nil
	d.stdin = nil
	d.stdout = nil
	d.waitErr = nil

	timer := time.NewTimer(turnHelperShutdownTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if writeErr != nil {
			return writeErr
		}
		return err
	case <-timer.C:
		_ = terminateCommand(cmd)
		if done != nil {
			<-done
		}
		if writeErr != nil {
			return writeErr
		}
		return errors.New("turn helper shutdown timed out")
	}
}

func logTurnHelperStderr(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		logging.Warnf("agent", "turn_detect", "helper stderr: %s", line)
	}
}
