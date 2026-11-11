// Collapsible tool activity groups and screenshot payload handling.
function createToolGroupNode(group) {
    const node = document.createElement('article');
    node.className = 'tool-group';
    node._open = false;
    node._openEntries = new Set();

    const head = document.createElement('div');
    head.className = 'tool-group-head';
    const summary = document.createElement('button');
    summary.type = 'button';
    summary.className = 'tool-group-summary';
    summary.setAttribute('aria-expanded', 'false');
    summary.addEventListener('click', function() {
        node._open = !node._open;
        renderToolGroupList(node);
    });
    const thumbs = document.createElement('div');
    thumbs.className = 'tool-thumbs';
    head.appendChild(summary);
    head.appendChild(thumbs);
    node.appendChild(head);
    node._thumbs = thumbs;

    const list = document.createElement('div');
    list.className = 'tool-group-list';
    node.appendChild(list);

    node._summary = summary;
    node._list = list;
    updateToolGroupNode(node, group);
    return node;
}

function updateToolGroupNode(node, group) {
    const fingerprint = toolGroupFingerprint(group.messages);
    node._toolGroup = group;
    if (node._fingerprint === fingerprint) return;
    node._fingerprint = fingerprint;
    node._entries = toolGroupEntries(group.messages);
    renderToolGroupSummary(node);
    renderToolGroupList(node);
}

function toolGroupFingerprint(messages) {
    return messages.map(function(msg) {
        return [messageIdentity(msg), (msg.content || '').length, (msg.attachments || []).length,
            msg.is_error ? 1 : 0].join('\u001e');
    }).join('\u001d');
}

function toolEntryState(entry) {
    if (!entry.result) return 'pending';
    return entry.result.is_error || entry.result.tool_error ? 'error' : 'ok';
}

function entryToolName(entry) {
    return (entry.call && entry.call.tool_name) || (entry.result && entry.result.tool_name) || 'tool';
}

function renderToolGroupSummary(node) {
    const entries = node._entries;
    const tools = entries.filter(entry => entry.kind === 'tool');
    const states = entries.length - tools.length;
    const failed = tools.filter(entry => toolEntryState(entry) === 'error').length;
    const pending = tools.filter(entry => toolEntryState(entry) === 'pending');

    const counts = new Map();
    tools.forEach(function(entry) {
        const name = entryToolName(entry);
        counts.set(name, (counts.get(name) || 0) + 1);
    });
    const names = Array.from(counts, ([name, count]) => count > 1 ? name + ' ×' + count : name);

    const summary = node._summary;
    summary.innerHTML = '';
    summary.classList.toggle('pending', pending.length > 0);

    const title = document.createElement('span');
    title.className = 'tool-group-title';
    title.textContent = pending.length > 0
        ? 'Running ' + entryToolName(pending[pending.length - 1]) + '…'
        : 'Ran ' + tools.length + (tools.length === 1 ? ' tool' : ' tools');
    summary.appendChild(title);

    const details = [];
    if (names.length) details.push(names.join(', '));
    if (states) details.push(states + (states === 1 ? ' state update' : ' state updates'));
    if (details.length) {
        const meta = document.createElement('span');
        meta.className = 'tool-group-meta';
        meta.textContent = details.join(' · ');
        summary.appendChild(meta);
    }
    if (failed) {
        const error = document.createElement('span');
        error.className = 'tool-group-error';
        error.textContent = failed + ' failed';
        summary.appendChild(error);
    }
    const chevron = document.createElement('span');
    chevron.className = 'tool-chevron';
    chevron.setAttribute('aria-hidden', 'true');
    summary.appendChild(chevron);
    renderToolGroupThumbs(node);
}

const maxGroupThumbs = 3;

// Thumbnails of the group's images (state screenshots and screenshot tool
// results), newest last, so the collapsed row shows what the tools saw.
function renderToolGroupThumbs(node) {
    const images = [];
    node._entries.forEach(entry => images.push.apply(images, toolEntryImages(entry)));
    node._images = images;
    const container = node._thumbs;
    container.innerHTML = '';
    const shown = images.slice(-maxGroupThumbs);
    const offset = images.length - shown.length;
    if (offset > 0) {
        const more = document.createElement('button');
        more.type = 'button';
        more.className = 'tool-thumb-more';
        more.textContent = '+' + offset;
        more.title = images.length + ' images';
        more.addEventListener('click', () => openImageViewer(images, 0));
        container.appendChild(more);
    }
    shown.forEach(function(image, index) {
        container.appendChild(createToolThumb(images, offset + index));
    });
}

function toolEntryImages(entry) {
    if (entry.kind !== 'tool') {
        return (entry.message.attachments || []).filter(item => item.kind === 'image').map(function(attachment) {
            return {
                src: attachmentDataURL(attachment),
                caption: (contextMarkerType(entry.message) === 'notice' ? 'Notice' : 'State') + ' · ' + (attachment.name || 'image')
            };
        }).filter(image => image.src);
    }
    const screenshot = entry.result && parseScreenshotPayload(entry.result);
    if (!screenshot) return [];
    return [{
        src: 'data:image/' + (screenshot.format || 'jpeg') + ';base64,' + screenshot.data,
        caption: entryToolName(entry) + ' · ' + screenshot.width + '×' + screenshot.height
    }];
}

function createToolThumb(images, index) {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'tool-thumb';
    button.title = images[index].caption;
    button.setAttribute('aria-label', 'View ' + images[index].caption);
    const image = document.createElement('img');
    image.alt = '';
    image.loading = 'lazy';
    image.decoding = 'async';
    image.src = images[index].src;
    button.appendChild(image);
    button.addEventListener('click', function(event) {
        event.stopPropagation();
        hideThumbPreview();
        openImageViewer(images, index);
    });
    button.addEventListener('mouseenter', () => scheduleThumbPreview(button, images, index));
    button.addEventListener('mouseleave', hideThumbPreview);
    button.addEventListener('focus', () => scheduleThumbPreview(button, images, index));
    button.addEventListener('blur', hideThumbPreview);
    return button;
}

// One floating preview, shown above (or below) the hovered thumbnail after a
// short delay so sweeping the pointer across a row doesn't flash it.
let thumbPreview = null;
let thumbPreviewTimer = null;

function scheduleThumbPreview(anchor, images, index) {
    if (thumbPreviewTimer) clearTimeout(thumbPreviewTimer);
    const delay = thumbPreview && thumbPreview.root.classList.contains('visible') ? 0 : 150;
    thumbPreviewTimer = setTimeout(() => showThumbPreview(anchor, images, index), delay);
}

function showThumbPreview(anchor, images, index) {
    thumbPreviewTimer = null;
    if (!anchor.isConnected || (imageViewer && !imageViewer.root.classList.contains('hidden'))) return;
    if (!thumbPreview) {
        const root = document.createElement('div');
        root.className = 'thumb-preview';
        root.setAttribute('aria-hidden', 'true');
        const image = document.createElement('img');
        image.alt = '';
        const caption = document.createElement('div');
        caption.className = 'thumb-preview-caption';
        const title = document.createElement('span');
        const count = document.createElement('span');
        caption.appendChild(title);
        caption.appendChild(count);
        root.appendChild(image);
        root.appendChild(caption);
        document.body.appendChild(root);
        thumbPreview = {root, image, title, count};
        image.addEventListener('load', () => positionThumbPreview(thumbPreview.anchor));
    }
    const current = images[index];
    thumbPreview.anchor = anchor;
    if (thumbPreview.image.getAttribute('src') !== current.src) thumbPreview.image.src = current.src;
    thumbPreview.title.textContent = current.caption;
    thumbPreview.count.textContent = images.length > 1 ? (index + 1) + ' of ' + images.length : '';
    positionThumbPreview(anchor);
    thumbPreview.root.classList.add('visible');
}

function positionThumbPreview(anchor) {
    if (!thumbPreview || !anchor || !anchor.isConnected) return;
    const root = thumbPreview.root;
    const box = anchor.getBoundingClientRect();
    const width = root.offsetWidth;
    const height = root.offsetHeight;
    const gap = 8;
    const left = Math.max(8, Math.min(box.left + box.width / 2 - width / 2, window.innerWidth - width - 8));
    const above = box.top - gap - height;
    const top = above >= 8 ? above : Math.min(box.bottom + gap, window.innerHeight - height - 8);
    root.style.left = Math.round(left) + 'px';
    root.style.top = Math.round(top) + 'px';
}

function hideThumbPreview() {
    if (thumbPreviewTimer) clearTimeout(thumbPreviewTimer);
    thumbPreviewTimer = null;
    if (thumbPreview) thumbPreview.root.classList.remove('visible');
}

// Full-size viewer shared by every thumbnail: arrows step through the
// images it was opened with, Escape or a click outside closes it.
let imageViewer = null;

function openImageViewer(images, index) {
    if (!images.length) return;
    if (!imageViewer) imageViewer = createImageViewer();
    imageViewer.images = images;
    imageViewer.returnFocus = document.activeElement;
    showImageViewerImage(Math.max(0, Math.min(index, images.length - 1)));
    imageViewer.root.classList.remove('hidden');
    document.body.classList.add('state-modal-open');
    imageViewer.close.focus();
}

function closeImageViewer() {
    if (!imageViewer || imageViewer.root.classList.contains('hidden')) return;
    imageViewer.root.classList.add('hidden');
    imageViewer.image.removeAttribute('src');
    if (!activeStateMessageKey) document.body.classList.remove('state-modal-open');
    if (imageViewer.returnFocus && imageViewer.returnFocus.focus) imageViewer.returnFocus.focus();
}

function showImageViewerImage(index) {
    const viewer = imageViewer;
    const count = viewer.images.length;
    viewer.index = (index + count) % count;
    const current = viewer.images[viewer.index];
    viewer.image.src = current.src;
    viewer.image.alt = current.caption;
    viewer.caption.textContent = current.caption + (count > 1 ? '  ·  ' + (viewer.index + 1) + ' / ' + count : '');
    viewer.prev.hidden = viewer.next.hidden = count < 2;
}

function createImageViewer() {
    const root = document.createElement('div');
    root.className = 'image-viewer hidden';
    root.setAttribute('role', 'dialog');
    root.setAttribute('aria-modal', 'true');
    root.setAttribute('aria-label', 'Image viewer');
    const figure = document.createElement('figure');
    const image = document.createElement('img');
    const caption = document.createElement('figcaption');
    figure.appendChild(image);
    figure.appendChild(caption);
    const button = function(className, label, text, action) {
        const element = document.createElement('button');
        element.type = 'button';
        element.className = className;
        element.setAttribute('aria-label', label);
        element.textContent = text;
        element.addEventListener('click', function(event) {
            event.stopPropagation();
            action();
        });
        root.appendChild(element);
        return element;
    };
    root.appendChild(figure);
    const viewer = {root, image, caption, images: [], index: 0};
    viewer.close = button('image-viewer-close', 'Close image', '×', closeImageViewer);
    viewer.prev = button('image-viewer-nav prev', 'Previous image', '‹', () => showImageViewerImage(viewer.index - 1));
    viewer.next = button('image-viewer-nav next', 'Next image', '›', () => showImageViewerImage(viewer.index + 1));
    root.addEventListener('click', function(event) {
        if (event.target === root || event.target === figure) closeImageViewer();
    });
    document.addEventListener('keydown', function(event) {
        if (root.classList.contains('hidden')) return;
        if (event.key === 'Escape') {
            event.stopImmediatePropagation();
            closeImageViewer();
        } else if (event.key === 'ArrowLeft' && viewer.images.length > 1) {
            showImageViewerImage(viewer.index - 1);
        } else if (event.key === 'ArrowRight' && viewer.images.length > 1) {
            showImageViewerImage(viewer.index + 1);
        }
    }, true);
    document.body.appendChild(root);
    return viewer;
}

function renderToolGroupList(node) {
    node.classList.toggle('open', node._open);
    node._summary.setAttribute('aria-expanded', node._open ? 'true' : 'false');
    const list = node._list;
    list.innerHTML = '';
    if (!node._open) return;
    node._entries.forEach(function(entry, index) {
        list.appendChild(createToolEntryNode(node, entry, index));
    });
}

function createToolEntryNode(groupNode, entry, index) {
    const open = groupNode._openEntries.has(index);
    const item = document.createElement('div');
    const status = entry.kind === 'tool' ? toolEntryState(entry) : 'state';
    item.className = 'tool-entry ' + status + (open ? ' open' : '');

    const row = document.createElement('button');
    row.type = 'button';
    row.className = 'tool-entry-row';
    row.setAttribute('aria-expanded', open ? 'true' : 'false');

    const verb = document.createElement('span');
    verb.className = 'tool-entry-verb';
    const name = document.createElement('span');
    name.className = 'tool-entry-name';
    const preview = document.createElement('span');
    preview.className = 'tool-entry-preview';

    if (entry.kind === 'tool') {
        verb.textContent = status === 'pending' ? 'Calling' : status === 'error' ? 'Failed' : 'Called';
        name.textContent = entryToolName(entry);
        preview.textContent = compactToolInput(entry.call ? entry.call.tool_input : '');
    } else {
        verb.textContent = contextMarkerType(entry.message) === 'notice' ? 'Notice' : 'State';
        const content = formatContextMarkerContent(entry.message.content || '', contextMarkerType(entry.message));
        const firstLine = content.split('\n').map(line => line.trim()).find(Boolean);
        const imageCount = (entry.message.attachments || []).filter(item => item.kind === 'image').length;
        name.textContent = imageCount ? (imageCount === 1 ? 'screenshot' : imageCount + ' images') : '';
        preview.textContent = firstLine || '';
    }
    row.appendChild(verb);
    if (name.textContent) row.appendChild(name);
    row.appendChild(preview);
    const chevron = document.createElement('span');
    chevron.className = 'tool-chevron';
    chevron.setAttribute('aria-hidden', 'true');
    row.appendChild(chevron);
    row.addEventListener('click', function() {
        if (groupNode._openEntries.has(index)) groupNode._openEntries.delete(index);
        else groupNode._openEntries.add(index);
        item.replaceWith(createToolEntryNode(groupNode, entry, index));
    });
    const head = document.createElement('div');
    head.className = 'tool-entry-head';
    head.appendChild(row);
    const entryImages = toolEntryImages(entry);
    if (entryImages.length) {
        // Step through the whole group's images from any entry thumbnail.
        const groupImages = groupNode._images || entryImages;
        const first = groupImages.findIndex(image => image.src === entryImages[0].src);
        head.appendChild(createToolThumb(first >= 0 ? groupImages : entryImages, Math.max(first, 0)));
    }
    item.appendChild(head);

    if (open) {
        const detail = document.createElement('div');
        detail.className = 'tool-entry-detail';
        if (entry.kind === 'tool') {
            renderToolEntryDetail(detail, entry);
        } else {
            renderContextMarkerDetails(detail, entry.message);
        }
        item.appendChild(detail);
    }
    return item;
}

function compactToolInput(value) {
    if (!value) return '';
    try {
        const parsed = redactToolPayloadForDisplay(JSON.parse(value));
        if (parsed && typeof parsed === 'object' && !Array.isArray(parsed) && Object.keys(parsed).length === 0) return '';
        return JSON.stringify(parsed);
    } catch (_) {
        return String(value).replace(/\s+/g, ' ');
    }
}

function renderToolEntryDetail(container, entry) {
    if (entry.call) {
        container.appendChild(createToolSection('Call', entry.call.tool_input || '{}'));
    }
    if (!entry.result) {
        const pending = document.createElement('div');
        pending.className = 'tool-section-label';
        pending.textContent = 'Waiting for result…';
        container.appendChild(pending);
        return;
    }
    renderToolResult(entry.result).forEach(section => container.appendChild(section));
}

function createToolSection(label, payload, labelClass) {
    const section = document.createElement('div');
    section.className = 'tool-section';

    const sectionLabel = document.createElement('div');
    sectionLabel.className = 'tool-section-label' + (labelClass ? ' ' + labelClass : '');
    sectionLabel.textContent = label;
    section.appendChild(sectionLabel);

    const block = document.createElement('pre');
    block.className = 'tool-block';
    renderToolPayload(block, payload || '');
    section.appendChild(block);
    return section;
}

function renderToolResult(msg) {
    const isError = msg.is_error || msg.tool_error;
    const screenshot = parseScreenshotPayload(msg);
    if (!screenshot) {
        return [createToolSection(isError ? 'Error' : 'Result', msg.content || '', isError ? 'error' : '')];
    }

    const sections = [];
    const metaSection = document.createElement('div');
    metaSection.className = 'tool-section';

    const metaLabel = document.createElement('div');
    metaLabel.className = 'tool-section-label';
    metaLabel.textContent = 'Screenshot';
    metaSection.appendChild(metaLabel);

    const grid = document.createElement('div');
    grid.className = 'tool-meta-grid';

    const metaEntries = [
        ['Format', screenshot.format || 'jpeg'],
        ['Width', String(screenshot.width)],
        ['Height', String(screenshot.height)],
        ['Bytes', String(screenshot.size)]
    ];
    if (screenshot.action_output) {
        metaEntries.unshift(['Action', screenshot.action_output]);
    }
    metaEntries.forEach(function(entry) {
        const item = document.createElement('div');
        item.className = 'tool-meta-item';

        const key = document.createElement('div');
        key.className = 'tool-meta-key';
        key.textContent = entry[0];

        const value = document.createElement('div');
        value.className = 'tool-meta-value';
        value.textContent = entry[1];

        item.appendChild(key);
        item.appendChild(value);
        grid.appendChild(item);
    });

    metaSection.appendChild(grid);
    sections.push(metaSection);

    const preview = document.createElement('div');
    preview.className = 'screenshot-preview';

    const image = document.createElement('img');
    image.alt = 'Screenshot preview';
    image.src = 'data:image/' + (screenshot.format || 'jpeg') + ';base64,' + screenshot.data;
    preview.appendChild(image);
    sections.push(preview);

    const details = document.createElement('details');
    details.className = 'tool-details';

    const summary = document.createElement('summary');
    summary.textContent = 'Raw payload';
    details.appendChild(summary);

    const rawBlock = document.createElement('pre');
    rawBlock.className = 'tool-block';
    renderToolPayload(rawBlock, msg.content || '');
    details.appendChild(rawBlock);
    sections.push(details);
    return sections;
}

function formatToolPayload(value) {
    if (!value) return '';
    try {
        return JSON.stringify(redactToolPayloadForDisplay(JSON.parse(value)), null, 2);
    } catch (_) {
        return value;
    }
}

function renderToolPayload(block, value) {
    if (!value) {
        block.textContent = '';
        return;
    }
    try {
        const parsed = redactToolPayloadForDisplay(JSON.parse(value));
        const formatted = JSON.stringify(parsed, null, 2);
        block.classList.add('json-highlight');
        block.innerHTML = highlightJson(formatted);
    } catch (_) {
        block.textContent = value;
    }
}

function highlightJson(value) {
    const tokenPattern = /(\"(?:\\.|[^\"\\])*\"(?=\s*:))|(\"(?:\\.|[^\"\\])*\")|(-?\b\d+(?:\.\d+)?(?:[eE][+-]?\d+)?\b)|(\btrue\b|\bfalse\b)|(\bnull\b)/g;
    let html = '';
    let lastIndex = 0;
    let match;
    while ((match = tokenPattern.exec(value)) !== null) {
        html += escapeHtml(value.slice(lastIndex, match.index));
        const tokenClass = match[1] ? 'json-key' : match[2] ? 'json-string' : match[3] ? 'json-number' : match[4] ? 'json-boolean' : 'json-null';
        html += '<span class="' + tokenClass + '">' + escapeHtml(match[0]) + '</span>';
        lastIndex = match.index + match[0].length;
    }
    return html + escapeHtml(value.slice(lastIndex));
}

function escapeHtml(value) {
    return String(value).replace(/[&<>\"']/g, function(character) {
        return {'&': '&amp;', '<': '&lt;', '>': '&gt;', '\"': '&quot;', "'": '&#39;'}[character];
    });
}

function redactToolPayloadForDisplay(value) {
    if (Array.isArray(value)) {
        return value.map(redactToolPayloadForDisplay);
    }
    if (!value || typeof value !== 'object') {
        return value;
    }

    const clone = {};
    Object.keys(value).forEach(function(key) {
        clone[key] = redactToolPayloadForDisplay(value[key]);
    });
    if (isScreenshotPayload(value)) {
        const bytes = Number(value.size);
        const byteLabel = Number.isFinite(bytes) && bytes >= 0 ? bytes + ' bytes' : 'base64 omitted';
        clone.data = '[base64 screenshot omitted: ' + byteLabel + ']';
    }
    return clone;
}

function parseScreenshotPayload(msg) {
    return parseScreenshotOutput(msg.tool_name || '', msg.content || '');
}

function parseScreenshotOutput(toolName, content) {
    if (!content) return null;
    try {
        const parsed = JSON.parse(content);
        if (!isScreenshotPayload(parsed)) return null;
        return parsed;
    } catch (_) {
        return null;
    }
}

function isScreenshotPayload(value) {
    return !!value &&
        typeof value === 'object' &&
        typeof value.data === 'string' &&
        value.data.length > 0 &&
        (value.format === 'jpeg' || value.format === 'jpg' || value.format === 'png' || value.width || value.height || value.size);
}
