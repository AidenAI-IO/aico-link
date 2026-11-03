#pragma once

static const char* HID_SERVER_HTML = R"HTML(
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Aico Link HID Control</title>
<style>
* { margin: 0; padding: 0; box-sizing: border-box; }
body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif; background: #1a1a1a; color: #e0e0e0; padding: 20px; }
h1 { text-align: center; margin-bottom: 30px; color: #4a9eff; }
h2 { margin: 20px 0 15px; color: #6ab7ff; font-size: 1.3em; }
.container { max-width: 1000px; margin: 0 auto; }
.section { background: #252525; padding: 20px; border-radius: 8px; margin-bottom: 20px; }
.input-group { display: flex; gap: 10px; margin-bottom: 15px; align-items: center; }
input[type="text"], input[type="number"] { flex: 1; padding: 10px; background: #333; border: 1px solid #444; border-radius: 4px; color: #e0e0e0; font-size: 14px; }
input[type="text"]:focus, input[type="number"]:focus { outline: none; border-color: #4a9eff; }
button { padding: 10px 16px; background: #4a9eff; border: none; border-radius: 4px; color: white; cursor: pointer; font-size: 14px; font-weight: 500; transition: all 0.2s; }
button:hover { background: #3a8eef; transform: translateY(-1px); }
button:active { transform: translateY(0); background: #2a7edf; }
.btn-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(80px, 1fr)); gap: 8px; margin-bottom: 15px; }
.btn-small { padding: 8px 12px; font-size: 13px; }
.btn-compact { padding: 6px 10px; font-size: 12px; }
.toolbar { display: flex; gap: 10px; align-items: center; margin-bottom: 10px; flex-wrap: wrap; }
.toolbar label { font-size: 13px; color: #888; }
select { padding: 8px; background: #333; border: 1px solid #444; border-radius: 4px; color: #e0e0e0; font-size: 13px; }
.screen-area { position: relative; width: 100%; background: #111; border: 2px solid #4a9eff; border-radius: 8px; overflow: auto; cursor: crosshair; min-height: 300px; display: flex; align-items: center; justify-content: center; }
.screen-area img { display: block; width: auto; height: auto; max-width: 100%; max-height: min(1080px, calc(100vh - 220px)); user-select: none; pointer-events: none; object-fit: contain; }
.screen-area .overlay { position: absolute; top: 0; left: 0; width: 100%; height: 100%; }
.screen-placeholder { color: #555; font-size: 18px; text-align: center; }
.coords { text-align: center; margin: 8px 0; color: #888; font-family: monospace; font-size: 13px; }
#log { background: #1a1a1a; padding: 15px; border-radius: 4px; max-height: 200px; overflow-y: auto; font-family: monospace; font-size: 12px; }
.log-entry { padding: 4px 0; border-bottom: 1px solid #333; }
.log-entry:last-child { border-bottom: none; }
</style>
</head>
<body>
<div class="container">
<h1>Aico Link HID Control</h1>

<div class="section">
<h2>Screen Capture</h2>
<div class="toolbar">
<button id="captureBtn" onclick="captureScreenshot()">Capture Screenshot</button>
<label><input type="checkbox" id="forceTrigger"> Force EDID trigger</label>
<label>Auto: <select id="autoInterval" onchange="setupAutoCapture()">
<option value="0">Off</option>
<option value="2">2s</option>
<option value="3">3s</option>
<option value="5">5s</option>
</select></label>
<span id="captureStatus" style="color:#888;font-size:13px;"></span>
</div>
<div class="screen-area" id="screenArea">
<div class="screen-placeholder" id="placeholder">Click "Capture Screenshot"<br>to view the remote screen</div>
<img id="screenshot" style="display:none" alt="screenshot">
<div class="overlay" id="overlay"></div>
</div>
<div class="coords" id="coordsDisplay">Click the screenshot to send a left click</div>
</div>

<div class="section">
<h2>Keyboard</h2>
<div class="input-group">
<input type="text" id="textInput" placeholder="Type text to send...">
<button onclick="sendText()">Type</button>
</div>
<div class="btn-grid">
<button class="btn-small" onclick="tap('ENTER')">Enter</button>
<button class="btn-small" onclick="tap('TAB')">Tab</button>
<button class="btn-small" onclick="tap('BACKSPACE')">Backspace</button>
<button class="btn-small" onclick="tap('ESCAPE')">Escape</button>
<button class="btn-small" onclick="tap('SPACE')">Space</button>
<button class="btn-small" onclick="tap('DELETE')">Delete</button>
</div>
<div class="btn-grid">
<button class="btn-small" onclick="tap('UP')">Up</button>
<button class="btn-small" onclick="tap('DOWN')">Down</button>
<button class="btn-small" onclick="tap('LEFT')">Left</button>
<button class="btn-small" onclick="tap('RIGHT')">Right</button>
</div>
<div class="btn-grid">
<button class="btn-compact" onclick="tap('CTRL','C')">Ctrl+C</button>
<button class="btn-compact" onclick="tap('CTRL','V')">Ctrl+V</button>
<button class="btn-compact" onclick="tap('CTRL','Z')">Ctrl+Z</button>
<button class="btn-compact" onclick="tap('CTRL','A')">Ctrl+A</button>
<button class="btn-compact" onclick="tap('CTRL','S')">Ctrl+S</button>
<button class="btn-compact" onclick="tap('ALT','TAB')">Alt+Tab</button>
<button class="btn-compact" onclick="tap('ALT','F4')">Alt+F4</button>
</div>
</div>

<div class="section">
<h2>Mouse Actions</h2>
<div class="btn-grid">
<button class="btn-small" onclick="mouseClick('left')">Left Click</button>
<button class="btn-small" onclick="mouseClick('right')">Right Click</button>
<button class="btn-small" onclick="mouseClick('middle')">Middle Click</button>
<button class="btn-small" onclick="mouseScroll(-3)">Scroll Up</button>
<button class="btn-small" onclick="mouseScroll(3)">Scroll Down</button>
</div>
<div class="input-group">
<input type="number" id="moveX" placeholder="X (0-32767)" min="0" max="32767">
<input type="number" id="moveY" placeholder="Y (0-32767)" min="0" max="32767">
<button onclick="manualMove()">Move</button>
<button onclick="manualClick()">Click</button>
</div>
</div>

<div class="section">
<h2>Log</h2>
<div id="log"></div>
</div>
</div>

<script>
let autoTimer = null;
let captureInFlight = false;
let imgNaturalW = 0, imgNaturalH = 0;
let activeArea = null;
const STANDARD_COORD_MAX = 32767;
const DEFAULT_COORDS_TEXT = 'Click the screenshot to send a left click';

function log(msg) {
    const el = document.getElementById('log');
    const t = new Date().toLocaleTimeString();
    el.innerHTML = '<div class="log-entry">[' + t + '] ' + msg + '</div>' + el.innerHTML;
    if (el.children.length > 50) el.lastChild.remove();
}

async function api(endpoint, body) {
    try {
        const res = await fetch(endpoint, {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify(body)
        });
        const text = await res.text();
        log(endpoint + ' [' + res.status + '] ' + text);
        return text;
    } catch (e) {
        log(endpoint + ' ERROR: ' + e.message);
        return null;
    }
}

function tap(...keys) { api('/api/keyboard/tap', {keys: keys}); }

function sendText() {
    const v = document.getElementById('textInput').value;
    if (v) api('/api/keyboard/text', {text: v});
}

function mouseClick(btn) { api('/api/touch/click', {button: btn}); }
function mouseScroll(amt) { api('/api/touch/scroll', {amount: amt}); }

function clamp(value, min, max) {
    return Math.max(min, Math.min(max, value));
}

function normalizeCoordinate(pixelCoord, size) {
    if (size <= 1) return 0;
    return clamp(Math.round((pixelCoord / (size - 1)) * STANDARD_COORD_MAX), 0, STANDARD_COORD_MAX);
}

function screenshotToStandardCoords(imgX, imgY) {
    const area = activeArea || {x: 0, y: 0, width: imgNaturalW, height: imgNaturalH};
    if (imgX < area.x || imgY < area.y || imgX > area.x + area.width - 1 || imgY > area.y + area.height - 1) {
        return null;
    }
    const pixelX = clamp(Math.round(imgX), area.x, area.x + area.width - 1);
    const pixelY = clamp(Math.round(imgY), area.y, area.y + area.height - 1);
    return {
        imgX: pixelX,
        imgY: pixelY,
        x: normalizeCoordinate(pixelX - area.x, area.width),
        y: normalizeCoordinate(pixelY - area.y, area.height)
    };
}

function manualMove() {
    const x = parseInt(document.getElementById('moveX').value);
    const y = parseInt(document.getElementById('moveY').value);
    if (!isNaN(x) && !isNaN(y)) api('/api/touch/move', {x, y});
}

function manualClick() {
    const x = parseInt(document.getElementById('moveX').value);
    const y = parseInt(document.getElementById('moveY').value);
    if (!isNaN(x) && !isNaN(y)) api('/api/touch/click', {x, y});
}

async function captureScreenshot() {
    if (captureInFlight) return;

    const btn = document.getElementById('captureBtn');
    const status = document.getElementById('captureStatus');
    const img = document.getElementById('screenshot');
    captureInFlight = true;
    btn.disabled = true;
    status.textContent = 'capturing...';

    const force = document.getElementById('forceTrigger').checked;
    const resp = await api('/api/capture', {force: force});

    captureInFlight = false;
    btn.disabled = false;
    if (resp) {
        try {
            const r = JSON.parse(resp);
            if (r.ok) {
                imgNaturalW = r.width;
                imgNaturalH = r.height;
                if (r.active_area && r.active_area.valid) {
                    activeArea = r.active_area;
                } else {
                    activeArea = null;
                }
                img.src = '/screenshot.jpg?t=' + Date.now();
                img.style.display = 'block';
                document.getElementById('placeholder').style.display = 'none';
                status.textContent = r.width + 'x' + r.height + ' OK';
            } else {
                status.textContent = r.error || 'failed';
            }
        } catch(e) {
            status.textContent = 'parse error';
        }
    } else {
        status.textContent = 'request failed';
    }
}

function setupAutoCapture() {
    const interval = parseInt(document.getElementById('autoInterval').value);
    if (autoTimer) { clearInterval(autoTimer); autoTimer = null; }
    if (interval > 0) {
        autoTimer = setInterval(captureScreenshot, interval * 1000);
    }
}

(function() {
    const overlay = document.getElementById('overlay');
    const display = document.getElementById('coordsDisplay');
    const img = document.getElementById('screenshot');

    function getTouchCoords(e) {
        if (img.style.display === 'none') return null;
        const r = img.getBoundingClientRect();
        if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) {
            return null;
        }
        const scaleX = imgNaturalW / r.width;
        const scaleY = imgNaturalH / r.height;
        const imgX = (e.clientX - r.left) * scaleX;
        const imgY = (e.clientY - r.top) * scaleY;
        return screenshotToStandardCoords(imgX, imgY);
    }

    overlay.addEventListener('click', function(e) {
        const coords = getTouchCoords(e);
        if (!coords) return;
        display.textContent = 'Left click: screenshot (' + coords.imgX + ', ' + coords.imgY +
            ') -> touch (' + coords.x + ', ' + coords.y + ')';
        api('/api/touch/click', {x: coords.x, y: coords.y});
    });

    overlay.addEventListener('contextmenu', function(e) {
        e.preventDefault();
        const coords = getTouchCoords(e);
        if (!coords) return;
        display.textContent = 'Right click: screenshot (' + coords.imgX + ', ' + coords.imgY +
            ') -> touch (' + coords.x + ', ' + coords.y + ')';
        api('/api/touch/click', {button: 'right', x: coords.x, y: coords.y});
    });

    overlay.addEventListener('mousemove', function(e) {
        const coords = getTouchCoords(e);
        if (!coords) return;
        display.textContent = 'Position: screenshot (' + coords.imgX + ', ' + coords.imgY +
            ') -> touch (' + coords.x + ', ' + coords.y + ')';
    });

    overlay.addEventListener('mouseleave', function() {
        display.textContent = DEFAULT_COORDS_TEXT;
    });

    img.addEventListener('load', function() {
        if (img.naturalWidth > 0 && img.naturalHeight > 0) {
            imgNaturalW = img.naturalWidth;
            imgNaturalH = img.naturalHeight;
        }
    });
})();
</script>
</body>
</html>
)HTML";
