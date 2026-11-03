import base64
import json
from unittest.mock import patch

from runner.capture import take_environment_screenshot


class FakeResponse:
    def __init__(self, body: dict):
        self.status = 200
        self._body = json.dumps(body).encode("utf-8")

    def read(self):
        return self._body

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


def test_take_environment_screenshot_writes_screen_snapshot(tmp_path):
    seen = {}
    image = b"jpeg-bytes"
    body = {
        "ok": True,
        "data": {
            "meta": {
                "width": 100,
                "height": 200,
                "pixel_format": "jpeg",
            },
            "capture_info": {"capture_backend": "adb"},
            "image": base64.b64encode(image).decode("ascii"),
        },
    }

    def fake_urlopen(req, timeout=None):
        seen["url"] = req.full_url
        seen["method"] = req.get_method()
        seen["headers"] = {k.lower(): v for k, v in req.header_items()}
        seen["timeout"] = timeout
        return FakeResponse(body)

    out = tmp_path / "post.jpg"
    with patch("urllib.request.urlopen", fake_urlopen):
        width, height = take_environment_screenshot(
            "http://127.0.0.1:19090",
            out,
            benchmark_task_id="suite.json:t1",
            timeout=7,
        )

    assert seen["method"] == "POST"
    assert seen["url"] == "http://127.0.0.1:19090/api/providers/screenshot"
    assert seen["headers"]["benchmark-task-id"] == "suite.json:t1"
    assert seen["headers"]["content-type"] == "application/json"
    assert seen["timeout"] == 7
    assert out.read_bytes() == image
    assert (width, height) == (100, 200)
