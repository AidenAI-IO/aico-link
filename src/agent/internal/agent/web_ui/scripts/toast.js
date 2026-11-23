// Page-level notices. Errors go here instead of the conversation: history
// refreshes rebuild the message list, which would drop a locally added
// error message moments after it appeared.
const toastLifetimeMs = 6000;
const toastLimit = 4;

function toastRegion() {
    let region = document.getElementById('toastRegion');
    if (!region) {
        region = document.createElement('div');
        region.id = 'toastRegion';
        region.className = 'toast-region';
        region.setAttribute('role', 'region');
        region.setAttribute('aria-label', 'Notifications');
        document.body.appendChild(region);
    }
    return region;
}

function dismissToast(toast) {
    if (!toast || !toast.isConnected) return;
    clearTimeout(toast._timer);
    toast.remove();
}

// showToast(message, {kind: 'error' | 'info', sticky}). Identical messages
// collapse into one toast whose timer restarts, so a repeating failure (for
// example a polling request) does not stack copies.
function showToast(message, options) {
    const text = String(message || '').trim();
    if (!text) return null;
    const kind = options && options.kind === 'info' ? 'info' : 'error';
    const region = toastRegion();
    let toast = Array.from(region.children).find(node => node._text === text && node.dataset.kind === kind);
    if (!toast) {
        toast = document.createElement('div');
        toast._text = text;
        toast.className = 'toast';
        toast.dataset.kind = kind;
        toast.setAttribute('role', kind === 'error' ? 'alert' : 'status');
        const body = document.createElement('div');
        body.className = 'toast-text';
        body.textContent = text;
        const close = document.createElement('button');
        close.type = 'button';
        close.className = 'toast-close';
        close.setAttribute('aria-label', 'Dismiss');
        close.textContent = '×';
        close.addEventListener('click', () => dismissToast(toast));
        toast.append(body, close);
        region.appendChild(toast);
        while (region.children.length > toastLimit) dismissToast(region.firstElementChild);
    }
    clearTimeout(toast._timer);
    if (!(options && options.sticky)) toast._timer = setTimeout(() => dismissToast(toast), toastLifetimeMs);
    return toast;
}

function showErrorToast(prefix, err) {
    const detail = err && err.message ? err.message : String(err || '');
    showToast(prefix ? prefix + ': ' + detail : detail, {kind: 'error'});
}
