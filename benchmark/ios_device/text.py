"""Bound native typing to leave headroom within WDA and Agent request deadlines."""
from mnk_provider import validate_insert_text


MAX_TEXT_UTF16_UNITS = 512
TYPING_FREQUENCY = 60


def validate_ios_text(text):
    """Reject oversized input before replacement or native typing can start."""
    validate_insert_text(text)
    # At the explicit 60 keys/s rate, 512 units need about 8.6 seconds. Keep
    # ample room for keyboard synchronization within the 25s WDA/30s Agent
    # deadlines. Mirroring retains the shared 4096-unit limit.
    if len(text.encode("utf-16-le")) // 2 > MAX_TEXT_UTF16_UNITS:
        raise ValueError(f"direct iOS input accepts at most {MAX_TEXT_UTF16_UNITS} UTF-16 units per request; no text was sent")
