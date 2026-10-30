// History loading and server-sent event synchronization.
//
// The page holds a window of the board history instead of the whole
// conversation: the newest page on load, new messages appended by
// incremental refreshes, and older pages fetched when the user scrolls up.
const historyPageLimit = 50;
const historyRefreshLimit = 200;
let historyWindow = null;
// Bumped on a clear. A request made before it may hold the cleared
// conversation and is dropped, even when the window was already empty.
let historyGeneration = 0;
let historyLoadInFlight = false;
let historyLoadPending = false;
let olderHistoryLoading = false;
let historyRefreshAfterReveal = false;

async function fetchHistoryPage(params) {
    const res = await fetch('/api/history?' + new URLSearchParams(params).toString());
    if (!res.ok) throw new Error(await res.text() || 'Failed to load context history');
    const page = await res.json();
    if (!page || !Array.isArray(page.messages)) throw new Error('History response is not a page');
    return page;
}

function historyWindowFromPage(page) {
    return {sessionId: page.session_id || '', start: page.start || 0, end: page.end || 0, messages: page.messages};
}

function resetHistoryWindow() {
    historyWindow = null;
    historyGeneration++;
    historyRefreshAfterReveal = false;
}

function chatRunActive() {
    return typeof currentChatRequestId !== 'undefined' && !!currentChatRequestId;
}

function deferHistoryForTextReveal() {
    if (!assistantTextReveal.hasPending()) return false;
    historyRefreshAfterReveal = true;
    return true;
}

function resumeHistoryAfterTextReveal() {
    if (!historyRefreshAfterReveal) return;
    historyRefreshAfterReveal = false;
    refreshHistoryFromContext();
}

async function loadHistory() {
    if (chatRunActive() || deferHistoryForTextReveal()) return;
    if (historyLoadInFlight) {
        historyLoadPending = true;
        return;
    }
    historyLoadInFlight = true;
    try {
        do {
            historyLoadPending = false;
            const current = historyWindow;
            const generation = historyGeneration;
            const page = await fetchHistoryPage(current
                ? {after: current.end, session_id: current.sessionId, limit: historyRefreshLimit}
                : {limit: historyPageLimit});
            // A run that started meanwhile renders its own live messages, and
            // this page would remove them; the refresh after the run catches up.
            if (chatRunActive() || deferHistoryForTextReveal()) return;
            // An older page or a clear replaced the window meanwhile; ask again
            // from the new window instead of appending to a stale one.
            if (generation !== historyGeneration || current !== historyWindow) {
                historyLoadPending = true;
                continue;
            }
            if (!current || page.reset || page.session_id !== current.sessionId) {
                historyWindow = historyWindowFromPage(page);
            } else if (page.messages.length > 0 || page.end !== current.end) {
                historyWindow = Object.assign({}, current, {
                    end: page.end,
                    messages: current.messages.concat(page.messages)
                });
            }
            renderHistory(historyWindow.messages);
            fillConversationWithOlderHistory();
        } while (historyLoadPending && !chatRunActive());
    } catch (err) {
        console.error('Failed to load history:', err);
    } finally {
        historyLoadInFlight = false;
        historyLoadPending = false;
    }
}

async function loadOlderHistory() {
    if (olderHistoryLoading || chatRunActive() || deferHistoryForTextReveal()) return;
    olderHistoryLoading = true;
    let loaded = false;
    try {
        for (;;) {
            const current = historyWindow;
            if (!current || current.start <= 0) return;
            const generation = historyGeneration;
            const page = await fetchHistoryPage({before: current.start, session_id: current.sessionId, limit: historyPageLimit});
            // Like a refresh, do not re-render over a run's live messages, and
            // drop a page fetched before a clear.
            if (chatRunActive() || generation !== historyGeneration || deferHistoryForTextReveal()) return;
            // A refresh replaced the window meanwhile; ask again from its start,
            // since a reader already at the top will not scroll there again.
            if (current !== historyWindow) continue;
            if (page.reset || page.session_id !== current.sessionId) {
                historyWindow = historyWindowFromPage(page);
                renderHistory(historyWindow.messages);
                return;
            }
            historyWindow = Object.assign({}, current, {
                start: page.start,
                messages: page.messages.concat(current.messages)
            });
            // Keep the messages the user is reading in place while older ones
            // are inserted above them.
            const previousHeight = conversationEl.scrollHeight;
            const previousTop = conversationEl.scrollTop;
            renderHistory(historyWindow.messages);
            conversationEl.scrollTop = previousTop + (conversationEl.scrollHeight - previousHeight);
            loaded = true;
            break;
        }
    } catch (err) {
        console.error('Failed to load earlier history:', err);
    } finally {
        olderHistoryLoading = false;
    }
    if (loaded) fillConversationWithOlderHistory();
}

// Older pages load when the user scrolls near the top. A window too short to
// scroll cannot get there, so keep loading until it fills the view or the
// start of the conversation is reached.
function fillConversationWithOlderHistory() {
    if (!historyWindow || historyWindow.start <= 0) return;
    // A hidden pane measures 0 and would pull in the whole history.
    if (conversationEl.clientHeight > 0 && conversationEl.scrollHeight <= conversationEl.clientHeight) loadOlderHistory();
}

function loadOlderHistoryNearTop() {
    if (conversationEl.scrollTop < 80) loadOlderHistory();
}

let historyRefreshTimer = null;
function refreshHistoryFromContext() {
    if (chatRunActive() || deferHistoryForTextReveal()) return;
    if (historyRefreshTimer) clearTimeout(historyRefreshTimer);
    historyRefreshTimer = setTimeout(function() {
        historyRefreshTimer = null;
        loadHistory();
    }, 80);
}

function connectSSE() {
    if (eventSource) {
        eventSource.close();
    }

    // The v2 endpoint keeps the legacy projection as a fallback. Older
    // daemons ignore the query parameter and still send the public message
    // envelope, which is handled below by handleServerEvent.
    eventSource = new EventSource('/api/events?version=2');

    eventSource.onopen = function() {
        console.log('[SSE] Connected');
    };

    eventSource.onmessage = function(e) {
        try {
            const data = JSON.parse(e.data);
            if (isSemanticEvent(data)) {
                handleSemanticEvent(data);
            } else {
                handleServerEvent(data);
            }
        } catch (err) {
            console.error('[SSE] Parse error:', err);
        }
    };

    eventSource.onerror = function(e) {
        console.error('[SSE] Connection error, will retry...');
    };
}

function isSemanticEvent(data) {
    return !!data && Number(data.schema_version) >= 2 &&
        (typeof data.kind === 'string' || data.type === 'resync_required');
}

function semanticField(value, ...names) {
    if (!value || typeof value !== 'object') return '';
    for (const name of names) {
        if (value[name] !== undefined && value[name] !== null) return value[name];
    }
    return '';
}

function semanticRequestID(event, payload) {
    return semanticField(event, 'turn_id', 'run_id', 'request_id') ||
        semanticField(payload, 'request_id', 'RequestID', 'run_id', 'RunID') ||
        semanticField(event, 'response_id', 'item_id', 'stream_id') ||
        semanticField(payload, 'response_id', 'ResponseID', 'item_id', 'ItemID', 'session_id', 'SessionID');
}

function semanticEpisodeID(event, payload) {
    return semanticField(payload, 'episode_id', 'EpisodeID') ||
        semanticField(event, 'turn_id', 'run_id', 'response_id', 'item_id') ||
        semanticField(payload, 'response_id', 'ResponseID', 'item_id', 'ItemID', 'session_id', 'SessionID');
}

function semanticTimestamp(event) {
    return semanticField(event, 'occurred_at', 'received_at') || new Date().toISOString();
}

function semanticText(payload) {
    return String(semanticField(payload, 'text', 'Text', 'content', 'Content', 'reasoning_content', 'ReasoningContent') || '');
}

// Convert the small subset of v2 events that can be rendered immediately to
// the existing live-message helpers. Other semantic events still trigger a
// history refresh, preserving the legacy public projection and ordering.
function handleSemanticEvent(event) {
    if (!event || event.type === 'connected') return;
    if (event.type === 'resync_required') {
        loadHistory();
        return;
    }

    const payload = event.payload || {};
    const requestId = semanticRequestID(event, payload);
    const episodeId = semanticEpisodeID(event, payload);
    const timestamp = semanticTimestamp(event);
    switch (event.kind) {
        case 'text.delta':
            if (currentChatRequestId === requestId || ndjsonOwnedRequestIds.has(requestId)) return;
            appendAssistantDelta({
                request_id: requestId,
                episode_id: episodeId,
                delta: semanticText(payload)
            });
            return;
        case 'reasoning.delta':
            if (currentChatRequestId === requestId || ndjsonOwnedRequestIds.has(requestId)) return;
            appendAssistantReasoningDelta({
                request_id: requestId,
                episode_id: episodeId,
                reasoning_content: semanticText(payload)
            });
            return;
        case 'reasoning.reset':
            if (currentChatRequestId === requestId || ndjsonOwnedRequestIds.has(requestId)) return;
            resetAssistantReasoning({request_id: requestId, episode_id: episodeId});
            return;
        case 'text.final':
            if (currentChatRequestId === requestId || ndjsonOwnedRequestIds.has(requestId)) return;
            // Role output describes an intermediate model response, often just
            // before a tool call. Only the final assistant output is durable.
            if (semanticField(payload, 'type', 'Type') === 'role_output' ||
                assistantDraftBlockedByTool.has(requestId)) return;
            finalizeAssistantMessage({
                type: 'assistant',
                request_id: requestId,
                episode_id: episodeId,
                content: semanticText(payload),
                reasoning_content: String(semanticField(payload, 'reasoning_content', 'ReasoningContent') || ''),
                timestamp
            });
            return;
        case 'state.updated':
            // State is persisted before the model starts, but history refreshes
            // are paused during a run to protect live assistant drafts. Render
            // the semantic state message directly so its divider appears as
            // soon as the context hook inserts it.
            if (payload && typeof payload === 'object') addMessage(payload, requestId);
            return;
        case 'tool.call_ready':
            if (currentChatRequestId === requestId || ndjsonOwnedRequestIds.has(requestId)) return;
            retireAssistantDraft({request_id: requestId});
            return;
        case 'tool.completed':
        case 'tool.failed':
            if (currentChatRequestId === requestId || ndjsonOwnedRequestIds.has(requestId)) return;
            assistantDraftBlockedByTool.delete(requestId);
            refreshHistoryFromContext();
            return;
        default:
            if (semanticEventNeedsHistoryRefresh(event.kind)) refreshHistoryFromContext();
    }
}

function semanticEventNeedsHistoryRefresh(kind) {
    return kind === 'text.final' ||
        kind === 'transcript.segment_final' ||
        kind === 'turn.committed' ||
        kind === 'response.terminal' ||
        kind === 'run.terminal' ||
        kind === 'tool.call_ready' ||
        kind === 'tool.completed' ||
        kind === 'tool.failed' ||
        kind === 'task.updated' ||
        kind === 'stream.error' ||
        kind === 'session.closed';
}

function handleServerEvent(data) {
    if (!data || data.type === 'connected') return;
    // Context markers are durable messages, but they are also produced before
    // the model starts generating a response. Render them from the live event
    // immediately so the state divider does not wait for the final history
    // refresh at run completion.
    if (data.type === 'state' || data.type === 'notice') {
        addMessage(data, data.type === 'state' ? currentChatRequestId : '');
        return;
    }
    if (data.type === 'assistant' && data.status === 'streaming') {
        const key = assistantStreamKey(data);
        let draft = streamingAssistantDrafts[key];
        if (!draft) {
            draft = Object.assign({}, data, {content: data.content || '', reasoning_content: data.reasoning_content || ''});
            streamingAssistantDrafts[key] = draft;
        } else {
            if (data.content) draft.content = data.content;
            if (data.reasoning_content) draft.reasoning_content = data.reasoning_content;
            draft.source = data.source || draft.source;
            draft.timestamp = data.timestamp || draft.timestamp;
        }
        addMessage(draft);
        return;
    }
    if (data.type) refreshHistoryFromContext();
}

document.addEventListener('visibilitychange', function() {
    // Animation frames pause in background tabs; finish the received text so
    // returning to the page never waits for an old presentation backlog.
    if (document.hidden) assistantTextReveal.flush();
    if (!document.hidden) loadHistory();
    if (!document.hidden && (!eventSource || eventSource.readyState === EventSource.CLOSED)) {
        connectSSE();
    }
});
