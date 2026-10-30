// Pure message reconciliation shared by history, NDJSON, and SSE updates.
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

function isControlMessage(msg) {
    return normalizeType((msg || {}).type) === 'todo_closed';
}

function isContextMarkerMessage(msg) {
    msg = msg || {};
    const type = normalizeType(msg.type);
    return type === 'state' || type === 'notice' || msg.role === 'state' || msg.role === 'notice';
}

function projectConversationMessages(current, action) {
    current = Array.isArray(current) ? current : [];
    if (action.type === 'history') {
        const seen = new Set();
        return (Array.isArray(action.messages) ? action.messages : []).filter(function(message) {
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
    if (index < 0) return current.concat(projectedMessage);
    if (normalizeType(message.type) !== 'assistant') return current;
    const next = current.slice();
    next[index] = projectedMessage;
    return next;
}
