package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"image"
	"image/jpeg"
	"math"
	"regexp"
	"strings"
	"time"
)

const (
	// screenshotProgressIgnoreTopFraction excludes the phone status bar, whose
	// clock and battery indicators can change without meaningful UI progress.
	screenshotProgressIgnoreTopFraction = 0.08
	// The normalized grid preserves layout changes across screenshot resolutions
	// while averaging away anti-aliasing, clock hands, and badge rendering noise.
	screenshotProgressGridColumns           = 40
	screenshotProgressGridRows              = 80
	screenshotProgressBlockChannelThreshold = 8
	screenshotProgressChangedBlockThreshold = 0.01
)

var toolResultArtifactFilePattern = regexp.MustCompile(`(^|[/\\])tool-results([/\\])tr_[A-Za-z0-9][A-Za-z0-9_-]*\.data([^A-Za-z0-9_.-]|$)`)

// StopReason explains why the agent loop stopped or intervened.
type StopReason string

const (
	StopReasonBudgetExceeded StopReason = "budget_exceeded"
	StopReasonLoopDetected   StopReason = "loop_detected"
	StopReasonNoProgress     StopReason = "no_progress"
	StopReasonExternal       StopReason = "external"
	StopReasonParseFailure   StopReason = "parse_failure"
)

// InterventionTier escalates from a notice to a stronger notice to
// termination. No tier blocks a tool.
type InterventionTier int

const (
	TierNone InterventionTier = iota
	TierSoftNotice
	TierFinalWarning
	TierTerminate
)

// TerminationPolicyConfig controls loop-guard thresholds. Zero values use defaults.
type TerminationPolicyConfig struct {
	Enabled                 *bool   `toml:"enabled,omitempty" json:"enabled,omitempty"`
	MaxSeconds              float64 `toml:"max_seconds,omitempty" json:"max_seconds,omitempty"`
	RepeatActionLimit       int     `toml:"repeat_action_limit,omitempty" json:"repeat_action_limit,omitempty"`
	SameResultLimit         int     `toml:"same_result_limit,omitempty" json:"same_result_limit,omitempty"`
	SoftNoticeStallScore    int     `toml:"soft_notice_stall_score,omitempty" json:"soft_notice_stall_score,omitempty"`
	RestrictToolsStallScore int     `toml:"restrict_tools_stall_score,omitempty" json:"restrict_tools_stall_score,omitempty"`
	TerminateStallScore     int     `toml:"terminate_stall_score,omitempty" json:"terminate_stall_score,omitempty"`
	ParseFailureLimit       int     `toml:"parse_failure_limit,omitempty" json:"parse_failure_limit,omitempty"`
}

func DefaultTerminationPolicyConfig() TerminationPolicyConfig {
	return TerminationPolicyConfig{
		Enabled:                 terminationPolicyBoolPtr(true),
		MaxSeconds:              0,
		RepeatActionLimit:       3,
		SameResultLimit:         3,
		SoftNoticeStallScore:    2,
		RestrictToolsStallScore: 4,
		TerminateStallScore:     6,
		ParseFailureLimit:       3,
	}
}

func (c TerminationPolicyConfig) resolved() TerminationPolicyConfig {
	defaults := DefaultTerminationPolicyConfig()
	if c.Enabled == nil {
		c.Enabled = terminationPolicyBoolPtr(defaults.enabled())
	}
	if c.RepeatActionLimit <= 0 {
		c.RepeatActionLimit = defaults.RepeatActionLimit
	}
	if c.SameResultLimit <= 0 {
		c.SameResultLimit = defaults.SameResultLimit
	}
	if c.SoftNoticeStallScore <= 0 {
		c.SoftNoticeStallScore = defaults.SoftNoticeStallScore
	}
	if c.RestrictToolsStallScore <= 0 {
		c.RestrictToolsStallScore = defaults.RestrictToolsStallScore
	}
	if c.TerminateStallScore <= 0 {
		c.TerminateStallScore = defaults.TerminateStallScore
	}
	if c.ParseFailureLimit <= 0 {
		c.ParseFailureLimit = defaults.ParseFailureLimit
	}
	return c
}

func (c TerminationPolicyConfig) enabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

func (c TerminationPolicyConfig) Validate() error {
	if c.MaxSeconds < 0 {
		return fmt.Errorf("termination_policy.max_seconds must be >= 0, got %g", c.MaxSeconds)
	}
	values := []struct {
		name  string
		value int
	}{
		{"repeat_action_limit", c.RepeatActionLimit},
		{"same_result_limit", c.SameResultLimit},
		{"soft_notice_stall_score", c.SoftNoticeStallScore},
		{"restrict_tools_stall_score", c.RestrictToolsStallScore},
		{"terminate_stall_score", c.TerminateStallScore},
		{"parse_failure_limit", c.ParseFailureLimit},
	}
	for _, item := range values {
		if item.value < 0 {
			return fmt.Errorf("termination_policy.%s must be >= 0, got %d", item.name, item.value)
		}
	}
	resolved := c.resolved()
	if resolved.SoftNoticeStallScore >= resolved.RestrictToolsStallScore ||
		resolved.RestrictToolsStallScore >= resolved.TerminateStallScore {
		return fmt.Errorf(
			"termination_policy stall scores must satisfy soft_notice < restrict_tools < terminate, got %d < %d < %d",
			resolved.SoftNoticeStallScore,
			resolved.RestrictToolsStallScore,
			resolved.TerminateStallScore,
		)
	}
	return nil
}

func terminationPolicyBoolPtr(value bool) *bool {
	return &value
}

// TerminationDecision is returned by the policy before/after each loop step.
type TerminationDecision struct {
	Stop    bool
	Reason  StopReason
	Message string
	Tier    InterventionTier
	Notice  string
}

// TerminationPolicy implements budget, repeat, and progress-aware loop guards.
type TerminationPolicy struct {
	cfg       TerminationPolicyConfig
	startedAt time.Time

	lastToolSig      string
	sameSigStreak    int
	lastResultHash   string
	sameResultStreak int

	// lastScreen is the previous screenshot returned by a tool that is not
	// a UI action, for the identical-result check on observations.
	lastScreen image.Image
	screenSeq  int

	// The UI loop check follows UI actions only, so screenshots, shell
	// calls or other tools in between neither hide nor reset a loop.
	lastUIActionSig    string
	lastUIActionScreen image.Image
	uiRepeatStreak     int

	parseFailures  int
	stallScore     int
	tier           InterventionTier
	lastNoticeTier InterventionTier

	artifactResultHash   string
	artifactResultStreak int

	lastToolName string
}

func NewTerminationPolicy(cfg TerminationPolicyConfig) *TerminationPolicy {
	return &TerminationPolicy{
		cfg:       cfg.resolved(),
		startedAt: time.Now(),
	}
}

func (p *TerminationPolicy) ResetForSteer() {
	if p == nil {
		return
	}
	p.startedAt = time.Now()
	p.lastToolSig = ""
	p.sameSigStreak = 0
	p.lastResultHash = ""
	p.sameResultStreak = 0
	p.lastScreen = nil
	p.lastUIActionSig = ""
	p.lastUIActionScreen = nil
	p.uiRepeatStreak = 0
	p.parseFailures = 0
	p.stallScore = 0
	p.tier = TierNone
	p.lastNoticeTier = TierNone
	p.lastToolName = ""
	p.artifactResultHash = ""
	p.artifactResultStreak = 0
}

func (p *TerminationPolicy) CheckBeforeIteration(ctx context.Context, iteration, maxIterations int) TerminationDecision {
	if p == nil || !p.cfg.enabled() {
		return TerminationDecision{}
	}
	if ctx != nil && ctx.Err() != nil {
		return p.terminate(StopReasonExternal, "run canceled")
	}
	if p.cfg.MaxSeconds > 0 && time.Since(p.startedAt).Seconds() >= p.cfg.MaxSeconds {
		return p.terminate(StopReasonBudgetExceeded, "wall_clock budget exhausted")
	}
	if maxIterations > 0 && maxIterations < math.MaxInt && iteration > maxIterations {
		return p.terminate(StopReasonBudgetExceeded, "max_iterations budget exhausted")
	}
	if p.parseFailures >= p.cfg.ParseFailureLimit {
		return p.terminate(StopReasonParseFailure, "too many unparseable model outputs")
	}
	if p.shouldTerminate() {
		return p.terminateFromStall()
	}
	return p.interventionDecision()
}

func (p *TerminationPolicy) RecordParseFailure() TerminationDecision {
	if p == nil || !p.cfg.enabled() {
		return TerminationDecision{}
	}
	p.parseFailures++
	p.bumpStall(1)
	if p.parseFailures >= p.cfg.ParseFailureLimit {
		return p.terminate(StopReasonParseFailure, "too many unparseable model outputs")
	}
	return p.interventionDecision()
}

// BeforeToolCall never blocks a tool: the guard warns about an obvious loop
// and then ends the run, and in between the model stays free to act on
// what it sees.
func (p *TerminationPolicy) BeforeToolCall(toolName, input string) (ToolResult, bool) {
	return ToolResult{}, true
}

func (p *TerminationPolicy) AfterToolCall(toolName, input, observation string, isError bool) TerminationDecision {
	if p == nil || !p.cfg.enabled() {
		return TerminationDecision{}
	}
	p.lastToolName = strings.TrimSpace(toolName)
	signature := toolCallSignature(toolName, input)
	resultHash := observationProgressHash(toolName, observation)
	// Artifact recovery often alternates a query with reading its saved output.
	// Compare the raw result across those command shapes; changing a slice bound
	// or reading a new copy of the same error is not new information.
	if isArtifactRecoveryCall(toolName, input) && resultHash != "" {
		if resultHash == p.artifactResultHash {
			p.artifactResultStreak++
		} else {
			p.artifactResultHash = resultHash
			p.artifactResultStreak = 1
		}
		if p.artifactResultStreak >= max(p.cfg.RepeatActionLimit, p.cfg.SameResultLimit) {
			return p.terminate(StopReasonLoopDetected, "artifact recovery calls repeated without new information")
		}
	} else {
		p.artifactResultHash = ""
		p.artifactResultStreak = 0
	}
	screen, hasScreenshot := extractScreenshotImage(observation)
	// The guard only catches obvious loops; whether an action worked is for
	// the model to judge from the screenshot. A UI action with a screenshot
	// is therefore judged by the UI loop check alone and never adds to the
	// stall score.
	if hasScreenshot && screen != nil && isLoopRestrictedActionTool(toolName) {
		return p.afterUIAction(signature, screen, isError)
	}
	// Any other screenshot is new information unless it is identical to the
	// previous one.
	if hasScreenshot && screen != nil {
		if p.lastScreen != nil && screenshotsIdentical(p.lastScreen, screen) {
			resultHash = "screen:same"
		} else {
			p.screenSeq++
			resultHash = fmt.Sprintf("screen:%d", p.screenSeq)
		}
		p.lastScreen = screen
	}
	previousSignature := p.lastToolSig
	previousResultHash := p.lastResultHash
	sameSignature := signature != "" && signature == previousSignature
	madeProgress := false

	if sameSignature {
		p.sameSigStreak++
	} else {
		p.lastToolSig = signature
		p.sameSigStreak = 1
	}
	sameResult := resultHash != "" && sameSignature &&
		(resultHash == previousResultHash || resultHash == "screen:same")
	if sameResult {
		p.sameResultStreak++
	} else {
		p.lastResultHash = resultHash
		p.sameResultStreak = 1
	}
	if resultHash != "" && previousResultHash != "" && !sameResult && resultHash != previousResultHash && !isError {
		madeProgress = true
	}

	if madeProgress {
		p.recordProgress()
	}
	if !madeProgress && p.sameSigStreak >= 2 {
		p.bumpStall(1)
	}
	if !madeProgress && p.sameResultStreak >= 2 {
		p.bumpStall(2)
	}
	if p.sameSigStreak >= p.cfg.RepeatActionLimit && p.sameResultStreak >= p.cfg.SameResultLimit {
		return p.terminate(StopReasonLoopDetected, "same tool call repeated without new information")
	}
	if p.shouldTerminate() {
		return p.terminateFromStall()
	}
	return p.interventionDecision()
}

// afterUIAction counts a UI action that repeats the previous UI action
// exactly and leaves the screen identical to what that action left. The
// second such repeat gets a notice and repeat_action_limit ends the run.
// Anything else, including a different action on an unchanged screen, is
// left to the model.
func (p *TerminationPolicy) afterUIAction(signature string, screen image.Image, isError bool) TerminationDecision {
	repeated := signature != "" && signature == p.lastUIActionSig &&
		p.lastUIActionScreen != nil && screenshotsIdentical(p.lastUIActionScreen, screen)
	p.lastUIActionSig = signature
	p.lastUIActionScreen = screen
	if !repeated {
		p.uiRepeatStreak = 1
		if !isError {
			p.recordProgress()
		}
		return p.interventionDecision()
	}
	p.uiRepeatStreak++
	if p.uiRepeatStreak >= p.cfg.RepeatActionLimit {
		return p.terminate(StopReasonLoopDetected, "the same UI action was repeated and the screen stayed identical")
	}
	if p.uiRepeatStreak == 2 {
		return TerminationDecision{
			Tier:   p.tier,
			Notice: "Loop guard: the same UI action was repeated and the screen stayed identical. Do not repeat it again; inspect the screenshot, try a different target or approach, or call request_user_action if blocked.",
		}
	}
	return p.interventionDecision()
}

// screenshotsIdentical reports whether no progress-grid block differs between
// two screenshots. On the board, captures of a static iPhone screen differ in
// no block, while the smallest real change seen (a pointer moving, one digit
// added) changed six or more; anything that keeps moving on screen therefore
// reads as different, and such loops are left to the iteration budget.
func screenshotsIdentical(before, after image.Image) bool {
	ratio, comparable := screenshotProgressChangeRatio(before, after, nil)
	return comparable && ratio == 0
}

func (p *TerminationPolicy) BudgetExhausted(reason string) TerminationDecision {
	if p == nil {
		return TerminationDecision{
			Stop:    true,
			Reason:  StopReasonBudgetExceeded,
			Message: reason,
			Tier:    TierTerminate,
		}
	}
	return p.terminate(StopReasonBudgetExceeded, reason)
}

func (p *TerminationPolicy) bumpStall(delta int) {
	if delta <= 0 {
		return
	}
	p.stallScore += delta
	p.refreshTier()
}

func (p *TerminationPolicy) recordProgress() {
	p.stallScore = 0
	p.tier = TierNone
	p.lastNoticeTier = TierNone
	p.sameSigStreak = 1
	p.sameResultStreak = 1
}

func (p *TerminationPolicy) refreshTier() {
	switch {
	case p.stallScore >= p.cfg.TerminateStallScore:
		p.tier = TierTerminate
	case p.stallScore >= p.cfg.RestrictToolsStallScore:
		p.tier = TierFinalWarning
	case p.stallScore >= p.cfg.SoftNoticeStallScore:
		p.tier = TierSoftNotice
	default:
		p.tier = TierNone
	}
}

func (p *TerminationPolicy) shouldTerminate() bool {
	return p.tier >= TierTerminate
}

func (p *TerminationPolicy) interventionDecision() TerminationDecision {
	if p.tier <= p.lastNoticeTier {
		return TerminationDecision{Tier: p.tier}
	}
	notice := p.noticeForTier(p.tier)
	p.lastNoticeTier = p.tier
	return TerminationDecision{
		Tier:   p.tier,
		Notice: notice,
	}
}

func (p *TerminationPolicy) noticeForTier(tier InterventionTier) string {
	switch tier {
	case TierSoftNotice:
		return "Loop guard: recent tool calls are repeating without new results. Change strategy, verify with observation tools, or call request_user_action if blocked."
	case TierFinalWarning:
		return "Loop guard: tool calls are still repeating without new results, and the run will stop if this continues. Change strategy now, or call request_user_action if blocked."
	default:
		return ""
	}
}

func (p *TerminationPolicy) terminate(reason StopReason, message string) TerminationDecision {
	p.tier = TierTerminate
	return TerminationDecision{
		Stop:    true,
		Reason:  reason,
		Message: message,
		Tier:    TierTerminate,
	}
}

func (p *TerminationPolicy) terminateFromStall() TerminationDecision {
	reason := StopReasonNoProgress
	message := "agent stalled without measurable progress"
	if p.sameSigStreak >= p.cfg.RepeatActionLimit {
		reason = StopReasonLoopDetected
		message = "repeated tool calls without progress"
	}
	return p.terminate(reason, message)
}

func formatLoopGuardStopMessage(decision TerminationDecision, lastTool string) string {
	lastTool = strings.TrimSpace(lastTool)
	parts := []string{
		"I'm stopping here because the task is not making measurable progress.",
		fmt.Sprintf("Stop reason: %s (%s).", decision.Reason, decision.Message),
	}
	if lastTool != "" {
		parts = append(parts, fmt.Sprintf("Most recent tool: %s.", lastTool))
	}
	parts = append(parts, "Please review the current screen, try a different approach, or request human handoff.")
	return strings.Join(parts, " ")
}

func toolCallSignature(toolName, input string) string {
	toolName = strings.ToLower(strings.TrimSpace(toolName))
	input = strings.TrimSpace(input)
	if toolName == "shell" {
		input = normalizeArtifactRecoveryInput(input)
	}
	if input == "" {
		return toolName
	}
	sum := sha256.Sum256([]byte(input))
	return toolName + ":" + hex.EncodeToString(sum[:8])
}

func isArtifactRecoveryCall(toolName, input string) bool {
	if !strings.EqualFold(strings.TrimSpace(toolName), "shell") {
		return false
	}
	var arguments struct {
		Command string `json:"command"`
	}
	return json.Unmarshal([]byte(input), &arguments) == nil && toolResultArtifactFilePattern.MatchString(arguments.Command)
}

func normalizeArtifactRecoveryInput(input string) string {
	var arguments map[string]any
	if err := json.Unmarshal([]byte(input), &arguments); err != nil {
		return input
	}
	command, ok := arguments["command"].(string)
	if !ok || !toolResultArtifactFilePattern.MatchString(command) {
		return input
	}
	normalized := toolResultArtifactFilePattern.ReplaceAllString(command, "$1tool-results$2tr_<artifact>.data$3")
	if normalized == command {
		return input
	}
	arguments["command"] = normalized
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return input
	}
	return string(encoded)
}

func observationProgressHash(toolName, observation string) string {
	observation = strings.TrimSpace(observation)
	if observation == "" {
		return ""
	}
	if _, ok := extractScreenshotData(observation); ok {
		return "screen:jpeg"
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(strings.ToLower(strings.TrimSpace(toolName))))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(observation))
	return fmt.Sprintf("obs:%x", hash.Sum64())
}

func extractScreenshotData(observation string) (string, bool) {
	observation = strings.TrimSpace(observation)
	if observation == "" || !strings.HasPrefix(observation, "{") {
		return "", false
	}
	var payload struct {
		Format string `json:"format"`
		Data   string `json:"data"`
	}
	if err := json.Unmarshal([]byte(observation), &payload); err != nil {
		return "", false
	}
	data := strings.TrimSpace(payload.Data)
	if !strings.EqualFold(strings.TrimSpace(payload.Format), "jpeg") || data == "" {
		return "", false
	}
	return data, true
}

func extractScreenshotImage(observation string) (image.Image, bool) {
	encoded, ok := extractScreenshotData(observation)
	if !ok {
		return nil, false
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, true
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, true
	}
	return img, true
}

// screenshotProgressChangeRatio returns the share of progress-grid blocks
// that changed, with blocks intersecting an excluded rectangle not counted.
// Screenshots of different sizes count as a full change.
func screenshotProgressChangeRatio(before, after image.Image, excluded []image.Rectangle) (float64, bool) {
	if before == nil || after == nil {
		return 0, false
	}
	if before.Bounds() != after.Bounds() {
		return 1, true
	}
	return screenshotProgressBlockDiffRatio(before, after, screenshotProgressBounds(after.Bounds()), excluded)
}

func screenshotProgressBounds(full image.Rectangle) image.Rectangle {
	height := full.Dy()
	if height <= 1 {
		return full
	}
	topRows := int(math.Ceil(float64(height) * screenshotProgressIgnoreTopFraction))
	if topRows <= 0 || topRows >= height {
		return full
	}
	full.Min.Y += topRows
	return full
}

func screenshotProgressBlockDiffRatio(before, after image.Image, bounds image.Rectangle, excluded []image.Rectangle) (float64, bool) {
	width := bounds.Dx()
	height := bounds.Dy()
	if width <= 0 || height <= 0 {
		return 0, false
	}
	if b, ok := before.(*image.YCbCr); ok {
		if a, ok := after.(*image.YCbCr); ok && a.SubsampleRatio == b.SubsampleRatio && a.Rect == b.Rect {
			return screenshotProgressYCbCrBlockDiffRatio(b, a, bounds, excluded)
		}
	}
	columns := screenshotProgressGridColumns
	if width < columns {
		columns = width
	}
	rows := screenshotProgressGridRows
	if height < rows {
		rows = height
	}

	changedBlocks := 0
	totalBlocks := columns * rows
	for row := 0; row < rows; row++ {
		y0 := bounds.Min.Y + height*row/rows
		y1 := bounds.Min.Y + height*(row+1)/rows
		for column := 0; column < columns; column++ {
			x0 := bounds.Min.X + width*column/columns
			x1 := bounds.Min.X + width*(column+1)/columns
			if rectanglesOverlap(image.Rect(x0, y0, x1, y1), excluded) {
				continue
			}
			pixelCount := uint64((x1 - x0) * (y1 - y0))
			var beforeR, beforeG, beforeB uint64
			var afterR, afterG, afterB uint64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					br, bg, bb, _ := before.At(x, y).RGBA()
					ar, ag, ab, _ := after.At(x, y).RGBA()
					beforeR += uint64(br >> 8)
					beforeG += uint64(bg >> 8)
					beforeB += uint64(bb >> 8)
					afterR += uint64(ar >> 8)
					afterG += uint64(ag >> 8)
					afterB += uint64(ab >> 8)
				}
			}
			threshold := uint64(screenshotProgressBlockChannelThreshold) * pixelCount
			if absDiffUint64(beforeR, afterR) > threshold ||
				absDiffUint64(beforeG, afterG) > threshold ||
				absDiffUint64(beforeB, afterB) > threshold {
				changedBlocks++
			}
		}
	}
	return float64(changedBlocks) / float64(totalBlocks), true
}

// screenshotProgressYCbCrBlockDiffRatio is the block comparison for decoded
// JPEG screenshots. It reads the Y, Cb and Cr planes directly and samples
// every other pixel, which keeps each block mean while avoiding a per-pixel
// color conversion that took most of a second per comparison on the board.
func screenshotProgressYCbCrBlockDiffRatio(before, after *image.YCbCr, bounds image.Rectangle, excluded []image.Rectangle) (float64, bool) {
	width, height := bounds.Dx(), bounds.Dy()
	columns := min(screenshotProgressGridColumns, width)
	rows := min(screenshotProgressGridRows, height)
	changedBlocks := 0
	for row := 0; row < rows; row++ {
		y0 := bounds.Min.Y + height*row/rows
		y1 := bounds.Min.Y + height*(row+1)/rows
		for column := 0; column < columns; column++ {
			x0 := bounds.Min.X + width*column/columns
			x1 := bounds.Min.X + width*(column+1)/columns
			if rectanglesOverlap(image.Rect(x0, y0, x1, y1), excluded) {
				continue
			}
			var beforeY, beforeCb, beforeCr, afterY, afterCb, afterCr, count int
			for y := y0; y < y1; y += tapSequenceSampleStep {
				for x := x0; x < x1; x += tapSequenceSampleStep {
					yi, ci := before.YOffset(x, y), before.COffset(x, y)
					beforeY += int(before.Y[yi])
					beforeCb += int(before.Cb[ci])
					beforeCr += int(before.Cr[ci])
					afterY += int(after.Y[yi])
					afterCb += int(after.Cb[ci])
					afterCr += int(after.Cr[ci])
					count++
				}
			}
			threshold := screenshotProgressBlockChannelThreshold * count
			if absInt(beforeY-afterY) > threshold || absInt(beforeCb-afterCb) > threshold || absInt(beforeCr-afterCr) > threshold {
				changedBlocks++
			}
		}
	}
	return float64(changedBlocks) / float64(columns*rows), true
}

func rectanglesOverlap(block image.Rectangle, rects []image.Rectangle) bool {
	for _, rect := range rects {
		if block.Overlaps(rect) {
			return true
		}
	}
	return false
}

func absDiffUint64(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

func isLoopRestrictedActionTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "keyboard_tap", "mouse_move", "mouse_scroll", "touch_gesture",
		"quick_action", "enter_text",
		toolSearchLaunchApp, toolOpenApp, toolOpenURL, toolBridgeOpenApp:
		return true
	default:
		return false
	}
}
