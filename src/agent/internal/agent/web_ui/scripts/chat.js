// Chat submission, steering, streaming, and history reconciliation.
async function sendMessage() {
    if (sendBtn.disabled) return;
    if (currentChatRequestId) {
        await submitSteerMessage();
        return;
    }
    const message = inputEl.value.trim();
    const attachments = cloneAttachmentsForTransport(draftAttachments);
    if (!message && attachments.length === 0) return;

    const pendingAttachments = cloneAttachmentsForMessage(draftAttachments);

    inputEl.value = '';
    autoResizeInput();
    clearDraftAttachments();
    currentChatRequestId = createRequestId();
    externalActiveRequestId = '';
    currentChatAbortController = new AbortController();
    currentChatCancelRequested = false;
    currentChatStartedAt = Date.now();
    resetStopRunArm();
    setComposerState(true); // Call after setting currentChatRequestId so button shows "Steer"

    addMessage({
        type: 'user',
        request_id: currentChatRequestId,
        content: message,
        attachments: pendingAttachments,
        timestamp: new Date().toISOString()
    });

    try {
        const res = await fetch('/api/chat', {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json',
                'Accept': 'application/x-ndjson',
                'X-Aico-Stream': 'ndjson'
            },
            signal: currentChatAbortController.signal,
            body: JSON.stringify({
                message: message,
                attachments: attachments,
                request_id: currentChatRequestId
            })
        });

        if (!res.ok) {
            const errorText = await res.text();
            throw new Error(errorText || 'Request failed');
        }

        await consumeChatStream(res);
    } catch (err) {
        if (currentChatCancelRequested || err.name === 'AbortError') {
            addMessage({
                type: 'assistant',
                content: 'Interrupted.',
                timestamp: new Date().toISOString()
            });
            return;
        }
        console.error('Failed to send message:', err);
        try {
            await loadHistory();
        } catch (_) {}

        addMessage({
            type: 'assistant',
            content: 'Error: ' + err.message,
            timestamp: new Date().toISOString()
        });
    } finally {
        currentChatRequestId = '';
        currentChatAbortController = null;
        currentChatCancelRequested = false;
        currentChatStartedAt = 0;
        resetStopRunArm();
        pendingSteer = null;
        renderPendingSteer();
        setComposerState(false);
        if (isConversationNearBottom()) scrollToBottom();
        refreshHistoryFromContext();
    }
}

async function submitSteerMessage() {
    if (!currentChatRequestId || pendingSteerSubmitting) return;

    const message = inputEl.value.trim();
    if (!message) return;
    if (draftAttachments.length > 0) {
        alert('Attachments can be sent after the current run finishes.');
        return;
    }

    const requestId = currentChatRequestId;
    const localSteer = {
        id: 'local-' + createRequestId(),
        request_id: requestId,
        content: message,
        timestamp: new Date().toISOString()
    };
    pendingSteer = localSteer;
    pendingSteerSubmitting = true;
    inputEl.value = '';
    autoResizeInput();
    renderPendingSteer();
    setComposerState(true);

    try {
        const res = await fetch('/api/chat/steer', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
                request_id: requestId,
                message: message
            })
        });
        if (!res.ok) {
            throw new Error(await res.text() || 'Failed to queue steer.');
        }
        const data = await res.json();
        if (data.steer) {
            pendingSteer = data.steer;
            renderPendingSteer();
        }
    } catch (err) {
        console.error('Failed to queue steer:', err);
        if (currentChatRequestId === requestId) {
            inputEl.value = message;
            autoResizeInput();
        }
        pendingSteer = null;
        renderPendingSteer();
        addMessage({
            type: 'assistant',
            content: 'Error: ' + err.message,
            timestamp: new Date().toISOString()
        });
    } finally {
        pendingSteerSubmitting = false;
        setComposerState(!!currentChatRequestId);
    }
}

function createRequestId() {
    if (window.crypto && window.crypto.randomUUID) {
        return window.crypto.randomUUID();
    }
    return 'web-' + Date.now().toString(36) + '-' + Math.random().toString(36).slice(2);
}

function activeChatRequestId() {
    return currentChatRequestId || externalActiveRequestId || '';
}

function markStopRunPointer(event) {
    stopRunPointer = {
        at: Date.now(),
        pointerType: event && event.pointerType ? event.pointerType : '',
        button: event && typeof event.button === 'number' ? event.button : -1,
        isPrimary: !event || event.isPrimary !== false
    };
}

function resetStopRunArm() {
    stopRunArmedUntil = 0;
    if (stopRunArmTimer) {
        clearTimeout(stopRunArmTimer);
        stopRunArmTimer = null;
    }
    if (stopRunBtn) {
        stopRunBtn.textContent = 'Stop';
    }
}

function armStopRun() {
    stopRunArmedUntil = Date.now() + 3000;
    stopRunBtn.textContent = 'Click again to stop';
    if (stopRunArmTimer) {
        clearTimeout(stopRunArmTimer);
    }
    stopRunArmTimer = setTimeout(function() {
        if (Date.now() >= stopRunArmedUntil) {
            resetStopRunArm();
        }
    }, 3100);
}

async function refreshCurrentLiveActivity() {
    if (currentChatRequestId) return;
    try {
        const res = await fetch('/api/live-activity/current');
        if (!res.ok) return;
        const data = await res.json();
        const state = data.live_activity;
        const isActive = data.status === 'ok' && state && (state.status === 'running' || state.status === 'needs_app');
        externalActiveRequestId = isActive ? state.request_id : '';
        setComposerState(!!activeChatRequestId());
    } catch (err) {
        console.warn('Failed to refresh current live activity:', err);
    }
}

async function cancelCurrentRun(event) {
    if (event) {
        event.preventDefault();
        event.stopPropagation();
    }
    const requestId = activeChatRequestId();
    if (!requestId) return;
    const now = Date.now();
    const sincePointerMs = stopRunPointer ? now - stopRunPointer.at : -1;
    const eventDetail = event && typeof event.detail === 'number' ? event.detail : 0;
    const isPrimaryPointer =
        stopRunPointer &&
        stopRunPointer.isPrimary &&
        (stopRunPointer.button === 0 || stopRunPointer.button === -1);
    const isPointerClick =
        event &&
        event.isTrusted === true &&
        event.type === 'click' &&
        eventDetail > 0 &&
        isPrimaryPointer &&
        sincePointerMs >= 0 &&
        sincePointerMs < 1500;
    if (!isPointerClick) {
        console.warn('Ignored chat cancel without explicit pointer click');
        return;
    }
    if (now >= stopRunArmedUntil) {
        armStopRun();
        return;
    }
    resetStopRunArm();
    currentChatCancelRequested = true;
    stopRunBtn.disabled = true;
    stopRunBtn.textContent = 'Stopping...';
    pendingSteer = null;
    renderPendingSteer();

    if (currentChatAbortController) {
        currentChatAbortController.abort();
    }

    fetch('/api/chat/cancel', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
            request_id: requestId
        }),
        keepalive: true
    }).catch(function(err) {
        console.error('Failed to cancel chat request:', err);
    }).finally(function() {
        if (externalActiveRequestId === requestId) externalActiveRequestId = '';
        setComposerState(!!activeChatRequestId());
    });
}

async function cancelPendingSteer() {
    if (!currentChatRequestId || !pendingSteer) return;
    const requestId = currentChatRequestId;
    const previous = pendingSteer;
    pendingSteer = null;
    renderPendingSteer();

    try {
        const res = await fetch('/api/chat/steer/cancel', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ request_id: requestId })
        });
        if (!res.ok) {
            throw new Error(await res.text() || 'Failed to cancel steer.');
        }
    } catch (err) {
        console.error('Failed to cancel steer:', err);
        if (currentChatRequestId === requestId) {
            pendingSteer = previous;
            renderPendingSteer();
        }
    }
}

async function consumeChatStream(res) {
    let sawDone = false;
    if (!res.body) {
        sawDone = consumeChatStreamText(await res.text());
        if (!sawDone) {
            throw new Error('Chat stream ended before done event.');
        }
        return;
    }

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';

    while (true) {
        const chunk = await reader.read();
        if (chunk.done) break;
        buffer += decoder.decode(chunk.value, { stream: true });
        const result = consumeChatStreamLines(buffer);
        buffer = result.buffer;
        sawDone = sawDone || result.sawDone;
    }

    buffer += decoder.decode();
    const result = consumeChatStreamLines(buffer, true);
    sawDone = sawDone || result.sawDone;
    if (!sawDone) {
        throw new Error('Chat stream ended before done event.');
    }
}

function consumeChatStreamText(text) {
    const trimmed = text.trim();
    if (!trimmed) return false;
    try {
        const payload = JSON.parse(trimmed);
        // A single JSON response carries only the latest turn; the history
        // window is refreshed from /api/history once the run ends.
        if (payload.history || payload.response) return true;
        if (payload.type) {
            return handleChatStreamEvent(payload);
        }
    } catch (_) {
        // Not a single JSON response; parse it below as NDJSON.
    }
    return consumeChatStreamLines(text, true).sawDone;
}

function consumeChatStreamLines(buffer, flush) {
    const lines = buffer.split('\n');
    if (!flush) {
        buffer = lines.pop() || '';
    } else {
        buffer = '';
    }

    let sawDone = false;
    lines.forEach(function(line) {
        line = line.trim();
        if (!line) return;
        let event;
        try {
            event = JSON.parse(line);
        } catch (err) {
            throw new Error('Invalid chat stream event: ' + err.message);
        }
        sawDone = handleChatStreamEvent(event) || sawDone;
    });

    return { buffer, sawDone };
}

function handleChatStreamEvent(event) {
    if (event.type === 'assistant_delta') {
        appendAssistantDelta(event);
        return false;
    }
    if (event.type === 'assistant_reasoning_delta') {
        appendAssistantReasoningDelta(event);
        return false;
    }
    if (event.type === 'assistant_reasoning_reset') {
        resetAssistantReasoning(event);
        return false;
    }
    if (event.type === 'assistant_delta_reset') {
        resetAssistantDelta(event);
        return false;
    }
    if (event.type === 'message' && event.message) {
        if (event.message.type === 'steer') {
            pendingSteer = null;
            renderPendingSteer();
        }
        if (event.message.type === 'assistant') {
            finalizeAssistantMessage(event.message);
        } else {
            addMessage(event.message);
        }
        return false;
    }
    if (event.type === 'done') {
        return true;
    }
    if (event.type === 'error') {
        throw new Error(event.error || 'Agent error');
    }
    return false;
}

function appendAssistantDelta(event) {
    const key = assistantStreamKey(event);
    if (!key) return;
    let msg = streamingAssistantDrafts[key];
    if (!msg) {
        msg = {
            type: 'assistant',
            request_id: event.request_id || '',
            episode_id: event.episode_id || '',
            content: '',
            timestamp: new Date().toISOString()
        };
        streamingAssistantDrafts[key] = msg;
    }
    msg.content += event.delta || '';
    addMessage(msg);
}

function appendAssistantReasoningDelta(event) {
    const key = assistantStreamKey(event);
    if (!key) return;
    let msg = streamingAssistantDrafts[key];
    if (!msg) {
        msg = {
            type: 'assistant',
            request_id: event.request_id || '',
            episode_id: event.episode_id || '',
            content: '',
            reasoning_content: '',
            timestamp: new Date().toISOString()
        };
        streamingAssistantDrafts[key] = msg;
    }
    msg.reasoning_content += event.reasoning_content || '';
    addMessage(msg);
}

function resetAssistantReasoning(event) {
    const key = assistantStreamKey(event);
    if (!key) return;
    const msg = streamingAssistantDrafts[key];
    if (!msg) return;
    msg.reasoning_content = '';
    addMessage(msg);
}

function resetAssistantDelta(event) {
    const key = assistantStreamKey(event);
    if (!key) return;
    const msg = streamingAssistantDrafts[key] || {
        type: 'assistant',
        request_id: event.request_id || '',
        episode_id: event.episode_id || ''
    };
    delete streamingAssistantDrafts[key];
    const messageKey = messageIdentity(msg);
    removeRenderedMessage(messageKey);
}

function removeRenderedMessage(messageKey) {
    const existing = renderedMessageNodes.get(messageKey);
    renderedMessageKeys.delete(messageKey);
    renderedMessageNodes.delete(messageKey);
    if (existing) {
        renderedHistoryFingerprint = null;
        const shouldStickToBottom = isConversationNearBottom();
        existing.remove();
        updateEmptyState();
        if (shouldStickToBottom) scrollToBottom();
    }
}

function finalizeAssistantMessage(msg) {
    const key = assistantStreamKey(msg);
    if (key) {
        delete streamingAssistantDrafts[key];
    }
    const messageKey = messageIdentity(msg);
    if (renderedMessageKeys.has(messageKey)) {
        // A streamed draft may have been inserted before tool events.
        // Re-append the finalized assistant message so the live chat
        // order matches the persisted history and episode trace.
        removeRenderedMessage(messageKey);
    }
    addMessage(msg);
}

function assistantStreamKey(value) {
    value = value || {};
    return value.request_id || value.episode_id || currentChatRequestId || '';
}

function closeClearMenu() {
    const menu = document.getElementById('clearMenu');
    if (menu) menu.removeAttribute('open');
}

async function clearSession() {
    closeClearMenu();
    if (!confirm('Clear the current session? This will remove the current conversation.')) return;

    try {
        const res = await fetch('/api/clear', { method: 'POST' });
        if (!res.ok) {
            throw new Error(await res.text() || 'Failed to clear the session.');
        }
        clearDraftAttachments();
        resetHistoryWindow();
        renderHistory([]);
    } catch (err) {
        console.error('Failed to clear the session:', err);
    }
}

async function clearSessionAndMemory() {
    closeClearMenu();
    if (!confirm('Clear the current session and permanently delete ALL memory, including long-term memories and the user profile?')) return;

    try {
        const res = await fetch('/api/clear-all', { method: 'POST' });
        if (!res.ok) {
            throw new Error(await res.text() || 'Failed to clear the session and memory.');
        }
        clearDraftAttachments();
        resetHistoryWindow();
        renderHistory([]);
    } catch (err) {
        console.error('Failed to clear the session and memory:', err);
    }
}

function renderHistory(history) {
    history = Array.isArray(history) ? history : [];
    const fingerprint = JSON.stringify(history);
    if (fingerprint === renderedHistoryFingerprint) return;

    const shouldStickToBottom = messagesDiv.children.length === 0 || isConversationNearBottom();
    const previousScrollTop = conversationEl.scrollTop;
    const previousMessageNodes = renderedMessageNodes;
    const previousStateMessages = renderedStateMessages;
    const nextMessageKeys = new Set();
    const nextMessageNodes = new Map();
    renderedStateMessages = new Map();
    streamingAssistantDrafts = {};

    const orderedNodes = [];
    history.forEach(function(msg) {
        if (isControlMessage(msg)) return;
        if (isContextMarkerMessage(msg)) {
            const key = messageIdentity(msg);
            if (renderedStateMessages.has(key)) return;

            const index = renderedStateMessages.size;
            let node = previousStateMessages.get(key);
            if (node) {
                updateContextMarkerNode(node, msg, index);
                renderedStateMessages.set(key, node);
            } else {
                node = createContextMarkerNode(msg, index);
            }
            orderedNodes.push(node);
            return;
        }
        const key = messageIdentity(msg);
        if (nextMessageKeys.has(key)) return;
        nextMessageKeys.add(key);

        const incomingFingerprint = messageRenderFingerprint(msg);
        const existing = previousMessageNodes.get(key);
        let node = existing;
        if (!existing || existing._messageFingerprint !== incomingFingerprint) {
            node = createTrackedMessageNode(msg, incomingFingerprint);
            if (existing) existing.replaceWith(node);
        }
        nextMessageNodes.set(key, node);
        orderedNodes.push(node);
    });

    previousMessageNodes.forEach(function(node, key) {
        if (!nextMessageNodes.has(key)) node.remove();
    });
    previousStateMessages.forEach(function(node, key) {
        if (!renderedStateMessages.has(key)) node.remove();
    });

    renderedMessageKeys = nextMessageKeys;
    renderedMessageNodes = nextMessageNodes;
    if (!renderedStateMessages.has(activeStateMessageKey)) {
        activeStateMessageKey = '';
    }
    renderStateModal();

    orderedNodes.forEach(function(node, index) {
        if (messagesDiv.children[index] !== node) {
            messagesDiv.insertBefore(node, messagesDiv.children[index] || null);
        }
    });
    updateEmptyState();
    if (shouldStickToBottom) {
        scrollToBottom();
    } else {
        conversationEl.scrollTop = previousScrollTop;
    }
    renderedHistoryFingerprint = fingerprint;
}

function addMessage(msg) {
    if (isControlMessage(msg)) return;
    if (isContextMarkerMessage(msg)) {
        const key = messageIdentity(msg);
        if (!renderedStateMessages.has(key)) {
            renderedHistoryFingerprint = null;
            const shouldStickToBottom = isConversationNearBottom();
            messagesDiv.appendChild(createContextMarkerNode(msg, renderedStateMessages.size));
            if (shouldStickToBottom) scrollToBottom();
        }
        return;
    }
    const key = messageIdentity(msg);
    if (renderedMessageKeys.has(key)) {
        if (normalizeType((msg || {}).type) === 'assistant') {
            updateMessageNode(key, msg);
        }
        return;
    }
    renderedHistoryFingerprint = null;
    renderedMessageKeys.add(key);
    const shouldStickToBottom = isConversationNearBottom();
    const node = createTrackedMessageNode(msg);
    renderedMessageNodes.set(key, node);
    messagesDiv.appendChild(node);
    updateEmptyState();
    if (shouldStickToBottom) scrollToBottom();
}

function isConversationNearBottom() {
    return conversationEl.scrollHeight - conversationEl.scrollTop - conversationEl.clientHeight < 72;
}

function updateMessageNode(key, msg) {
    const existing = renderedMessageNodes.get(key);
    if (!existing) return;
    const incomingFingerprint = messageRenderFingerprint(msg);
    if (existing._messageFingerprint === incomingFingerprint) return;
    renderedHistoryFingerprint = null;
    const shouldStickToBottom = isConversationNearBottom();
    const replacement = createTrackedMessageNode(msg, incomingFingerprint);
    renderedMessageNodes.set(key, replacement);
    existing.replaceWith(replacement);
    updateEmptyState();
    if (shouldStickToBottom) scrollToBottom();
}

function createTrackedMessageNode(msg, fingerprint) {
    const node = createMessageNode(msg);
    node._messageFingerprint = fingerprint || messageRenderFingerprint(msg);
    return node;
}

function messageRenderFingerprint(msg) {
    const displayMessage = Object.assign({}, msg || {});
    displayMessage.timestamp = formatTime(displayMessage.timestamp);
    return stableSerialize(displayMessage);
}

function stableSerialize(value) {
    if (value === null || typeof value !== 'object') {
        return JSON.stringify(value);
    }
    if (Array.isArray(value)) {
        return '[' + value.map(stableSerialize).join(',') + ']';
    }
    return '{' + Object.keys(value).sort().filter(function(key) {
        return typeof value[key] !== 'undefined';
    }).map(function(key) {
        return JSON.stringify(key) + ':' + stableSerialize(value[key]);
    }).join(',') + '}';
}

function updateContextMarkerNode(node, msg, index) {
    node._stateMessage = msg;
    const button = node.querySelector('.state-divider-button');
    if (!button) return;
    button.title = contextMarkerSummary(msg, index);
    button.setAttribute('aria-expanded', node.classList.contains('open') ? 'true' : 'false');
}

function isControlMessage(msg) {
    return normalizeType((msg || {}).type) === 'todo_closed';
}

function isContextMarkerMessage(msg) {
    msg = msg || {};
    const type = normalizeType(msg.type);
    return type === 'state' || type === 'notice' || msg.role === 'state' || msg.role === 'notice';
}

function messageIdentity(msg) {
    msg = msg || {};
    const type = isContextMarkerMessage(msg) ? contextMarkerType(msg) : normalizeType(msg.type);
    const content = msg.content || '';
    const requestId = msg.request_id || '';
    if (requestId && (type === 'tool_call' || type === 'tool_result')) {
        return [
            'request', requestId, type,
            msg.tool_name || '', msg.tool_input || '', content, msg.timestamp || ''
        ].join('\u001f');
    }
    if (requestId && type === 'assistant') {
        return ['request', requestId, type].join('\u001f');
    }
    if (requestId && type === 'user') {
        return ['request', requestId, type, content].join('\u001f');
    }
    if (requestId && (type === 'state' || type === 'notice')) {
        return ['request', requestId, type].join('\u001f');
    }

    const episodeId = msg.episode_id || '';
    if (episodeId && type === 'assistant') {
        return ['episode', episodeId, type].join('\u001f');
    }
    if (episodeId) {
        return ['episode', episodeId, type, msg.role || '', msg.tool_name || '', msg.tool_input || '', content, msg.timestamp || ''].join('\u001f');
    }

    return ['local', type, content, msg.timestamp || ''].join('\u001f');
}
