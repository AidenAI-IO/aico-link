// Pure message reconciliation shared by history, NDJSON, and SSE updates.
function messageIdentity(msg) {
    msg = msg || {};
    const type = isContextMarkerMessage(msg) ? contextMarkerType(msg) : normalizeType(msg.type);
    if (type === 'assistant' && msg.tool_text_key) return ['tool-text', msg.tool_text_key].join('\u001f');
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

function isControlMessage(msg) {
    return normalizeType((msg || {}).type) === 'todo_closed';
}

// A tool response is one context item containing both assistant text and the
// call. Present the text as an ordinary assistant message without losing its
// association with that specific call when the same run continues generating.
function assistantMessageForToolCall(msg) {
    if (!msg || normalizeType(msg.type) !== 'tool_call' ||
        (!msg.content && !msg.reasoning_content)) return null;
    return {
        type: 'assistant',
        request_id: msg.request_id || '',
        episode_id: msg.episode_id || '',
        tool_text_key: messageIdentity(msg),
        content: msg.content || '',
        reasoning_content: msg.reasoning_content || '',
        source: msg.source,
        timestamp: msg.timestamp
    };
}

function isContextMarkerMessage(msg) {
    msg = msg || {};
    const type = normalizeType(msg.type);
    return type === 'state' || type === 'notice' || msg.role === 'state' || msg.role === 'notice';
}

function isToolMessage(msg) {
    const type = normalizeType((msg || {}).type);
    return type === 'tool_call' || type === 'tool_result';
}

// State markers directly before a user turn are that turn's context, not
// output of the preceding tool calls.
function startsUserTurnContext(messages, index) {
    for (let i = index; i < messages.length; i++) {
        const msg = messages[i];
        if (!isContextMarkerMessage(msg)) {
            const type = normalizeType(msg.type);
            return type === 'user' || type === 'steer';
        }
    }
    return false;
}

// Consecutive tool calls, their results, and the state they produce render
// as one collapsible activity group. Notices stay visible as dividers.
function groupConversationItems(messages) {
    const items = [];
    let group = null;
    messages.forEach(function(msg, index) {
        if (isToolMessage(msg) ||
            (group && isContextMarkerMessage(msg) && contextMarkerType(msg) === 'state' &&
                !startsUserTurnContext(messages, index))) {
            if (!group) {
                group = {type: 'tools', key: messageIdentity(msg), messages: []};
                items.push(group);
            }
            group.messages.push(msg);
            return;
        }
        group = null;
        items.push({type: 'message', message: msg});
    });
    return items;
}

// Pair each result with the earliest open call of the same tool so one entry
// shows the call and its result together. Markers keep their own entries.
function toolGroupEntries(messages) {
    const entries = [];
    messages.forEach(function(msg) {
        const type = normalizeType(msg.type);
        if (type === 'tool_result') {
            const name = msg.tool_name || '';
            const call = entries.find(function(entry) {
                return entry.call && !entry.result && (!name || (entry.call.tool_name || '') === name);
            });
            if (call) {
                call.result = msg;
                return;
            }
            entries.push({kind: 'tool', key: messageIdentity(msg), call: null, result: msg});
            return;
        }
        if (type === 'tool_call') {
            entries.push({kind: 'tool', key: messageIdentity(msg), call: msg, result: null});
            return;
        }
        entries.push({kind: 'marker', key: messageIdentity(msg), message: msg});
    });
    return entries;
}

// A live turn-start marker belongs before its user message only until the
// turn produces output; later markers come from that output.
function contextMarkerAnchorIndex(messages, beforeRequestId) {
    if (!beforeRequestId) return -1;
    const index = messages.findIndex(item => normalizeType(item.type) === 'user' && item.request_id === beforeRequestId);
    if (index < 0) return -1;
    for (let i = index + 1; i < messages.length; i++) {
        if (!isContextMarkerMessage(messages[i])) return -1;
    }
    return index;
}

function projectConversationMessages(current, action) {
    current = Array.isArray(current) ? current : [];
    if (action.type === 'history') {
        const seen = new Set();
        return (Array.isArray(action.messages) ? action.messages : []).flatMap(function(message) {
            const text = assistantMessageForToolCall(message);
            return text ? [text, message] : [message];
        }).filter(function(message) {
            if (isControlMessage(message)) return false;
            const key = messageIdentity(message);
            if (seen.has(key)) return false;
            seen.add(key);
            return true;
        });
    }

    if (action.type === 'remove') {
        const remaining = current.filter(message => messageIdentity(message) !== action.key);
        return remaining.length === current.length ? current : remaining;
    }

    const message = action.message;
    if (!message || isControlMessage(message)) return current;
    const key = messageIdentity(message);
    const index = current.findIndex(item => messageIdentity(item) === key);
    if (action.type === 'final') {
        const withoutDraft = index < 0 ? current : current.filter(item => messageIdentity(item) !== key);
        return withoutDraft.concat(message);
    }
    if (action.type !== 'live') return current;
    const projectedMessage = normalizeType(message.type) === 'assistant' ? Object.assign({}, message) : message;
    const anchorIndex = isContextMarkerMessage(message)
        ? contextMarkerAnchorIndex(current, action.beforeRequestId) : -1;
    if (index < 0) {
        if (anchorIndex < 0) return current.concat(projectedMessage);
        const next = current.slice();
        next.splice(anchorIndex, 0, projectedMessage);
        return next;
    }
    if (anchorIndex >= 0 && index > anchorIndex) {
        const next = current.slice();
        next.splice(index, 1);
        next.splice(anchorIndex, 0, message);
        return next;
    }
    if (normalizeType(message.type) !== 'assistant') return current;
    const next = current.slice();
    next[index] = projectedMessage;
    return next;
}
