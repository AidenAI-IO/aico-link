"""Simulated Aico App for MobileGym benchmark tasks.

In environment-bridge mode the Go Agent forwards its Phone Bridge protocol to
the environment bridge instead of a phone. This module plays the Aico App's
side of that protocol: it reports the connection status a task configures and
executes commands against MobileGym's simulated OS services (clipboard,
contacts provider, calendar store, notifications, app launch).

The simulated app has no launcher icon or UI. Its app state is part of the
task configuration rather than derived from the foreground app.
"""

from __future__ import annotations

import threading
from collections import Counter
from collections.abc import Callable
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any


PLATFORM = "android"
DEFAULT_PHONE_ID = "mobilegym-aico-app"
DEFAULT_COMMAND_TIMEOUT_MS = 5000
DEFAULT_CONTACTS_LIMIT = 20
MAX_CONTACTS_LIMIT = 200

PERMISSION_BY_COMMAND = {
    "calendar_create": "calendar",
    "calendar_query": "calendar",
    "calendar_delete": "calendar",
    "contacts_query": "contacts",
    "contacts_create": "contacts",
    "contacts_update": "contacts",
    "notification_send": "notifications",
}
PERMISSIONS = frozenset(PERMISSION_BY_COMMAND.values())
COMMAND_TYPES = frozenset({"open_app", "clipboard_read", "clipboard_write", *PERMISSION_BY_COMMAND})
# The Android app's foreground service runs data commands while backgrounded;
# launching another app requires the Aico App in the foreground.
BACKGROUND_COMMAND_TYPES = COMMAND_TYPES - {"open_app"}
APP_STATES = ("active", "background")
OPEN_URL_SCHEMES = ("http", "https", "sms", "mailto", "tel")

# Mirrors the code registry in src/agent/internal/agent/errors.go for the codes
# this simulator emits.
ERROR_CATEGORIES = {
    "invalid_arguments": "invalid_input",
    "bridge_not_connected": "precondition_failed",
    "app_not_installed": "precondition_failed",
    "module_unavailable": "precondition_failed",
    "permission_denied": "user_action_required",
    "app_backgrounded": "user_action_required",
    "bridge_timeout": "transient",
    "bridge_connection_closed": "transient",
    "app_launch_failed": "transient",
    "native_module_failed": "internal",
}
DISCONNECT_FAULT = "disconnect"
FAULT_ERRORS = frozenset(
    {"bridge_timeout", "bridge_connection_closed", "app_launch_failed", "native_module_failed", DISCONNECT_FAULT}
)
CONFIG_KEYS = frozenset({"connected", "app_state", "fgs_bridge_enabled", "permissions", "faults"})


@dataclass(frozen=True)
class AicoAppFault:
    command: str
    error: str
    count: int = 1


@dataclass(frozen=True)
class AicoAppConfig:
    connected: bool = True
    app_state: str = "active"
    fgs_bridge_enabled: bool = False
    denied_permissions: frozenset[str] = frozenset()
    faults: tuple[AicoAppFault, ...] = ()


class CommandError(Exception):
    def __init__(self, code: str, message: str, details: dict[str, Any] | None = None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.details = details


def parse_aico_app_config(raw: Any) -> AicoAppConfig | None:
    """Validate a task's ``aico_app`` block; ``None`` means no Aico App."""
    if raw is None:
        return None
    if not isinstance(raw, dict):
        raise ValueError("aico_app must be an object")
    unknown = sorted(set(raw) - CONFIG_KEYS)
    if unknown:
        raise ValueError(f"aico_app has unsupported keys: {', '.join(unknown)}")
    for key in ("connected", "fgs_bridge_enabled"):
        if key in raw and not isinstance(raw[key], bool):
            raise ValueError(f"aico_app.{key} must be a boolean")
    app_state = raw.get("app_state", "active")
    if app_state not in APP_STATES:
        raise ValueError(f"aico_app.app_state must be one of: {', '.join(APP_STATES)}")
    permissions = raw.get("permissions", {})
    if not isinstance(permissions, dict):
        raise ValueError("aico_app.permissions must be an object")
    denied = set()
    for name, value in permissions.items():
        if name not in PERMISSIONS:
            raise ValueError(f"aico_app.permissions has unknown permission: {name}")
        if value not in ("granted", "denied"):
            raise ValueError(f"aico_app.permissions.{name} must be granted or denied")
        if value == "denied":
            denied.add(name)
    faults = raw.get("faults", [])
    if not isinstance(faults, list):
        raise ValueError("aico_app.faults must be a list")
    return AicoAppConfig(
        connected=raw.get("connected", True),
        app_state=app_state,
        fgs_bridge_enabled=raw.get("fgs_bridge_enabled", False),
        denied_permissions=frozenset(denied),
        faults=tuple(_parse_fault(item, index) for index, item in enumerate(faults)),
    )


def _parse_fault(raw: Any, index: int) -> AicoAppFault:
    path = f"aico_app.faults[{index}]"
    if not isinstance(raw, dict) or set(raw) - {"command", "error", "count"}:
        raise ValueError(f"{path} must be an object with command, error and optional count")
    command = raw.get("command")
    if command != "*" and command not in COMMAND_TYPES:
        raise ValueError(f"{path}.command must be a command type or *")
    error = raw.get("error")
    if error not in FAULT_ERRORS:
        raise ValueError(f"{path}.error must be one of: {', '.join(sorted(FAULT_ERRORS))}")
    count = raw.get("count", 1)
    if isinstance(count, bool) or not isinstance(count, int) or count < 1:
        raise ValueError(f"{path}.count must be a positive integer")
    return AicoAppFault(command=command, error=error, count=count)


def error_response(command_id: str, code: str, message: str, details: dict[str, Any] | None = None) -> dict[str, Any]:
    error: dict[str, Any] = {"category": ERROR_CATEGORIES[code], "code": code, "message": message}
    if details:
        error["details"] = details
    return {"id": command_id, "error": error}


class AicoAppSession:
    """Per-task Aico App state: connection, policy, command log and results."""

    def __init__(self, config: AicoAppConfig, phone_id: str = DEFAULT_PHONE_ID):
        self.config = config
        self.phone_id = phone_id
        self._lock = threading.Lock()
        self._connected = config.connected
        self._fault_budget = [fault.count for fault in config.faults]
        self._commands: list[dict[str, Any]] = []
        self._results: dict[str, dict[str, Any]] = {}
        self._scheduled_notifications: list[dict[str, Any]] = []

    def status(self) -> dict[str, Any]:
        """Return the native PhoneBridgeStatus object the Agent proxy decodes."""
        with self._lock:
            connected = self._connected
        status: dict[str, Any] = {
            "connected": connected,
            "platform": PLATFORM,
            "device_type": "Android",
            "phone_id": self.phone_id,
        }
        if connected:
            now = _utc_now()
            status.update(
                last_heartbeat_at=now,
                app_state=self.config.app_state,
                app_state_updated_at=now,
                fgs_bridge_enabled=self.config.fgs_bridge_enabled,
            )
            if self.config.fgs_bridge_enabled:
                status["fgs_bridge_updated_at"] = now
        return status

    def execute(self, command: Any, run: Callable[[dict[str, Any]], dict[str, Any]]) -> dict[str, Any]:
        """Execute one Phone Bridge command; ``run`` evaluates an operation in the page."""
        if not isinstance(command, dict):
            return error_response("", "invalid_arguments", "command must be a JSON object")
        command_id = str(command.get("id") or "").strip()
        if not command_id:
            return error_response("", "invalid_arguments", "command id is required")
        with self._lock:
            cached = self._results.get(command_id)
        if cached is not None:
            # The real app re-sends a completed result for a duplicate id.
            return cached
        response = self._admit(command_id, command)
        if response is None:
            response = self._run(command_id, command, run)
        self._record(command_id, command, response)
        return response

    def has_result(self, command_id: str) -> bool:
        with self._lock:
            return command_id in self._results

    def result(self, command_id: str) -> dict[str, Any] | None:
        with self._lock:
            return self._results.get(command_id)

    def discard(self, command_id: str) -> bool:
        with self._lock:
            return self._results.pop(command_id, None) is not None

    def snapshot(self) -> dict[str, Any]:
        """Return the evaluation view merged into the bridge's /state."""
        with self._lock:
            commands = [dict(entry) for entry in self._commands]
            connected = self._connected
            scheduled = [dict(item) for item in self._scheduled_notifications]
        return {
            "connected": connected,
            "app_state": self.config.app_state,
            "fgs_bridge_enabled": self.config.fgs_bridge_enabled,
            "denied_permissions": sorted(self.config.denied_permissions),
            "commands": commands,
            # Zero entries let suites assert that a command never ran or never failed.
            "command_counts": _counts(entry["type"] for entry in commands),
            "failed_command_counts": _counts(entry["type"] for entry in commands if not entry["ok"]),
            "scheduled_notifications": scheduled,
        }

    def _admit(self, command_id: str, command: dict[str, Any]) -> dict[str, Any] | None:
        command_type = str(command.get("type") or "")
        with self._lock:
            if not self._connected:
                return error_response(command_id, "bridge_not_connected", "Aico App is not connected")
            for index, fault in enumerate(self.config.faults):
                if self._fault_budget[index] <= 0 or fault.command not in ("*", command_type):
                    continue
                self._fault_budget[index] -= 1
                if fault.error == DISCONNECT_FAULT:
                    self._connected = False
                    return error_response(command_id, "bridge_connection_closed", "Aico App disconnected")
                return error_response(command_id, fault.error, f"Simulated {fault.error} fault")
        if command_type not in COMMAND_TYPES:
            return error_response(command_id, "invalid_arguments", f"Unsupported command type: {command_type}")
        if self.config.app_state != "active" and command_type not in BACKGROUND_COMMAND_TYPES:
            return error_response(
                command_id,
                "app_backgrounded",
                "Aico App must be in the foreground for this command",
                {"supported_background_commands": sorted(BACKGROUND_COMMAND_TYPES)},
            )
        permission = PERMISSION_BY_COMMAND.get(command_type)
        if permission in self.config.denied_permissions:
            return error_response(command_id, "permission_denied", f"{permission.capitalize()} access not granted")
        return None

    def _run(
        self,
        command_id: str,
        command: dict[str, Any],
        run: Callable[[dict[str, Any]], dict[str, Any]],
    ) -> dict[str, Any]:
        try:
            operation = build_operation(command)
        except CommandError as exc:
            return error_response(command_id, exc.code, exc.message, exc.details)
        try:
            result = run(operation)
        except TimeoutError:
            return error_response(command_id, "bridge_timeout", "Aico App command timed out")
        except Exception as exc:
            return error_response(command_id, "native_module_failed", f"{type(exc).__name__}: {exc}")
        if not isinstance(result, dict):
            return error_response(command_id, "native_module_failed", "simulator returned no command result")
        error = result.get("error")
        if isinstance(error, dict):
            code = str(error.get("code") or "")
            if code not in ERROR_CATEGORIES:
                code = "native_module_failed"
            return error_response(command_id, code, str(error.get("message") or code), error.get("details"))
        if result.get("scheduled"):
            with self._lock:
                self._scheduled_notifications.append(
                    {
                        "notification_id": result["data"]["notification_id"],
                        "title": operation["title"],
                        "body": operation.get("body"),
                        "schedule_at": command["payload"]["schedule_at"],
                        "schedule_ts": operation["schedule_ts"],
                    }
                )
        response: dict[str, Any] = {"id": command_id}
        if result.get("method"):
            response["method"] = str(result["method"])
        if isinstance(result.get("data"), dict):
            response["data"] = result["data"]
        return response

    def _record(self, command_id: str, command: dict[str, Any], response: dict[str, Any]) -> None:
        error = response.get("error") or {}
        entry = {
            "id": command_id,
            "type": str(command.get("type") or ""),
            "payload": command.get("payload") if isinstance(command.get("payload"), dict) else {},
            "ok": not error,
            "error_code": error.get("code"),
            "method": response.get("method"),
        }
        for key in ("app", "url"):
            if command.get(key):
                entry[key] = command[key]
        with self._lock:
            self._commands.append(entry)
            self._results[command_id] = response


def _counts(command_types: Any) -> dict[str, int]:
    counts = dict.fromkeys(sorted(COMMAND_TYPES), 0)
    counts.update(Counter(command_types))
    return counts


def command_timeout_sec(command: dict[str, Any]) -> float:
    timeout_ms = command.get("timeout_ms")
    if isinstance(timeout_ms, bool) or not isinstance(timeout_ms, (int, float)) or timeout_ms <= 0:
        timeout_ms = DEFAULT_COMMAND_TIMEOUT_MS
    return float(timeout_ms) / 1000


def build_operation(command: dict[str, Any]) -> dict[str, Any]:
    """Validate a command's arguments and normalize them for the page script."""
    command_type = command["type"]
    payload = command.get("payload") or {}
    if not isinstance(payload, dict):
        raise CommandError("invalid_arguments", "payload must be an object")
    operation: dict[str, Any] = {"type": command_type}
    if command_type == "open_app":
        operation.update(_open_app_target(command, payload))
    elif command_type == "clipboard_write":
        operation["text"] = _required_text(payload, "text", allow_empty=True)
    elif command_type == "calendar_create":
        operation.update(
            title=_required_text(payload, "title"),
            start_ts=_timestamp_ms(payload, "start_at"),
            end_ts=_timestamp_ms(payload, "end_at"),
            all_day=_optional_bool(payload, "all_day", False),
            location=_optional_text(payload, "location"),
            notes=_optional_text(payload, "notes"),
        )
        if operation["end_ts"] < operation["start_ts"]:
            raise CommandError("invalid_arguments", "end_at must not be before start_at")
    elif command_type == "calendar_query":
        operation.update(from_ts=_timestamp_ms(payload, "from"), to_ts=_timestamp_ms(payload, "to"))
        if operation["to_ts"] <= operation["from_ts"]:
            raise CommandError("invalid_arguments", "to must be after from")
    elif command_type == "calendar_delete":
        operation["event_id"] = _required_text(payload, "event_id")
    elif command_type == "contacts_query":
        limit = payload.get("limit", DEFAULT_CONTACTS_LIMIT)
        if isinstance(limit, bool) or not isinstance(limit, int) or limit < 1:
            raise CommandError("invalid_arguments", "limit must be a positive integer")
        operation.update(query=_optional_text(payload, "query") or "", limit=min(limit, MAX_CONTACTS_LIMIT))
    elif command_type in ("contacts_create", "contacts_update"):
        if command_type == "contacts_update":
            operation["contact_id"] = _required_text(payload, "contact_id")
        else:
            operation["name"] = _required_text(payload, "name")
        # The Agent sends every field; null, "" and [] mean "leave unchanged".
        for key in ("name", "organization", "notes"):
            if payload.get(key) not in (None, "") and key not in operation:
                operation[key] = _required_text(payload, key)
        for key in ("phone_numbers", "emails"):
            if payload.get(key) not in (None, []):
                operation[key] = _text_list(payload, key)
    elif command_type == "notification_send":
        operation.update(title=_required_text(payload, "title"), body=_optional_text(payload, "body"))
        if payload.get("schedule_at") not in (None, ""):
            operation["schedule_ts"] = _timestamp_ms(payload, "schedule_at")
    return operation


def _open_app_target(command: dict[str, Any], payload: dict[str, Any]) -> dict[str, Any]:
    targets = {
        key: command.get(key) or payload.get(key)
        for key in ("app", "url", "phone_number")
        if command.get(key) or payload.get(key)
    }
    if len(targets) != 1:
        raise CommandError("invalid_arguments", "open_app requires exactly one of app, url or phone_number")
    key, value = next(iter(targets.items()))
    if not isinstance(value, str) or not value.strip():
        raise CommandError("invalid_arguments", f"{key} must be a non-empty string")
    value = value.strip()
    if key == "app":
        return {"app": value}
    url = value if key == "url" else f"tel:{value}"
    scheme = url.split(":", 1)[0].lower() if ":" in url else ""
    if scheme not in OPEN_URL_SCHEMES:
        raise CommandError("invalid_arguments", f"unsupported URL scheme: {scheme or '(none)'}")
    return {"url": url, "scheme": scheme}


def _required_text(payload: dict[str, Any], key: str, *, allow_empty: bool = False) -> str:
    value = payload.get(key)
    if not isinstance(value, str) or (not allow_empty and not value.strip()):
        raise CommandError("invalid_arguments", f"{key} is required")
    return value if allow_empty else value.strip()


def _optional_text(payload: dict[str, Any], key: str) -> str | None:
    value = payload.get(key)
    if value is None:
        return None
    if not isinstance(value, str):
        raise CommandError("invalid_arguments", f"{key} must be a string")
    return value


def _optional_bool(payload: dict[str, Any], key: str, default: bool) -> bool:
    value = payload.get(key, default)
    if not isinstance(value, bool):
        raise CommandError("invalid_arguments", f"{key} must be a boolean")
    return value


def _text_list(payload: dict[str, Any], key: str) -> list[str]:
    value = payload.get(key)
    if not isinstance(value, list) or not all(isinstance(item, str) and item.strip() for item in value):
        raise CommandError("invalid_arguments", f"{key} must be a list of non-empty strings")
    return [item.strip() for item in value]


def _timestamp_ms(payload: dict[str, Any], key: str) -> int:
    value = payload.get(key)
    if not isinstance(value, str) or not value.strip():
        raise CommandError("invalid_arguments", f"{key} is required")
    try:
        parsed = datetime.fromisoformat(value.strip().replace("Z", "+00:00"))
    except ValueError as exc:
        raise CommandError("invalid_arguments", f"{key} must be an RFC3339 time") from exc
    if parsed.tzinfo is None:
        raise CommandError("invalid_arguments", f"{key} must include a timezone offset")
    return int(parsed.timestamp() * 1000)


def _utc_now() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


async def evaluate_operation(env: Any, operation: dict[str, Any]) -> dict[str, Any]:
    """Run one normalized operation against the MobileGym page."""
    page = getattr(env, "page", None)
    if page is None:
        return {"error": {"code": "module_unavailable", "message": "MobileGym page is unavailable"}}
    app_names = {str(name).strip().lower(): app_id for name, app_id in getattr(env, "APP_NAME_MAP", {}).items()}
    return await page.evaluate(PAGE_SCRIPT, {**operation, "app_names": app_names})


# Runs inside the MobileGym page. It uses the simulator's own services so the
# results are visible in the GUI and in __SIM__.getState().
PAGE_SCRIPT = r"""
async (op) => {
  const os = window.__OS__;
  const sim = window.__SIM__;
  const fail = (code, message, details) => ({ error: details ? { code, message, details } : { code, message } });
  if (!os || !sim) return fail('module_unavailable', 'MobileGym OS APIs are not ready');
  const randomId = (prefix) => `${prefix}_aico_${Math.random().toString(36).slice(2, 10)}`;
  // RFC3339 in the simulated phone's time zone, as the real app reports times.
  const iso = (ts) => {
    const d = new Date(ts);
    const pad = (n) => String(Math.abs(n)).padStart(2, '0');
    const offset = -d.getTimezoneOffset();
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
      + `${offset >= 0 ? '+' : '-'}${pad(Math.trunc(offset / 60))}:${pad(offset % 60)}`;
  };
  const contactsUri = 'content://contacts/contacts';
  const contactView = (c) => ({
    contact_id: c.id,
    name: c.displayName,
    phone_numbers: (c.phones || []).map((p) => p.number),
    emails: (c.emails || []).map((e) => e.email),
    ...(c.company ? { organization: c.company } : {}),
    ...(c.notes ? { notes: c.notes } : {}),
  });
  const contactValues = (op) => {
    const values = {};
    if (op.name !== undefined) values.displayName = op.name;
    if (op.phone_numbers !== undefined) {
      values.phones = op.phone_numbers.map((number, i) => ({ number, isPrimary: i === 0 }));
    }
    if (op.emails !== undefined) values.emails = op.emails.map((email, i) => ({ email, isPrimary: i === 0 }));
    if (op.organization !== undefined) values.company = op.organization;
    if (op.notes !== undefined) values.notes = op.notes;
    return values;
  };
  const calendarEvents = () => {
    const calendar = sim.getState().apps.calendar;
    if (!calendar || !Array.isArray(calendar.events)) throw new Error('Calendar store is not loaded');
    return calendar.events;
  };

  switch (op.type) {
    case 'clipboard_read':
      return { data: { text: os.clipboard.getText() ?? '' } };
    case 'clipboard_write':
      os.clipboard.copyText(op.text, 'aico');
      return { method: 'clipboard_write' };
    case 'calendar_create': {
      const event = {
        id: randomId('evt'),
        type: 'event',
        title: op.title,
        allDay: op.all_day,
        startTs: op.start_ts,
        endTs: op.end_ts,
        ...(op.location ? { location: op.location } : {}),
        ...(op.notes ? { description: op.notes } : {}),
      };
      sim.setState({ apps: { calendar: { events: [event, ...calendarEvents()] } } });
      return { data: { event_id: event.id } };
    }
    case 'calendar_query': {
      const events = calendarEvents()
        .filter((e) => e.startTs < op.to_ts && e.endTs >= op.from_ts)
        .sort((a, b) => a.startTs - b.startTs)
        .map((e) => ({
          event_id: e.id,
          title: e.title,
          start_at: iso(e.startTs),
          end_at: iso(e.endTs),
          all_day: Boolean(e.allDay),
          ...(e.location ? { location: e.location } : {}),
          ...(e.description ? { notes: e.description } : {}),
        }));
      return { data: { events } };
    }
    case 'calendar_delete': {
      const events = calendarEvents();
      if (!events.some((e) => e.id === op.event_id)) {
        return fail('invalid_arguments', 'Calendar event not found', { native_code: 'not_found' });
      }
      sim.setState({ apps: { calendar: { events: events.filter((e) => e.id !== op.event_id) } } });
      return { method: 'calendar_delete' };
    }
    case 'contacts_query': {
      const uri = op.query ? `${contactsUri}/search?q=${encodeURIComponent(op.query)}` : contactsUri;
      const contacts = os.content.query(uri).items.slice(0, op.limit).map(contactView);
      return { data: { contacts } };
    }
    case 'contacts_create': {
      const uri = os.content.insert(contactsUri, contactValues(op));
      return { data: { contact_id: String(uri).split('/').pop() } };
    }
    case 'contacts_update': {
      const uri = `${contactsUri}/${encodeURIComponent(op.contact_id)}`;
      if (os.content.update(uri, contactValues(op)) === 0) {
        return fail('invalid_arguments', 'Contact not found', { native_code: 'not_found' });
      }
      return { method: 'contacts_update' };
    }
    case 'notification_send': {
      const notificationId = randomId('n');
      const now = sim.getState().os.time?.timestamp;
      if (op.schedule_ts !== undefined && Number.isFinite(now) && op.schedule_ts > now) {
        return { data: { notification_id: notificationId }, scheduled: true };
      }
      os.notifications.push({ id: notificationId, title: op.title, ...(op.body ? { body: op.body } : {}) });
      return { data: { notification_id: notificationId } };
    }
    case 'open_app': {
      if (op.app) {
        const installed = sim.getState().os.installedApps || [];
        const key = op.app.toLowerCase();
        const appId = op.app_names[key]
          ?? installed.find((a) => a.id.toLowerCase() === key || String(a.name).toLowerCase() === key)?.id;
        if (!appId || !installed.some((a) => a.id === appId)) return fail('app_not_installed', `App not installed: ${op.app}`);
        os.openApp(appId);
        return { method: 'launch_package' };
      }
      const intent = { action: 'ACTION_VIEW', scheme: op.scheme, data: { url: op.url } };
      if (op.scheme === 'tel') {
        // MobileGym's dialer cannot receive a number; show its dial pad so the
        // Agent sees a dialer, as ACTION_DIAL does on a phone.
        const appId = (os.resolveActivity(intent) || [])[0];
        if (!appId) return fail('app_not_installed', 'No app handles tel: URLs');
        os.openApp(appId, appId === 'contacts' ? '/?dialpad=true' : undefined);
        return { method: 'dial' };
      }
      if (!os.startActivity(intent, { newTask: true })) {
        // startActivity needs a calling task, which the home screen lacks. The
        // real app launches from its own task, so open the first handler directly.
        const appId = (os.resolveActivity(intent) || [])[0];
        if (!appId) return fail('app_not_installed', `No app handles ${op.scheme}: URLs`);
        os.openApp(appId);
      }
      return { method: op.scheme === 'tel' ? 'dial' : 'open_url' };
    }
    default:
      return fail('invalid_arguments', `Unsupported command type: ${op.type}`);
  }
}
"""
