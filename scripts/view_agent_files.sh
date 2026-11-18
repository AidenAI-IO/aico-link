#!/bin/bash
# Generate agent files visualization report on Luckfox device
# This script should be deployed to the device

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUTPUT="${1:-/userdata/agent/files_report.html}"
DATA_DIR="${2:-/userdata/agent}"

echo "Generating agent files report..."
python3 "$SCRIPT_DIR/generate_agent_files_report.py" \
    --memory-dir "$DATA_DIR/memory" \
    --skills-dir "$DATA_DIR/skills" \
    --skill-state-dir "$DATA_DIR/skill-state" \
    --output "$OUTPUT"

if [ $? -eq 0 ]; then
    echo ""
    echo "✓ Report generated successfully!"
    echo "  Location: $OUTPUT"
    echo ""
    echo "To view the report:"
    echo "  1. Access via web: http://<device-ip>:8080/user_files"
    echo "  2. Or download and open locally"
fi
