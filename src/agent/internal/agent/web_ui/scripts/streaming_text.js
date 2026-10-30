// Reveal received text independently of the complete conversation projection.
function createStreamingTextReveal(options) {
    options = options || {};
    const requestFrame = options.requestFrame || (callback => requestAnimationFrame(callback));
    const cancelFrame = options.cancelFrame || (id => cancelAnimationFrame(id));
    const now = options.now || (() => performance.now());
    const immediate = options.immediate || (() => false);
    const segmenter = typeof Intl !== 'undefined' && Intl.Segmenter
        ? new Intl.Segmenter(undefined, {granularity: 'grapheme'}) : null;
    const entries = new Map();
    const normalRate = 70;
    const catchUpSeconds = 0.8;
    let frameId = null;
    let frameToken = 0;
    let lastFrameAt = 0;
    let pending = false;

    function characters(text) {
        // A transport chunk can end between the two UTF-16 surrogate units.
        // Wait for the next bind before exposing that unfinished code point.
        const last = text.charCodeAt(text.length - 1);
        if (last >= 0xD800 && last <= 0xDBFF) text = text.slice(0, -1);
        return segmenter ? Array.from(segmenter.segment(text), part => part.segment) : Array.from(text);
    }

    function fieldPending(field) {
        return field.node && field.visibleCount < field.characters.length;
    }

    function anyPending() {
        for (const entry of entries.values()) {
            if (fieldPending(entry.content) || fieldPending(entry.reasoning)) return true;
        }
        return false;
    }

    function visibleText(field) {
        return field.characters.slice(0, field.visibleCount).join('');
    }

    function writeFields(fields) {
        const updates = fields.filter(field => field.node && field.node.textContent !== visibleText(field));
        if (!updates.length) return;
        const context = options.beforeFrame ? options.beforeFrame() : undefined;
        updates.forEach(field => { field.node.textContent = visibleText(field); });
        if (options.afterFrame) options.afterFrame(context);
    }

    function synchronize() {
        const wasPending = pending;
        pending = anyPending();
        if (pending && frameId === null) {
            if (!wasPending) lastFrameAt = now();
            const token = ++frameToken;
            frameId = requestFrame(function() {
                if (token !== frameToken) return;
                tick();
            });
        } else if (!pending && frameId !== null) {
            cancelFrame(frameId);
            frameId = null;
            frameToken++;
        }
        if (wasPending && !pending && options.onIdle) options.onIdle();
    }

    function tick() {
        frameId = null;
        const timestamp = now();
        const elapsedSeconds = Math.max(0, Math.min(timestamp - lastFrameAt, 100)) / 1000;
        lastFrameAt = timestamp;
        const showImmediately = immediate();
        const changed = [];
        entries.forEach(function(entry) {
            [entry.content, entry.reasoning].forEach(function(field) {
                if (!fieldPending(field)) return;
                field.credit += elapsedSeconds * field.rate;
                const count = showImmediately ? field.characters.length : Math.floor(field.credit);
                if (count === 0) return;
                field.visibleCount = Math.min(field.characters.length, field.visibleCount + count);
                field.credit -= count;
                if (!fieldPending(field)) {
                    field.credit = 0;
                    field.rate = normalRate;
                }
                changed.push(field);
            });
        });
        writeFields(changed);
        synchronize();
    }

    function bindField(previous, node, text, showImmediately) {
        const parts = characters(String(text || ''));
        let visibleCount = 0;
        if (previous) {
            // Final output can correct an already visible streamed prefix.
            while (visibleCount < previous.visibleCount && visibleCount < parts.length &&
                previous.characters[visibleCount] === parts[visibleCount]) visibleCount++;
        }
        if (showImmediately) visibleCount = parts.length;
        const remaining = parts.length - visibleCount;
        return {
            node: node || null,
            characters: parts,
            visibleCount,
            credit: remaining && previous ? previous.credit : 0,
            // Keep the faster rate through a burst so its last characters do
            // not decelerate into several extra seconds of playback.
            rate: remaining ? Math.max(normalRate, previous ? previous.rate : 0, remaining / catchUpSeconds) : normalRate
        };
    }

    function bind(key, nodes, text) {
        if (!key) return;
        nodes = nodes || {};
        text = text || {};
        if (!nodes.content && !nodes.reasoning) {
            remove(key);
            return;
        }
        const previous = entries.get(key);
        const showImmediately = immediate();
        const entry = {
            content: bindField(previous && previous.content, nodes.content, text.content, showImmediately),
            reasoning: bindField(previous && previous.reasoning, nodes.reasoning, text.reasoning, showImmediately)
        };
        entries.set(key, entry);
        writeFields([entry.content, entry.reasoning]);
        synchronize();
    }

    function remove(key) {
        entries.delete(key);
        synchronize();
    }

    function clear() {
        entries.clear();
        synchronize();
    }

    function flush() {
        const fields = [];
        entries.forEach(function(entry) {
            [entry.content, entry.reasoning].forEach(function(field) {
                field.visibleCount = field.characters.length;
                field.credit = 0;
                field.rate = normalRate;
                fields.push(field);
            });
        });
        writeFields(fields);
        synchronize();
    }

    return {bind, has: key => entries.has(key), hasPending: () => pending, remove, clear, flush};
}
