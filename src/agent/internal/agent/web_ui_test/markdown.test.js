const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const context = {};
const scripts = path.join(__dirname, '..', 'web_ui', 'scripts');
vm.runInNewContext(fs.readFileSync(path.join(scripts, 'markdown.js'), 'utf8'), context);
vm.runInNewContext(fs.readFileSync(path.join(scripts, 'messages.js'), 'utf8'), context);
const {parseMarkdown, parseMarkdownInline, splitSpeechText} = context;
const plain = value => JSON.parse(JSON.stringify(value));

// Inline nodes as compact strings: **x** -> strong(x), [a](u) -> link(u:a).
function inline(nodes) {
    return nodes.map(function(node) {
        if (node.type === 'text') return node.text;
        if (node.type === 'break') return '\n';
        if (node.type === 'code') return 'code(' + node.text + ')';
        if (node.type === 'link') return 'link(' + node.href + ':' + inline(node.children) + ')';
        return node.type + '(' + inline(node.children) + ')';
    }).join('');
}

for (const [source, expected] of [
    ['**bold** and *em* and ~~gone~~', 'strong(bold) and em(em) and del(gone)'],
    ['**.name Grandfathered** — a win', 'strong(.name Grandfathered) — a win'],
    ['use `a*b*c` here', 'use code(a*b*c) here'],
    ['snake_case_name and __strong__', 'snake_case_name and strong(strong)'],
    ['2 * 3 * 4 and a* b*', '2 * 3 * 4 and a* b*'],
    ['**unclosed while streaming', '**unclosed while streaming'],
    ['\\*literal\\*', '*literal*'],
    ['[docs](https://a.dev/x) and https://b.dev/y.', 'link(https://a.dev/x:docs) and link(https://b.dev/y:https://b.dev/y).'],
    ['(see https://a.dev/p_(x))', '(see link(https://a.dev/p_(x):https://a.dev/p_(x)))'],
    ['[file](/share/a.txt)', 'link(/share/a.txt:file)'],
    ['[wiki](https://w.org/A_(b))', 'link(https://w.org/A_(b):wiki)'],
    ['line one\nline two', 'line one\nline two']
]) {
    assert.equal(inline(parseMarkdownInline(source)), expected, source);
}

for (const unsafe of ['javascript:alert(1)', 'data:text/html,x', '//evil.example/x', 'vbscript:x']) {
    assert.equal(inline(parseMarkdownInline('[x](' + unsafe + ')')), 'x', 'unsafe link must render as text: ' + unsafe);
}

{
    const blocks = plain(parseMarkdown([
        '# Title',
        'First paragraph',
        'continues here.',
        '',
        '- one',
        '- two',
        '  - nested',
        '',
        '3. three',
        '4. four',
        '',
        '> quoted',
        '',
        '| A | B |',
        '|:--|--:|',
        '| 1 | **2** |',
        '',
        '---',
        '```js',
        'const a = 1;',
        '```'
    ].join('\n')));
    assert.deepEqual(blocks.map(block => block.type),
        ['heading', 'paragraph', 'list', 'list', 'quote', 'table', 'rule', 'code']);
    assert.equal(inline(blocks[1].inline), 'First paragraph\ncontinues here.');
    assert.equal(blocks[2].items.length, 2);
    assert.equal(blocks[2].items[1][1].type, 'list', 'an indented item nests under its parent');
    assert.equal(blocks[3].ordered, true);
    assert.equal(blocks[3].start, 3);
    assert.deepEqual(blocks[5].align, ['left', 'right']);
    assert.equal(inline(blocks[5].rows[0][1]), 'strong(2)');
    assert.deepEqual([blocks[7].lang, blocks[7].text, blocks[7].open], ['js', 'const a = 1;', false]);
}

{
    const loose = plain(parseMarkdown('1. first\n\n2. second\n\nAfter'));
    assert.deepEqual(loose.map(block => block.type), ['list', 'paragraph'],
        'blank lines between items keep one list');
    assert.equal(loose[0].items.length, 2);

    const streaming = plain(parseMarkdown('Run:\n```sh\nls -la'));
    assert.deepEqual([streaming[1].type, streaming[1].text, streaming[1].open], ['code', 'ls -la', true],
        'an unclosed fence renders as code so far');

    const fenced = plain(parseMarkdown('```\n# not a heading\n- not a list\n```'));
    assert.deepEqual([fenced.length, fenced[0].text], [1, '# not a heading\n- not a list']);

    assert.equal(plain(parseMarkdown('a | b')).length, 1, 'a pipe without a delimiter row is not a table');
}

{
    const makefile = plain(parseMarkdown('```make\nall:\n\tgcc main.c\n```\n\n\tindented'));
    assert.equal(makefile[0].text, 'all:\n\tgcc main.c', 'fenced code keeps its tabs');
    assert.equal(makefile[1].type, 'paragraph', 'tabs outside fences still count as indentation');
}

// Speech markup is only recognized outside Markdown code.
const speech = text => plain(splitSpeechText(text)).map(part => (part.speech ? 'S:' : 'T:') + part.text.trim());
assert.deepEqual(speech('<tts>Hi.</tts>\n\nUse `<tts>x</tts>` here.'), ['S:Hi.', 'T:Use `<tts>x</tts>` here.']);
assert.deepEqual(speech('<tts>Config.</tts>\n\n```toml\n[tts]\nvoice = "a"\n```'),
    ['S:Config.', 'T:```toml\n[tts]\nvoice = "a"\n```']);
assert.deepEqual(speech('<tts>Streaming'), ['S:Streaming']);

// Expanded spoken/reasoning rows, against the real createCollapsibleLine.
{
    class Element {
        constructor() {
            this.children = [];
            this.classes = new Set();
            this.handlers = {};
            this.isConnected = false;
            this.classList = {
                contains: name => this.classes.has(name),
                toggle: (name, on) => (on ? this.classes.add(name) : this.classes.delete(name))
            };
        }
        set className(value) { this.classes = new Set(String(value).split(/\s+/).filter(Boolean)); }
        setAttribute() {}
        appendChild(child) { this.children.push(child); return child; }
        addEventListener(type, handler) { this.handlers[type] = handler; }
        click() { this.handlers.click(); }
    }
    context.document = {createElement: () => new Element()};
    context.window = {};
    const line = (key, text, kind = 'speech:0') => {
        const block = context.createCollapsibleLine({className: 'tts-block', iconClass: 'tts-icon', textClass: 'tts-text',
            label: 'Spoken aloud', kind, text, expandKey: key});
        block.isConnected = true;
        return block;
    };
    const expanded = block => block.classList.contains('expanded');
    const expandedLines = () => vm.runInContext('expandedLines', context);
    const entries = () => Array.from(expandedLines().values()).map(entry => entry.text);

    let row = line('live', 'Hel');
    row.click();
    row.isConnected = false;
    row = line('live', 'Hello there');
    assert.ok(expanded(row), 'a streaming re-render keeps the row expanded');
    assert.deepEqual(entries(), ['Hello there'], 'only the latest text is retained');
    row.click();
    assert.deepEqual(entries(), [], 'collapsing releases the row');

    const first = line('a', 'Same words.');
    const second = line('b', 'Same words.');
    context.migrateExpandedLines();
    first.click();
    assert.equal(expanded(line('b', 'Same words.')), false, 'identical text in another row does not expand it');

    first.isConnected = false;
    second.isConnected = false;
    const fromHistory = line('context-1', 'Same words.');
    context.migrateExpandedLines();
    assert.ok(expanded(fromHistory), 'history replacing the live row keeps it expanded');
    assert.deepEqual(Array.from(expandedLines().keys()), ['context-1']);

    // Two identical spoken rows in one message: the second stays the open one.
    fromHistory.click();
    context.migrateExpandedLines();
    const liveFirst = line('run\u001f0', 'Twice.', 'speech:0');
    const liveSecond = line('run\u001f1', 'Twice.', 'speech:1');
    context.migrateExpandedLines();
    liveSecond.click();
    liveFirst.isConnected = false;
    liveSecond.isConnected = false;
    const historyFirst = line('context-2\u001f0', 'Twice.', 'speech:0');
    const historySecond = line('context-2\u001f1', 'Twice.', 'speech:1');
    context.migrateExpandedLines();
    assert.deepEqual([expanded(historyFirst), expanded(historySecond)], [false, true]);
}

console.log('markdown tests passed');
