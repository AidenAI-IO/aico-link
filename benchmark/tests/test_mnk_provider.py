import pytest

from mnk_provider import execute_mnk_request, mnk_tool_calls


@pytest.mark.parametrize(
    ("payload", "expected"),
    [
        (
            {
                "operation": "click",
                "click": {"x": 100, "y": 200, "button": "left", "hold_ms": 0},
            },
            [("touch_gesture", {"type": "tap", "point": {"x": 100.0, "y": 200.0}})],
        ),
        (
            {
                "operation": "click",
                "click": {"x": 100, "y": 200, "button": "left", "hold_ms": 600},
            },
            [
                (
                    "touch_gesture",
                    {
                        "type": "long_press",
                        "point": {"x": 100.0, "y": 200.0},
                        "hold_ms": 600,
                    },
                )
            ],
        ),
        (
            {
                "operation": "double_click",
                "double_click": {"x": 300, "y": 400, "button": "left"},
            },
            [
                (
                    "touch_gesture",
                    {"type": "double_tap", "point": {"x": 300.0, "y": 400.0}},
                )
            ],
        ),
        (
            {
                "operation": "swipe",
                "swipe": {"path": [[100, 200], [500, 600]], "button": "left"},
            },
            [
                (
                    "touch_gesture",
                    {
                        "type": "swipe",
                        "start": {"x": 100.0, "y": 200.0},
                        "end": {"x": 500.0, "y": 600.0},
                    },
                ),
            ],
        ),
        (
            {
                "operation": "drag",
                "drag": {"path": [[100, 200], [500, 600]], "button": "left"},
            },
            [
                (
                    "touch_gesture",
                    {
                        "type": "drag",
                        "start": {"x": 100.0, "y": 200.0},
                        "end": {"x": 500.0, "y": 600.0},
                    },
                )
            ],
        ),
        (
            {"operation": "keypress", "keypress": {"keys": ["ctrl", "a"]}},
            [("keyboard_tap", {"keys": ["ctrl", "a"]})],
        ),
        (
            {"operation": "move", "move": {"x": 250, "y": 750}},
            [("mouse_move", {"x": 250.0, "y": 750.0})],
        ),
        (
            {"operation": "scroll", "scroll": {"scroll_x": 0, "scroll_y": -3}},
            [("mouse_scroll", {"delta": -3})],
        ),
    ],
)
def test_mnk_tool_calls(payload, expected):
    assert mnk_tool_calls(payload) == expected


@pytest.mark.parametrize(
    "payload",
    [
        {},
        {"operation": "unknown"},
        {"operation": "click", "click": {"x": -1, "y": 0}},
        {"operation": "click", "click": {"x": 1, "y": 2, "button": "side"}},
        {"operation": "click", "click": {"x": 1, "y": 2, "button": "right"}},
        {
            "operation": "double_click",
            "double_click": {"x": 1, "y": 2, "button": "middle"},
        },
        {"operation": "drag", "drag": {"path": [[0, 0]]}},
        {
            "operation": "swipe",
            "swipe": {"path": [[0, 0], [500, 500], [1000, 1000]], "button": "left"},
        },
        {
            "operation": "drag",
            "drag": {"path": [[0, 0], [1000, 1000]], "button": "right"},
        },
        {"operation": "keypress", "keypress": {"keys": []}},
        {"operation": "scroll", "scroll": {"scroll_x": 1, "scroll_y": 0}},
    ],
)
def test_execute_mnk_request_rejects_invalid_payload(payload):
    status, response = execute_mnk_request(payload, lambda *_: pytest.fail("must not invoke a tool"))
    assert status == 400
    assert response["error"]


@pytest.mark.parametrize("operation", ["swipe", "drag"])
@pytest.mark.parametrize("duration_ms", [-1, 10_001, True, 120.5, "120", None])
def test_execute_mnk_request_rejects_invalid_motion_duration(operation, duration_ms):
    status, response = execute_mnk_request(
        {
            "operation": operation,
            operation: {"path": [[500, 800], [500, 400]], "duration_ms": duration_ms},
        },
        lambda *_: pytest.fail("must not invoke a tool"),
    )

    assert status == 400
    assert "duration_ms" in response["error"]


def test_execute_mnk_request_stops_on_tool_error():
    calls = []

    def invoke(tool_name, tool_input):
        calls.append((tool_name, tool_input))
        return {"is_error": True, "output": "device unavailable"}

    status, response = execute_mnk_request(
        {"operation": "move", "move": {"x": 250, "y": 750}},
        invoke,
    )

    assert status == 500
    assert response == {"error": "device unavailable"}
    assert calls == [("mouse_move", {"x": 250.0, "y": 750.0})]


def test_keyboard_text_and_persistent_drag_dispatch():
    assert mnk_tool_calls({"operation": "keyboard_text", "keyboard_text": {"text": "Aiden test!"}}) == [
        ("keyboard_text", {"text": "Aiden test!"})
    ]
    for operation in ("drag_start", "drag_release"):
        assert mnk_tool_calls({"operation": operation, operation: {"x": 148, "y": 114}}) == [
            ("touch_gesture", {"type": operation, "point": {"x": 148, "y": 114}})
        ]
