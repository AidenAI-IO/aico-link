package agent

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"aico-agent/internal/logging"
)

const (
	defaultUIGroundingHelperPath = "/usr/lib/aico/ui_grounding_helper"
	defaultUIGroundingModelDir   = "/usr/lib/aico/models/ui-detr"
	uiGroundingReadinessTimeout  = 30 * time.Second
	uiGroundingDetectTimeout     = 10 * time.Second
	uiGroundingShutdownTimeout   = 2 * time.Second
	// While UI Grounding is on, the helper stays resident so no grounded
	// screenshot waits for a model load. When it is off, a helper started by an
	// explicit screenshot request exits after this long without a request,
	// releasing its roughly 40 MB of CMA.
	uiGroundingIdleTimeout = 2 * time.Minute
	uiGroundingMaxElements = 50
	// The detector's validated defaults (confidence 0, NMS IoU 0.8) favor
	// recall and leave several near-identical boxes on one control. Marks the
	// model reads must be distinct, so drop low scores and merge boxes that
	// overlap by more than 60%, accepting that a few controls go unmarked.
	uiGroundingConfidence = "0.3"
	uiGroundingNMSIoU     = "0.6"
)

// uiElementBox is one detected interface component in source image pixels.
type uiElementBox struct {
	X1, Y1, X2, Y2 float64
	Score          float64
}

// uiGrounder detects interface components on a packed RGB888 image.
type uiGrounder interface {
	Detect(ctx context.Context, rgb []byte, width, height int) ([]uiElementBox, error)
}

// helperUIGrounder runs ui_grounding_helper, which keeps the UI-DETR RKNN
// model loaded on the NPU between requests.
type helperUIGrounder struct {
	helperPath  string
	runtimePath string
	modelPath   string
	anchorsPath string
	idleTimeout time.Duration

	// mu serializes helper use: start, detection and shutdown. A start can
	// hold it for the whole model load, so nothing that must stay responsive
	// (config apply, shutdown) waits on it; those use stateMu instead.
	mu        sync.Mutex
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	waitErr   chan error
	idleTimer *time.Timer

	// stateMu guards resident and starting, never held across blocking work.
	stateMu sync.Mutex
	// resident keeps the helper loaded with no idle exit, and loads it ahead
	// of the first request. It follows the ui_grounding_enabled setting.
	resident bool
	// starting is the helper being started, so SetResident(false) can kill a
	// load in progress instead of waiting for it.
	starting *exec.Cmd
}

func (g *helperUIGrounder) isResident() bool {
	g.stateMu.Lock()
	defer g.stateMu.Unlock()
	return g.resident
}

// uiGroundingResidency is implemented by grounders that can keep their model
// loaded between requests.
type uiGroundingResidency interface {
	SetResident(resident bool)
}

// setUIGroundingResident keeps the packaged detector loaded while UI
// Grounding is on and releases it when the setting is turned off. It does
// nothing on a device without the detector.
func setUIGroundingResident(resident bool) {
	if grounder, ok := defaultUIGrounder().(uiGroundingResidency); ok {
		grounder.SetResident(resident)
	}
}

// defaultUIGrounder returns the packaged grounder, or nil when the helper or
// its model is not installed (host builds, benchmark containers). Tool sets
// are rebuilt on configuration reload; sharing one grounder keeps a single
// helper, and so a single copy of the model, resident on the NPU.
var defaultUIGrounder = sync.OnceValue(func() uiGrounder {
	grounder := newHelperUIGrounder(defaultUIGroundingHelperPath, defaultUIGroundingModelDir)
	for _, path := range []string{grounder.helperPath, grounder.runtimePath, grounder.modelPath, grounder.anchorsPath} {
		if _, err := os.Stat(path); err != nil {
			return nil
		}
	}
	return grounder
})

func newHelperUIGrounder(helperPath, modelDir string) *helperUIGrounder {
	return &helperUIGrounder{
		helperPath:  helperPath,
		runtimePath: filepath.Join(modelDir, "librknnmrt.so"),
		modelPath:   filepath.Join(modelDir, "model.rknn"),
		anchorsPath: filepath.Join(modelDir, "RK_anchors.txt"),
		idleTimeout: uiGroundingIdleTimeout,
	}
}

func (g *helperUIGrounder) Detect(ctx context.Context, rgb []byte, width, height int) ([]uiElementBox, error) {
	if width <= 0 || height <= 0 || len(rgb) != width*height*3 {
		return nil, fmt.Errorf("grounding input must be %dx%d RGB888", width, height)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.idleTimer != nil {
		g.idleTimer.Stop()
	}
	defer g.scheduleIdleCloseLocked()
	boxes, err := g.detectLocked(ctx, rgb, width, height)
	if err != nil && g.cmd == nil && g.isResident() {
		// The helper was stopped by this failure; load it again in the
		// background so the next grounded screenshot does not wait for it.
		go g.warm()
	}
	return boxes, err
}

func (g *helperUIGrounder) detectLocked(ctx context.Context, rgb []byte, width, height int) ([]uiElementBox, error) {
	// A request nobody waits for must not reach the helper: its read would
	// time out at once and kill a resident model that then has to reload.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := g.ensureStartedLocked(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	header := make([]byte, 9)
	header[0] = 'D'
	binary.LittleEndian.PutUint32(header[1:5], uint32(width))
	binary.LittleEndian.PutUint32(header[5:9], uint32(height))
	if _, err := g.stdin.Write(header); err != nil {
		g.closeLocked()
		return nil, fmt.Errorf("write grounding request: %w", err)
	}
	if _, err := g.stdin.Write(rgb); err != nil {
		g.closeLocked()
		return nil, fmt.Errorf("write grounding image: %w", err)
	}

	timeout := uiGroundingDetectTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline))
	}
	line, err := readHelperLineWithTimeout(g.cmd, g.stdout, "read grounding result", timeout)
	if err != nil {
		g.closeLocked()
		return nil, err
	}
	fields := strings.Fields(line)
	if len(fields) > 0 && fields[0] == "ERR" {
		// An inference failure leaves the NPU context suspect; start over next time.
		g.closeLocked()
		return nil, fmt.Errorf("grounding helper: %s", strings.TrimSpace(strings.TrimPrefix(line, "ERR")))
	}
	if len(fields) < 2 || fields[0] != "OK" {
		g.closeLocked()
		return nil, fmt.Errorf("unexpected grounding helper response %q", strings.TrimSpace(line))
	}
	count, err := strconv.Atoi(fields[1])
	if err != nil || count < 0 || count > uiGroundingMaxElements {
		g.closeLocked()
		return nil, fmt.Errorf("unexpected grounding element count %q", fields[1])
	}
	boxes := make([]uiElementBox, 0, count)
	for i := 0; i < count; i++ {
		line, err := readHelperLineWithTimeout(g.cmd, g.stdout, "read grounding element", uiGroundingDetectTimeout)
		if err != nil {
			g.closeLocked()
			return nil, err
		}
		box, err := parseUIElementBox(line, width, height)
		if err != nil {
			g.closeLocked()
			return nil, err
		}
		boxes = append(boxes, box)
	}
	return boxes, nil
}

func parseUIElementBox(line string, width, height int) (uiElementBox, error) {
	fields := strings.Fields(line)
	if len(fields) != 5 {
		return uiElementBox{}, fmt.Errorf("unexpected grounding element %q", strings.TrimSpace(line))
	}
	var values [5]float64
	for i, field := range fields {
		value, err := strconv.ParseFloat(field, 64)
		if err != nil {
			return uiElementBox{}, fmt.Errorf("parse grounding element %q: %w", strings.TrimSpace(line), err)
		}
		values[i] = value
	}
	box := uiElementBox{X1: values[0], Y1: values[1], X2: values[2], Y2: values[3], Score: values[4]}
	if !(box.X1 >= 0 && box.X1 <= box.X2 && box.X2 <= float64(width) &&
		box.Y1 >= 0 && box.Y1 <= box.Y2 && box.Y2 <= float64(height)) {
		return uiElementBox{}, fmt.Errorf("grounding element %q is outside the %dx%d image", strings.TrimSpace(line), width, height)
	}
	return box, nil
}

func (g *helperUIGrounder) ensureStartedLocked() error {
	if g.cmd != nil {
		return nil
	}
	cmd := exec.Command(g.helperPath, "--runtime", g.runtimePath, "--model", g.modelPath, "--anchors", g.anchorsPath,
		"--confidence", uiGroundingConfidence, "--nms-iou", uiGroundingNMSIoU)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open grounding helper stdin: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("open grounding helper stdout: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("open grounding helper stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("start grounding helper %q: %w", g.helperPath, err)
	}
	g.stateMu.Lock()
	g.starting = cmd
	g.stateMu.Unlock()
	defer func() {
		g.stateMu.Lock()
		g.starting = nil
		g.stateMu.Unlock()
	}()
	go logUIGroundingHelperStderr(stderrPipe)

	reader := bufio.NewReader(stdoutPipe)
	line, err := readHelperLineWithTimeout(cmd, reader, "wait for grounding helper readiness", uiGroundingReadinessTimeout)
	if err != nil {
		_ = stdin.Close()
		_ = terminateCommand(cmd)
		return err
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "READY") {
		_ = stdin.Close()
		_ = terminateCommand(cmd)
		if strings.HasPrefix(line, "ERR ") {
			return fmt.Errorf("grounding helper: %s", strings.TrimPrefix(line, "ERR "))
		}
		return fmt.Errorf("unexpected grounding helper readiness response %q", line)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	g.cmd, g.stdin, g.stdout, g.waitErr = cmd, stdin, reader, waitErr
	return nil
}

// SetResident switches between a resident helper, loaded now and kept until
// the setting changes, and an on-demand one that exits when idle. It never
// waits for a model load or a detection: config apply and shutdown call it.
func (g *helperUIGrounder) SetResident(resident bool) {
	if g == nil {
		return
	}
	g.stateMu.Lock()
	changed := g.resident != resident
	g.resident = resident
	starting := g.starting
	g.stateMu.Unlock()
	if resident {
		go g.warm()
		return
	}
	if !changed {
		return
	}
	// Release the model's CMA as soon as grounding is turned off: abort a
	// load in progress, then stop a loaded helper once its current request,
	// if any, finishes.
	if starting != nil && starting.Process != nil {
		_ = starting.Process.Kill()
	}
	go g.releaseIfNotResident()
}

func (g *helperUIGrounder) releaseIfNotResident() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.isResident() {
		return
	}
	if g.idleTimer != nil {
		g.idleTimer.Stop()
	}
	_ = g.closeLocked()
}

// warm loads the helper ahead of the first request while resident.
func (g *helperUIGrounder) warm() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.isResident() {
		return
	}
	if g.idleTimer != nil {
		g.idleTimer.Stop()
	}
	if err := g.ensureStartedLocked(); err != nil {
		logging.Warnf("agent", "ui_grounding", "preload grounding helper: %v", err)
		return
	}
	// Residency may have been turned off while the model loaded.
	if !g.isResident() {
		_ = g.closeLocked()
	}
}

func (g *helperUIGrounder) scheduleIdleCloseLocked() {
	if g.cmd == nil || g.idleTimeout <= 0 || g.isResident() {
		return
	}
	if g.idleTimer == nil {
		g.idleTimer = time.AfterFunc(g.idleTimeout, func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.closeLocked()
		})
		return
	}
	g.idleTimer.Reset(g.idleTimeout)
}

// Close stops the helper process, if running.
func (g *helperUIGrounder) Close() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.idleTimer != nil {
		g.idleTimer.Stop()
	}
	return g.closeLocked()
}

func (g *helperUIGrounder) closeLocked() error {
	if g.cmd == nil {
		return nil
	}
	_, writeErr := g.stdin.Write([]byte{'Q'})
	_ = g.stdin.Close()
	cmd, done := g.cmd, g.waitErr
	g.cmd, g.stdin, g.stdout, g.waitErr = nil, nil, nil, nil

	timer := time.NewTimer(uiGroundingShutdownTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return errors.Join(writeErr, err)
	case <-timer.C:
		_ = cmd.Process.Kill()
		<-done
		return errors.New("grounding helper shutdown timed out")
	}
}

func logUIGroundingHelperStderr(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			logging.Warnf("agent", "ui_grounding", "helper stderr: %s", line)
		}
	}
}
