// History loading and server-sent event synchronization.
//
// The page holds a window of the board history instead of the whole
// conversation: the newest page on load, new messages appended by
// incremental refreshes, and older pages fetched when the user scrolls up.
const historyPageLimit = 50;
const historyRefreshLimit = 200;
let historyWindow = null;
let historyLoadInFlight = false;
let historyLoadPending = false;
let olderHistoryLoading = false;

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
}

async function loadHistory() {
    if (typeof currentChatRequestId !== 'undefined' && currentChatRequestId) return;
    if (historyLoadInFlight) {
        historyLoadPending = true;
        return;
    }
    historyLoadInFlight = true;
    try {
        do {
            historyLoadPending = false;
            const current = historyWindow;
            const page = await fetchHistoryPage(current
                ? {after: current.end, session_id: current.sessionId, limit: historyRefreshLimit}
                : {limit: historyPageLimit});
            // An older page or a clear replaced the window meanwhile; ask again
            // from the new window instead of appending to a stale one.
            if (current !== historyWindow) {
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
        } while (historyLoadPending && !(typeof currentChatRequestId !== 'undefined' && currentChatRequestId));
    } catch (err) {
        console.error('Failed to load history:', err);
    } finally {
        historyLoadInFlight = false;
        historyLoadPending = false;
    }
}

async function loadOlderHistory() {
    const current = historyWindow;
    if (!current || current.start <= 0 || olderHistoryLoading) return;
    olderHistoryLoading = true;
    let loaded = false;
    try {
        const page = await fetchHistoryPage({before: current.start, session_id: current.sessionId, limit: historyPageLimit});
        if (current !== historyWindow) return;
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
    if (typeof currentChatRequestId !== 'undefined' && currentChatRequestId) return;
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

    eventSource = new EventSource('/api/events');

    eventSource.onopen = function() {
        console.log('[SSE] Connected');
    };

    eventSource.onmessage = function(e) {
        try {
            handleServerEvent(JSON.parse(e.data));
        } catch (err) {
            console.error('[SSE] Parse error:', err);
        }
    };

    eventSource.onerror = function(e) {
        console.error('[SSE] Connection error, will retry...');
    };
}

function handleServerEvent(data) {
    if (!data || data.type === 'connected') return;
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
    if (!document.hidden) loadHistory();
    if (!document.hidden && (!eventSource || eventSource.readyState === EventSource.CLOSED)) {
        connectSSE();
    }
});
