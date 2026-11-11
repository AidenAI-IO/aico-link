// File, image, and audio attachments.
async function handleComposerPaste(event) {
    const files = clipboardImageFiles(event.clipboardData);
    if (files.length === 0) return;
    event.preventDefault();
    await addAttachmentFiles(files, 'pasted');
}

function clipboardImageFiles(clipboardData) {
    if (!clipboardData) return [];
    const files = [];
    const items = Array.from(clipboardData.items || []);
    items.forEach(function(item) {
        if (item.kind === 'file' && item.type && item.type.indexOf('image/') === 0) {
            const file = item.getAsFile();
            if (file) files.push(file);
        }
    });
    if (files.length > 0) return files;
    return Array.from(clipboardData.files || []).filter(isImageFile);
}

function isImageFile(file) {
    if (!file) return false;
    const mimeType = String(file.type || '').toLowerCase();
    if (mimeType.indexOf('image/') === 0) return true;
    return /\.(png|jpe?g|gif|webp|bmp|tiff?)$/i.test(file.name || '');
}

function getDraftImageCount() {
    return draftAttachments.filter(function(attachment) {
        return attachment.kind === 'image';
    }).length;
}

function canAttachFilesNow() {
    return !fileUploadPending && !loadingDiv.classList.contains('active');
}

function imageAttachmentName(file, source, id) {
    if (file && file.name && file.name.trim()) return file.name.trim();
    const prefix = source === 'pasted' ? 'pasted-image' : 'image';
    return prefix + '-' + id + '.' + imageExtensionFromType(file ? file.type : '');
}

function imageExtensionFromType(mimeType) {
    switch (String(mimeType || '').toLowerCase()) {
    case 'image/jpeg':
    case 'image/jpg':
        return 'jpg';
    case 'image/gif':
        return 'gif';
    case 'image/webp':
        return 'webp';
    case 'image/png':
    default:
        return 'png';
    }
}

function imageMIMEType(file, dataUrl) {
    const fileType = String(file && file.type || '').toLowerCase();
    if (fileType.indexOf('image/') === 0) return fileType;
    const name = String(file && file.name || '').toLowerCase();
    if (/\.(jpe?g)$/.test(name)) return 'image/jpeg';
    if (/\.gif$/.test(name)) return 'image/gif';
    if (/\.webp$/.test(name)) return 'image/webp';
    if (/\.bmp$/.test(name)) return 'image/bmp';
    if (/\.tiff?$/.test(name)) return 'image/tiff';
    return imageMIMETypeFromDataURL(dataUrl);
}

function imageMIMETypeFromDataURL(dataUrl) {
    const match = String(dataUrl || '').match(/^data:([^;,]+)[;,]/);
    const mimeType = match && match[1] ? match[1].toLowerCase() : '';
    return mimeType.indexOf('image/') === 0 ? mimeType : 'image/png';
}

function setComposerHint(text, isError) {
    if (!composerHintEl) return;
    if (composerHintTimer) {
        clearTimeout(composerHintTimer);
        composerHintTimer = null;
    }
    composerHintEl.textContent = text || defaultComposerHint;
    composerHintEl.classList.toggle('error', !!isError);
    if (text) {
        composerHintTimer = setTimeout(resetComposerHint, 2800);
    }
}

function resetComposerHint() {
    if (!composerHintEl) return;
    if (composerHintTimer) {
        clearTimeout(composerHintTimer);
        composerHintTimer = null;
    }
    composerHintEl.textContent = defaultComposerHint;
    composerHintEl.classList.remove('error');
}

function renderDraftAttachments() {
    draftAttachmentsEl.innerHTML = '';

    draftAttachments.forEach(function(attachment) {
        const row = document.createElement('div');
        row.className = 'draft-attachment';

        if (attachment.kind === 'image') {
            const image = document.createElement('img');
            image.alt = attachment.name || 'Image attachment';
            image.src = attachmentDataURL(attachment);
            row.appendChild(image);
        } else if (attachment.kind === 'audio') {
            const audio = document.createElement('audio');
            audio.controls = true;
            audio.src = attachmentDataURL(attachment);
            row.appendChild(audio);
        }

        const copy = document.createElement('div');
        copy.className = 'draft-attachment-copy';

        const name = document.createElement('div');
        name.className = 'draft-attachment-name';
        name.textContent = attachment.name || getAttachmentTitle(attachment.kind);
        copy.appendChild(name);

        const meta = document.createElement('div');
        meta.className = 'draft-attachment-meta';
        meta.textContent = getAttachmentTitle(attachment.kind) + ' · ' + formatBytes(attachment.size || 0);
        copy.appendChild(meta);
        row.appendChild(copy);

        const removeBtn = document.createElement('button');
        removeBtn.type = 'button';
        removeBtn.className = 'draft-remove';
        removeBtn.textContent = 'Remove';
        removeBtn.onclick = function() {
            removeDraftAttachment(attachment.id);
        };
        row.appendChild(removeBtn);

        draftAttachmentsEl.appendChild(row);
    });

    setComposerState(loadingDiv.classList.contains('active'));
}

function removeDraftAttachment(id) {
    draftAttachments = draftAttachments.filter(function(attachment) {
        if (attachment.id === id) {
            revokeAttachmentPreview(attachment);
            return false;
        }
        return true;
    });
    renderDraftAttachments();
}

function clearDraftAttachments() {
    draftAttachments.forEach(revokeAttachmentPreview);
    draftAttachments = [];
    renderDraftAttachments();
}

function revokeAttachmentPreview(attachment) {
    if (attachment && attachment.preview_url && attachment.preview_url.indexOf('blob:') === 0) {
        URL.revokeObjectURL(attachment.preview_url);
    }
}

function renderMessageAttachments(attachments) {
    if (!attachments || attachments.length === 0) return null;

    const wrapper = document.createElement('div');
    wrapper.className = 'message-attachments';

    attachments.forEach(function(attachment) {
        const card = document.createElement('div');
        card.className = 'attachment-card';

        if (attachment.kind === 'image') {
            const image = document.createElement('img');
            image.alt = attachment.name || 'Image attachment';
            image.src = attachmentDataURL(attachment);
            image.className = 'zoomable';
            image.addEventListener('click', function() {
                openImageViewer([{src: image.src, caption: attachment.name || 'Image'}], 0);
            });
            card.appendChild(image);
        } else if (attachment.kind === 'audio') {
            const audio = document.createElement('audio');
            audio.controls = true;
            audio.src = attachmentDataURL(attachment);
            card.appendChild(audio);
        }

        const meta = document.createElement('div');
        meta.className = 'attachment-meta';

        const badge = document.createElement('span');
        badge.className = 'attachment-kind';
        badge.textContent = getAttachmentTitle(attachment.kind);
        meta.appendChild(badge);

        const name = document.createElement('span');
        name.textContent = attachment.name || getAttachmentTitle(attachment.kind);
        meta.appendChild(name);
        card.appendChild(meta);

        const downloadURL = safeSharedURL(attachment.download_url);
        if (downloadURL) {
            const link = document.createElement('a');
            link.href = downloadURL;
            // Let Content-Disposition trigger the save. A forced download
            // would save a redirected login page as if it were the artifact.
            link.target = '_blank';
            link.rel = 'noopener';
            link.textContent = 'Download';
            card.appendChild(link);
        }

        if (attachment.transcript) {
            const transcript = document.createElement('div');
            transcript.className = 'attachment-transcript';
            transcript.textContent = 'Transcript: ' + attachment.transcript;
            card.appendChild(transcript);
        }

        wrapper.appendChild(card);
    });

    return wrapper;
}

function cloneAttachmentsForTransport(attachments) {
    return attachments.map(function(attachment) {
        return {
            kind: attachment.kind,
            name: attachment.name,
            mime_type: attachment.mime_type,
            data: attachment.data,
            size: attachment.size,
            path: attachment.path,
            download_url: attachment.download_url,
            transcript: attachment.transcript || ''
        };
    });
}

function cloneAttachmentsForMessage(attachments) {
    return attachments.map(function(attachment) {
        return {
            kind: attachment.kind,
            name: attachment.name,
            mime_type: attachment.mime_type,
            data: attachment.data,
            size: attachment.size,
            path: attachment.path,
            download_url: attachment.download_url,
            preview_url: attachment.preview_url || '',
            transcript: attachment.transcript || ''
        };
    });
}

function attachmentDataURL(attachment) {
    if (attachment.preview_url) return attachment.preview_url;
    if (!attachment.data) return '';
    return 'data:' + (attachment.mime_type || 'application/octet-stream') + ';base64,' + attachment.data;
}

function getAttachmentTitle(kind) {
    if (kind === 'audio') return 'Audio';
    if (kind === 'image') return 'Image';
    return 'Attachment';
}

function formatBytes(size) {
    if (!size) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB'];
    let value = size;
    let unitIndex = 0;
    while (value >= 1024 && unitIndex < units.length - 1) {
        value = value / 1024;
        unitIndex++;
    }
    const rounded = unitIndex === 0 ? String(Math.round(value)) : value.toFixed(1);
    return rounded + ' ' + units[unitIndex];
}

function extractBase64(dataUrl) {
    const parts = String(dataUrl || '').split(',');
    return parts.length > 1 ? parts[1] : '';
}

function readFileAsDataURL(file) {
    return new Promise(function(resolve, reject) {
        const reader = new FileReader();
        reader.onload = function() { resolve(reader.result); };
        reader.onerror = function() { reject(reader.error || new Error('Failed to read file.')); };
        reader.readAsDataURL(file);
    });
}

async function handleFileSelection(event) {
    const files = Array.from(event.target.files || []);
    event.target.value = '';
    await addAttachmentFiles(files, 'selected');
}

// Process mixed selections independently, preserving successes and reporting
// failures together. Login expiration stops the batch so the draft can be retried.
// Images retain visual input; other files upload separately and use board paths.
async function addAttachmentFiles(files, source) {
    if (!files.length) return;
    if (!canAttachFilesNow()) {
        setComposerHint('Attachments can be added after the current operation finishes.', true);
        return;
    }
    fileUploadPending = true;
    setComposerState(loadingDiv.classList.contains('active'));
    let skipped = 0;
    let failed = 0;
    let firstError = '';
    try {
        for (const file of files) {
            if (draftAttachments.length >= maxDraftAttachments) {
                skipped++;
                continue;
            }
            try {
                if (isImageFile(file)) {
                    if (getDraftImageCount() >= maxDraftImageAttachments) {
                        skipped++;
                        continue;
                    }
                    const id = nextAttachmentId++;
                    const dataUrl = await readFileAsDataURL(file);
                    draftAttachments.push({
                        id: id,
                        kind: 'image',
                        name: imageAttachmentName(file, source, id),
                        mime_type: imageMIMEType(file, dataUrl),
                        data: extractBase64(dataUrl),
                        size: file.size || 0
                    });
                    continue;
                }
                if (file.size > 100 * 1024 * 1024) throw new Error('File exceeds the 100 MiB limit: ' + file.name);
                setComposerHint('Uploading ' + file.name + '…');
                const form = new FormData();
                form.append('file', file, file.name);
                const controller = new AbortController();
                const timeout = setTimeout(() => controller.abort(), 300000);
                try {
                    const response = await fetch('/api/files', {method: 'POST', body: form, signal: controller.signal});
                    if (!response.ok) {
                        const message = await response.text();
                        let code = '';
                        try { code = JSON.parse(message).error; } catch (_) {}
                        const error = new Error(code === 'lan_access_disabled'
                            ? 'LAN access is off. Connect over USB and enable it in Settings.'
                            : message || 'Upload failed');
                        error.loginRequired = response.status === 401 && code === 'login_required';
                        throw error;
                    }
                    const uploaded = await response.json();
                    draftAttachments.push(Object.assign({}, uploaded, {id: nextAttachmentId++}));
                } finally { clearTimeout(timeout); }
            } catch (err) {
                if (err.loginRequired) throw err;
                failed++;
                if (!firstError) firstError = err.name === 'AbortError' ? 'Upload timed out. Please try again.' : err.message;
            }
        }
        const hints = [];
        if (failed > 0) hints.push(failed + (failed === 1 ? ' file' : ' files') + ' could not be attached. ' + firstError);
        if (skipped > 0) hints.push(skipped + (skipped === 1 ? ' file' : ' files') + ' skipped. Attach up to ' + maxDraftImageAttachments + ' images and ' + maxDraftAttachments + ' files/images in total.');
        if (hints.length > 0) setComposerHint(hints.join(' '), true);
        else resetComposerHint();
    } catch (err) {
        if (err.loginRequired) showSharedFileLoginHint();
        else setComposerHint(err.name === 'AbortError' ? 'Upload timed out. Please try again.' : err.message, true);
    } finally {
        fileUploadPending = false;
        renderDraftAttachments();
    }
}

function showSharedFileLoginHint() {
    setComposerHint('Sign in to upload files, then select the file again. ', true);
    if (composerHintTimer) {
        clearTimeout(composerHintTimer);
        composerHintTimer = null;
    }
    if (!composerHintEl) return;
    const link = document.createElement('a');
    // Keep this page and its draft intact while the shared host cookie is set.
    link.href = '/login?next=%2Fshare%2F';
    link.target = '_blank';
    link.rel = 'noopener';
    link.textContent = 'Sign in';
    composerHintEl.appendChild(link);
}

function safeSharedURL(value) {
    if (!value) return '';
    try {
        const url = new URL(value, window.location.origin);
        if (!['http:', 'https:'].includes(url.protocol) || url.hostname !== window.location.hostname || !url.pathname.startsWith('/share/')) return '';
        // Use the current origin so downloads also work through an Agent reverse proxy.
        return url.pathname;
    } catch (_) { return ''; }
}

function sharedLinksFromText(text) {
    const result = [];
    const seen = new Set();
    const pattern = /\[([^\]\n]+)\]\(([^\s)]+)\)|https?:\/\/[^\s<>"')]+|\/share\/[^\s<>"')\]]+/g;
    for (const match of String(text || '').matchAll(pattern)) {
        const target = match[2] || match[0].replace(/[.,;:!?。，；：！？、]+$/, '');
        const href = safeSharedURL(target);
        if (!href || seen.has(href) || href.endsWith('/')) continue;
        seen.add(href);
        let name = href.split('/').pop();
        try { name = decodeURIComponent(name); } catch (_) {}
        result.push({kind: 'file', name: match[1] || name, download_url: href});
    }
    return result;
}
