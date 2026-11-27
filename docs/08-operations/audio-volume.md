---
sidebar_position: 2
---

# Audio Volume Initialization and Adjustment

`scripts/board/setup_audio_volume.sh` is used to set audio output mixer volumes to maximum on Luckfox Pico Zero.

## Default Settings

The script sets:

- `DAC HPMIX`: `2/2` (100%)
- `DAC LINEOUT`: `30/30` (100%)

## Deploy Script

```bash
scp scripts/board/setup_audio_volume.sh root@<device-ip>:/root/
```

After logging into the device, execute:

```bash
cd /root
chmod +x setup_audio_volume.sh
./setup_audio_volume.sh
```

## Auto-Execute on Boot

### Method 1: `/etc/rc.local`

Add before `exit 0`:

```sh
/root/setup_audio_volume.sh
```

### Method 2: systemd service

If the system uses systemd:

```bash
cat > /etc/systemd/system/audio-volume.service << 'EOF'
[Unit]
Description=Set audio volume to maximum
After=sound.target

[Service]
Type=oneshot
ExecStart=/root/setup_audio_volume.sh
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable audio-volume.service
systemctl start audio-volume.service
systemctl status audio-volume.service
```

### Method 3: Application startup script

```sh
#!/bin/sh
/root/setup_audio_volume.sh
/path/to/your/app
```

## Verification

```bash
amixer sget 'DAC HPMIX'
amixer sget 'DAC LINEOUT'
```

Expected output:

- `DAC HPMIX`: `Mono: 2 [100%]`
- `DAC LINEOUT`: `Mono: 30 [100%]`

## Manual Adjustment

```bash
amixer sset 'DAC HPMIX' 2
amixer sset 'DAC LINEOUT' 30
alsactl store   # Optional, save ALSA settings
```

## Relationship with audio_service Volume

- Mixer volume determines the hardware output upper limit;
- `audio_service_cli set-volume --volume N` sets the logical volume `0..100` within the service;
- If hardware mixer is too low, even if logical volume is 100, actual sound may still be quiet.
