#!/bin/bash
# Deploy agent files visualization to Luckfox device

set -e

# Check for device IP
if [ -z "$1" ]; then
    echo "Usage: $0 <device-ip>"
    echo "Example: $0 192.168.2.2"
    exit 1
fi

DEVICE_IP="$1"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "Deploying agent files visualization to $DEVICE_IP..."

# Test connection
if ! ping -c 1 -W 2 "$DEVICE_IP" > /dev/null 2>&1; then
    echo "Error: Cannot reach device at $DEVICE_IP"
    exit 1
fi

# Create target directory on device
echo "Creating target directory..."
ssh root@"$DEVICE_IP" "mkdir -p /usr/share/aico/agent-tools"

# Copy files
echo "Copying files..."
scp "$SCRIPT_DIR/generate_agent_files_report.py" \
    root@"$DEVICE_IP":/usr/share/aico/agent-tools/

scp "$SCRIPT_DIR/agent_files_template.html" \
    root@"$DEVICE_IP":/usr/share/aico/agent-tools/

scp "$SCRIPT_DIR/view_agent_files.sh" \
    root@"$DEVICE_IP":/usr/share/aico/agent-tools/

# Make script executable
echo "Setting permissions..."
ssh root@"$DEVICE_IP" "chmod +x /usr/share/aico/agent-tools/*.sh /usr/share/aico/agent-tools/*.py"

# Generate initial report
echo "Generating initial report..."
ssh root@"$DEVICE_IP" "cd /usr/share/aico/agent-tools && ./view_agent_files.sh"

echo ""
echo "✓ Deployment complete!"
echo ""
echo "Files deployed to: /usr/share/aico/agent-tools/"
echo "Report generated at: /userdata/agent/files_report.html"
echo ""
echo "Access the report at:"
echo "  http://$DEVICE_IP:8080/user_files"
echo ""
echo "Or SSH to device and run:"
echo "  ssh root@$DEVICE_IP"
echo "  cd /usr/share/aico/agent-tools"
echo "  ./view_agent_files.sh"
