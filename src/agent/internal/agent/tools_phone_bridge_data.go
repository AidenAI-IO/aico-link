package agent

import (
	"aiden-agent/internal/ble"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	langtools "github.com/tmc/langchaingo/tools"
)

// nextBridgeCmdID builds a unique command id for a bridge command type. It
// reuses openAppCmdSeq so every outbound bridge command shares one counter.
func nextBridgeCmdID(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixMilli(), openAppCmdSeq.Add(1))
}

// ClipboardTool reads and writes the connected phone's system clipboard.
type ClipboardTool struct {
	bridge   *PhoneBridge
	restorer *PhoneBridgeRestorer
}

func NewClipboardTool(bridge *PhoneBridge, restorer *PhoneBridgeRestorer) *ClipboardTool {
	return &ClipboardTool{bridge: bridge, restorer: restorer}
}

func (t *ClipboardTool) Name() string { return toolBridgeClipboard }

func (t *ClipboardTool) Description() string {
	return `Read or write the connected phone's system clipboard via the phone bridge. ` +
		`Use this when the user explicitly wants clipboard read/write or when a separate clipboard state is the goal. For filling a visible input field, use enter_text; do not manually chain bridge_clipboard with quick_action paste.`
}

func (t *ClipboardTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"action": stringEnumArgSchema("Clipboard action.", "read", "write"),
		"text":   stringArgSchema("Required when action is write. Use an explicit empty string to clear the clipboard."),
	}, "action")
}

type clipboardArgs struct {
	Action string  `json:"action"`
	Text   *string `json:"text"`
}

func (t *ClipboardTool) Call(ctx context.Context, input string) (string, error) {
	var args clipboardArgs
	if err := json.Unmarshal([]byte(strings.TrimSpace(input)), &args); err != nil {
		te := NewToolError(CodeInvalidArguments, fmt.Sprintf("invalid input: %v. Expected JSON like {\"action\":\"read\"} or {\"action\":\"write\",\"text\":\"...\"}", err))
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}

	action := strings.ToLower(strings.TrimSpace(args.Action))
	switch action {
	case "read":
		return t.read(ctx)
	case "write":
		if args.Text == nil {
			te := NewToolError(CodeInvalidArguments, `clipboard write requires a "text" string`)
			SetToolError(ctx, te)
			return toolErrorString(te), nil
		}
		return t.write(ctx, *args.Text)
	default:
		te := NewToolError(CodeInvalidArguments, fmt.Sprintf("unknown action %q, expected \"read\" or \"write\"", args.Action))
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
}

func (t *ClipboardTool) read(ctx context.Context) (string, error) {
	resp, restored, err := sendRoutedBridgeCommand(ctx, t.bridge, t.restorer, BridgeCommand{
		ID:        nextBridgeCmdID("clip_read"),
		Type:      "clipboard_read",
		TimeoutMs: 5000,
	})
	if err != nil {
		te := NewToolError(CodeBridgeNotConnected, err.Error())
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	if resp.Error != nil {
		SetToolError(ctx, resp.Error)
		return toolErrorString(resp.Error), nil
	}
	var data struct {
		Text string `json:"text"`
	}
	if len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			te := NewToolError(CodeToolExecutionFailed, fmt.Sprintf("decode clipboard data: %v", err))
			SetToolError(ctx, te)
			return toolErrorString(te), nil
		}
	}
	result := map[string]interface{}{"ok": true, "text": data.Text}
	if restored {
		result["restored_from_return_entry"] = true
	}
	return jsonString(result), nil
}

func (t *ClipboardTool) write(ctx context.Context, text string) (string, error) {
	payload, _ := json.Marshal(map[string]string{"text": text})
	resp, restored, err := sendRoutedBridgeCommand(ctx, t.bridge, t.restorer, BridgeCommand{
		ID:        nextBridgeCmdID("clip_write"),
		Type:      "clipboard_write",
		Payload:   payload,
		TimeoutMs: 5000,
	})
	if err != nil {
		te := NewToolError(CodeBridgeNotConnected, err.Error())
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	if resp.Error != nil {
		SetToolError(ctx, resp.Error)
		return toolErrorString(resp.Error), nil
	}
	result := map[string]interface{}{"ok": true}
	if restored {
		result["restored_from_return_entry"] = true
	}
	if t.bridge != nil {
		t.bridge.NoteClipboardWrite(text)
	}
	return jsonString(result), nil
}

// CalendarTool creates, queries, and deletes system calendar events on the
// connected phone via the phone bridge.
type CalendarTool struct {
	bridge   *PhoneBridge
	restorer *PhoneBridgeRestorer
}

func NewCalendarTool(bridge *PhoneBridge, restorer *PhoneBridgeRestorer) *CalendarTool {
	return &CalendarTool{bridge: bridge, restorer: restorer}
}

func (t *CalendarTool) Name() string { return toolBridgeCalendar }

func (t *CalendarTool) Description() string {
	return `Create, query, or delete system calendar events on the connected phone via the phone bridge. ` +
		`Times are RFC3339 strings with timezone offset, e.g. "2026-06-02T15:00:00+08:00". Use the connected phone environment timezone when available; otherwise use shell to obtain a controller-time baseline and do not assume it matches the phone timezone. ` +
		`Confirm details with the user before creating or deleting events.`
}

func (t *CalendarTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"action":               stringEnumArgSchema("Calendar action.", "create", "query", "delete"),
		"event_id":             stringArgSchema("Calendar event id for delete."),
		"title":                stringArgSchema("Event title for create."),
		"start_at":             stringArgSchema("Event start time as RFC3339 with timezone."),
		"end_at":               stringArgSchema("Optional event end time as RFC3339 with timezone."),
		"from":                 stringArgSchema("Query start time as RFC3339 with timezone."),
		"to":                   stringArgSchema("Query end time as RFC3339 with timezone."),
		"all_day":              boolArgSchema("Whether the created event is all-day."),
		"location":             stringArgSchema("Optional event location."),
		"notes":                stringArgSchema("Optional event notes."),
		"alarm_minutes_before": minIntegerArgSchema("Reminder offset in minutes before the event.", 0),
	}, "action")
}

type calendarArgs struct {
	Action             string `json:"action"`
	EventID            string `json:"event_id"`
	Title              string `json:"title"`
	StartAt            string `json:"start_at"`
	EndAt              string `json:"end_at"`
	From               string `json:"from"`
	To                 string `json:"to"`
	AllDay             bool   `json:"all_day"`
	Location           string `json:"location"`
	Notes              string `json:"notes"`
	AlarmMinutesBefore int    `json:"alarm_minutes_before"`
}

func (t *CalendarTool) Call(ctx context.Context, input string) (string, error) {
	var args calendarArgs
	if err := json.Unmarshal([]byte(strings.TrimSpace(input)), &args); err != nil {
		te := NewToolError(CodeInvalidArguments, fmt.Sprintf("invalid input: %v. Expected JSON like {\"action\":\"create\",\"title\":\"...\",\"start_at\":\"2026-06-02T15:00:00+08:00\"} or {\"action\":\"query\",\"from\":\"...\",\"to\":\"...\"}", err))
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}

	switch strings.ToLower(strings.TrimSpace(args.Action)) {
	case "create":
		return t.create(ctx, args)
	case "query":
		return t.query(ctx, args)
	case "delete":
		return t.delete(ctx, args)
	default:
		te := NewToolError(CodeInvalidArguments, fmt.Sprintf("unknown action %q, expected \"create\", \"query\", or \"delete\"", args.Action))
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
}

func (t *CalendarTool) create(ctx context.Context, args calendarArgs) (string, error) {
	if strings.TrimSpace(args.Title) == "" {
		te := NewToolError(CodeInvalidArguments, "create requires a title")
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	if strings.TrimSpace(args.StartAt) == "" {
		te := NewToolError(CodeInvalidArguments, "create requires a start_at time (RFC3339)")
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"title":                args.Title,
		"start_at":             args.StartAt,
		"end_at":               args.EndAt,
		"all_day":              args.AllDay,
		"location":             args.Location,
		"notes":                args.Notes,
		"alarm_minutes_before": args.AlarmMinutesBefore,
	})
	resp, restored, err := sendRoutedBridgeCommand(ctx, t.bridge, t.restorer, BridgeCommand{
		ID:        nextBridgeCmdID("cal_create"),
		Type:      "calendar_create",
		Payload:   payload,
		TimeoutMs: 8000,
	})
	if err != nil {
		te := NewToolError(CodeBridgeNotConnected, err.Error())
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	if resp.Error != nil {
		SetToolError(ctx, resp.Error)
		return toolErrorString(resp.Error), nil
	}
	var data struct {
		EventID string `json:"event_id"`
	}
	if len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			te := NewToolError(CodeToolExecutionFailed, fmt.Sprintf("decode calendar data: %v", err))
			SetToolError(ctx, te)
			return toolErrorString(te), nil
		}
	}
	result := map[string]interface{}{"ok": true, "event_id": data.EventID}
	if restored {
		result["restored_from_return_entry"] = true
	}
	return jsonString(result), nil
}

func (t *CalendarTool) query(ctx context.Context, args calendarArgs) (string, error) {
	if strings.TrimSpace(args.From) == "" || strings.TrimSpace(args.To) == "" {
		te := NewToolError(CodeInvalidArguments, "query requires both from and to times (RFC3339)")
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	payload, _ := json.Marshal(map[string]string{"from": args.From, "to": args.To})
	resp, restored, err := sendRoutedBridgeCommand(ctx, t.bridge, t.restorer, BridgeCommand{
		ID:        nextBridgeCmdID("cal_query"),
		Type:      "calendar_query",
		Payload:   payload,
		TimeoutMs: 8000,
	})
	if err != nil {
		te := NewToolError(CodeBridgeNotConnected, err.Error())
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	if resp.Error != nil {
		SetToolError(ctx, resp.Error)
		return toolErrorString(resp.Error), nil
	}
	var data struct {
		Events []map[string]interface{} `json:"events"`
	}
	if len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			te := NewToolError(CodeToolExecutionFailed, fmt.Sprintf("decode calendar data: %v", err))
			SetToolError(ctx, te)
			return toolErrorString(te), nil
		}
	}
	if data.Events == nil {
		data.Events = []map[string]interface{}{}
	}
	result := map[string]interface{}{"ok": true, "events": data.Events}
	if restored {
		result["restored_from_return_entry"] = true
	}
	return jsonString(result), nil
}

func (t *CalendarTool) delete(ctx context.Context, args calendarArgs) (string, error) {
	if strings.TrimSpace(args.EventID) == "" {
		te := NewToolError(CodeInvalidArguments, "delete requires an event_id")
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	payload, _ := json.Marshal(map[string]string{"event_id": args.EventID})
	resp, restored, err := sendRoutedBridgeCommand(ctx, t.bridge, t.restorer, BridgeCommand{
		ID:        nextBridgeCmdID("cal_delete"),
		Type:      "calendar_delete",
		Payload:   payload,
		TimeoutMs: 8000,
	})
	if err != nil {
		te := NewToolError(CodeBridgeNotConnected, err.Error())
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	if resp.Error != nil {
		SetToolError(ctx, resp.Error)
		return toolErrorString(resp.Error), nil
	}
	result := map[string]interface{}{"ok": true}
	if restored {
		result["restored_from_return_entry"] = true
	}
	return jsonString(result), nil
}

// ContactsTool queries, creates, and updates contacts on the connected phone.
type ContactsTool struct {
	bridge   *PhoneBridge
	restorer *PhoneBridgeRestorer
}

func NewContactsTool(bridge *PhoneBridge, restorer *PhoneBridgeRestorer) *ContactsTool {
	return &ContactsTool{bridge: bridge, restorer: restorer}
}

func (t *ContactsTool) Name() string { return toolBridgeContacts }

func (t *ContactsTool) Description() string {
	return `Query, create, or update contacts on the connected phone via the phone bridge. ` +
		`Confirm details with the user before creating or updating contacts.`
}

func (t *ContactsTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"action":        stringEnumArgSchema("Contacts action.", "query", "create", "update"),
		"contact_id":    stringArgSchema("Contact id for update."),
		"query":         stringArgSchema("Search query for contact lookup."),
		"limit":         minIntegerArgSchema("Maximum query results.", 1),
		"name":          stringArgSchema("Contact display name."),
		"phone_numbers": stringArrayArgSchema("Contact phone numbers."),
		"emails":        stringArrayArgSchema("Contact email addresses."),
		"organization":  stringArgSchema("Contact organization."),
		"notes":         stringArgSchema("Contact notes."),
	}, "action")
}

type contactsArgs struct {
	Action       string   `json:"action"`
	ContactID    string   `json:"contact_id"`
	Query        string   `json:"query"`
	Limit        int      `json:"limit"`
	Name         string   `json:"name"`
	PhoneNumbers []string `json:"phone_numbers"`
	Emails       []string `json:"emails"`
	Organization string   `json:"organization"`
	Notes        string   `json:"notes"`
}

type contactsCapabilityTool struct {
	inner       langtools.Tool
	allowQuery  bool
	allowCreate bool
	allowUpdate bool
}

func newContactsCapabilityTool(inner langtools.Tool, allowQuery, allowCreate, allowUpdate bool) langtools.Tool {
	return &contactsCapabilityTool{
		inner:       inner,
		allowQuery:  allowQuery,
		allowCreate: allowCreate,
		allowUpdate: allowUpdate,
	}
}

func (t *contactsCapabilityTool) Name() string { return toolBridgeContacts }

func (t *contactsCapabilityTool) Description() string {
	actions := make([]string, 0, 3)
	if t.allowQuery {
		actions = append(actions, "query contacts")
	}
	if t.allowCreate {
		actions = append(actions, "create contacts")
	}
	if t.allowUpdate {
		actions = append(actions, "update contacts")
	}
	return fmt.Sprintf("Use the connected phone bridge to %s. Confirm details with the user before creating or updating contacts. Unlisted actions are unavailable in the current runtime state.", strings.Join(actions, ", "))
}

func (t *contactsCapabilityTool) ArgsSchema() map[string]any {
	actions := make([]string, 0, 3)
	if t.allowQuery {
		actions = append(actions, "query")
	}
	if t.allowCreate {
		actions = append(actions, "create")
	}
	if t.allowUpdate {
		actions = append(actions, "update")
	}
	properties := map[string]any{
		"action":        stringEnumArgSchema("Contacts action.", actions...),
		"contact_id":    stringArgSchema("Contact id for update."),
		"query":         stringArgSchema("Search query for contact lookup."),
		"limit":         minIntegerArgSchema("Maximum query results.", 1),
		"name":          stringArgSchema("Contact display name."),
		"phone_numbers": stringArrayArgSchema("Contact phone numbers."),
		"emails":        stringArrayArgSchema("Contact email addresses."),
		"organization":  stringArgSchema("Contact organization."),
		"notes":         stringArgSchema("Contact notes."),
	}
	if !t.allowUpdate {
		delete(properties, "contact_id")
	}
	return objectArgsSchema(properties, "action")
}

func (t *contactsCapabilityTool) Call(ctx context.Context, input string) (string, error) {
	var args contactsArgs
	if err := json.Unmarshal([]byte(strings.TrimSpace(input)), &args); err == nil {
		action := strings.ToLower(strings.TrimSpace(args.Action))
		known := action == "query" || action == "create" || action == "update"
		allowed := !known ||
			(action == "query" && t.allowQuery) ||
			(action == "create" && t.allowCreate) ||
			(action == "update" && t.allowUpdate)
		if known && !allowed {
			te := NewToolError(CodeModuleUnavailable, fmt.Sprintf("contacts action %s is unavailable in the current phone bridge state", action))
			SetToolError(ctx, te)
			return toolErrorString(te), nil
		}
	}
	return t.inner.Call(ctx, input)
}

func (t *ContactsTool) Call(ctx context.Context, input string) (string, error) {
	var args contactsArgs
	if err := json.Unmarshal([]byte(strings.TrimSpace(input)), &args); err != nil {
		te := NewToolError(CodeInvalidArguments, fmt.Sprintf("invalid input: %v. Expected JSON format: {\"action\":\"query\",\"query\":\"name\",\"limit\":20} or {\"action\":\"create\",\"name\":\"...\",\"phone_numbers\":[\"...\"],\"emails\":[\"...\"]} or {\"action\":\"update\",\"contact_id\":\"...\",\"name\":\"...\"}. Arrays must use square brackets", err))
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}

	switch strings.ToLower(strings.TrimSpace(args.Action)) {
	case "query":
		return t.query(ctx, args)
	case "create":
		return t.create(ctx, args)
	case "update":
		return t.update(ctx, args)
	default:
		te := NewToolError(CodeInvalidArguments, fmt.Sprintf("unknown action %q, expected \"query\", \"create\", or \"update\"", args.Action))
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
}

func (t *ContactsTool) query(ctx context.Context, args contactsArgs) (string, error) {
	limit := args.Limit
	if limit <= 0 {
		limit = 20
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"query": args.Query,
		"limit": limit,
	})
	resp, restored, err := sendRoutedBridgeCommand(ctx, t.bridge, t.restorer, BridgeCommand{
		ID:        nextBridgeCmdID("contacts_query"),
		Type:      "contacts_query",
		Payload:   payload,
		TimeoutMs: 8000,
	})
	if err != nil {
		te := NewToolError(CodeBridgeNotConnected, err.Error())
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	result := map[string]interface{}{"ok": true}
	if restored {
		result["restored_from_return_entry"] = true
	}
	if resp.Error != nil {
		SetToolError(ctx, resp.Error)
		return toolErrorString(resp.Error), nil
	}
	var data struct {
		Contacts []map[string]interface{} `json:"contacts"`
	}
	if len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			te := NewToolError(CodeToolExecutionFailed, fmt.Sprintf("decode contacts data: %v", err))
			SetToolError(ctx, te)
			return toolErrorString(te), nil
		}
	}
	if data.Contacts == nil {
		data.Contacts = []map[string]interface{}{}
	}
	result["contacts"] = data.Contacts
	return jsonString(result), nil
}

func (t *ContactsTool) create(ctx context.Context, args contactsArgs) (string, error) {
	if strings.TrimSpace(args.Name) == "" {
		te := NewToolError(CodeInvalidArguments, "create requires a name")
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"name":          args.Name,
		"phone_numbers": args.PhoneNumbers,
		"emails":        args.Emails,
		"organization":  args.Organization,
		"notes":         args.Notes,
	})
	resp, restored, err := sendRoutedBridgeCommand(ctx, t.bridge, t.restorer, BridgeCommand{
		ID:        nextBridgeCmdID("contacts_create"),
		Type:      "contacts_create",
		Payload:   payload,
		TimeoutMs: 8000,
	})
	if err != nil {
		te := NewToolError(CodeBridgeNotConnected, err.Error())
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	result := map[string]interface{}{"ok": true}
	if restored {
		result["restored_from_return_entry"] = true
	}
	if resp.Error != nil {
		SetToolError(ctx, resp.Error)
		return toolErrorString(resp.Error), nil
	}
	var data struct {
		ContactID string `json:"contact_id"`
	}
	if len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			te := NewToolError(CodeToolExecutionFailed, fmt.Sprintf("decode contacts data: %v", err))
			SetToolError(ctx, te)
			return toolErrorString(te), nil
		}
	}
	result["contact_id"] = data.ContactID
	return jsonString(result), nil
}

func (t *ContactsTool) update(ctx context.Context, args contactsArgs) (string, error) {
	if strings.TrimSpace(args.ContactID) == "" {
		te := NewToolError(CodeInvalidArguments, "update requires a contact_id")
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"contact_id":    args.ContactID,
		"name":          args.Name,
		"phone_numbers": args.PhoneNumbers,
		"emails":        args.Emails,
		"organization":  args.Organization,
		"notes":         args.Notes,
	})
	resp, restored, err := sendRoutedBridgeCommand(ctx, t.bridge, t.restorer, BridgeCommand{
		ID:        nextBridgeCmdID("contacts_update"),
		Type:      "contacts_update",
		Payload:   payload,
		TimeoutMs: 8000,
	})
	if err != nil {
		te := NewToolError(CodeBridgeNotConnected, err.Error())
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	result := map[string]interface{}{"ok": true}
	if restored {
		result["restored_from_return_entry"] = true
	}
	if resp.Error != nil {
		SetToolError(ctx, resp.Error)
		return toolErrorString(resp.Error), nil
	}
	return jsonString(result), nil
}

// NotificationTool sends local notifications through Phone Bridge and reads
// shared system-notification events retained by ble_service.
type NotificationTool struct {
	bridge       *PhoneBridge
	restorer     *PhoneBridgeRestorer
	eventsReader func(context.Context, string, string, string, int) (ble.EventPage, error)
	statusReader func(context.Context, string) (ble.RuntimeStatus, error)
	socketPath   func() string
}

func NewNotificationTool(bridge *PhoneBridge, restorer *PhoneBridgeRestorer) *NotificationTool {
	return &NotificationTool{
		bridge:       bridge,
		restorer:     restorer,
		eventsReader: ble.RequestEvents,
		statusReader: ble.RequestStatus,
		socketPath:   configuredBLEServiceSocketPath,
	}
}

func (t *NotificationTool) Name() string { return toolBridgeNotification }

func (t *NotificationTool) Description() string {
	return `Send local notifications through the companion app or query shared phone system-notification events from the board's BLE notification ring. ` +
		`Send format: {"action":"send","title":"Reminder","body":"Time to take medicine","sound":true}. ` +
		`Query format: {"action":"query","limit":20}. ` +
		`Use action=send to remind the user or bring the companion app back to foreground. ` +
		`Use action=query only when the user asks to inspect notifications; query does not require the companion app to be foregrounded. ` +
		`For incremental queries, also pass the previous last_id as since together with the previous generation.`
}

func (t *NotificationTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"action":      stringEnumArgSchema("Notification action. send is the default for backwards compatibility.", "send", "query"),
		"title":       stringArgSchema("Notification title."),
		"body":        stringArgSchema("Notification body."),
		"schedule_at": stringArgSchema("Optional scheduled send time as RFC3339 with timezone; if omitted, sent immediately."),
		"sound":       boolArgSchema("Whether to play a sound."),
		"badge":       minIntegerArgSchema("Optional app badge count.", 0),
		"since":       stringArgSchema("Optional query cursor. Omit it for the most recent retained notifications, use 0 to page from the oldest retained event, or use the previous last_id for incremental reads."),
		"generation":  stringArgSchema("Generation returned by a previous query. Include it with a non-zero since cursor."),
		"limit":       rangedIntegerArgSchema("Maximum notification events to return.", 1, 100),
	})
}

type notificationCapabilityTool struct {
	inner      langtools.Tool
	allowSend  bool
	allowQuery bool
}

func newNotificationCapabilityTool(inner langtools.Tool, allowSend, allowQuery bool) langtools.Tool {
	return &notificationCapabilityTool{inner: inner, allowSend: allowSend, allowQuery: allowQuery}
}

func (t *notificationCapabilityTool) Name() string { return toolBridgeNotification }

func (t *notificationCapabilityTool) Description() string {
	sendFormat := `Send format: {"action":"send","title":"Reminder","body":"Time to take medicine","sound":true}.`
	queryFormat := `Query format: {"action":"query","limit":20}.`
	switch {
	case t.allowSend && t.allowQuery:
		return "Send local notifications through the companion app or query shared phone system-notification events from the board's BLE notification ring. " +
			sendFormat + " " + queryFormat +
			" Query does not require the companion app to be foregrounded. For incremental queries, also pass the previous last_id as since together with the previous generation."
	case t.allowSend:
		return "Send local notifications through the companion app. " + sendFormat
	case t.allowQuery:
		return "Query shared phone system-notification events from the board's BLE notification ring. " + queryFormat +
			" Query does not require the companion app to be foregrounded. For incremental queries, also pass the previous last_id as since together with the previous generation."
	default:
		return "Notification actions are currently unavailable."
	}
}

func (t *notificationCapabilityTool) ArgsSchema() map[string]any {
	switch {
	case t.allowSend && t.allowQuery:
		if structured, ok := t.inner.(structuredInputTool); ok {
			return structured.ArgsSchema()
		}
	case t.allowSend:
		return objectArgsSchema(map[string]any{
			"action":      stringEnumArgSchema("Notification action.", "send"),
			"title":       stringArgSchema("Notification title."),
			"body":        stringArgSchema("Notification body."),
			"schedule_at": stringArgSchema("Optional scheduled send time as RFC3339 with timezone; if omitted, sent immediately."),
			"sound":       boolArgSchema("Whether to play a sound."),
			"badge":       minIntegerArgSchema("Optional app badge count.", 0),
		}, "title")
	case t.allowQuery:
		return objectArgsSchema(map[string]any{
			"action":     stringEnumArgSchema("Notification action.", "query"),
			"since":      stringArgSchema("Optional query cursor. Omit it for recent retained notifications, use 0 for the oldest event, or pass the previous last_id for incremental reads."),
			"generation": stringArgSchema("Generation returned by a previous query. Include it with a non-zero since cursor."),
			"limit":      rangedIntegerArgSchema("Maximum notification events to return.", 1, 100),
		}, "action")
	}
	return objectArgsSchema(map[string]any{})
}

func (t *notificationCapabilityTool) Call(ctx context.Context, input string) (string, error) {
	var args notificationArgs
	if err := json.Unmarshal([]byte(strings.TrimSpace(input)), &args); err == nil {
		action := strings.ToLower(strings.TrimSpace(args.Action))
		if action == "" {
			if t.allowQuery && !t.allowSend {
				action = "query"
				args.Action = action
				if encoded, err := json.Marshal(args); err == nil {
					input = string(encoded)
				}
			} else {
				action = "send"
			}
		}
		if (action == "send" && !t.allowSend) || (action == "query" && !t.allowQuery) {
			te := NewToolError(CodeModuleUnavailable, fmt.Sprintf("notification action %s is not available in the current runtime state", action))
			SetToolError(ctx, te)
			return toolErrorString(te), nil
		}
	}
	return t.inner.Call(ctx, input)
}

type notificationArgs struct {
	Action     string `json:"action"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	ScheduleAt string `json:"schedule_at"`
	Sound      bool   `json:"sound"`
	Badge      int    `json:"badge"`
	Since      string `json:"since"`
	Generation string `json:"generation"`
	Limit      int    `json:"limit"`
}

func (t *NotificationTool) Call(ctx context.Context, input string) (string, error) {
	var args notificationArgs
	if err := json.Unmarshal([]byte(strings.TrimSpace(input)), &args); err != nil {
		te := NewToolError(CodeInvalidArguments, fmt.Sprintf("invalid input: %v. Expected action=send with a title, or action=query with optional since, generation, and limit", err))
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	action := strings.ToLower(strings.TrimSpace(args.Action))
	if action == "" {
		action = "send"
	}
	if action == "query" {
		return t.query(ctx, args)
	}
	if action != "send" {
		te := NewToolError(CodeInvalidArguments, "notification action must be send or query")
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}

	if strings.TrimSpace(args.Title) == "" {
		te := NewToolError(CodeInvalidArguments, "notification send requires a title")
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"title":       args.Title,
		"body":        args.Body,
		"schedule_at": args.ScheduleAt,
		"sound":       args.Sound,
		"badge":       args.Badge,
	})
	resp, restored, err := sendRoutedBridgeCommand(ctx, t.bridge, t.restorer, BridgeCommand{
		ID:        nextBridgeCmdID("notification"),
		Type:      "notification_send",
		Payload:   payload,
		TimeoutMs: 5000,
	})
	if err != nil {
		te := NewToolError(CodeBridgeNotConnected, err.Error())
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	result := map[string]interface{}{"ok": true}
	if restored {
		result["restored_from_return_entry"] = true
	}
	if resp.Error != nil {
		SetToolError(ctx, resp.Error)
		return toolErrorString(resp.Error), nil
	}
	var data struct {
		NotificationID string `json:"notification_id"`
	}
	if len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			te := NewToolError(CodeToolExecutionFailed, fmt.Sprintf("decode notification data: %v", err))
			SetToolError(ctx, te)
			return toolErrorString(te), nil
		}
	}
	result["notification_id"] = data.NotificationID
	return jsonString(result), nil
}

func (t *NotificationTool) query(ctx context.Context, args notificationArgs) (string, error) {
	since := strings.TrimSpace(args.Since)
	limit := args.Limit
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 {
		te := NewToolError(CodeInvalidArguments, "notification query limit must be between 1 and 100")
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	if t == nil || t.eventsReader == nil || t.statusReader == nil || t.socketPath == nil {
		te := NewToolError(CodeModuleUnavailable, "BLE notification reader is not configured")
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	generation := strings.TrimSpace(args.Generation)
	queryMode := "cursor"
	if since == "" {
		queryMode = "latest"
		status, err := t.statusReader(ctx, t.socketPath())
		if err != nil {
			te := NewToolError(CodeToolExecutionFailed, fmt.Sprintf("inspect shared notification cursor: %v", err))
			SetToolError(ctx, te)
			return toolErrorString(te), nil
		}
		last, err := strconv.ParseUint(defaultString(status.LastEventID, "0"), 10, 64)
		if err != nil {
			te := NewToolError(CodeToolExecutionFailed, fmt.Sprintf("decode shared notification cursor: %v", err))
			SetToolError(ctx, te)
			return toolErrorString(te), nil
		}
		if last > uint64(limit) {
			since = strconv.FormatUint(last-uint64(limit), 10)
		} else {
			since = "0"
		}
		generation = status.EventGeneration
	} else {
		if _, err := strconv.ParseUint(since, 10, 64); err != nil {
			te := NewToolError(CodeInvalidArguments, "notification query since must be a non-negative decimal cursor")
			SetToolError(ctx, te)
			return toolErrorString(te), nil
		}
		if since != "0" && generation == "" {
			te := NewToolError(CodeInvalidArguments, "notification query generation is required with a non-zero since cursor")
			SetToolError(ctx, te)
			return toolErrorString(te), nil
		}
	}
	page, err := t.eventsReader(ctx, t.socketPath(), since, generation, limit)
	if err != nil {
		te := NewToolError(CodeToolExecutionFailed, fmt.Sprintf("query shared notifications: %v", err))
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	return jsonString(map[string]any{
		"ok":             true,
		"action":         "query",
		"query_mode":     queryMode,
		"since":          since,
		"events":         page.Events,
		"generation":     page.Generation,
		"reset_required": page.ResetRequired,
		"truncated":      page.Truncated,
		"oldest_id":      page.OldestID,
		"last_id":        page.LastID,
	}), nil
}
