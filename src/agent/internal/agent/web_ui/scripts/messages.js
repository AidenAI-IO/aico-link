// Conversation message and episode rendering.
function createMessageNode(msg) {
    const card = document.createElement('article');
    card.className = 'message ' + normalizeType(msg.type);

    const shell = document.createElement('div');
    shell.className = 'message-shell';

    const body = document.createElement('div');
    body.className = 'message-body';

    // User and assistant turns are identified by layout; other kinds keep a label.
    const type = normalizeType(msg.type);
    if (type !== 'user' && type !== 'assistant') {
        const role = document.createElement('div');
        role.className = 'message-role';
        role.textContent = getRoleLabel(msg.type, msg.tool_name, msg.role);
        body.appendChild(role);
    }

    const attachmentsEl = renderMessageAttachments(msg.attachments || []);
    if (attachmentsEl) {
        body.appendChild(attachmentsEl);
    }

    if (msg.reasoning_content) {
        // Streaming reveal writes into .reasoning-text inside this line.
        body.appendChild(createCollapsibleLine({
            className: 'message-reasoning',
            iconClass: 'reasoning-icon',
            textClass: 'reasoning-text',
            label: 'Reasoning',
            text: msg.reasoning_content,
            kind: 'reasoning',
            expandKey: messageIdentity(msg) + '\u001freasoning'
        }));
    }

    if (msg.content) {
        const contentDiv = document.createElement('div');
        contentDiv.className = 'message-copy';
        if (type === 'user' || type === 'steer') {
            contentDiv.textContent = msg.content || '';
        } else {
            // Streaming reveal writes through this target so partial
            // <tts> markup is styled as it appears.
            card._copyTarget = createSpeechTextTarget(contentDiv, messageIdentity(msg));
            card._copyTarget.textContent = msg.content || '';
        }
        body.appendChild(contentDiv);
    }

    if (msg.type === 'assistant') {
        const downloads = renderMessageAttachments(sharedLinksFromText(msg.content));
        if (downloads) body.appendChild(downloads);
    }

    // Add audio playback button for voice messages with archived audio
    if (msg.audio_file && msg.audio_file !== '') {
        const audioBtn = document.createElement('button');
        audioBtn.type = 'button';
        audioBtn.className = 'audio-playback-btn';
        audioBtn.textContent = 'Play recording';

        const filename = msg.audio_file.split('/').pop();
        const audioUrl = '/api/audio/' + encodeURIComponent(filename);

        if (msg.audio_duration_ms > 0) {
            const durationSec = (msg.audio_duration_ms / 1000).toFixed(1);
            audioBtn.title = 'Duration: ' + durationSec + 's';
        }

        audioBtn.addEventListener('click', function() {
            playAudio(audioUrl, audioBtn);
        });

        body.appendChild(audioBtn);
    }

    const footer = document.createElement('div');
    footer.className = 'message-footer';

    if (msg.source === 'voice') {
        const voice = document.createElement('span');
        voice.className = 'message-voice';
        voice.textContent = 'Voice';
        voice.title = 'Voice message';
        footer.appendChild(voice);
    }

    const formattedTime = formatTime(msg.timestamp);
    if (formattedTime) {
        const timeDiv = document.createElement('div');
        timeDiv.className = 'message-time';
        timeDiv.textContent = formattedTime;
        footer.appendChild(timeDiv);
    }

    const usageDiv = renderMessageUsage(msg.usage);
    if (usageDiv) {
        footer.appendChild(usageDiv);
    }
    if (footer.childNodes.length > 0) {
        body.appendChild(footer);
    }

    shell.appendChild(body);
    card.appendChild(shell);
    return card;
}

const speechTagPairs = [['<tts>', '</tts>'], ['[tts]', '[/tts]']];

// Spoken and reasoning rows the user expanded: one entry per row, keyed by
// message identity, holding the row's latest text and element. Streaming
// re-renders reuse the key. When history replaces a live message (its run ID
// becomes a context ID), migrateExpandedLines moves the entry from the
// removed row to the new row with the same text.
const expandedLines = new Map();
// Rows rendered since the last migration. Only rows whose key is new (history
// re-keys a replaced message) can inherit an entry, so a re-render of another
// row with identical text never expands.
let freshCollapsibleLines = [];
const knownLineKeys = new Set();

function migrateExpandedLines() {
    const fresh = freshCollapsibleLines.filter(block => block.isConnected &&
        !expandedLines.has(block._lineKey) && !knownLineKeys.has(block._lineKey));
    freshCollapsibleLines.forEach(block => knownLineKeys.add(block._lineKey));
    if (knownLineKeys.size > 2000) knownLineKeys.clear();
    freshCollapsibleLines = [];
    Array.from(expandedLines).forEach(function([key, entry]) {
        if (entry.block.isConnected) return;
        expandedLines.delete(key);
        // Kind includes the row's slot in its message, so of two identical
        // spoken rows the same one stays open.
        const index = fresh.findIndex(block => block._lineKind === entry.kind && block._lineText === entry.text);
        if (index < 0) return;
        const target = fresh.splice(index, 1)[0];
        expandedLines.set(target._lineKey, {kind: entry.kind, text: entry.text, block: target});
        target._syncExpanded(true);
    });
}

function createSpeechTextTarget(element, key) {
    let text = '';
    return {
        get textContent() { return text; },
        set textContent(value) {
            text = String(value || '');
            renderSpeechText(element, text, key);
        }
    };
}

// Split assistant text into spoken (<tts>/[tts]) and visible segments. An
// unterminated block is still being streamed; a trailing partial tag is hidden.
function splitSpeechText(text) {
    const segments = [];
    let rest = String(text || '');
    while (rest) {
        const code = markdownCodeRanges(rest);
        let start = -1;
        let pair = null;
        speechTagPairs.forEach(function(candidate) {
            const index = indexOutsideCode(rest, candidate[0], 0, code);
            if (index >= 0 && (start < 0 || index < start)) {
                start = index;
                pair = candidate;
            }
        });
        if (start < 0) {
            segments.push({speech: false, text: trimPartialTag(rest, speechTagPairs.map(p => p[0]))});
            break;
        }
        if (start > 0) segments.push({speech: false, text: rest.slice(0, start)});
        rest = rest.slice(start + pair[0].length);
        const end = rest.toLowerCase().indexOf(pair[1]);
        if (end < 0) {
            segments.push({speech: true, open: true, text: trimPartialTag(rest, [pair[1]])});
            break;
        }
        segments.push({speech: true, text: rest.slice(0, end)});
        rest = rest.slice(end + pair[1].length);
    }
    return segments.filter(segment => segment.speech || segment.text);
}

// [start, end) ranges of fenced code blocks and inline code spans, so a
// documented <tts> or a TOML [tts] section is not taken as speech markup.
// An unclosed fence runs to the end (it may still be streaming).
function markdownCodeRanges(text) {
    const ranges = [];
    const fence = /^ {0,3}(`{3,}|~{3,})[^\n]*$/gm;
    let match;
    while ((match = fence.exec(text))) {
        const closing = new RegExp('^ {0,3}' + match[1][0] + '{' + match[1].length + ',}[ \\t]*$', 'gm');
        closing.lastIndex = match.index + match[0].length;
        const close = closing.exec(text);
        const end = close ? close.index + close[0].length : text.length;
        ranges.push([match.index, end]);
        fence.lastIndex = end;
    }
    const inline = /(`+)[^`\n]*?\1/g;
    while ((match = inline.exec(text))) {
        const start = match.index;
        if (!ranges.some(range => start >= range[0] && start < range[1])) {
            ranges.push([start, start + match[0].length]);
        }
    }
    return ranges;
}

function indexOutsideCode(text, tag, from, ranges) {
    const lower = text.toLowerCase();
    for (let index = lower.indexOf(tag, from); index >= 0; index = lower.indexOf(tag, index + 1)) {
        if (!ranges.some(range => index >= range[0] && index < range[1])) return index;
    }
    return -1;
}

function trimPartialTag(text, tags) {
    const lower = text.toLowerCase();
    for (let length = Math.min(text.length, 6); length > 0; length--) {
        const suffix = lower.slice(-length);
        if (tags.some(tag => tag.length > length && tag.indexOf(suffix) === 0)) {
            return text.slice(0, -length);
        }
    }
    return text;
}

function renderSpeechText(element, text, key) {
    element.innerHTML = '';
    let speechIndex = 0;
    splitSpeechText(text).forEach(function(segment) {
        if (segment.speech) {
            const slot = speechIndex++;
            element.appendChild(createSpeechBlock(segment, (key || '') + '\u001f' + slot, 'speech:' + slot));
            return;
        }
        if (!segment.text.trim()) return;
        const markdown = document.createElement('div');
        markdown.className = 'markdown';
        renderMarkdown(markdown, segment.text);
        element.appendChild(markdown);
    });
}

// Spoken text shows one line; clicking toggles the full text.
function createSpeechBlock(segment, expandKey, kind) {
    return createCollapsibleLine({
        className: 'tts-block' + (segment.open ? ' streaming' : ''),
        iconClass: 'tts-icon',
        textClass: 'tts-text',
        label: 'Spoken aloud',
        kind,
        text: segment.text.trim(),
        expandKey
    });
}

// A quiet one-line row (icon + text with an ellipsis) that expands to the
// full text on click or Enter. Expanded rows stay open across re-renders.
function createCollapsibleLine(options) {
    const block = document.createElement('div');
    const key = options.expandKey;
    const entry = expandedLines.get(key);
    const expanded = !!entry;
    if (entry) {
        // A streaming re-render: keep one entry, now pointing at this row.
        entry.text = options.text;
        entry.block = block;
    } else {
        block._lineKey = key;
        block._lineKind = options.kind;
        block._lineText = options.text;
        freshCollapsibleLines.push(block);
        if (freshCollapsibleLines.length > 400) freshCollapsibleLines.splice(0, 200);
    }
    block.className = 'collapsible-line ' + options.className + (expanded ? ' expanded' : '');
    block.tabIndex = 0;
    block.setAttribute('role', 'button');
    const icon = document.createElement('span');
    icon.className = 'line-icon ' + options.iconClass;
    icon.setAttribute('aria-hidden', 'true');
    const copy = document.createElement('span');
    copy.className = 'line-text ' + options.textClass;
    copy.textContent = options.text;
    block.appendChild(icon);
    block.appendChild(copy);

    function sync(open) {
        block.classList.toggle('expanded', open);
        block.setAttribute('aria-expanded', open ? 'true' : 'false');
        block.title = open ? options.label : options.label + ' · click to show all';
    }
    function toggle() {
        // Keep a text selection inside the row from collapsing it.
        const selection = window.getSelection ? window.getSelection() : null;
        if (selection && !selection.isCollapsed && block.contains(selection.anchorNode)) return;
        const open = !block.classList.contains('expanded');
        if (open) expandedLines.set(key, {kind: options.kind, text: options.text, block});
        else expandedLines.delete(key);
        sync(open);
    }
    block._syncExpanded = sync;
    sync(expanded);
    block.addEventListener('click', toggle);
    block.addEventListener('keydown', function(event) {
        if (event.key !== 'Enter' && event.key !== ' ') return;
        event.preventDefault();
        toggle();
    });
    return block;
}

function renderMessageUsage(usage) {
    if (!usage || typeof usage !== 'object') return null;

    const root = document.createElement('div');
    root.className = 'message-usage';
    root.title = 'LLM token usage';

    [
        ['Input', usage.input_tokens],
        ['Output', usage.output_tokens],
        ['Total', usage.total_tokens]
    ].forEach(function(metric) {
        const item = document.createElement('span');
        item.className = 'message-usage-item';
        item.textContent = metric[0] + ' ' + formatTokenCount(metric[1]);
        root.appendChild(item);
    });
    return root;
}

function formatTokenCount(value) {
    const count = Number(value);
    if (!Number.isFinite(count) || count < 0) return '0';
    return Math.floor(count).toLocaleString();
}

function createContextMarkerNode(msg, index) {
    const key = messageIdentity(msg);
    const markerType = contextMarkerType(msg);
    const markerLabel = markerType === 'notice' ? 'Notice' : 'State';
    const card = document.createElement('article');
    card.className = 'state-divider ' + markerType + '-divider' + (key === activeStateMessageKey ? ' open' : '');
    card.dataset.stateKey = key;
    card._stateMessage = msg;
    renderedStateMessages.set(key, card);

    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'state-divider-button';
    button.setAttribute('aria-controls', 'stateModal');
    button.setAttribute('aria-expanded', key === activeStateMessageKey ? 'true' : 'false');
    const label = document.createElement('span');
    label.className = 'state-divider-label';
    label.textContent = markerLabel;
    const preview = document.createElement('span');
    preview.className = 'state-divider-preview';
    button.appendChild(label);
    button.appendChild(preview);
    button.addEventListener('click', function() {
        toggleStateDetails(key);
    });
    const head = document.createElement('div');
    head.className = 'state-divider-head';
    head.appendChild(button);
    const thumbs = document.createElement('div');
    thumbs.className = 'tool-thumbs';
    head.appendChild(thumbs);
    card.appendChild(head);
    setContextMarkerPreview(button, msg, index);
    return card;
}

function renderContextMarkerThumbs(card, msg) {
    const container = card.querySelector('.tool-thumbs');
    if (!container) return;
    container.innerHTML = '';
    const images = toolEntryImages({kind: 'marker', message: msg});
    images.slice(0, maxGroupThumbs).forEach(function(image, index) {
        container.appendChild(createToolThumb(images, index));
    });
}

function setContextMarkerPreview(button, msg, index) {
    button.title = contextMarkerSummary(msg, index);
    const preview = button.querySelector('.state-divider-preview');
    if (!preview) return;
    const markerType = contextMarkerType(msg);
    const content = formatContextMarkerContent(msg.content || '', markerType);
    const firstLine = content.split('\n').map(line => line.trim()).find(Boolean);
    preview.textContent = firstLine || ((msg.attachments || []).length ? 'attachment' : '');
    const card = button.closest ? button.closest('.state-divider') : null;
    if (card) renderContextMarkerThumbs(card, msg);
}

function toggleStateDetails(key) {
    activeStateMessageKey = activeStateMessageKey === key ? '' : key;
    renderedStateMessages.forEach(function(node, nodeKey) {
        const open = nodeKey === activeStateMessageKey;
        node.classList.toggle('open', open);
        const button = node.querySelector('.state-divider-button');
        if (button) button.setAttribute('aria-expanded', open ? 'true' : 'false');
    });
    renderStateModal();
}

function closeStateDetails() {
    activeStateMessageKey = '';
    renderedStateMessages.forEach(function(node) {
        node.classList.remove('open');
        const button = node.querySelector('.state-divider-button');
        if (button) button.setAttribute('aria-expanded', 'false');
    });
    renderStateModal();
}

function renderStateModal() {
    const node = renderedStateMessages.get(activeStateMessageKey);
    const msg = node && node._stateMessage;
    stateModalEl.classList.toggle('hidden', !msg);
    document.body.classList.toggle('state-modal-open', !!msg);
    if (!msg) {
        stateModalKickerEl.textContent = 'State details';
        stateModalTitleEl.textContent = 'State';
        stateModalBodyEl.innerHTML = '';
        return;
    }

    const markerType = contextMarkerType(msg);
    const markerLabel = markerType === 'notice' ? 'Notice' : 'State';
    stateModalKickerEl.textContent = markerLabel + ' details';
    stateModalTitleEl.textContent = markerLabel;
    stateModalBodyEl.innerHTML = '';
    stateModalBodyEl.classList.remove('has-attachments');
    renderContextMarkerDetails(stateModalBodyEl, msg);
}

function renderContextMarkerDetails(container, msg) {
    const markerType = contextMarkerType(msg);
    const markerLabel = markerType === 'notice' ? 'Notice' : 'State';
    const content = formatContextMarkerContent(msg.content || '', markerType);
    if (content) {
        const rows = markerType === 'state' ? parseStateKeyValues(content) : null;
        if (rows) {
            const table = document.createElement('table');
            table.className = 'state-detail-table';
            const body = document.createElement('tbody');
            rows.forEach(function(row) {
                const tr = document.createElement('tr');
                const key = document.createElement('th');
                key.scope = 'row';
                key.textContent = row.key;
                const value = document.createElement('td');
                value.textContent = row.value;
                tr.appendChild(key);
                tr.appendChild(value);
                body.appendChild(tr);
            });
            table.appendChild(body);
            container.appendChild(table);
        } else {
            const copy = document.createElement('pre');
            copy.className = 'state-detail-copy';
            copy.textContent = content;
            container.appendChild(copy);
        }
    }
    const attachmentsEl = renderMessageAttachments(msg.attachments || []);
    if (attachmentsEl) {
        attachmentsEl.classList.add('state-detail-attachments');
        container.classList.add('has-attachments');
        container.appendChild(attachmentsEl);
    }
    if (!content && !attachmentsEl) {
        const empty = document.createElement('div');
        empty.className = 'state-detail-empty';
        empty.textContent = 'No ' + markerLabel.toLowerCase() + ' details.';
        container.appendChild(empty);
    }
}

function formatContextMarkerContent(content, markerType) {
    const trimmed = String(content || '').trim();
    const tag = markerType === 'notice' ? 'notice' : 'state';
    const match = trimmed.match(new RegExp('^<' + tag + '>\\s*([\\s\\S]*?)\\s*</' + tag + '>$', 'i'));
    return match ? match[1].trim() : trimmed;
}

function parseStateKeyValues(content) {
    const lines = String(content || '').split(/\r?\n/);
    const rows = [];
    let invalid = false;
    lines.forEach(function(line) {
        const trimmed = line.trim();
        if (!trimmed) return;
        const separator = trimmed.indexOf(':');
        if (separator <= 0) {
            if (rows.length > 0) {
                rows[rows.length - 1].value += '\n' + trimmed;
            } else {
                invalid = true;
            }
            return;
        }
        rows.push({
            key: trimmed.slice(0, separator).trim(),
            value: trimmed.slice(separator + 1).trim()
        });
    });
    return !invalid && rows.length > 0 ? rows : null;
}

function contextMarkerType(msg) {
    msg = msg || {};
    return normalizeType(msg.type) === 'notice' || msg.role === 'notice' ? 'notice' : 'state';
}

function contextMarkerSummary(msg, index) {
    const markerType = contextMarkerType(msg);
    const markerLabel = markerType === 'notice' ? 'Notice' : 'State';
    const content = formatContextMarkerContent(msg.content || '', markerType);
    const firstLine = content.split('\n').map(function(line) { return line.trim(); }).find(Boolean);
    if (firstLine) return markerLabel + ' ' + (index + 1) + ': ' + firstLine;
    if ((msg.attachments || []).length > 0) return markerLabel + ' ' + (index + 1) + ': attachment';
    return markerLabel + ' ' + (index + 1);
}

function playAudio(url, button) {
    const audio = new Audio(url);

    const originalText = button.textContent;
    button.textContent = '⏸️ Playing...';
    button.disabled = true;

    audio.onended = function() {
        button.textContent = originalText;
        button.disabled = false;
    };

    audio.onerror = function() {
        button.textContent = '❌ Error';
        setTimeout(function() {
            button.textContent = originalText;
            button.disabled = false;
        }, 2000);
    };

    audio.play().catch(function(err) {
        console.error('[Audio] Play failed:', err);
        button.textContent = '❌ Error';
        setTimeout(function() {
            button.textContent = originalText;
            button.disabled = false;
        }, 2000);
    });
}

function updateEmptyState() {
    emptyStateEl.classList.toggle('hidden', messagesDiv.children.length > 0);
}

function setComposerState(isLoading) {
    sendBtn.disabled = fileUploadPending || pendingSteerSubmitting || (isLoading && !currentChatRequestId);
    sendBtn.textContent = currentChatRequestId ? 'Steer' : 'Send';
    const attachmentSlotsFull = draftAttachments.length >= maxDraftAttachments;
    const fileBtn = document.getElementById('fileBtn');
    if (fileBtn) {
        fileBtn.disabled = fileUploadPending || isLoading || attachmentSlotsFull;
        fileBtn.title = attachmentSlotsFull ? 'Attachment limit reached' : 'Add file';
    }
    stopRunBtn.disabled = !isLoading;
    if (!isLoading) {
        resetStopRunArm();
    } else if (Date.now() < stopRunArmedUntil) {
        stopRunBtn.textContent = 'Click again to stop';
    } else {
        stopRunBtn.textContent = 'Stop';
    }
    loadingDiv.classList.toggle('active', isLoading);
    setComposerBusy(isLoading);
    setActivityRunning(isLoading);
    renderPendingSteer();
}

// The light sweep starts past the left edge when a run starts. When the run
// ends it finishes its current pass instead of vanishing mid-edge; the wait
// is timed from the pass start because animation events are throttled in
// background tabs.
const composerSweepMs = 1800;

function setComposerBusy(isLoading) {
    if (!composerShellEl) return;
    if (composerBusyStopTimer) clearTimeout(composerBusyStopTimer);
    composerBusyStopTimer = null;
    if (isLoading) {
        if (!composerShellEl.classList.contains('busy')) {
            composerShellEl.classList.add('busy');
            composerBusyStartedAt = Date.now();
        }
        return;
    }
    if (!composerShellEl.classList.contains('busy')) return;
    const elapsed = Date.now() - composerBusyStartedAt;
    const remaining = composerSweepMs - (elapsed % composerSweepMs);
    composerBusyStopTimer = setTimeout(function() {
        composerBusyStopTimer = null;
        composerShellEl.classList.remove('busy');
    }, remaining);
}

function setActivityRunning(isLoading) {
    if (!isLoading) {
        activityStartedAt = 0;
        if (activityTimer) clearInterval(activityTimer);
        activityTimer = null;
        return;
    }
    if (!activityStartedAt) activityStartedAt = currentChatStartedAt || Date.now();
    if (!activityTimer) activityTimer = setInterval(renderActivity, 250);
    renderActivity();
}

function renderActivity() {
    const label = currentActivityLabel();
    if (activityLabelEl.textContent !== label) activityLabelEl.textContent = label;
    const seconds = Math.max(0, Math.floor((Date.now() - activityStartedAt) / 1000));
    activityTimeEl.textContent = seconds < 60
        ? seconds + 's'
        : Math.floor(seconds / 60) + 'm ' + String(seconds % 60).padStart(2, '0') + 's';
}

// Describe the run's current step from the latest projected message.
function currentActivityLabel() {
    for (let i = projectedMessages.length - 1; i >= 0; i--) {
        const msg = projectedMessages[i];
        if (isContextMarkerMessage(msg)) continue;
        const type = normalizeType(msg.type);
        if (isToolMessage(msg)) {
            let start = i;
            while (start > 0 && (isToolMessage(projectedMessages[start - 1]) ||
                isContextMarkerMessage(projectedMessages[start - 1]))) start--;
            const pending = toolGroupEntries(projectedMessages.slice(start, i + 1).filter(isToolMessage))
                .filter(entry => entry.kind === 'tool' && !entry.result);
            return pending.length ? 'Running ' + entryToolName(pending[pending.length - 1]) : 'Thinking';
        }
        if (type === 'assistant' && streamingAssistantDrafts[assistantStreamKey(msg)]) {
            return msg.content ? 'Writing' : 'Reasoning';
        }
        return 'Thinking';
    }
    return 'Thinking';
}

function renderPendingSteer() {
    if (!pendingSteer) {
        pendingSteerEl.classList.add('hidden');
        pendingSteerTextEl.textContent = '';
        cancelSteerBtn.disabled = true;
        return;
    }
    pendingSteerEl.classList.remove('hidden');
    pendingSteerTextEl.textContent = pendingSteer.content || '';
    cancelSteerBtn.disabled = pendingSteerSubmitting;
}

function autoResizeInput() {
    inputEl.style.height = 'auto';
    inputEl.style.height = Math.min(inputEl.scrollHeight, 180) + 'px';
}

function scrollToBottom() {
    conversationEl.scrollTop = conversationEl.scrollHeight;
}

function normalizeType(type) {
    return type || 'assistant';
}

function getRoleLabel(type, toolName, role) {
    if (type === 'user') return 'You';
    if (type === 'steer') return 'Steer';
    if (type === 'episode_status') return 'Task';
    if (type === 'role_output') return 'Role · ' + (role || 'agent');
    if (type === 'tool_call') return toolName ? 'Tool Call · ' + toolName : 'Tool Call';
    if (type === 'tool_result') return toolName ? 'Tool Result · ' + toolName : 'Tool Result';
    if (type === 'state') return 'State';
    return 'Aico';
}

function formatTime(timestamp) {
    if (!timestamp) return '';
    const date = new Date(timestamp);
    if (Number.isNaN(date.getTime())) return '';
    return date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}
