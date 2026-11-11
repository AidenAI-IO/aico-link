#!/usr/bin/env bash
# The host helper follows Config Web's default-off USB Internet switch.
set -euo pipefail
exec python3 "$(dirname "$0")/usb_internet_macos.py" "$@"
