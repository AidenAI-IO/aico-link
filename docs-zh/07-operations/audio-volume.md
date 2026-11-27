---
sidebar_position: 2
---

# 音频音量初始化与调节

`scripts/board/setup_audio_volume.sh` 用于在 Luckfox Pico Zero 上把音频输出混音器音量设为最大。

## 默认设置

该脚本设置：

- `DAC HPMIX`：`2/2`（100%）
- `DAC LINEOUT`：`30/30`（100%）

## 部署脚本

```bash
scp scripts/board/setup_audio_volume.sh root@<device-ip>:/root/
```

登录设备后执行：

```bash
cd /root
chmod +x setup_audio_volume.sh
./setup_audio_volume.sh
```

## 开机自动执行

### 方法 1：`/etc/rc.local`

在 `exit 0` 之前添加：

```sh
/root/setup_audio_volume.sh
```

### 方法 2：systemd 服务

如果系统使用 systemd：

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

### 方法 3：应用启动脚本

```sh
#!/bin/sh
/root/setup_audio_volume.sh
/path/to/your/app
```

## 验证

```bash
amixer sget 'DAC HPMIX'
amixer sget 'DAC LINEOUT'
```

预期输出：

- `DAC HPMIX`：`Mono: 2 [100%]`
- `DAC LINEOUT`：`Mono: 30 [100%]`

## 手动调节

```bash
amixer sset 'DAC HPMIX' 2
amixer sset 'DAC LINEOUT' 30
alsactl store   # Optional, save ALSA settings
```

## 与 audio_service 音量的关系

- 混音器音量决定硬件输出的上限；
- `audio_service_cli set-volume --volume N` 设置服务内的逻辑音量 `0..100`；
- 如果硬件混音器音量过低，即使逻辑音量为 100，实际声音仍可能很小。
