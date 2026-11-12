from __future__ import annotations

import hashlib
import json
from collections import Counter
from xml.etree import ElementTree


def deduplicate_alert_buttons(text, buttons, source):
    """Collapse nested accessibility aliases only when label, ID and bounds agree."""
    if not isinstance(source, str):
        return buttons
    try:
        root = ElementTree.fromstring(source)
    except ElementTree.ParseError:
        return buttons
    matches = []
    for alert in root.iter("XCUIElementTypeAlert"):
        label = alert.get("label")
        if label and (text == label or text.startswith(label + "\n")):
            candidates = list(alert.iter("XCUIElementTypeButton"))
            if Counter(node.get("label") for node in candidates) == Counter(buttons):
                matches.append(candidates)
    if len(matches) != 1:
        return buttons
    seen, result = set(), []
    for node in matches[0]:
        identity = tuple(node.get(key) for key in ("label", "name", "x", "y", "width", "height"))
        if not all(identity):
            return buttons
        if identity not in seen:
            seen.add(identity)
            result.append(node.get("label"))
    return result


def alert_snapshot(text, buttons):
    if not isinstance(text, str) or not isinstance(buttons, list) or not buttons or not all(isinstance(x, str) and x for x in buttons):
        raise ValueError("WDA returned an invalid alert")
    fingerprint = hashlib.sha256(json.dumps([text, buttons], ensure_ascii=False).encode()).hexdigest()
    return {"present": True, "alert_id": fingerprint, "text": text, "buttons": buttons}


class PermissionPolicy:
    """Explicit prompt signatures; unknown or ambiguous prompts remain visible."""

    def __init__(self, rules=()):
        self.rules = []
        for rule in rules:
            if not isinstance(rule, dict) or set(rule) != {"text_contains", "buttons", "button"}:
                raise ValueError("permission rules require text_contains, buttons, and button")
            text, buttons, button = rule["text_contains"], rule["buttons"], rule["button"]
            if (not isinstance(text, str) or not text.strip() or not isinstance(buttons, list) or
                    len(buttons) < 2 or not all(isinstance(x, str) and x for x in buttons) or
                    not isinstance(button, str) or buttons.count(button) != 1):
                raise ValueError("permission rules require an app-specific prompt text and exactly one occurrence of the selected button")
            # Location alerts can expose the precision toggle twice. Preserve
            # multiplicity while allowing WDA to enumerate buttons in any order.
            self.rules.append({"text_contains": text, "buttons": sorted(buttons), "button": button})

    @classmethod
    def load(cls, path):
        payload = json.loads(path.read_text())
        if not isinstance(payload, dict) or set(payload) != {"rules"} or not isinstance(payload["rules"], list):
            raise ValueError("permission policy must contain a rules array")
        return cls(payload["rules"])

    def choose(self, alert):
        if not alert.get("present"):
            return None
        matches = [rule["button"] for rule in self.rules
                   if rule["text_contains"] in alert["text"] and rule["buttons"] == sorted(alert["buttons"])]
        return matches[0] if len(matches) == 1 else None
