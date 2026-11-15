// Shared-file links in the conversation: hovering one shows a small preview
// card, clicking opens the shared-file preview dialog (inspector.js).
// Links are found by the .shared-file-link class that markdown.js and
// attachments.js give to every link under /share/.
const sharedLinkHoverDelayMs = 250;
const sharedLinkMetaTtlMs = 5000;
const sharedLinkHoverImageMaxBytes = 8 << 20;
const sharedLinkHoverHTMLMaxBytes = 1 << 20;
const sharedLinkHoverTextBytes = 4096;
const sharedLinkCacheLimit = 24;

// Directory listings by folder, reused for a few seconds while the pointer
// moves across several links to the same folder.
const sharedLinkListings = new Map();
// Hover previews by href: {kind, url?, text?}. Object URLs are revoked when
// an entry is evicted.
const sharedLinkPreviews = new Map();
let sharedLinkHover = null;
let sharedLinkHoverTimer = null;
let sharedLinkHoverAnchor = null;

// sharedLinkLocation splits /share/a%20b/c.png into its folder ("a b") and
// file name ("c.png"), decoded as the listing API reports them.
function sharedLinkLocation(href) {
    const path = safeSharedURL(href);
    if (!path || path.endsWith('/')) return null;
    const parts = path.slice('/share/'.length).split('/').map(part => {
        try { return decodeURIComponent(part); } catch (_) { return part; }
    });
    const name = parts.pop();
    return name ? {folder: parts.join('/'), name, href: path} : null;
}

async function sharedLinkListing(folder) {
    const cached = sharedLinkListings.get(folder);
    if (cached && Date.now() - cached.at < sharedLinkMetaTtlMs) return cached.files;
    const response = await fetch('/api/files' + (folder ? '?path=' + encodeURIComponent(folder) : ''), {cache: 'no-store'});
    if (response.status === 404) return [];
    if (!response.ok) throw new Error(response.status === 401 ? 'Sign in to preview shared files.' : 'Preview failed.');
    const result = await response.json();
    const files = Array.isArray(result.files) ? result.files : [];
    sharedLinkListings.set(folder, {at: Date.now(), files});
    return files;
}

// sharedLinkFile returns the listing entry for a link, or null when the file
// no longer exists.
async function sharedLinkFile(href) {
    const location = sharedLinkLocation(href);
    if (!location) return null;
    const files = await sharedLinkListing(location.folder);
    return files.find(file => file.kind === 'file' && file.name === location.name) || null;
}

function forgetSharedLinkPreview(href) {
    const entry = sharedLinkPreviews.get(href);
    if (entry && entry.url) URL.revokeObjectURL(entry.url);
    sharedLinkPreviews.delete(href);
}

function rememberSharedLinkPreview(href, entry) {
    forgetSharedLinkPreview(href);
    sharedLinkPreviews.set(href, entry);
    while (sharedLinkPreviews.size > sharedLinkCacheLimit) {
        forgetSharedLinkPreview(sharedLinkPreviews.keys().next().value);
    }
    return entry;
}

// loadSharedLinkPreview fetches what the hover card shows: an image, the
// start of a text file, or a small HTML page. Other kinds show details only.
async function loadSharedLinkPreview(file) {
    const href = file.download_url;
    const cached = sharedLinkPreviews.get(href);
    if (cached && cached.modified === file.modified && cached.size === file.size) return cached;
    const kind = filePreviewKind(file);
    const entry = {kind, modified: file.modified, size: file.size};
    if (kind === 'image' && file.size <= sharedLinkHoverImageMaxBytes) {
        const response = await fetch(href);
        if (!response.ok) throw new Error('Preview failed.');
        const blob = new Blob([await response.arrayBuffer()], {type: previewMimeType(file, kind)});
        entry.url = URL.createObjectURL(blob);
    } else if (kind === 'html' && file.size <= sharedLinkHoverHTMLMaxBytes) {
        const response = await fetch(href);
        if (!response.ok) throw new Error('Preview failed.');
        entry.text = await response.text();
    } else if (kind === 'text') {
        const response = await fetch(href, {headers: {Range: 'bytes=0-' + (sharedLinkHoverTextBytes - 1)}});
        if (!response.ok) throw new Error('Preview failed.');
        // A multi-byte character cut at the range end decodes as U+FFFD.
        entry.text = (await response.text()).replace(/\uFFFD+$/, '');
        entry.truncated = file.size > sharedLinkHoverTextBytes;
    } else {
        entry.kind = '';
    }
    return rememberSharedLinkPreview(href, entry);
}

function sharedLinkHoverCard() {
    if (sharedLinkHover) return sharedLinkHover;
    const root = document.createElement('div');
    root.className = 'shared-link-preview';
    root.setAttribute('aria-hidden', 'true');
    const media = document.createElement('div');
    media.className = 'shared-link-preview-media';
    const caption = document.createElement('div');
    caption.className = 'shared-link-preview-caption';
    const title = document.createElement('span');
    title.className = 'shared-link-preview-name';
    const meta = document.createElement('span');
    meta.className = 'shared-link-preview-meta';
    caption.append(title, meta);
    root.append(media, caption);
    document.body.appendChild(root);
    sharedLinkHover = {root, media, title, meta, token: 0};
    return sharedLinkHover;
}

function setSharedLinkHoverMedia(card, node) {
    card.media.replaceChildren(node);
    card.media.hidden = !node;
}

function renderSharedLinkHoverPreview(card, file, entry) {
    if (entry.kind === 'image' && entry.url) {
        const image = document.createElement('img');
        image.alt = '';
        image.addEventListener('load', () => positionSharedLinkHover(sharedLinkHoverAnchor));
        image.src = entry.url;
        setSharedLinkHoverMedia(card, image);
    } else if (entry.kind === 'html' && entry.text !== undefined) {
        const frame = createHTMLPreviewFrame(entry.text, file.name);
        frame.className = 'shared-link-preview-html';
        frame.tabIndex = -1;
        const viewport = document.createElement('div');
        viewport.className = 'shared-link-preview-html-viewport';
        viewport.appendChild(frame);
        setSharedLinkHoverMedia(card, viewport);
    } else if (entry.kind === 'text' && entry.text !== undefined) {
        const text = entry.text + (entry.truncated ? '\n…' : '');
        setSharedLinkHoverMedia(card, inspectorElement('pre', 'shared-link-preview-text', text || '(empty file)'));
    } else {
        const label = fileExtension(file.name).toUpperCase() || 'FILE';
        setSharedLinkHoverMedia(card, inspectorElement('div', 'shared-link-preview-badge', label));
    }
    positionSharedLinkHover(sharedLinkHoverAnchor);
}

async function showSharedLinkHover(anchor) {
    sharedLinkHoverTimer = null;
    if (!anchor.isConnected || !fileModalEl().classList.contains('hidden')) return;
    const card = sharedLinkHoverCard();
    const token = ++card.token;
    sharedLinkHoverAnchor = anchor;
    card.title.textContent = sharedFileNameFromURL(anchor.getAttribute('href'));
    card.meta.textContent = 'Loading…';
    setSharedLinkHoverMedia(card, null);
    positionSharedLinkHover(anchor);
    card.root.classList.add('visible');
    try {
        const file = await sharedLinkFile(anchor.getAttribute('href'));
        if (token !== card.token) return;
        if (!file) {
            card.meta.textContent = 'This file no longer exists.';
            positionSharedLinkHover(anchor);
            return;
        }
        card.title.textContent = file.name;
        card.meta.textContent = sharedFileMeta(file, false);
        positionSharedLinkHover(anchor);
        const entry = await loadSharedLinkPreview(file);
        if (token !== card.token) return;
        renderSharedLinkHoverPreview(card, file, entry);
    } catch (err) {
        if (token !== card.token) return;
        card.meta.textContent = err.message || 'Preview failed.';
        positionSharedLinkHover(anchor);
    }
}

function positionSharedLinkHover(anchor) {
    if (!sharedLinkHover || !anchor || !anchor.isConnected) return;
    const root = sharedLinkHover.root;
    const box = anchor.getBoundingClientRect();
    const width = root.offsetWidth;
    const height = root.offsetHeight;
    const gap = 8;
    const left = Math.max(8, Math.min(box.left, window.innerWidth - width - 8));
    const above = box.top - gap - height;
    const top = above >= 8 ? above : Math.min(box.bottom + gap, window.innerHeight - height - 8);
    root.style.left = Math.round(left) + 'px';
    root.style.top = Math.round(top) + 'px';
}

function scheduleSharedLinkHover(anchor) {
    if (sharedLinkHoverAnchor === anchor && sharedLinkHover && sharedLinkHover.root.classList.contains('visible')) return;
    if (sharedLinkHoverTimer) clearTimeout(sharedLinkHoverTimer);
    sharedLinkHoverTimer = setTimeout(() => showSharedLinkHover(anchor), sharedLinkHoverDelayMs);
}

function hideSharedLinkHover() {
    if (sharedLinkHoverTimer) clearTimeout(sharedLinkHoverTimer);
    sharedLinkHoverTimer = null;
    sharedLinkHoverAnchor = null;
    if (!sharedLinkHover) return;
    sharedLinkHover.token++;
    sharedLinkHover.root.classList.remove('visible');
    // Drop a running HTML preview so its scripts stop while hidden.
    setSharedLinkHoverMedia(sharedLinkHover, null);
}

async function openSharedLink(anchor) {
    hideSharedLinkHover();
    const href = anchor.getAttribute('href');
    let file;
    try {
        // Always read fresh metadata: the file may have changed since hover.
        const location = sharedLinkLocation(href);
        if (location) sharedLinkListings.delete(location.folder);
        file = await sharedLinkFile(href);
    } catch (err) {
        showErrorToast('Preview failed', err);
        return;
    }
    if (!file) {
        showToast('"' + sharedFileNameFromURL(href) + '" no longer exists.', {kind: 'error'});
        return;
    }
    anchor.focus({preventScroll: true});
    openFileModal(file);
}

function sharedLinkFromEvent(event) {
    const target = event.target;
    const link = target && target.closest ? target.closest('a.shared-file-link') : null;
    return link && conversationEl.contains(link) ? link : null;
}

function startSharedFileLinks() {
    conversationEl.addEventListener('click', event => {
        const link = sharedLinkFromEvent(event);
        // Modified clicks keep the browser's own behaviour (download in a new tab).
        if (!link || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
        event.preventDefault();
        openSharedLink(link);
    });
    conversationEl.addEventListener('mouseover', event => {
        const link = sharedLinkFromEvent(event);
        if (link) scheduleSharedLinkHover(link);
    });
    conversationEl.addEventListener('mouseout', event => {
        const link = sharedLinkFromEvent(event);
        if (link && !link.contains(event.relatedTarget)) hideSharedLinkHover();
    });
    conversationEl.addEventListener('focusin', event => {
        const link = sharedLinkFromEvent(event);
        if (link && link.matches(':focus-visible')) scheduleSharedLinkHover(link);
    });
    conversationEl.addEventListener('focusout', event => {
        if (sharedLinkFromEvent(event)) hideSharedLinkHover();
    });
    conversationEl.addEventListener('scroll', hideSharedLinkHover, {passive: true});
    document.addEventListener('keydown', event => {
        if (event.key === 'Escape') hideSharedLinkHover();
    });
}
