package agent

// coordinateDebugHTML is the normalized-coordinate debug tool served at
// /coordinate-debug. It loads the live device screen via
// POST /api/providers/screenshot (or a local upload) and maps clicks to the
// agent's 0-1000 normalized scale used by the HID tools.
const coordinateDebugHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Coordinate Debug · Aiden Agent</title>
<link rel="stylesheet" href="/web-ui/styles.css">
<style>
.debug-layout { display: grid; grid-template-columns: minmax(280px, 340px) minmax(0, 1fr); gap: 1px; min-height: calc(100vh - 64px); background: var(--line); }
.debug-controls { display: flex; flex-direction: column; gap: 12px; padding: 16px; background: var(--surface-soft); }
.debug-workspace { min-width: 0; padding: 20px; background: var(--surface-code); }
.debug-card { padding: 16px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--panel); }
.debug-card h2 { margin-bottom: 14px; color: var(--muted-strong); font: 600 12px ui-monospace, SFMono-Regular, Menlo, monospace; letter-spacing: .08em; text-transform: uppercase; }
.debug-intro { margin-bottom: 16px; color: var(--muted); font-size: 13px; }
.debug-upload { margin-top: 12px; padding: 22px 12px; border: 1px dashed var(--line); border-radius: var(--radius-md); text-align: center; cursor: pointer; color: var(--muted-strong); font-size: 13px; }
.debug-upload:hover, .debug-upload.drag-over, .debug-upload:focus-visible { border-color: var(--accent); color: var(--accent); background: var(--surface-soft); }
.debug-hint { margin-top: 7px; color: var(--muted); font-size: 12px; }
#fileInput { display: none; }
.debug-field { margin-bottom: 12px; }
.debug-field label { display: block; margin-bottom: 5px; color: var(--muted-strong); font-size: 12px; }
.debug-field input, .debug-field select { width: 100%; padding: 8px 10px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--panel-strong); color: var(--text); }
.debug-field input:focus-visible, .debug-field select:focus-visible, .debug-upload:focus-visible, .debug-button:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
.debug-check { display: flex; align-items: center; gap: 8px; margin-top: 10px; color: var(--muted-strong); font-size: 13px; }
.debug-check input { accent-color: var(--accent); }
.debug-button { width: 100%; margin-bottom: 8px; padding: 9px 12px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--panel-strong); color: var(--text); cursor: pointer; font: 600 12px ui-monospace, SFMono-Regular, Menlo, monospace; }
.debug-button:hover:not(:disabled) { border-color: var(--accent); color: var(--accent); }
.debug-button.primary { background: var(--accent); border-color: var(--accent); color: var(--bg); }
.debug-button.primary:hover:not(:disabled) { background: var(--accent-strong); border-color: var(--accent-strong); color: var(--bg); }
.debug-button:disabled { opacity: .5; cursor: not-allowed; }
.debug-status { min-height: 18px; margin-top: 4px; color: var(--muted); font-size: 12px; overflow-wrap: anywhere; }
.debug-canvas { display: flex; justify-content: center; margin-top: 16px; }
.debug-canvas canvas { display: block; max-width: 100%; height: auto; border: 1px solid var(--line); border-radius: var(--radius-md); cursor: crosshair; }
.debug-info { margin-top: 16px; }
.debug-info-item { display: flex; justify-content: space-between; gap: 12px; padding: 8px 0; border-top: 1px solid var(--line); font-size: 13px; }
.debug-info-label { color: var(--muted); }
.debug-info-value { color: var(--text); font: 12px ui-monospace, SFMono-Regular, Menlo, monospace; text-align: right; overflow-wrap: anywhere; }
@media (max-width: 760px) { .debug-layout { grid-template-columns: 1fr; } .debug-workspace { padding: 16px; } }
</style>
</head>
<body>
<header class="topbar">
<div class="topbar-title"><span class="brand-dot" aria-hidden="true"></span><div><h1>Coordinate Debug</h1><span class="topbar-subtitle">WORKSPACE</span></div></div>
<a class="new-chat-btn" href="/" style="text-decoration:none;">Back to chat</a>
</header>
<main class="debug-layout">
<aside class="debug-controls" aria-label="Coordinate controls">
<section class="debug-card">
<h2>Screenshot</h2>
<button class="debug-button primary" id="loadDeviceBtn">Capture device screen</button>
<label class="debug-check"><input type="checkbox" id="cropBlackBars" checked> Crop black bars</label>
<label class="debug-check"><input type="checkbox" id="autoRefresh"> Auto-refresh every 2 seconds</label>
<div class="debug-status" id="deviceStatus" role="status"></div>
<div class="debug-upload" id="uploadArea" role="button" tabindex="0" aria-label="Upload a screenshot">
<p>Click or drop an image here</p>
<p class="debug-hint">Or paste with Ctrl/Cmd+V</p>
</div>
<input type="file" id="fileInput" accept="image/*">
</section>
<section class="debug-card">
<h2>Normalized coordinates · 0–1000</h2>
<div class="debug-field"><label for="coordX">X coordinate</label><input type="number" id="coordX" min="0" max="1000" placeholder="0–1000"></div>
<div class="debug-field"><label for="coordY">Y coordinate</label><input type="number" id="coordY" min="0" max="1000" placeholder="0–1000"></div>
<div class="debug-field"><label for="tapType">Action</label><select id="tapType"><option value="tap">Tap</option><option value="double_tap">Double tap</option><option value="long_press">Long press</option></select></div>
<button class="debug-button" id="showCoordBtn" disabled>Show marker</button>
<button class="debug-button primary" id="tapCoordBtn" disabled>Send action</button>
<button class="debug-button" id="clearBtn" disabled>Clear marker</button>
<div class="debug-status" id="tapStatus" role="status"></div>
</section>
</aside>
<section class="debug-workspace" aria-label="Screenshot preview">
<p class="debug-intro">Capture or upload a screenshot, then click it to inspect normalized coordinates.</p>
<div id="canvasWrapper" style="display:none;"><div class="debug-canvas"><canvas id="canvas" aria-label="Device screenshot"></canvas></div></div>
<div class="debug-card debug-info" id="infoPanel" style="display:none;">
<h2>Capture details</h2>
<div class="debug-info-item"><span class="debug-info-label">Image size</span><span class="debug-info-value" id="imageSize">-</span></div>
<div class="debug-info-item"><span class="debug-info-label">Original screen size</span><span class="debug-info-value" id="originalScreenSize">-</span></div>
<div class="debug-info-item"><span class="debug-info-label">Capture backend</span><span class="debug-info-value" id="captureBackend">-</span></div>
<div class="debug-info-item"><span class="debug-info-label">ADB device</span><span class="debug-info-value" id="adbDeviceInfo">-</span></div>
<div class="debug-info-item"><span class="debug-info-label">Clicked pixel</span><span class="debug-info-value" id="clickPixel">-</span></div>
<div class="debug-info-item"><span class="debug-info-label">Normalized coordinates</span><span class="debug-info-value" id="normalizedCoord">-</span></div>
<div class="debug-info-item"><span class="debug-info-label">Marker pixel</span><span class="debug-info-value" id="markerPixel">-</span></div>
</div>
</section>
</main>
<script>
let currentImage = null;
let currentScreenshotMeta = null;
let autoTimer = null;
let captureInFlight = false;
let tapInFlight = false;

const uploadArea = document.getElementById('uploadArea');
const fileInput = document.getElementById('fileInput');
const canvas = document.getElementById('canvas');
const ctx = canvas.getContext('2d');
const canvasWrapper = document.getElementById('canvasWrapper');
const infoPanel = document.getElementById('infoPanel');
const coordX = document.getElementById('coordX');
const coordY = document.getElementById('coordY');
const tapType = document.getElementById('tapType');
const showCoordBtn = document.getElementById('showCoordBtn');
const tapCoordBtn = document.getElementById('tapCoordBtn');
const clearBtn = document.getElementById('clearBtn');
const loadDeviceBtn = document.getElementById('loadDeviceBtn');
const cropBlackBars = document.getElementById('cropBlackBars');
const autoRefresh = document.getElementById('autoRefresh');
const deviceStatus = document.getElementById('deviceStatus');
const tapStatus = document.getElementById('tapStatus');

function clamp(value, min, max) {
    return Math.min(Math.max(value, min), max);
}

function setCoordinateActionsEnabled(enabled) {
    showCoordBtn.disabled = !enabled;
    tapCoordBtn.disabled = !enabled || tapInFlight;
    clearBtn.disabled = !enabled;
}

function currentCropBlackBars() {
    return !!cropBlackBars.checked;
}

function defaultScreenshotMeta(width, height) {
    return {
        width,
        height,
        source_width: width,
        source_height: height,
        source_active_area: null,
        original_screen_width_pixels: null,
        original_screen_height_pixels: null,
        capture_backend: null,
        adb_device: null
    };
}

function formatOriginalScreenSize(meta) {
    if (!meta) return '-';
    const width = meta.original_screen_width_pixels || meta.source_width;
    const height = meta.original_screen_height_pixels || meta.source_height;
    if (!Number.isInteger(width) || !Number.isInteger(height) || width <= 0 || height <= 0) return '-';
    return width + ' × ' + height + ' px';
}

function formatCaptureBackend(meta) {
    if (!meta || !meta.capture_backend) return '-';
    return String(meta.capture_backend);
}

function formatADBDevice(meta) {
    if (!meta || !meta.adb_device) return '-';
    const device = meta.adb_device;
    const name = device.name || device.serial || '-';
    const label = device.serial && device.serial !== name ? (name + ' (' + device.serial + ')') : name;
    return device.state ? (label + ' / ' + device.state) : label;
}

function normalizeActiveArea(meta) {
    if (!meta || !meta.source_active_area || !meta.source_active_area.valid) return null;
    return meta.source_active_area;
}

function imageIsCropped(meta) {
    if (!meta) return false;
    return (meta.width || 0) !== (meta.source_width || meta.width || 0) ||
           (meta.height || 0) !== (meta.source_height || meta.height || 0);
}

function imagePixelToSourcePixel(pixelX, pixelY) {
    const meta = currentScreenshotMeta || defaultScreenshotMeta(canvas.width, canvas.height);
    const active = normalizeActiveArea(meta);
    if (imageIsCropped(meta) && active) {
        return {
            pixelX: clamp(active.x + pixelX, 0, Math.max((meta.source_width || canvas.width) - 1, 0)),
            pixelY: clamp(active.y + pixelY, 0, Math.max((meta.source_height || canvas.height) - 1, 0))
        };
    }
    return {
        pixelX: clamp(pixelX, 0, Math.max((meta.source_width || canvas.width) - 1, 0)),
        pixelY: clamp(pixelY, 0, Math.max((meta.source_height || canvas.height) - 1, 0))
    };
}

function sourcePixelToNormalized(pixelX, pixelY) {
    const meta = currentScreenshotMeta || defaultScreenshotMeta(canvas.width, canvas.height);
    const active = normalizeActiveArea(meta);
    // Normalized coordinates are relative to active_area (0-1000 maps to the mirrored phone touch region)
    if (active) {
        const activeWidth = Math.max(active.width, 1);
        const activeHeight = Math.max(active.height, 1);
        const activePixelX = clamp(pixelX - active.x, 0, active.width - 1);
        const activePixelY = clamp(pixelY - active.y, 0, active.height - 1);
        return {
            x: clamp(Math.round((activePixelX / Math.max(activeWidth - 1, 1)) * 1000), 0, 1000),
            y: clamp(Math.round((activePixelY / Math.max(activeHeight - 1, 1)) * 1000), 0, 1000)
        };
    }
    // Fallback: no active_area, treat as full frame
    const sourceWidth = Math.max(meta.source_width || canvas.width, 1);
    const sourceHeight = Math.max(meta.source_height || canvas.height, 1);
    const maxPixelX = Math.max(sourceWidth - 1, 1);
    const maxPixelY = Math.max(sourceHeight - 1, 1);
    return {
        x: clamp(Math.round((pixelX / maxPixelX) * 1000), 0, 1000),
        y: clamp(Math.round((pixelY / maxPixelY) * 1000), 0, 1000)
    };
}

function normalizedToSourcePixel(x, y) {
    const meta = currentScreenshotMeta || defaultScreenshotMeta(canvas.width, canvas.height);
    const active = normalizeActiveArea(meta);
    // Normalized coordinates are relative to active_area (0-1000 maps to the mirrored phone touch region)
    if (active) {
        const activeWidth = Math.max(active.width, 1);
        const activeHeight = Math.max(active.height, 1);
        const activePixelX = clamp(Math.round((x / 1000) * Math.max(activeWidth - 1, 0)), 0, activeWidth - 1);
        const activePixelY = clamp(Math.round((y / 1000) * Math.max(activeHeight - 1, 0)), 0, activeHeight - 1);
        return {
            pixelX: clamp(activePixelX + active.x, 0, (meta.source_width || canvas.width) - 1),
            pixelY: clamp(activePixelY + active.y, 0, (meta.source_height || canvas.height) - 1)
        };
    }
    // Fallback: no active_area, treat as full frame
    const sourceWidth = Math.max(meta.source_width || canvas.width, 1);
    const sourceHeight = Math.max(meta.source_height || canvas.height, 1);
    return {
        pixelX: clamp(Math.round((x / 1000) * Math.max(sourceWidth - 1, 0)), 0, sourceWidth - 1),
        pixelY: clamp(Math.round((y / 1000) * Math.max(sourceHeight - 1, 0)), 0, sourceHeight - 1)
    };
}

function sourcePixelToImagePixel(pixelX, pixelY) {
    const meta = currentScreenshotMeta || defaultScreenshotMeta(canvas.width, canvas.height);
    const active = normalizeActiveArea(meta);
    if (imageIsCropped(meta) && active) {
        return {
            pixelX: clamp(pixelX - active.x, 0, canvas.width - 1),
            pixelY: clamp(pixelY - active.y, 0, canvas.height - 1)
        };
    }
    return {
        pixelX: clamp(pixelX, 0, canvas.width - 1),
        pixelY: clamp(pixelY, 0, canvas.height - 1)
    };
}

uploadArea.addEventListener('click', () => fileInput.click());
uploadArea.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); fileInput.click(); }
});
uploadArea.addEventListener('dragover', (e) => { e.preventDefault(); uploadArea.classList.add('drag-over'); });
uploadArea.addEventListener('dragleave', () => uploadArea.classList.remove('drag-over'));
uploadArea.addEventListener('drop', (e) => {
    e.preventDefault();
    uploadArea.classList.remove('drag-over');
    if (e.dataTransfer.files.length > 0) handleFile(e.dataTransfer.files[0]);
});
fileInput.addEventListener('change', (e) => {
    if (e.target.files.length > 0) handleFile(e.target.files[0]);
});

function screenshotMetaFromProvider(frame, capture) {
    const width = Number(frame && frame.width) || 0;
    const height = Number(frame && frame.height) || 0;
    const sourceWidth = Number(frame && frame.source_width) || width;
    const sourceHeight = Number(frame && frame.source_height) || height;
    const cropX = Number(frame && frame.crop_x) || 0;
    const cropY = Number(frame && frame.crop_y) || 0;
    const cropW = Number(frame && frame.crop_width) || width;
    const cropH = Number(frame && frame.crop_height) || height;
    let sourceActive = null;
    if (cropW > 0 && cropH > 0 && (cropX !== 0 || cropY !== 0 || cropW !== sourceWidth || cropH !== sourceHeight)) {
        sourceActive = { x: cropX, y: cropY, width: cropW, height: cropH, valid: true };
    }
    return {
        width,
        height,
        source_width: sourceWidth,
        source_height: sourceHeight,
        source_active_area: sourceActive,
        original_screen_width_pixels: sourceWidth || null,
        original_screen_height_pixels: sourceHeight || null,
        capture_backend: (capture && capture.capture_backend) || null,
        adb_device: (capture && capture.adb_device) || null
    };
}

async function captureFromDevice() {
    if (captureInFlight) return;
    captureInFlight = true;
    loadDeviceBtn.disabled = true;
    deviceStatus.textContent = 'Capturing...';
    try {
        const res = await fetch('/api/providers/screenshot', {
            method: 'POST',
            cache: 'no-store',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
                format: 'jpeg',
                quality: 80,
                crop_black: currentCropBlackBars(),
                minimal_width: 0
            })
        });
        const body = await res.json().catch(() => null);
        if (!res.ok || !body || !body.ok) {
            const message = (body && body.error && body.error.message) || res.statusText || String(res.status);
            deviceStatus.textContent = 'Capture failed: ' + message;
            return;
        }
        const data = body.data || {};
        if (!data.image) {
            deviceStatus.textContent = 'Capture failed: no image in response';
            return;
        }
        const meta = screenshotMetaFromProvider(data.meta || {}, data.capture_info || {});
        const format = (data.meta && data.meta.pixel_format === 'png') ? 'png' : 'jpeg';
        await loadImageFromUrl('data:image/' + format + ';base64,' + data.image, 'Device screen loaded', meta);
    } catch (e) {
        deviceStatus.textContent = 'Request failed: ' + e.message;
    } finally {
        captureInFlight = false;
        loadDeviceBtn.disabled = false;
    }
}

loadDeviceBtn.addEventListener('click', captureFromDevice);
autoRefresh.addEventListener('change', () => {
    if (autoTimer) { clearInterval(autoTimer); autoTimer = null; }
    if (autoRefresh.checked) autoTimer = setInterval(captureFromDevice, 2000);
});

function handleFile(file) {
    if (!file.type.startsWith('image/')) { alert('Choose an image file.'); return; }
    const reader = new FileReader();
    reader.onload = async (e) => {
        try {
            await loadImageFromUrl(e.target.result, 'Local image loaded');
        } catch (_) {}
    };
    reader.readAsDataURL(file);
}

function loadImageFromUrl(url, statusText, meta) {
    return new Promise((resolve, reject) => {
        const img = new Image();
        img.onload = () => {
            currentImage = img;
            currentScreenshotMeta = meta || defaultScreenshotMeta(img.width, img.height);
            drawImage();
            canvasWrapper.style.display = 'block';
            infoPanel.style.display = 'block';
            setCoordinateActionsEnabled(true);
            document.getElementById('imageSize').textContent = img.width + ' × ' + img.height + ' px';
            document.getElementById('originalScreenSize').textContent = formatOriginalScreenSize(currentScreenshotMeta);
            document.getElementById('captureBackend').textContent = formatCaptureBackend(currentScreenshotMeta);
            document.getElementById('adbDeviceInfo').textContent = formatADBDevice(currentScreenshotMeta);
            deviceStatus.textContent = statusText;
            resolve(img);
        };
        img.onerror = () => {
            deviceStatus.textContent = 'Image could not be loaded';
            reject(new Error('Image could not be loaded'));
        };
        img.src = url;
    });
}

function loadImageFromScreenshotResult(result, statusText) {
    const format = result && result.format ? result.format : 'jpeg';
    return loadImageFromUrl('data:image/' + format + ';base64,' + result.data, statusText, result);
}

function drawImage() {
    if (!currentImage) return;
    canvas.width = currentImage.width;
    canvas.height = currentImage.height;
    ctx.clearRect(0, 0, canvas.width, canvas.height);
    ctx.drawImage(currentImage, 0, 0);
}

canvas.addEventListener('click', (e) => {
    const rect = canvas.getBoundingClientRect();
    const scaleX = canvas.width / rect.width;
    const scaleY = canvas.height / rect.height;
    const maxPixelX = canvas.width - 1;
    const maxPixelY = canvas.height - 1;
    const pixelX = clamp(Math.round((e.clientX - rect.left) * scaleX), 0, maxPixelX);
    const pixelY = clamp(Math.round((e.clientY - rect.top) * scaleY), 0, maxPixelY);
    const sourcePixel = imagePixelToSourcePixel(pixelX, pixelY);
    const normalized = sourcePixelToNormalized(sourcePixel.pixelX, sourcePixel.pixelY);
    document.getElementById('clickPixel').textContent = 'X: ' + pixelX + ', Y: ' + pixelY;
    document.getElementById('normalizedCoord').textContent = 'X: ' + normalized.x + ', Y: ' + normalized.y;
    coordX.value = normalized.x;
    coordY.value = normalized.y;
    showMarker(pixelX, pixelY);
});

function getNormalizedInputs() {
    const x = parseInt(coordX.value);
    const y = parseInt(coordY.value);
    if (isNaN(x) || isNaN(y) || x < 0 || x > 1000 || y < 0 || y > 1000) {
        return null;
    }
    return { x, y };
}

function normalizedToPixel(x, y) {
    const sourcePixel = normalizedToSourcePixel(x, y);
    return sourcePixelToImagePixel(sourcePixel.pixelX, sourcePixel.pixelY);
}

function showNormalizedMarker(x, y) {
    const pixel = normalizedToPixel(x, y);
    showMarker(pixel.pixelX, pixel.pixelY);
    document.getElementById('normalizedCoord').textContent = 'X: ' + x + ', Y: ' + y;
}

showCoordBtn.addEventListener('click', () => {
    const point = getNormalizedInputs();
    if (!point) {
        alert('Enter valid coordinates (0–1000).');
        return;
    }
    showNormalizedMarker(point.x, point.y);
});

tapCoordBtn.addEventListener('click', async () => {
    const point = getNormalizedInputs();
    if (!point) {
        alert('Enter valid coordinates (0–1000).');
        return;
    }
    tapInFlight = true;
    setCoordinateActionsEnabled(true);
    tapStatus.textContent = 'Sending action...';
    try {
        showNormalizedMarker(point.x, point.y);
        const res = await fetch('/api/coordinate-debug/tap', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
                x: point.x,
                y: point.y,
                type: tapType.value,
                crop_black_bars: currentCropBlackBars()
            })
        });
        const bodyText = await res.text();
        let payload = null;
        try {
            payload = JSON.parse(bodyText);
        } catch (_) {}
        if (!res.ok) {
            tapStatus.textContent = payload && payload.error ? 'Action failed: ' + payload.error : 'Action failed: ' + bodyText;
            return;
        }
        if (!payload || !payload.ok || !payload.screenshot || !payload.screenshot.data) {
            tapStatus.textContent = 'Action failed: incomplete response';
            return;
        }
        await loadImageFromScreenshotResult(payload.screenshot, 'Screen refreshed after action');
        showNormalizedMarker(point.x, point.y);
        tapStatus.textContent = tapType.options[tapType.selectedIndex].text + ' sent; screen refreshed';
    } catch (e) {
        tapStatus.textContent = 'Action failed: ' + e.message;
    } finally {
        tapInFlight = false;
        setCoordinateActionsEnabled(!!currentImage);
    }
});

clearBtn.addEventListener('click', () => {
    drawImage();
    document.getElementById('clickPixel').textContent = '-';
    document.getElementById('normalizedCoord').textContent = '-';
    document.getElementById('markerPixel').textContent = '-';
    coordX.value = '';
    coordY.value = '';
    tapStatus.textContent = '';
});

function showMarker(pixelX, pixelY) {
    drawImage();
    ctx.strokeStyle = '#f18a8a';
    ctx.lineWidth = 3;
    ctx.beginPath();
    ctx.moveTo(pixelX - 15, pixelY); ctx.lineTo(pixelX + 15, pixelY);
    ctx.moveTo(pixelX, pixelY - 15); ctx.lineTo(pixelX, pixelY + 15);
    ctx.stroke();
    ctx.beginPath(); ctx.arc(pixelX, pixelY, 10, 0, 2 * Math.PI); ctx.stroke();
    ctx.fillStyle = '#f18a8a';
    ctx.beginPath(); ctx.arc(pixelX, pixelY, 3, 0, 2 * Math.PI); ctx.fill();
    document.getElementById('markerPixel').textContent = 'X: ' + pixelX + ', Y: ' + pixelY;
}

document.addEventListener('keydown', (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key === 'v') {
        if (!navigator.clipboard || typeof navigator.clipboard.read !== 'function') return;
        navigator.clipboard.read().then(items => {
            for (const item of items) {
                for (const type of item.types) {
                    if (type.startsWith('image/')) {
                        item.getType(type).then(blob => handleFile(blob));
                        return;
                    }
                }
            }
        }).catch(() => {});
    }
    if (e.key === 'Escape' && !clearBtn.disabled) clearBtn.click();
});

</script>
</body>
</html>
`
