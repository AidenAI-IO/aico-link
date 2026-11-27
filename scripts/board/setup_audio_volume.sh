#!/bin/sh
# Audio volume setup script for Luckfox Pico Zero (RV1106)
# This script sets the audio output volume to maximum levels

echo "Setting up audio volume..."

# Set DAC HPMIX to maximum (range: 0-2)
amixer sset 'DAC HPMIX' 2
if [ $? -eq 0 ]; then
    echo "✓ DAC HPMIX set to maximum (2/2, 100%)"
else
    echo "✗ Failed to set DAC HPMIX"
fi

# Set DAC LINEOUT to maximum (range: 0-30)
amixer sset 'DAC LINEOUT' 30
if [ $? -eq 0 ]; then
    echo "✓ DAC LINEOUT set to maximum (30/30, 100%)"
else
    echo "✗ Failed to set DAC LINEOUT"
fi

# Display current settings
echo ""
echo "Current audio settings:"
amixer sget 'DAC HPMIX' | grep -E "Mono:|Limits:"
amixer sget 'DAC LINEOUT' | grep -E "Mono:|Limits:"

echo ""
echo "Audio volume setup complete."
