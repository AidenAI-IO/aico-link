const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

class FakeElement {
    constructor() {
        this.childNodes = [];
        this.parentNode = null;
        this.attributes = new Map();
        this.classes = new Set();
        this.classList = {contains: className => this.classes.has(className)};
    }

    get children() {
        return this.childNodes;
    }

    set innerHTML(value) {
        while (this.childNodes.length) this.removeChild(this.childNodes[0]);
    }

    set className(value) {
        this.classes = new Set(String(value || '').split(/\s+/).filter(Boolean));
    }

    appendChild(node) {
        return this.insertBefore(node, null);
    }

    insertBefore(node, reference) {
        if (node._isFragment) {
            node.childNodes.slice().forEach(child => this.insertBefore(child, reference));
            return node;
        }
        if (node.parentNode) node.parentNode.removeChild(node);
        const index = reference ? this.childNodes.indexOf(reference) : -1;
        node.parentNode = this;
        if (index < 0) this.childNodes.push(node);
        else this.childNodes.splice(index, 0, node);
        return node;
    }

    removeChild(node) {
        const index = this.childNodes.indexOf(node);
        if (index >= 0) this.childNodes.splice(index, 1);
        node.parentNode = null;
        return node;
    }

    remove() {
        if (this.parentNode) this.parentNode.removeChild(this);
    }

    replaceWith(node) {
        if (!this.parentNode) return;
        const parent = this.parentNode;
        const nextSibling = parent.childNodes[parent.childNodes.indexOf(this) + 1] || null;
        this.remove();
        parent.insertBefore(node, nextSibling);
    }

    querySelector(selector) {
        const className = selector.startsWith('.') ? selector.slice(1) : '';
        for (const child of this.childNodes) {
            if (child.classes.has(className)) return child;
            const nested = child.querySelector(selector);
            if (nested) return nested;
        }
        return null;
    }

    setAttribute(name, value) {
        this.attributes.set(name, String(value));
    }
}

class FakeDocument {
    constructor() {
        this.elements = new Map();
    }

    createElement() {
        return new FakeElement();
    }

    createDocumentFragment() {
        const fragment = new FakeElement();
        fragment._isFragment = true;
        return fragment;
    }

    getElementById(id) {
        if (!this.elements.has(id)) this.elements.set(id, new FakeElement());
        return this.elements.get(id);
    }

    addEventListener() {}
}

const document = new FakeDocument();
global.document = document;
global.window = global;

function createMessageNode(msg) {
    const node = document.createElement('article');
    node._message = msg;
    return node;
}

function createContextMarkerNode(msg, index) {
    const key = messageIdentity(msg);
    const node = document.createElement('article');
    node._stateMessage = msg;
    const button = document.createElement('button');
    button.className = 'state-divider-button';
    button.title = contextMarkerSummary(msg, index);
    node.appendChild(button);
    renderedStateMessages.set(key, node);
    return node;
}

function contextMarkerSummary(msg, index) {
    return 'State ' + (index + 1) + ': ' + (msg.content || '');
}

Object.assign(global, {
    assert,
    createMessageNode,
    createContextMarkerNode,
    contextMarkerSummary,
    contextMarkerType: () => 'state',
    formatTime: timestamp => timestamp ? String(timestamp) : '',
    normalizeType: type => type || 'assistant',
    renderStateModal: () => {},
    scrollToBottom: () => {},
    updateEmptyState: () => {}
});

const sourceDir = path.join(__dirname, '..', 'web_ui');
const source = [
    fs.readFileSync(path.join(sourceDir, 'scripts', 'conversation_projection.js'), 'utf8'),
    fs.readFileSync(path.join(sourceDir, 'scripts', 'state.js'), 'utf8'),
    fs.readFileSync(path.join(sourceDir, 'scripts', 'chat.js'), 'utf8'),
    fs.readFileSync(path.join(sourceDir, 'scripts', 'events.js'), 'utf8'),
    `
const historyA = [
    {type: 'user', request_id: 'user-1', content: 'hello', timestamp: 't1'},
    {type: 'state', request_id: 'state-1', content: 'device: phone', timestamp: 't2'},
    {type: 'state', request_id: 'state-1', content: 'duplicate marker', timestamp: 't2-duplicate'},
    {type: 'assistant', request_id: 'assistant-1', content: 'first answer', timestamp: 't3'}
];
renderHistory(historyA);
const firstNodes = Array.from(messagesDiv.children);
assert.equal(firstNodes.length, 3, 'duplicate context marker should render once');
assert.strictEqual(firstNodes[1]._stateMessage, historyA[1]);

const historyB = [
    historyA[0],
    historyA[1],
    Object.assign({}, historyA[3], {content: 'updated answer'})
];
renderHistory(historyB);
const secondNodes = Array.from(messagesDiv.children);
assert.equal(secondNodes.length, 3);
assert.strictEqual(secondNodes[0], firstNodes[0], 'unchanged user node should be reused');
assert.strictEqual(secondNodes[1], firstNodes[1], 'unchanged state marker should be reused');
assert.notStrictEqual(secondNodes[2], firstNodes[2], 'changed assistant node should be replaced');
assert.equal(secondNodes[2]._message.content, 'updated answer');

renderHistory(historyB);
const thirdNodes = Array.from(messagesDiv.children);
assert.strictEqual(thirdNodes[0], secondNodes[0]);
assert.strictEqual(thirdNodes[1], secondNodes[1]);
assert.strictEqual(thirdNodes[2], secondNodes[2]);

handleServerEvent({
    type: 'assistant', status: 'streaming', source: 'voice',
    request_id: 'voice-1', reasoning_content: 'first thought'
});
const firstVoiceReasoningNode = Array.from(messagesDiv.children)[3];
const firstVoiceProjection = projectedMessages;
assert.equal(firstVoiceReasoningNode._message.reasoning_content, 'first thought');

handleServerEvent({
    type: 'assistant', status: 'streaming', source: 'voice',
    request_id: 'voice-1', reasoning_content: 'first thought, then another'
});
const updatedVoiceReasoningNode = Array.from(messagesDiv.children)[3];
assert.notStrictEqual(updatedVoiceReasoningNode, firstVoiceReasoningNode);
assert.equal(updatedVoiceReasoningNode._message.reasoning_content, 'first thought, then another');
assert.equal(firstVoiceProjection[3].reasoning_content, 'first thought',
    'a later SSE delta should not change an earlier projection');
assert.equal(messagesDiv.children.length, 4, 'reasoning deltas should update one draft');

handleSemanticEvent({
    schema_version: 2,
    kind: 'text.delta',
    turn_id: 'semantic-1',
    payload: {text: 'hello'}
});
const firstSemanticProjection = projectedMessages;
handleSemanticEvent({
    schema_version: 2,
    kind: 'text.delta',
    turn_id: 'semantic-1',
    payload: {text: ' world'}
});
const semanticDraft = Array.from(messagesDiv.children)[4];
assert.equal(semanticDraft._message.content, 'hello world', 'semantic text deltas should accumulate');
assert.equal(firstSemanticProjection[4].content, 'hello',
    'a later NDJSON delta should not change an earlier projection');

handleSemanticEvent({
    schema_version: 2,
    kind: 'reasoning.delta',
    turn_id: 'semantic-1',
    payload: {reasoning_content: 'first'}
});
handleSemanticEvent({
    schema_version: 2,
    kind: 'reasoning.delta',
    turn_id: 'semantic-1',
    payload: {reasoning_content: ' thought'}
});
assert.equal(Array.from(messagesDiv.children)[4]._message.reasoning_content, 'first thought');

handleSemanticEvent({
    schema_version: 2,
    kind: 'text.final',
    turn_id: 'semantic-1',
    payload: {content: 'hello world', reasoning_content: 'first thought'}
});
assert.equal(Array.from(messagesDiv.children)[4]._message.content, 'hello world');
assert.equal(streamingAssistantDrafts['semantic-1'], undefined, 'semantic final should clear the draft');

handleSemanticEvent({
    schema_version: 2,
    kind: 'text.delta',
    response_id: 'response-a',
    payload: {Text: 'a'}
});
handleSemanticEvent({
    schema_version: 2,
    kind: 'text.delta',
    response_id: 'response-b',
    payload: {Text: 'b'}
});
assert.equal(streamingAssistantDrafts['response-a'].content, 'a');
assert.equal(streamingAssistantDrafts['response-b'].content, 'b');

handleServerEvent({
    type: 'state', request_id: 'state-live-1', content: 'app: Browser', timestamp: 't-live'
});
const liveStateNode = Array.from(messagesDiv.children).find(node => node._stateMessage && node._stateMessage.request_id === 'state-live-1');
assert.ok(liveStateNode, 'live state should render before history refresh');
assert.equal(liveStateNode._stateMessage.content, 'app: Browser');

handleSemanticEvent({
    schema_version: 2,
    kind: 'state.updated',
    turn_id: 'state-live-semantic',
    payload: {type: 'state', request_id: 'state-live-semantic', content: 'app: Settings', timestamp: 't-live-2'}
});
const semanticStateNode = Array.from(messagesDiv.children).find(node => node._stateMessage && node._stateMessage.request_id === 'state-live-semantic');
assert.ok(semanticStateNode, 'semantic state should render before history refresh');
assert.equal(semanticStateNode._stateMessage.content, 'app: Settings');

const persistedState = {
    type: 'state', request_id: 'state-live-semantic', content: 'app: Settings', timestamp: 't-live-2'
};
renderHistory(historyB.concat(persistedState));
assert.strictEqual(renderedStateMessages.get(messageIdentity(persistedState)), semanticStateNode,
    'a state event with a context ID should reuse its node when history catches up');

const pureHistory = [
    {type: 'user', request_id: 'pure-request', content: 'hello'},
    {type: 'state', request_id: 'context-backend-99', content: 'device: ready'}
];
let pureMessages = projectConversationMessages([], {type: 'history', messages: pureHistory.concat(pureHistory[1])});
assert.equal(pureMessages.length, 2, 'projection should deduplicate persisted context markers');
assert.strictEqual(projectConversationMessages(pureMessages, {type: 'live', message: pureHistory[1]}), pureMessages,
    'SSE and history state with the same context ID should have one identity');

const pureDraft = {type: 'assistant', request_id: 'pure-request', content: 'partial'};
pureMessages = projectConversationMessages(pureMessages, {type: 'live', message: pureDraft});
pureMessages = projectConversationMessages(pureMessages, {
    type: 'live', message: {type: 'tool_call', request_id: 'pure-request', tool_name: 'shell', content: 'run'}
});
const updatedProjection = projectConversationMessages(pureMessages, {
    type: 'live', message: Object.assign({}, pureDraft, {content: 'more text'})
});
assert.equal(updatedProjection.length, pureMessages.length, 'SSE and NDJSON updates should replace one draft');
assert.equal(updatedProjection[2].content, 'more text');
assert.equal(pureMessages[2].content, 'partial', 'the reducer should not mutate prior state');

pureMessages = projectConversationMessages(updatedProjection, {
    type: 'final', message: Object.assign({}, pureDraft, {content: 'final'})
});
assert.equal(pureMessages[3].content, 'final', 'final output should follow intervening tool messages');
pureMessages = projectConversationMessages(pureMessages, {type: 'remove', key: messageIdentity(pureMessages[3])});
assert.equal(pureMessages.length, 3, 'a reset should remove the matching live draft');
pureMessages = projectConversationMessages(pureMessages, {type: 'history', messages: pureHistory});
assert.equal(pureMessages.length, 2, 'durable history should replace transient live messages');

function renderedTypes() {
    return Array.from(messagesDiv.children).map(function(node) {
        return node._stateMessage ? 'state' : node._message.type;
    });
}

renderHistory([]);
const runId = 'ordering-run';
const liveUser = {type: 'user', request_id: runId, content: 'set volume to 0', timestamp: 't-order-user'};
const liveState = {type: 'state', request_id: 'context-backend-40', content: 'device: ready', timestamp: 't-order-state'};
addMessage(liveUser);
handleSemanticEvent({schema_version: 2, kind: 'state.updated', turn_id: runId, payload: liveState});
const orderedStateNode = renderedStateMessages.get(messageIdentity(liveState));
assert.deepEqual(renderedTypes(), ['state', 'user'],
    'live state should occupy its durable position before the current user turn');

handleChatStreamEvent({type: 'assistant_delta', request_id: runId, delta: 'Setting playback volume now.'});
handleSemanticEvent({schema_version: 2, kind: 'text.final', turn_id: runId,
    payload: {type: 'role_output', content: 'Setting playback volume now.'}});
assert.equal(streamingAssistantDrafts[runId].content, 'Setting playback volume now.',
    'intermediate role output must not finalize the assistant draft');
handleSemanticEvent({schema_version: 2, kind: 'tool.call_ready', turn_id: runId});
assert.deepEqual(renderedTypes(), ['state', 'user'],
    'semantic tool boundary should retire the draft before its NDJSON tool call arrives');
handleChatStreamEvent({type: 'message', message: {
    type: 'tool_call', request_id: runId, tool_name: 'audio_volume', tool_input: '{"volume":0}',
    timestamp: 't-order-call'
}});
assert.deepEqual(renderedTypes(), ['state', 'user', 'tool_call'],
    'tool-call boundary should retire the provisional assistant draft');

handleSemanticEvent({schema_version: 2, kind: 'text.delta', turn_id: runId,
    payload: {text: 'late tool-call text'}});
assert.deepEqual(renderedTypes(), ['state', 'user', 'tool_call'],
    'a late delta from the tool-call response should not recreate its draft');

handleChatStreamEvent({type: 'message', message: {
    type: 'tool_result', request_id: runId, tool_name: 'audio_volume', content: '{"volume":0}',
    timestamp: 't-order-result'
}});
handleChatStreamEvent({type: 'assistant_delta', request_id: runId, delta: 'Volume is now 0.'});
handleChatStreamEvent({type: 'message', message: {
    type: 'assistant', request_id: runId, content: 'Volume is now 0.', timestamp: 't-order-final'
}});
const stableOrder = ['state', 'user', 'tool_call', 'tool_result', 'assistant'];
assert.deepEqual(renderedTypes(), stableOrder,
    'live final response should follow its tool result without moving earlier messages');

renderHistory([
    liveState,
    Object.assign({}, liveUser, {request_id: 'context-backend-41'}),
    {type: 'tool_call', request_id: 'context-backend-42', tool_name: 'audio_volume',
        tool_input: '{"volume":0}', timestamp: 't-order-call'},
    {type: 'tool_result', request_id: 'context-backend-43', tool_name: 'audio_volume',
        content: '{"volume":0}', timestamp: 't-order-result'},
    {type: 'assistant', request_id: 'context-backend-44', content: 'Volume is now 0.',
        timestamp: 't-order-final'}
]);
assert.deepEqual(renderedTypes(), stableOrder,
    'history reconciliation should preserve the live message order');
assert.strictEqual(messagesDiv.children[0], orderedStateNode,
    'the live state divider should be reused at its durable position');

renderHistory([]);
const laterRunId = 'later-ordering-run';
addMessage({type: 'user', request_id: laterRunId, content: 'another tool', timestamp: 't-later-user'});
handleChatStreamEvent({type: 'assistant_delta', request_id: laterRunId, delta: 'Old draft'});
handleChatStreamEvent({type: 'message', message: {
    type: 'tool_call', request_id: laterRunId, tool_name: 'audio_volume', timestamp: 't-later-call'
}});
handleChatStreamEvent({type: 'message', message: {
    type: 'tool_result', request_id: laterRunId, tool_name: 'audio_volume', timestamp: 't-later-result'
}});
handleChatStreamEvent({type: 'assistant_delta', request_id: laterRunId, delta: 'New draft'});
handleSemanticEvent({schema_version: 2, kind: 'tool.call_ready', turn_id: laterRunId});
assert.equal(streamingAssistantDrafts[laterRunId].content, 'New draft',
    'a delayed semantic tool event must not retire the next model response');
assert.deepEqual(renderedTypes(), ['user', 'tool_call', 'tool_result', 'assistant']);

currentChatRequestId = laterRunId;
handleSemanticEvent({schema_version: 2, kind: 'text.delta', turn_id: laterRunId,
    payload: {text: 'duplicate SSE delta'}});
handleSemanticEvent({schema_version: 2, kind: 'text.final', turn_id: laterRunId,
    payload: {type: 'assistant_output', content: 'duplicate SSE final'}});
assert.equal(streamingAssistantDrafts[laterRunId].content, 'New draft',
    'the local NDJSON stream owns assistant text during its run');
currentChatRequestId = '';
handleSemanticEvent({schema_version: 2, kind: 'text.delta', turn_id: laterRunId,
    payload: {text: 'late SSE delta'}});
handleSemanticEvent({schema_version: 2, kind: 'text.final', turn_id: laterRunId,
    payload: {type: 'assistant_output', content: 'late SSE final'}});
assert.equal(streamingAssistantDrafts[laterRunId].content, 'New draft',
    'late SSE text should not change a local NDJSON run after its request ID clears');

renderHistory([]);
const externalRunId = 'external-ordering-run';
handleSemanticEvent({schema_version: 2, kind: 'text.delta', turn_id: externalRunId,
    payload: {text: 'Before tool'}});
handleSemanticEvent({schema_version: 2, kind: 'tool.call_ready', turn_id: externalRunId});
assert.deepEqual(renderedTypes(), [], 'SSE-only tool boundary should retire its draft');
handleSemanticEvent({schema_version: 2, kind: 'tool.completed', turn_id: externalRunId});
handleSemanticEvent({schema_version: 2, kind: 'text.delta', turn_id: externalRunId,
    payload: {text: 'After tool'}});
assert.equal(streamingAssistantDrafts[externalRunId].content, 'After tool',
    'SSE-only runs should resume streaming after tool completion');
clearTimeout(historyRefreshTimer);
`
].join('\n');

vm.runInThisContext(source, {filename: path.join(sourceDir, 'scripts', 'chat.js')});
