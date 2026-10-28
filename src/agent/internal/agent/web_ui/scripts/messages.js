// Conversation message and episode rendering.
function createMessageNode(msg) {
    const card = document.createElement('article');
    card.className = 'message ' + normalizeType(msg.type);

    const shell = document.createElement('div');
    shell.className = 'message-shell';

    const avatar = document.createElement('div');
    avatar.className = 'message-avatar';
    avatar.textContent = getAvatarLabel(msg.type);

    const body = document.createElement('div');
    body.className = 'message-body';

    const role = document.createElement('div');
    role.className = 'message-role';
    role.textContent = getRoleLabel(msg.type, msg.tool_name, msg.role);

    // Add voice indicator for voice messages
    if (msg.source === 'voice') {
        const voiceIcon = document.createElement('span');
        voiceIcon.className = 'voice-icon';
        voiceIcon.textContent = ' 🎤';
        voiceIcon.title = 'Voice message';
        role.appendChild(voiceIcon);
    }

    body.appendChild(role);

    if (msg.type === 'tool_call') {
        body.appendChild(renderToolCall(msg));
    } else if (msg.type === 'tool_result') {
        body.appendChild(renderToolResult(msg));
    } else {
        const attachmentsEl = renderMessageAttachments(msg.attachments || []);
        if (attachmentsEl) {
            body.appendChild(attachmentsEl);
        }

        if (msg.reasoning_content) {
            const reasoningDiv = document.createElement('div');
            reasoningDiv.className = 'message-reasoning';
            const reasoningLabel = document.createElement('div');
            reasoningLabel.className = 'reasoning-label';
            reasoningLabel.textContent = 'Reasoning';
            reasoningDiv.appendChild(reasoningLabel);
            const reasoningText = document.createElement('div');
            reasoningText.className = 'reasoning-text';
            reasoningText.textContent = msg.reasoning_content;
            reasoningDiv.appendChild(reasoningText);
            body.appendChild(reasoningDiv);
        }

        if (msg.content) {
            const contentDiv = document.createElement('div');
            contentDiv.className = 'message-copy';
            contentDiv.textContent = msg.content || '';
            body.appendChild(contentDiv);
        }
    }

    // Add audio playback button for voice messages with archived audio
    if (msg.audio_file && msg.audio_file !== '') {
        const audioBtn = document.createElement('button');
        audioBtn.type = 'button';
        audioBtn.className = 'audio-playback-btn';
        audioBtn.textContent = '▶️ Play Audio';

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

    shell.appendChild(avatar);
    shell.appendChild(body);
    card.appendChild(shell);
    return card;
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

    const line = document.createElement('div');
    line.className = 'state-divider-line';

    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'state-divider-button';
    button.textContent = markerLabel;
    button.title = contextMarkerSummary(msg, index);
    button.setAttribute('aria-controls', 'stateModal');
    button.setAttribute('aria-expanded', key === activeStateMessageKey ? 'true' : 'false');
    button.addEventListener('click', function() {
        toggleStateDetails(key);
    });
    line.appendChild(button);
    card.appendChild(line);
    return card;
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
            stateModalBodyEl.appendChild(table);
        } else {
            const copy = document.createElement('pre');
            copy.className = 'state-detail-copy';
            copy.textContent = content;
            stateModalBodyEl.appendChild(copy);
        }
    }
    const attachmentsEl = renderMessageAttachments(msg.attachments || []);
    if (attachmentsEl) {
        attachmentsEl.classList.add('state-detail-attachments');
        stateModalBodyEl.classList.add('has-attachments');
        stateModalBodyEl.appendChild(attachmentsEl);
    }
    if (!content && !attachmentsEl) {
        const empty = document.createElement('div');
        empty.className = 'state-detail-empty';
        empty.textContent = 'No ' + markerLabel.toLowerCase() + ' details.';
        stateModalBodyEl.appendChild(empty);
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
    sendBtn.disabled = pendingSteerSubmitting || (isLoading && !currentChatRequestId);
    sendBtn.textContent = currentChatRequestId ? 'Steer' : 'Send';
    const imageSlotsFull = getDraftImageCount() >= maxDraftImageAttachments;
    imageBtn.disabled = isLoading || imageSlotsFull;
    imageBtn.title = imageSlotsFull ? 'Maximum 4 images attached' : 'Add image';
    stopRunBtn.disabled = !isLoading;
    if (!isLoading) {
        resetStopRunArm();
    } else if (Date.now() < stopRunArmedUntil) {
        stopRunBtn.textContent = 'Click again to stop';
    } else {
        stopRunBtn.textContent = 'Stop';
    }
    loadingDiv.classList.toggle('active', isLoading);
    renderPendingSteer();
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

function getAvatarLabel(type) {
    if (type === 'user') return 'You';
    if (type === 'steer') return 'Edit';
    if (type === 'episode_status') return 'Task';
    if (type === 'role_output') return 'Role';
    if (type === 'tool_call') return 'Call';
    if (type === 'tool_result') return 'Tool';
    if (type === 'state') return 'State';
    return 'AI';
}

function formatTime(timestamp) {
    if (!timestamp) return '';
    const date = new Date(timestamp);
    if (Number.isNaN(date.getTime())) return '';
    return date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

function openImagePicker() {
    if (imageBtn.disabled) return;
    imageInputEl.click();
}
