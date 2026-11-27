---
sidebar_position: 3
---

# 图像处理工具

项目提供 `libaico_image.a` 和 `image_process` CLI，用于基础的截图和图像处理。

## 能力

- 裁剪；
- 自动裁剪黑边；
- 缩放；
- 旋转；
- 输入支持 PPM(P6) 以及 stb_image 支持的格式，如 JPEG/PNG；
- 输出支持 `.ppm`、`.jpg`、`.jpeg`。

## CLI 用法

```text
image_process crop -b T,R,B,L -i INPUT -o OUTPUT [-q 1-100 for JPEG]
image_process crop_black_bars -i INPUT -o OUTPUT [-q JPEG quality]
image_process scale -s FACTOR -i INPUT -o OUTPUT [-q JPEG quality]
image_process rotate -d DEG -i INPUT -o OUTPUT [-q JPEG quality]
```

## 示例

裁剪上、右、下、左边距：

```bash
image_process crop -b 10,20,10,20 -i input.jpg -o cropped.jpg -q 90
```

裁剪黑边：

```bash
image_process crop_black_bars -i screenshot.jpg -o no-bars.jpg
```

缩放到 50%：

```bash
image_process scale -s 0.5 -i input.png -o small.jpg
```

旋转 90 度：

```bash
image_process rotate -d 90 -i input.jpg -o rotated.jpg
```

## 库目标

CMake 目标：

- `aico_image`：静态库（生成 `libaico_image.a`）；
- `image_process`：CLI。

主要源文件：

- `src/image_process.cpp`
- `src/image_process.h`
- `src/image_process_cli.cpp`
