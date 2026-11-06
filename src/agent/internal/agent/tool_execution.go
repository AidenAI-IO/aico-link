package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tmc/langchaingo/callbacks"
	"github.com/tmc/langchaingo/schema"
	langtools "github.com/tmc/langchaingo/tools"
)

type ToolCall struct {
	Spec      ToolSpec
	Action    schema.AgentAction
	Input     string
	Content   string
	StartedAt time.Time
}

type ToolResult struct {
	Output           string
	Summary          string
	SummaryTruncated bool
	Error            *ToolError
	Terminate        bool
	Duration         time.Duration
}

func (r ToolResult) IsError() bool { return r.Error != nil }

type ToolCallExecution struct {
	Specs          *ToolSpecs
	Action         schema.AgentAction
	Before         BeforeToolCallHook
	After          AfterToolCallHook
	Callback       callbacks.Handler
	ResultObserver ToolResultObserver
}

type ToolCallExecutionResult struct {
	Call               ToolCall
	Result             ToolResult
	Step               schema.AgentStep
	Error              error
	InterruptedBySteer bool
	ActionCompleted    bool
}

type BeforeToolCallHook func(context.Context, ToolCall) (ToolResult, bool)
type AfterToolCallHook func(context.Context, ToolCall, ToolResult) ToolResult

// ToolResultObserver receives the final normalized result for every tool
// execution, regardless of whether it ran locally or through the environment
// bridge. Observers may update local derived state but must not mutate results.
type ToolResultObserver func(context.Context, ToolCall, ToolResult)

type ToolProgress struct {
	Status  string
	Content string
}

type toolProgressContextKey struct{}
type benchmarkToolCallContextKey struct{}

type toolExecutionHookHandler interface {
	BeforeToolCall(ctx context.Context, call ToolCall) (ToolResult, bool)
	AfterToolCall(ctx context.Context, call ToolCall, result ToolResult) ToolResult
}

type toolResultCallbackHandler interface {
	HandleToolCallResult(ctx context.Context, call ToolCall, result ToolResult)
}

type toolCallStartCallbackHandler interface {
	HandleToolCallStart(ctx context.Context, call ToolCall)
}

type toolProgressCallbackHandler interface {
	HandleToolProgress(ctx context.Context, call ToolCall, progress ToolProgress)
}

func executeToolCall(ctx context.Context, execution ToolCallExecution) ToolCallExecutionResult {
	action := execution.Action
	startedAt := time.Now()
	spec, ok := execution.Specs.Lookup(action.Tool)
	if !ok || spec.Tool == nil {
		call := invalidToolCall(action, startedAt)
		emitToolStart(ctx, execution.Callback, call)
		result := invalidToolResult(call)
		emitToolResult(ctx, execution.Callback, call, result)
		return resultForToolCall(call, result, nil, false)
	}

	input := spec.NormalizeInput(action.ToolInput)
	action.Tool = spec.Name
	action.ToolInput = input
	call := ToolCall{
		Spec:      spec,
		Action:    action,
		Input:     input,
		Content:   toolContentFromAction(action),
		StartedAt: startedAt,
	}

	emitToolStart(ctx, execution.Callback, call)
	if allowed, result := runBeforeToolCallHook(ctx, execution, call); !allowed {
		result = normalizeRejectedToolResult(spec.Name, result)
		result.Duration = time.Since(call.StartedAt)
		result = runAfterToolCallHook(ctx, execution, call, result)
		emitToolResult(ctx, execution.Callback, call, result)
		return resultForToolCall(call, result, nil, false)
	}

	if err := spec.ValidateInput(input); err != nil {
		msg := err.Error()
		result := ToolResult{
			Output:   msg,
			Error:    NewToolError(CodeInvalidArguments, msg),
			Duration: time.Since(call.StartedAt),
		}
		result = runAfterToolCallHook(ctx, execution, call, result)
		emitToolResult(ctx, execution.Callback, call, result)
		return resultForToolCall(call, result, nil, false)
	}

	ctx2, _ := WithToolError(ctx)
	ctx2 = context.WithValue(ctx2, benchmarkToolCallContextKey{}, action.ToolID)
	if progressHandler, ok := execution.Callback.(toolProgressCallbackHandler); ok {
		ctx2 = withToolProgressReporter(ctx2, func(progress ToolProgress) {
			progressHandler.HandleToolProgress(ctx, call, progress)
		})
	}
	output, err := spec.Tool.Call(ctx2, input)
	var toolErr *ToolError
	hardErr := ctx.Err()
	if hardErr != nil && errors.Is(context.Cause(ctx), errSteerInterruptToolCancel) && err == nil {
		hardErr = nil
	}
	if hardErr != nil {
		if errors.Is(hardErr, context.Canceled) {
			toolErr = NewToolError(CodeCanceled, hardErr.Error())
		} else if errors.Is(hardErr, context.DeadlineExceeded) {
			toolErr = NewToolError(CodeDeadlineExceeded, hardErr.Error())
		} else {
			toolErr = NewToolError(CodeToolExecutionFailed, hardErr.Error())
		}
	} else if err != nil {
		if errors.Is(err, context.Canceled) {
			toolErr = NewToolError(CodeCanceled, err.Error())
		} else if errors.Is(err, context.DeadlineExceeded) {
			toolErr = NewToolError(CodeDeadlineExceeded, err.Error())
		} else {
			toolErr = NewToolError(CodeToolExecutionFailed, err.Error())
		}
	} else if attached := ToolErrorFromContext(ctx2); attached != nil {
		// Adopt only intentional top-level SetToolError + toolErrorString
		// results. Nested helpers (e.g. ClipboardTool inside enter_text /
		// search_launch_app) may leave a leftover ToolError while the parent
		// recovers and returns its own observation — do not overwrite that.
		if strings.TrimSpace(output) == "" || output == attached.Message {
			toolErr = attached
		}
	}
	result := ToolResult{
		Output:   output,
		Error:    toolErr,
		Duration: time.Since(call.StartedAt),
	}
	if err == nil && toolErr != nil && strings.TrimSpace(result.Output) == "" {
		result.Output = toolErr.Message
	}
	if err != nil || hardErr != nil {
		result.Output = toolErr.Message
		if hardErr != nil {
			result = runAfterToolCallHook(ctx, execution, call, result)
			emitToolResult(ctx, execution.Callback, call, result)
			return resultForToolCall(call, result, hardErr, true)
		}
	}

	result = runAfterToolCallHook(ctx, execution, call, result)
	emitToolResult(ctx, execution.Callback, call, result)
	return resultForToolCall(call, result, nil, true)
}

func withToolProgressReporter(ctx context.Context, reporter func(ToolProgress)) context.Context {
	if ctx == nil || reporter == nil {
		return ctx
	}
	return context.WithValue(ctx, toolProgressContextKey{}, reporter)
}

func reportToolProgress(ctx context.Context, progress ToolProgress) {
	if ctx == nil {
		return
	}
	if reporter, ok := ctx.Value(toolProgressContextKey{}).(func(ToolProgress)); ok && reporter != nil {
		reporter(progress)
	}
}

func invalidToolCall(action schema.AgentAction, startedAt time.Time) ToolCall {
	action.ToolInput = normalizeToolInput(action.ToolInput)
	toolName := strings.TrimSpace(action.Tool)
	return ToolCall{
		Spec:      ToolSpec{Name: toolName},
		Action:    action,
		Input:     action.ToolInput,
		Content:   toolContentFromAction(action),
		StartedAt: startedAt,
	}
}

func invalidToolResult(call ToolCall) ToolResult {
	toolName := strings.TrimSpace(call.Action.Tool)
	if toolName == "" {
		toolName = strings.TrimSpace(call.Spec.Name)
	}
	var msg string
	if toolName == "" {
		msg = "requested tool is not a valid tool, try another one"
	} else {
		msg = fmt.Sprintf("%s is not a valid tool, try another one", toolName)
	}
	return ToolResult{
		Output:   msg,
		Error:    NewToolError(CodeToolNotFound, msg),
		Duration: time.Since(call.StartedAt),
	}
}

func runBeforeToolCallHook(ctx context.Context, execution ToolCallExecution, call ToolCall) (bool, ToolResult) {
	if execution.Before != nil {
		result, allowed := execution.Before(ctx, call)
		if !allowed {
			return false, result
		}
	}
	if handler, ok := execution.Callback.(toolExecutionHookHandler); ok {
		result, allowed := handler.BeforeToolCall(ctx, call)
		if !allowed {
			return false, result
		}
	}
	return true, ToolResult{}
}

func runAfterToolCallHook(ctx context.Context, execution ToolCallExecution, call ToolCall, result ToolResult) ToolResult {
	if execution.After != nil {
		result = normalizeToolResult(execution.After(ctx, call, result))
	} else {
		result = normalizeToolResult(DefaultAfterToolCall(ctx, call, result))
	}
	if handler, ok := execution.Callback.(toolExecutionHookHandler); ok {
		result = normalizeToolResult(handler.AfterToolCall(ctx, call, result))
	}
	if execution.ResultObserver != nil {
		execution.ResultObserver(ctx, call, result)
	}
	return result
}

func normalizeRejectedToolResult(toolName string, result ToolResult) ToolResult {
	result = normalizeToolResult(result)
	if strings.TrimSpace(result.Output) == "" {
		if result.Error != nil {
			result.Output = result.Error.Message
		} else {
			result.Output = fmt.Sprintf("%s call rejected", toolName)
		}
	}
	if result.Error == nil {
		result.Error = NewToolError(CodeToolExecutionFailed, result.Output)
	}
	// Ensure Output == Error.Message invariant.
	if result.Error != nil && result.Output != result.Error.Message {
		result.Error.Message = result.Output
	}
	return result
}

func normalizeToolResult(result ToolResult) ToolResult {
	if result.Error != nil {
		if result.Output == "" {
			result.Output = result.Error.Message
		} else if result.Output != result.Error.Message {
			result.Error.Message = result.Output
		}
	}
	return result
}

func resultForToolCall(call ToolCall, result ToolResult, err error, actionCompleted bool) ToolCallExecutionResult {
	stepOutput := result.Output
	return ToolCallExecutionResult{
		Call:   call,
		Result: result,
		Step: schema.AgentStep{
			Action:      call.Action,
			Observation: stepOutput,
		},
		Error:           err,
		ActionCompleted: actionCompleted,
	}
}

func emitToolStart(ctx context.Context, handler callbacks.Handler, call ToolCall) {
	if handler == nil {
		return
	}
	if rich, ok := handler.(toolCallStartCallbackHandler); ok {
		rich.HandleToolCallStart(ctx, call)
		return
	}
	if named, ok := handler.(namedToolCallbackHandler); ok {
		named.HandleNamedToolStart(ctx, call.Spec.Name, call.Input)
		return
	}
	handler.HandleToolStart(ctx, call.Input)
}

func emitToolResult(ctx context.Context, handler callbacks.Handler, call ToolCall, result ToolResult) {
	if handler == nil {
		return
	}
	if rich, ok := handler.(toolResultCallbackHandler); ok {
		rich.HandleToolCallResult(ctx, call, result)
		return
	}
	output := result.EventOutput()
	if named, ok := handler.(namedToolCallbackHandler); ok {
		if result.Error != nil {
			named.HandleNamedToolError(ctx, call.Spec.Name, call.Input, result.Error)
			return
		}
		named.HandleNamedToolEnd(ctx, call.Spec.Name, call.Input, output)
		return
	}
	if result.Error != nil {
		handler.HandleToolError(ctx, result.Error)
		return
	}
	handler.HandleToolEnd(ctx, output)
}

func (r ToolResult) EventOutput() string {
	if strings.TrimSpace(r.Summary) != "" {
		return r.Summary
	}
	return r.Output
}

func DefaultBeforeToolCall(ctx context.Context, call ToolCall) (ToolResult, bool) {
	return ToolResult{}, true
}

func DefaultAfterToolCall(ctx context.Context, call ToolCall, result ToolResult) ToolResult {
	result = normalizeToolResult(result)
	if result.Summary == "" {
		if summary, ok := compactScreenshotObservation(call.Spec.Name, result.Output); ok {
			result.Summary = summary
		} else {
			result.Summary, result.SummaryTruncated = compactToolObservationWithStatus(result.Output)
		}
	}
	if call.Spec.Name == "screenshot" || returnsVisualObservation(call.Spec.Tool) {
		result.Terminate = true
	}
	return result
}

func returnsVisualObservation(tool langtools.Tool) bool {
	if visual, ok := tool.(visualObservationTool); ok {
		return visual.ReturnsVisualObservation()
	}
	return false
}
