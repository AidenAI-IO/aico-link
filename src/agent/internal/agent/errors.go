package agent

import (
	"context"
	"errors"
	"fmt"
)

// Category enumerates the six fixed error categories used by the LLM and
// downstream consumers to decide what to do about a failure. The set is
// deliberately small — adding a 7th requires a spec amendment.
const (
	CategoryInvalidInput       = "invalid_input"
	CategoryPreconditionFailed = "precondition_failed"
	CategoryUserActionRequired = "user_action_required"
	CategoryUnsupported        = "unsupported"
	CategoryTransient          = "transient"
	CategoryInternal           = "internal"
)

// Stable error codes. Wire consumers (LLM, aico-app, telemetry) may switch
// on these. Adding a new code requires a registry entry below.
// The code set comes from a real inventory of error sites in the codebase
// (tool_execution.go, phone_bridge.go, tools_phone_bridge.go,
// tools_quick_actions.go, aico-app/src/services/PhoneBridge.ts).
const (
	// invalid_input — caller can fix args and retry
	CodeInvalidArguments   = "invalid_arguments"
	CodeToolNotFound       = "tool_not_found"
	CodeUnknownApp         = "unknown_app"
	CodeQuickActionUnknown = "quick_action_unknown"
	CodeAppNoLaunchTarget  = "app_no_launch_target"

	// precondition_failed — environment not ready; try alternative or set up
	CodeBridgeNotConnected = "bridge_not_connected"
	CodeAppNotInstalled    = "app_not_installed"
	CodeModuleUnavailable  = "module_unavailable"

	// user_action_required — needs human in the loop
	CodePermissionDenied = "permission_denied"
	CodeAppBackgrounded  = "app_backgrounded"

	// unsupported — change path; do not retry the same tool
	CodeQuickActionReserved            = "quick_action_reserved"
	CodeQuickActionUnsupportedPlatform = "quick_action_unsupported_platform"

	// transient — retry may help
	CodeBridgeTimeout          = "bridge_timeout"
	CodeBridgeWriteFailed      = "bridge_write_failed"
	CodeBridgeConnectionClosed = "bridge_connection_closed"
	CodeAppLaunchFailed        = "app_launch_failed"
	CodeCanceled               = "canceled"
	CodeDeadlineExceeded       = "deadline_exceeded"

	// internal — bug or protocol violation; record and surface
	CodeCommandMarshalFailed      = "command_marshal_failed"
	CodeCommandIDCollision        = "command_id_collision"
	CodeQuickActionInvalidBinding = "quick_action_invalid_binding"
	CodeSubtoolFailed             = "subtool_failed" // category overridden to inherit sub-tool category at construction
	CodeToolExecutionFailed       = "tool_execution_failed"
	CodeNativeModuleFailed        = "native_module_failed"
	CodeUnknownErrorCode          = "unknown_error_code"
)

// ToolErrorSpec is the registry entry for a code.
type ToolErrorSpec struct {
	Category string
	Severity string // "info" | "warning" | "error"; telemetry only, never on the wire
}

// codeRegistry is the single source of truth for code → category mapping.
// Adding a new code MUST add a row here in the same PR.
var codeRegistry = map[string]ToolErrorSpec{
	// invalid_input
	CodeInvalidArguments:   {Category: CategoryInvalidInput, Severity: "warning"},
	CodeToolNotFound:       {Category: CategoryInvalidInput, Severity: "warning"},
	CodeUnknownApp:         {Category: CategoryInvalidInput, Severity: "warning"},
	CodeQuickActionUnknown: {Category: CategoryInvalidInput, Severity: "warning"},
	CodeAppNoLaunchTarget:  {Category: CategoryInvalidInput, Severity: "warning"},

	// precondition_failed
	CodeBridgeNotConnected: {Category: CategoryPreconditionFailed, Severity: "warning"},
	CodeAppNotInstalled:    {Category: CategoryPreconditionFailed, Severity: "warning"},
	CodeModuleUnavailable:  {Category: CategoryPreconditionFailed, Severity: "warning"},

	// user_action_required
	CodePermissionDenied: {Category: CategoryUserActionRequired, Severity: "warning"},
	CodeAppBackgrounded:  {Category: CategoryUserActionRequired, Severity: "info"},

	// unsupported
	CodeQuickActionReserved:            {Category: CategoryUnsupported, Severity: "info"},
	CodeQuickActionUnsupportedPlatform: {Category: CategoryUnsupported, Severity: "info"},

	// transient
	CodeBridgeTimeout:          {Category: CategoryTransient, Severity: "warning"},
	CodeBridgeWriteFailed:      {Category: CategoryTransient, Severity: "warning"},
	CodeBridgeConnectionClosed: {Category: CategoryTransient, Severity: "warning"},
	CodeAppLaunchFailed:        {Category: CategoryTransient, Severity: "warning"},
	CodeCanceled:               {Category: CategoryTransient, Severity: "info"},
	CodeDeadlineExceeded:       {Category: CategoryTransient, Severity: "warning"},

	// internal
	CodeCommandMarshalFailed:      {Category: CategoryInternal, Severity: "error"},
	CodeCommandIDCollision:        {Category: CategoryInternal, Severity: "error"},
	CodeQuickActionInvalidBinding: {Category: CategoryInternal, Severity: "error"},
	CodeSubtoolFailed:             {Category: CategoryInternal, Severity: "warning"}, // category overridden at construction
	CodeToolExecutionFailed:       {Category: CategoryInternal, Severity: "error"},
	CodeNativeModuleFailed:        {Category: CategoryInternal, Severity: "error"},
	CodeUnknownErrorCode:          {Category: CategoryInternal, Severity: "error"},
}

// ToolError is the canonical structured-error shape exchanged across the
// agent, the phone-bridge wire protocol, and quick_action aggregation.
// Fields are minimal by design: the LLM only ever sees Message (via
// ToolResult.Output); Category and Code exist for non-LLM consumers
// (telemetry, app UI, future programmatic policies). User-facing copy is
// produced app-side via i18n(code); retry/recovery policy is implied by
// Category, not a separate field.
type ToolError struct {
	Category string         `json:"category"`
	Code     string         `json:"code"`
	Message  string         `json:"message"`
	Details  map[string]any `json:"details,omitempty"`
}

// NewToolError constructs a ToolError, deriving Category from the registry.
// Unknown codes degrade to category=internal, code=unknown_error_code,
// preserving the original code in Details for forensics.
func NewToolError(code, message string) *ToolError {
	spec, ok := codeRegistry[code]
	if !ok {
		return &ToolError{
			Category: CategoryInternal,
			Code:     CodeUnknownErrorCode,
			Message:  message,
			Details:  map[string]any{"original_code": code},
		}
	}
	return &ToolError{Category: spec.Category, Code: code, Message: message}
}

// NewToolErrorWithDetails is NewToolError plus a Details map. A nil details
// arg is treated as empty.
func NewToolErrorWithDetails(code, message string, details map[string]any) *ToolError {
	e := NewToolError(code, message)
	if details != nil {
		if e.Details == nil {
			e.Details = map[string]any{}
		}
		for k, v := range details {
			e.Details[k] = v
		}
	}
	return e
}

// Error makes ToolError implement the error interface so it can be returned
// alongside Go's error idioms when convenient.
func (e *ToolError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// toolErrorSlot is the heap-allocated container used by WithToolError to give
// tools a way to attach a structured error to their call context. The slot
// lives in the context but is mutated in place so the caller can read the
// final value after the tool returns.
type toolErrorSlot struct{ err *ToolError }

type toolErrorCtxKey struct{}

// WithToolError attaches an empty ToolError slot to ctx and returns the new
// context plus a setter. Tools that want to surface a structured failure call
// the setter; the executor reads it back via ToolErrorFromContext after the
// tool returns. The setter is safe to call zero or one times.
func WithToolError(ctx context.Context) (context.Context, func(*ToolError)) {
	slot := &toolErrorSlot{}
	ctx = context.WithValue(ctx, toolErrorCtxKey{}, slot)
	return ctx, func(e *ToolError) { slot.err = e }
}

// ToolErrorFromContext returns the structured error a tool attached to ctx, or
// nil if none was set (or no slot was installed).
func ToolErrorFromContext(ctx context.Context) *ToolError {
	if slot, ok := ctx.Value(toolErrorCtxKey{}).(*toolErrorSlot); ok {
		return slot.err
	}
	return nil
}

// SetToolError stores e in the context's slot if one was installed by
// WithToolError. Tools call this just before returning their LLM-facing string
// so the executor can promote the structured error onto ToolResult.Error.
func SetToolError(ctx context.Context, e *ToolError) {
	if slot, ok := ctx.Value(toolErrorCtxKey{}).(*toolErrorSlot); ok {
		slot.err = e
	}
}

// toolErrorString is the LLM-facing string a tool should return alongside
// SetToolError. By construction it equals e.Message, preserving the
// Output == Error.Message invariant.
func toolErrorString(e *ToolError) string {
	if e == nil {
		return ""
	}
	return e.Message
}

func toolErrorResultString(ctx context.Context, code, message string) string {
	te := NewToolError(code, message)
	SetToolError(ctx, te)
	return toolErrorString(te)
}

func toolErrorResultf(ctx context.Context, code, format string, args ...any) string {
	return toolErrorResultString(ctx, code, fmt.Sprintf(format, args...))
}

func cloneToolError(e *ToolError) *ToolError {
	if e == nil {
		return nil
	}
	clone := *e
	if len(e.Details) > 0 {
		clone.Details = make(map[string]any, len(e.Details))
		for k, v := range e.Details {
			clone.Details[k] = v
		}
	}
	return &clone
}

func contextError(ctx context.Context, err error) error {
	if ctx != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}
