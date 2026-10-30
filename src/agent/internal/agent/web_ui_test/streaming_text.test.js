const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web_ui', 'scripts', 'streaming_text.js'), 'utf8');

function createHarness(settings) {
    settings = settings || {};
    const context = {Intl: settings.fallback ? {} : Intl};
    vm.runInNewContext(source, context);
    let timestamp = 0;
    let nextId = 0;
    let idleCount = 0;
    const frames = new Map();
    const batches = [];
    const reveal = context.createStreamingTextReveal({
        requestFrame(callback) { frames.set(++nextId, callback); return nextId; },
        cancelFrame(id) { frames.delete(id); },
        now: () => timestamp,
        immediate: () => !!settings.immediate,
        beforeFrame() { batches.push('before'); return 'scroll-position'; },
        afterFrame(value) { assert.equal(value, 'scroll-position'); batches.push('after'); },
        onIdle() {
            assert.equal(frames.size, 0, 'idle must be notified after scheduled playback ends');
            idleCount++;
        }
    });
    return {
        reveal, frames, batches,
        idleCount: () => idleCount,
        advance(ms) {
            timestamp += ms;
            const callbacks = Array.from(frames.values());
            frames.clear();
            callbacks.forEach(callback => callback(timestamp));
        },
        drain() {
            for (let i = 0; reveal.hasPending() && i < 1000; i++) this.advance(16);
            assert.equal(reveal.hasPending(), false, 'playback should finish in bounded time');
        }
    };
}

const node = () => ({textContent: ''});

{
    const h = createHarness();
    const content = node();
    h.reveal.bind('run', {content}, {content: 'Hello world'});
    assert.equal(content.textContent, '', 'a received chunk should not appear all at once');
    assert.equal(h.reveal.has('run'), true);
    assert.equal(h.frames.size, 1);
    h.advance(20);
    assert.equal(content.textContent, 'H');
    h.reveal.bind('run', {content}, {content: 'Hello world!'});
    assert.equal(content.textContent, 'H', 'new chunks must retain visible progress');
    assert.equal(h.frames.size, 1, 'multiple binds share one animation frame');
    h.drain();
    assert.equal(content.textContent, 'Hello world!');
    assert.equal(h.idleCount(), 1);
    for (let i = 0; i < h.batches.length; i += 2) {
        assert.deepEqual(h.batches.slice(i, i + 2), ['before', 'after']);
    }

    const replacement = {textContent: 'Hello world! More'};
    h.reveal.bind('run', {content: replacement}, {content: 'Hello world! More'});
    assert.equal(replacement.textContent, 'Hello world!', 'a replacement node inherits the revealed prefix');
    h.reveal.flush();
    assert.equal(replacement.textContent, 'Hello world! More');
    assert.equal(h.idleCount(), 2);
    h.reveal.flush();
    assert.equal(h.idleCount(), 2, 'already idle flush must not trigger onIdle again');

    h.reveal.bind('run', {content: replacement}, {content: 'Hello there'});
    assert.equal(replacement.textContent, 'Hello ', 'corrected text retains only the common visible prefix');
    h.drain();
    assert.equal(replacement.textContent, 'Hello there');
}

{
    const h = createHarness();
    const content = node();
    const expected = ['', 'A', 'A👨‍👩‍👧‍👦', 'A👨‍👩‍👧‍👦e\u0301', 'A👨‍👩‍👧‍👦e\u0301中'];
    h.reveal.bind('unicode', {content}, {content: expected.at(-1)});
    while (h.reveal.hasPending()) {
        h.advance(16);
        assert.ok(expected.includes(content.textContent), 'each frame must end at a complete grapheme');
    }
    assert.equal(content.textContent, expected.at(-1));

    const surrogate = node();
    h.reveal.bind('surrogate', {content: surrogate}, {content: '\uD83D'});
    h.reveal.flush();
    assert.equal(surrogate.textContent, '', 'an unfinished transport surrogate must not be displayed');
    h.reveal.bind('surrogate', {content: surrogate}, {content: '😀'});
    h.drain();
    assert.equal(surrogate.textContent, '😀');

    const fallback = createHarness({fallback: true});
    fallback.reveal.bind('fallback', {content}, {content: '😀中'});
    fallback.advance(20);
    assert.equal(content.textContent, '😀', 'fallback must still preserve complete code points');
}

{
    const h = createHarness();
    const content = node();
    const reasoning = node();
    h.reveal.bind('first', {content, reasoning}, {content: 'Answer', reasoning: 'Thought'});
    h.reveal.bind('second', {content: node()}, {content: 'Another answer'});
    assert.equal(h.frames.size, 1, 'content, reasoning, and requests share one frame');
    h.advance(20);
    assert.equal(content.textContent, 'A');
    assert.equal(reasoning.textContent, 'T');
    h.reveal.remove('first');
    assert.equal(h.reveal.has('first'), false);
    assert.equal(h.reveal.hasPending(), true, 'removing one request must not stop another');
    const stale = Array.from(h.frames.values())[0];
    h.reveal.clear();
    assert.equal(h.reveal.hasPending(), false);
    assert.equal(h.frames.size, 0);
    stale();
    assert.equal(content.textContent, 'A', 'a canceled frame must not reveal removed text');
    assert.equal(h.frames.size, 0);
    assert.equal(h.idleCount(), 1);
    h.reveal.bind('absent', {}, {content: 'No node'});
    assert.equal(h.reveal.hasPending(), false);
    assert.equal(h.frames.size, 0);
}

{
    const h = createHarness();
    const content = node();
    const burst = '字'.repeat(2000);
    h.reveal.bind('burst', {content}, {content: burst});
    h.advance(16);
    assert.ok(content.textContent.length > 0 && content.textContent.length < burst.length);
    for (let i = 0; i < 55; i++) h.advance(16);
    assert.equal(content.textContent, burst, 'a burst should catch up within about one second');
    assert.equal(h.reveal.hasPending(), false);
}

{
    const settings = {immediate: true};
    const h = createHarness(settings);
    const content = node();
    h.reveal.bind('immediate', {content}, {content: 'Complete'});
    assert.equal(content.textContent, 'Complete');
    assert.equal(h.frames.size, 0);
    assert.equal(h.idleCount(), 0, 'immediate initial text never enters pending state');
    settings.immediate = false;
    h.reveal.bind('immediate', {content}, {content: 'Complete sentence'});
    settings.immediate = true;
    h.advance(16);
    assert.equal(content.textContent, 'Complete sentence');
    assert.equal(h.idleCount(), 1);
}
