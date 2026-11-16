// Chat submission, steering, streaming, and history reconciliation.
// Canonical messages retain every received character. Only their text nodes
// reveal incrementally, so presentation never blocks stream consumption.
const assistantTextReveal = createStreamingTextReveal({
    requestFrame: callback => window.requestAnimationFrame(callback),
    cancelFrame: id => window.cancelAnimationFrame(id),
    now: () => performance.now(),
    immediate: () => document.hidden || (window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches),
    beforeFrame: () => isConversationNearBottom(),
    afterFrame: shouldStickToBottom => { if (shouldStickToBottom) scrollToBottom(); },
    onIdle: () => resumeHistoryAfterTextReveal()
});

async function sendMessage(options) {
    // With an empty box the primary button starts or ends voice. Enter only
    // sends, so a stray keypress never opens the microphone.
    const mode = composerPrimaryMode();
    if (mode === 'voice' || mode === 'end') {
        if (options && options.fromKeyboard) return;
        return mode === 'voice' ? startRealtimeVoice() : endRealtimeVoice();
    }
    if (sendBtn.disabled) return;
    if (steerableRequestId()) {
        await submitSteerMessage();
        return;
    }
    const message = inputEl.value.trim();
    const attachments = cloneAttachmentsForTransport(draftAttachments);
    if (!message && attachments.length === 0) return;

    const pendingAttachments = cloneAttachmentsForMessage(draftAttachments);

    assistantTextReveal.flush();
    inputEl.value = '';
    autoResizeInput();
    clearDraftAttachments();
    currentChatRequestId = createRequestId();
    rememberNDJSONOwnedRequest(currentChatRequestId);
    externalActiveRequestId = '';
    externalActiveSteerable = false;
    externalActiveGeneration++;
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

    let completed = false;
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
        completed = true;
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
        showErrorToast('Message failed', err);
        try {
            await loadHistory();
        } catch (_) {}
    } finally {
        if (!completed) assistantTextReveal.flush();
        // SSE terminal events leave this request's draft to its stream. Once
        // the stream is over, a draft still held (an error or interruption
        // before done) must not block later history refreshes.
        delete streamingAssistantDrafts[currentChatRequestId];
        assistantDraftBlockedByTool.delete(currentChatRequestId);
        currentChatRequestId = '';
        currentChatAbortController = null;
        currentChatCancelRequested = false;
        currentChatStartedAt = 0;
        resetStopRunArm();
        pendingSteer = null;
        renderPendingSteer();
        setComposerState(false);
        if (isConversationNearBottom()) scrollToBottom();
        // Only after a completed run: an error or interruption shown locally
        // may not be in the context yet, and a refresh would remove it.
        if (completed) refreshHistoryFromContext();
    }
}

async function submitSteerMessage() {
    if (!steerableRequestId() || pendingSteerSubmitting) return;

    const message = inputEl.value.trim();
    if (!message) return;
    if (draftAttachments.length > 0) {
        showToast('Attachments can be sent after the current run finishes.', {kind: 'info'});
        return;
    }

    const requestId = steerableRequestId();
    // A refresh read before this steer would clear it from the page.
    externalActiveGeneration++;
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
        if (res.status === 409) {
            // The run is past its last steer check and is finishing.
            restoreSteerDraft(requestId, message, localSteer);
            showToast('The run is finishing. Send this after it ends.', {kind: 'info'});
            return;
        }
        if (!res.ok) {
            throw new Error(await res.text() || 'Failed to queue steer.');
        }
        const data = await res.json();
        // The run may already have taken the steer and streamed it back.
        if (data.steer && pendingSteer === localSteer) {
            pendingSteer = data.steer;
            renderPendingSteer();
        }
    } catch (err) {
        console.error('Failed to queue steer:', err);
        restoreSteerDraft(requestId, message, localSteer);
        showErrorToast('Steer failed', err);
    } finally {
        // A status read started while the steer was in flight may predate it.
        externalActiveGeneration++;
        pendingSteerSubmitting = false;
        setComposerState(!!activeChatRequestId());
        refreshRestoredRunAfterSteerRequest();
    }
}

// Status reads are ignored while a steer request is in flight; read a
// restored run's state again once it ends instead of waiting for the poll.
function refreshRestoredRunAfterSteerRequest() {
    if (!currentChatRequestId && externalActiveRequestId) refreshCurrentLiveActivity();
}

function restoreSteerDraft(requestId, message, localSteer) {
    // The run may have ended while the steer was in flight; the text still
    // belongs in the composer then, but never in a different run's.
    const currentRequestId = steerableRequestId();
    const sameOrNoRun = !currentRequestId || currentRequestId === requestId;
    if (sameOrNoRun && !inputEl.value.trim()) {
        inputEl.value = message;
        autoResizeInput();
    }
    if (pendingSteer === localSteer) {
        pendingSteer = null;
        renderPendingSteer();
    }
}

// The server publishes a consumed steer as the run's user message. Text
// streamed before it is a finished reply; seal it so the reply to the steer
// starts below the steer instead of extending the earlier text.
function settlePendingSteer(message) {
    if (message.type !== 'user') return;
    sealAssistantDraft(message);
    if (!pendingSteer) return;
    if (message.request_id && message.request_id !== pendingSteer.request_id) return;
    if ((message.content || '').trim() !== (pendingSteer.content || '').trim()) return;
    pendingSteer = null;
    renderPendingSteer();
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

// The run that typed messages steer: this page's own, or one it restored.
function steerableRequestId() {
    return currentChatRequestId || (externalActiveSteerable ? externalActiveRequestId : '');
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

// A run this page did not start (another tab, voice, or a run that was
// already going when the page loaded or reloaded) still shows its progress.
// One refresh runs at a time. A request made meanwhile, such as the 2 s poll
// on a slow link or a terminal event, runs once after it, so a slow board
// still gets its reads applied.
async function refreshCurrentLiveActivity() {
    if (externalActiveRefreshInFlight) {
        externalActiveRefreshQueued = true;
        return;
    }
    externalActiveRefreshInFlight = true;
    try {
        do {
            externalActiveRefreshQueued = false;
            await refreshCurrentLiveActivityOnce();
        } while (externalActiveRefreshQueued);
    } finally {
        externalActiveRefreshInFlight = false;
    }
}

async function refreshCurrentLiveActivityOnce() {
    if (currentChatRequestId) return;
    const generation = externalActiveGeneration;
    const chatRun = await fetchActiveChatRun();
    let active = chatRun;
    if (!active) {
        // While the chat check answers idle, a running Live Activity is only a
        // run finishing its speech; one waiting on the phone still counts.
        const liveActivity = await fetchActiveLiveActivity(chatRun === undefined);
        // Keep the current state when neither check reached the board.
        if (chatRun === undefined && liveActivity === undefined) return;
        active = liveActivity || null;
    }
    // A steer, a stop, or a run this page started meanwhile supersedes this read.
    // A steer request in flight owns the pending steer card until it ends.
    if (currentChatRequestId || generation !== externalActiveGeneration ||
        pendingSteerSubmitting || pendingSteerCanceling) return;
    const requestId = active ? active.requestId : '';
    if (requestId !== externalActiveRequestId) {
        // Restart the timer from the run's own start, not from this page's.
        setActivityRunning(false);
        externalActiveStartedAt = active ? active.startedAt : 0;
    }
    externalActiveRequestId = requestId;
    externalActiveSteerable = !!(active && active.steerable);
    pendingSteer = externalActiveSteerable ? active.steer : null;
    setComposerState(!!activeChatRequestId());
}

// Each check returns the active run, null when there is none, or undefined
// when it could not be read, including a read that outlasts its deadline so a
// stalled request cannot hold up later refreshes.
const activeRunReadTimeoutMs = 5000;

function fetchActiveRunStatus(url) {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), activeRunReadTimeoutMs);
    return fetch(url, {cache: 'no-store', signal: controller.signal})
        .then(res => res.ok ? res.json() : undefined)
        .finally(() => clearTimeout(timeout));
}

async function fetchActiveChatRun() {
    try {
        const data = await fetchActiveRunStatus('/api/chat/active');
        if (data === undefined) return undefined;
        if (data.status !== 'running' || !data.request_id) return null;
        return {
            requestId: data.request_id,
            startedAt: Date.now() - Math.max(0, Number(data.elapsed_ms) || 0),
            steerable: data.steerable === true,
            steer: data.steer || null
        };
    } catch (err) {
        console.warn('Failed to refresh active chat run:', err);
        return undefined;
    }
}

// Live Activity also covers a finished run that waits for the user on the
// phone. It is reachable only over USB and when Live Activity is enabled.
// A running state counts only when includeRunning is set.
async function fetchActiveLiveActivity(includeRunning) {
    try {
        const data = await fetchActiveRunStatus('/api/live-activity/current');
        if (data === undefined) return undefined;
        const state = data.live_activity;
        const isActive = data.status === 'ok' && state &&
            (state.status === 'needs_app' || (includeRunning && state.status === 'running'));
        if (!isActive || !state.request_id) return null;
        const startedAt = Date.parse(state.started_at);
        return {requestId: state.request_id, startedAt: Number.isFinite(startedAt) ? Math.min(startedAt, Date.now()) : Date.now()};
    } catch (err) {
        console.warn('Failed to refresh current live activity:', err);
        return undefined;
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
    assistantTextReveal.flush();
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
        showErrorToast('Stop failed', err);
    }).finally(function() {
        if (externalActiveRequestId === requestId) {
            externalActiveRequestId = '';
            externalActiveSteerable = false;
            externalActiveGeneration++;
        }
        setComposerState(!!activeChatRequestId());
    });
}

async function cancelPendingSteer() {
    if (!steerableRequestId() || !pendingSteer) return;
    const requestId = steerableRequestId();
    externalActiveGeneration++;
    pendingSteerCanceling = true;
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
        const data = await res.json();
        if (data.status !== 'canceled') {
            showToast('The steer was already delivered.', {kind: 'info'});
        }
    } catch (err) {
        console.error('Failed to cancel steer:', err);
        showErrorToast('Cancel failed', err);
        if (steerableRequestId() === requestId) {
            pendingSteer = previous;
            renderPendingSteer();
        }
    } finally {
        // A status read started while the cancel was in flight may predate it.
        externalActiveGeneration++;
        pendingSteerCanceling = false;
        refreshRestoredRunAfterSteerRequest();
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
    rememberNDJSONOwnedRequest(event.request_id || (event.message && event.message.request_id));
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
        settlePendingSteer(event.message);
        if (event.message.type === 'tool_call') {
            retireAssistantDraft(event.message);
        }
        if (event.message.type === 'assistant') {
            finalizeAssistantMessage(event.message);
        } else {
            addMessage(event.message);
        }
        if (event.message.type === 'tool_result') {
            assistantDraftBlockedByTool.delete(assistantStreamKey(event.message));
        }
        return false;
    }
    if (event.type === 'done') {
        finalizeStreamDraft(event);
        return true;
    }
    if (event.type === 'error') {
        throw new Error(event.error || 'Agent error');
    }
    return false;
}

function rememberNDJSONOwnedRequest(requestId) {
    if (!requestId) return;
    ndjsonOwnedRequestIds.delete(requestId);
    ndjsonOwnedRequestIds.add(requestId);
    if (ndjsonOwnedRequestIds.size > 32) {
        ndjsonOwnedRequestIds.delete(ndjsonOwnedRequestIds.values().next().value);
    }
}

function appendAssistantDelta(event) {
    const key = assistantStreamKey(event);
    if (!key || assistantDraftBlockedByTool.has(key)) return;
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
    if (typeof msg.content !== 'string') msg.content = '';
    msg.content += event.delta || '';
    addMessage(msg);
}

function appendAssistantReasoningDelta(event) {
    const key = assistantStreamKey(event);
    if (!key || assistantDraftBlockedByTool.has(key)) return;
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
    if (typeof msg.reasoning_content !== 'string') msg.reasoning_content = '';
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

function retireAssistantDraft(event) {
    const key = assistantStreamKey(event);
    if (!key) return;
    const draft = streamingAssistantDrafts[key];
    const text = assistantMessageForToolCall(event);
    assistantDraftBlockedByTool.add(key);
    if (draft) {
        const draftKey = messageIdentity(draft);
        if (text) assistantTextReveal.rekey(draftKey, messageIdentity(text));
        delete streamingAssistantDrafts[key];
        removeRenderedMessage(draftKey);
    }
    if (text) addMessage(text);
}

function sealAssistantDraft(boundary) {
    const key = assistantStreamKey(boundary);
    const draft = key && streamingAssistantDrafts[key];
    if (!draft) return;
    const draftKey = messageIdentity(draft);
    const sealed = Object.assign({}, draft, {tool_text_key: messageIdentity(boundary)});
    assistantTextReveal.rekey(draftKey, messageIdentity(sealed));
    delete streamingAssistantDrafts[key];
    removeRenderedMessage(draftKey);
    addMessage(sealed);
}

function removeRenderedMessage(messageKey) {
    assistantTextReveal.remove(messageKey);
    projectedMessages = projectConversationMessages(projectedMessages, {type: 'remove', key: messageKey});
    removeRenderedMessageNode(messageKey);
}

function removeRenderedMessageNode(messageKey) {
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
    projectedMessages = projectConversationMessages(projectedMessages, {type: 'final', message: msg});
    const messageKey = messageIdentity(msg);
    if (renderedMessageKeys.has(messageKey)) {
        // A streamed draft may have been inserted before tool events.
        // Re-append the finalized assistant message so the live chat
        // order matches the persisted history and episode trace.
        removeRenderedMessageNode(messageKey);
    }
    renderLiveMessage(msg);
}

// A stream that ends without its own assistant message (realtime chat sends
// only deltas) still owns a live draft. Settle it with the run's response so
// the draft shows the whole reply and no longer holds back history refreshes.
function finalizeStreamDraft(event) {
    const key = assistantStreamKey(event);
    const draft = key && streamingAssistantDrafts[key];
    if (!draft) return;
    finalizeAssistantMessage(Object.assign({}, draft, {
        content: event.response || draft.content || ''
    }));
}

function assistantStreamKey(value) {
    value = value || {};
    return value.request_id || value.episode_id || currentChatRequestId || '';
}

function closeTopbarMenu() {
    const menu = document.getElementById('topbarMenu');
    if (menu) menu.removeAttribute('open');
}

async function clearSession() {
    closeTopbarMenu();
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
        showErrorToast('Clear failed', err);
    }
}

async function clearSessionAndMemory() {
    closeTopbarMenu();
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
        showErrorToast('Clear failed', err);
    }
}

function renderHistory(history) {
    projectedMessages = projectConversationMessages(projectedMessages, {type: 'history', messages: history});
    history = projectedMessages;
    const fingerprint = JSON.stringify(history);
    if (fingerprint === renderedHistoryFingerprint) return;

    // Explicit history replacement (including a clear) cannot leave callbacks
    // writing to removed nodes. Unchanged nodes must also show their full text.
    assistantTextReveal.flush();
    assistantTextReveal.clear();
    streamingAssistantDrafts = {};
    assistantDraftBlockedByTool.clear();
    reconcileConversation(history, false);
    renderedHistoryFingerprint = fingerprint;
}

// Reconcile rendered nodes with the projected messages, reusing unchanged
// nodes. Tool calls, results, and the state they produce render as groups.
function reconcileConversation(messages, live) {
    const shouldStickToBottom = messagesDiv.children.length === 0 || isConversationNearBottom();
    const previousScrollTop = conversationEl.scrollTop;
    const previousMessageNodes = renderedMessageNodes;
    const previousStateMessages = renderedStateMessages;
    const previousToolGroups = renderedToolGroups;
    const nextMessageKeys = new Set();
    const nextMessageNodes = new Map();
    const nextToolGroups = new Map();
    const usedToolGroups = new Set();
    renderedStateMessages = new Map();

    const orderedNodes = [];
    groupConversationItems(messages.filter(msg => !isControlMessage(msg))).forEach(function(item) {
        if (item.type === 'tools') {
            let node = previousToolGroups.get(item.key);
            if (!node || usedToolGroups.has(node)) {
                node = matchingToolGroupNode(previousToolGroups, usedToolGroups, item);
            }
            if (node) updateToolGroupNode(node, item);
            else node = createToolGroupNode(item);
            usedToolGroups.add(node);
            nextToolGroups.set(item.key, node);
            orderedNodes.push(node);
            return;
        }
        const msg = item.message;
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
            node = createTrackedMessageNode(msg, incomingFingerprint, live);
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
    previousToolGroups.forEach(function(node) {
        if (!usedToolGroups.has(node)) node.remove();
    });

    renderedMessageKeys = nextMessageKeys;
    renderedMessageNodes = nextMessageNodes;
    renderedToolGroups = nextToolGroups;
    if (!renderedStateMessages.has(activeStateMessageKey)) {
        activeStateMessageKey = '';
    }
    renderStateModal();

    orderedNodes.forEach(function(node, index) {
        if (messagesDiv.children[index] !== node) {
            messagesDiv.insertBefore(node, messagesDiv.children[index] || null);
        }
    });
    // After insertion: an expanded row replaced by a re-keyed one stays open.
    migrateExpandedLines();
    updateEmptyState();
    if (shouldStickToBottom) {
        scrollToBottom();
    } else {
        conversationEl.scrollTop = previousScrollTop;
    }
}

// Live tool messages carry run-scoped IDs that history later replaces with
// context IDs. Keep the same group node (and its expanded state) across that.
function matchingToolGroupNode(previousToolGroups, usedToolGroups, group) {
    const first = group.messages[0] || {};
    for (const node of previousToolGroups.values()) {
        if (usedToolGroups.has(node)) continue;
        const candidate = (node._toolGroup && node._toolGroup.messages[0]) || {};
        if (normalizeType(candidate.type) === normalizeType(first.type) &&
            (candidate.tool_name || '') === (first.tool_name || '') &&
            (candidate.tool_input || '') === (first.tool_input || '')) return node;
    }
    return null;
}

function addMessage(msg, beforeRequestId) {
    const toolText = assistantMessageForToolCall(msg);
    if (toolText) addMessage(toolText);
    const next = projectConversationMessages(projectedMessages, {
        type: 'live', message: msg, beforeRequestId: beforeRequestId
    });
    if (next === projectedMessages && !beforeRequestId) return;
    projectedMessages = next;
    renderLiveMessage(msg, beforeRequestId);
}

// addLiveUserTranscript shows what the user said in a voice turn. The reply
// to it may already be streaming, so the message goes above the live reply
// drafts rather than at the end.
function addLiveUserTranscript(msg) {
    const drafts = new Set(Object.values(streamingAssistantDrafts).map(messageIdentity));
    const index = projectedMessages.findIndex(item => drafts.has(messageIdentity(item)));
    if (index < 0) {
        addMessage(msg);
        return;
    }
    projectedMessages = projectedMessages.slice(0, index).concat(msg, projectedMessages.slice(index));
    renderedHistoryFingerprint = null;
    reconcileConversation(projectedMessages, true);
}

function renderLiveMessage(msg, beforeRequestId) {
    if (isContextMarkerMessage(msg) || isToolMessage(msg)) {
        // Markers and tool messages may join or split an activity group.
        renderedHistoryFingerprint = null;
        reconcileConversation(projectedMessages, true);
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
    const node = createTrackedMessageNode(msg, undefined, true);
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
    const replacement = createTrackedMessageNode(msg, incomingFingerprint, true);
    renderedMessageNodes.set(key, replacement);
    existing.replaceWith(replacement);
    updateEmptyState();
    if (shouldStickToBottom) scrollToBottom();
}

function createTrackedMessageNode(msg, fingerprint, live) {
    const node = createMessageNode(msg);
    node._messageFingerprint = fingerprint || messageRenderFingerprint(msg);
    const key = messageIdentity(msg);
    if (live && normalizeType(msg.type) === 'assistant' &&
        (streamingAssistantDrafts[assistantStreamKey(msg)] || assistantTextReveal.has(key))) {
        assistantTextReveal.bind(key, {
            content: node._copyTarget || node.querySelector('.message-copy'),
            reasoning: node.querySelector('.reasoning-text')
        }, {content: msg.content || '', reasoning: msg.reasoning_content || ''});
    }
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
    setContextMarkerPreview(button, msg, index);
    button.setAttribute('aria-expanded', node.classList.contains('open') ? 'true' : 'false');
}
