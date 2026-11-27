#!/bin/bash
# USB Gadget ConfigFS Setup Script for the Aico Link board

set -e

echo "=== USB Gadget Setup ==="

# 1. Mount configfs if not already mounted
if ! mountpoint -q /sys/kernel/config; then
    echo "[1/4] Mounting configfs..."
    mount -t configfs none /sys/kernel/config
else
    echo "[1/4] ConfigFS already mounted"
fi

# 2. Load dwc2 module if not loaded
if ! lsmod | grep -q dwc2; then
    echo "[2/4] Loading dwc2 module..."
    modprobe dwc2
else
    echo "[2/4] dwc2 module already loaded"
fi

# 3. Load libcomposite module if not loaded
if ! lsmod | grep -q libcomposite; then
    echo "[3/4] Loading libcomposite module..."
    modprobe libcomposite
else
    echo "[3/4] libcomposite module already loaded"
fi

# 4. Setup USB gadget
echo "[4/4] Setting up USB HID gadget..."
./example_usb_hid setup composite

echo ""
echo "=== Setup Complete ==="
echo "USB HID gadget is now ready."
echo ""
echo "Test commands:"
echo "  ./example_usb_hid keyboard tap ENTER"
echo "  ./example_usb_hid keyboard text 'hello world'"
echo "  ./example_usb_hid touch tap 16000 16000"
echo ""
echo "Web UI:"
echo "  ./example_usb_hid server"
echo "  Then open http://<device-ip>:8000 in browser"
