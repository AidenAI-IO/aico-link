"""Validate Android reset before model calls and retain focused device evidence."""

from __future__ import annotations

import os
from pathlib import Path

from adbandroid.bridge.adb import ADBAndroidDevice


def main() -> None:
    device = ADBAndroidDevice(os.environ["ANDROID_SERIAL"])
    output = Path("runs") / f"ci-{os.environ['GITHUB_RUN_ID']}-{os.environ['GITHUB_RUN_ATTEMPT']}-android-reset"
    output.mkdir(parents=True, exist_ok=True)
    fields = (
        "mInputShown", "mIsInputViewShown", "mShowRequested", "mInputViewStarted",
        "mCurMethodId", "mImeWindowVis", "mInputViewShown", "mShowInputRequested",
    )

    def capture(stage: str) -> None:
        dump = device._run_text(["shell", "dumpsys", "input_method"], timeout=5)
        state = "\n".join(line for line in dump.splitlines() if any(field in line for field in fields))
        (output / f"{stage}.log").write_text(state + "\n", encoding="utf-8")
        print(f"Android reset {stage}:\n{state}", flush=True)
        jpeg, _, _ = device.screenshot_jpeg()
        (output / f"{stage}.jpg").write_bytes(jpeg)

    capture("before")
    try:
        device.reset_home()
    finally:
        capture("after")


if __name__ == "__main__":
    main()
