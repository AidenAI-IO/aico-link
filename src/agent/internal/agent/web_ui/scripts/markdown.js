// Markdown for assistant replies. Parsing is pure (testable without a DOM);
// rendering only creates elements and text nodes, so model output can never
// inject HTML. Partial input from a stream renders sensibly: an unclosed
// fence is a code block so far, and unclosed emphasis stays literal.
const mdFencePattern = /^ {0,3}(`{3,}|~{3,})\s*([^`\s]*)[^`]*$/;
const mdHeadingPattern = /^ {0,3}(#{1,6})\s+(.*?)(?:\s+#+)?\s*$/;
const mdRulePattern = /^ {0,3}([-*_])(?:[ \t]*\1){2,}[ \t]*$/;
const mdQuotePattern = /^ {0,3}> ?(.*)$/;
const mdListPattern = /^( *)([-*+]|\d{1,9}[.)])(?:[ \t]+(.*))?$/;
const mdTableDelimiterPattern = /^ *\|? *:?-+:? *(?:\| *:?-+:? *)*\|? *$/;

function parseMarkdown(text) {
    // Tabs become spaces for block structure, but fenced code keeps them
    // (a Makefile recipe needs its tab).
    let fence = null;
    const lines = String(text || '').replace(/\r\n?/g, '\n').split('\n').map(function(line) {
        const match = line.match(mdFencePattern);
        if (fence) {
            if (fence.test(line)) fence = null;
            return line;
        }
        if (match) fence = new RegExp('^ {0,3}' + match[1][0] + '{' + match[1].length + ',}\\s*$');
        return line.replace(/\t/g, '    ');
    });
    return parseMarkdownBlocks(lines);
}

function parseMarkdownBlocks(lines) {
    const blocks = [];
    let i = 0;
    while (i < lines.length) {
        const line = lines[i];
        if (!line.trim()) {
            i++;
            continue;
        }
        let match = line.match(mdFencePattern);
        if (match) {
            const closing = new RegExp('^ {0,3}' + match[1][0] + '{' + match[1].length + ',}\\s*$');
            const body = [];
            let closed = false;
            for (i++; i < lines.length; i++) {
                if (closing.test(lines[i])) {
                    closed = true;
                    i++;
                    break;
                }
                body.push(lines[i]);
            }
            blocks.push({type: 'code', lang: match[2] || '', text: body.join('\n'), open: !closed});
            continue;
        }
        match = line.match(mdHeadingPattern);
        if (match) {
            blocks.push({type: 'heading', level: match[1].length, inline: parseMarkdownInline(match[2])});
            i++;
            continue;
        }
        if (mdRulePattern.test(line)) {
            blocks.push({type: 'rule'});
            i++;
            continue;
        }
        if (mdQuotePattern.test(line)) {
            const body = [];
            while (i < lines.length && (match = lines[i].match(mdQuotePattern))) {
                body.push(match[1]);
                i++;
            }
            blocks.push({type: 'quote', blocks: parseMarkdownBlocks(body)});
            continue;
        }
        if (isMarkdownTableStart(lines, i)) {
            const header = splitMarkdownRow(line);
            const align = splitMarkdownRow(lines[i + 1]).map(function(cell) {
                const left = cell.startsWith(':');
                const right = cell.endsWith(':');
                return left && right ? 'center' : right ? 'right' : left ? 'left' : '';
            });
            const rows = [];
            for (i += 2; i < lines.length && lines[i].trim() && lines[i].includes('|'); i++) {
                const cells = splitMarkdownRow(lines[i]);
                rows.push(header.map((_, column) => parseMarkdownInline(cells[column] || '')));
            }
            blocks.push({
                type: 'table',
                align: header.map((_, column) => align[column] || ''),
                header: header.map(parseMarkdownInline),
                rows
            });
            continue;
        }
        if (mdListPattern.test(line)) {
            const list = parseMarkdownList(lines, i);
            blocks.push(list.block);
            i = list.next;
            continue;
        }
        const paragraph = [line.trim()];
        for (i++; i < lines.length && lines[i].trim() &&
            !startsMarkdownBlock(lines[i]) && !isMarkdownTableStart(lines, i); i++) {
            paragraph.push(lines[i].trim());
        }
        blocks.push({type: 'paragraph', inline: parseMarkdownInline(paragraph.join('\n'))});
    }
    return blocks;
}

function startsMarkdownBlock(line) {
    return mdFencePattern.test(line) || mdHeadingPattern.test(line) || mdRulePattern.test(line) ||
        mdQuotePattern.test(line) || mdListPattern.test(line);
}

function isMarkdownTableStart(lines, index) {
    return index + 1 < lines.length && lines[index].includes('|') &&
        lines[index + 1].includes('|') && mdTableDelimiterPattern.test(lines[index + 1]);
}

function splitMarkdownRow(line) {
    return line.trim().replace(/^\|/, '').replace(/(?<!\\)\|$/, '')
        .split(/(?<!\\)\|/).map(cell => cell.trim().replace(/\\\|/g, '|'));
}

function leadingSpaces(line) {
    return line.length - line.trimStart().length;
}

function parseMarkdownList(lines, start) {
    const first = lines[start].match(mdListPattern);
    const indent = first[1].length;
    const ordered = /\d/.test(first[2]);
    const isSibling = function(line) {
        const match = line.match(mdListPattern);
        return !!match && match[1].length === indent && /\d/.test(match[2]) === ordered;
    };
    const items = [];
    let i = start;
    while (i < lines.length && isSibling(lines[i])) {
        const marker = lines[i].match(mdListPattern);
        const contentIndent = indent + marker[2].length + 1;
        const body = [marker[3] || ''];
        for (i++; i < lines.length;) {
            const line = lines[i];
            if (!line.trim()) {
                let next = i + 1;
                while (next < lines.length && !lines[next].trim()) next++;
                if (next < lines.length && leadingSpaces(lines[next]) >= contentIndent) {
                    for (; i < next; i++) body.push('');
                    continue;
                }
                // A blank line between items keeps one (loose) list.
                if (next < lines.length && isSibling(lines[next])) i = next;
                break;
            }
            const nested = line.match(mdListPattern);
            if (nested && nested[1].length <= indent) break;
            const spaces = leadingSpaces(line);
            if (spaces >= contentIndent || nested) {
                body.push(line.slice(Math.min(spaces, contentIndent)));
            } else if (!startsMarkdownBlock(line)) {
                body.push(line.trim());
            } else {
                break;
            }
            i++;
        }
        items.push(parseMarkdownBlocks(body));
    }
    return {
        block: {type: 'list', ordered, start: ordered ? parseInt(first[2], 10) : 1, items},
        next: i
    };
}

const mdEmphasisDelimiters = [['**', 'strong'], ['__', 'strong'], ['~~', 'del'], ['*', 'em'], ['_', 'em']];
const mdWordCharacter = /[\p{L}\p{N}]/u;

function parseMarkdownInline(text) {
    text = String(text || '');
    const nodes = [];
    let buffer = '';
    const flush = function() {
        if (buffer) nodes.push({type: 'text', text: buffer});
        buffer = '';
    };
    let i = 0;
    while (i < text.length) {
        const character = text[i];
        if (character === '\\' && /[\\`*_{}[\]()#+\-.!|~>]/.test(text[i + 1] || '')) {
            buffer += text[i + 1];
            i += 2;
            continue;
        }
        if (character === '\n') {
            flush();
            nodes.push({type: 'break'});
            i++;
            continue;
        }
        if (character === '`') {
            const run = text.slice(i).match(/^`+/)[0];
            const end = text.indexOf(run, i + run.length);
            if (end > i) {
                flush();
                let code = text.slice(i + run.length, end);
                if (/^ .* $/.test(code)) code = code.slice(1, -1);
                nodes.push({type: 'code', text: code});
                i = end + run.length;
                continue;
            }
            buffer += run;
            i += run.length;
            continue;
        }
        if (character === '*' || character === '_' || character === '~') {
            const span = matchMarkdownEmphasis(text, i);
            if (span) {
                flush();
                nodes.push({type: span.type, children: parseMarkdownInline(span.inner)});
                i = span.end;
                continue;
            }
        }
        if (character === '[' || (character === '!' && text[i + 1] === '[')) {
            const offset = character === '!' ? 1 : 0;
            const link = text.slice(i + offset).match(/^\[([^\]\n]+)\]\(((?:[^()\s]|\([^()\s]*\))+)(?:\s+"[^"]*")?\)/);
            if (link) {
                flush();
                const href = safeMarkdownUrl(link[2]);
                const children = parseMarkdownInline(link[1]);
                if (href) nodes.push({type: 'link', href, children});
                else nodes.push.apply(nodes, children);
                i += offset + link[0].length;
                continue;
            }
        }
        if (character === 'h' && text.startsWith('http', i) && !mdWordCharacter.test(text[i - 1] || '')) {
            const match = text.slice(i).match(/^https?:\/\/[^\s<>"'`]+/);
            if (match) {
                let url = match[0].replace(/[.,;:!?。，；：！？]+$/, '');
                while (url.endsWith(')') && (url.match(/\(/g) || []).length < (url.match(/\)/g) || []).length) {
                    url = url.slice(0, -1);
                }
                flush();
                nodes.push({type: 'link', href: url, children: [{type: 'text', text: url}]});
                i += url.length;
                continue;
            }
        }
        buffer += character;
        i++;
    }
    flush();
    return nodes;
}

function matchMarkdownEmphasis(text, start) {
    for (const [delimiter, type] of mdEmphasisDelimiters) {
        if (!text.startsWith(delimiter, start)) continue;
        const underscore = delimiter[0] === '_';
        if (underscore && mdWordCharacter.test(text[start - 1] || '')) continue;
        const open = start + delimiter.length;
        if (!text[open] || /\s/.test(text[open])) continue;
        for (let search = open; ;) {
            const end = text.indexOf(delimiter, search);
            if (end < 0) break;
            search = end + 1;
            const inner = text.slice(open, end);
            if (!inner || /\s$/.test(inner)) continue;
            // A single delimiter must not close on part of a double one.
            if (delimiter.length === 1 && (text[end + 1] === delimiter || text[end - 1] === delimiter)) continue;
            if (underscore && mdWordCharacter.test(text[end + delimiter.length] || '')) continue;
            return {type, inner, end: end + delimiter.length};
        }
    }
    return null;
}

function safeMarkdownUrl(value) {
    const url = String(value || '').trim();
    if (/[\u0000-\u001f\u007f]/.test(url)) return '';
    if (/^(https?:|mailto:)/i.test(url)) return url;
    if (url[0] === '#' || (url[0] === '/' && url[1] !== '/' && url[1] !== '\\')) return url;
    return '';
}

function renderMarkdown(container, text) {
    appendMarkdownBlocks(container, parseMarkdown(text));
}

function appendMarkdownBlocks(parent, blocks) {
    blocks.forEach(function(block) {
        parent.appendChild(createMarkdownBlock(block));
    });
}

function createMarkdownBlock(block) {
    let node;
    switch (block.type) {
    case 'heading':
        node = document.createElement('h' + block.level);
        appendMarkdownInline(node, block.inline);
        return node;
    case 'code': {
        node = document.createElement('pre');
        const code = document.createElement('code');
        code.textContent = block.text;
        if (block.lang) node.dataset.lang = block.lang;
        node.appendChild(code);
        return node;
    }
    case 'quote':
        node = document.createElement('blockquote');
        appendMarkdownBlocks(node, block.blocks);
        return node;
    case 'rule':
        return document.createElement('hr');
    case 'list':
        node = document.createElement(block.ordered ? 'ol' : 'ul');
        if (block.ordered && block.start !== 1) node.start = block.start;
        block.items.forEach(function(item) {
            const li = document.createElement('li');
            // A one-paragraph item stays tight, without a <p> wrapper.
            if (item.length === 1 && item[0].type === 'paragraph') appendMarkdownInline(li, item[0].inline);
            else appendMarkdownBlocks(li, item);
            node.appendChild(li);
        });
        return node;
    case 'table': {
        node = document.createElement('div');
        node.className = 'md-table';
        const table = document.createElement('table');
        const appendRow = function(section, cells, tag) {
            const tr = document.createElement('tr');
            cells.forEach(function(cell, column) {
                const td = document.createElement(tag);
                if (block.align[column]) td.style.textAlign = block.align[column];
                appendMarkdownInline(td, cell);
                tr.appendChild(td);
            });
            section.appendChild(tr);
        };
        const head = document.createElement('thead');
        appendRow(head, block.header, 'th');
        table.appendChild(head);
        if (block.rows.length) {
            const body = document.createElement('tbody');
            block.rows.forEach(row => appendRow(body, row, 'td'));
            table.appendChild(body);
        }
        node.appendChild(table);
        return node;
    }
    default:
        node = document.createElement('p');
        appendMarkdownInline(node, block.inline);
        return node;
    }
}

const mdInlineTags = {strong: 'strong', em: 'em', del: 'del'};

function appendMarkdownInline(parent, nodes) {
    nodes.forEach(function(item) {
        if (item.type === 'text') {
            parent.appendChild(document.createTextNode(item.text));
        } else if (item.type === 'break') {
            parent.appendChild(document.createElement('br'));
        } else if (item.type === 'code') {
            const code = document.createElement('code');
            code.textContent = item.text;
            parent.appendChild(code);
        } else if (item.type === 'link') {
            const link = document.createElement('a');
            link.href = item.href;
            if (/^https?:/i.test(item.href)) {
                link.target = '_blank';
                link.rel = 'noopener noreferrer';
            }
            appendMarkdownInline(link, item.children);
            parent.appendChild(link);
        } else if (mdInlineTags[item.type]) {
            const element = document.createElement(mdInlineTags[item.type]);
            appendMarkdownInline(element, item.children);
            parent.appendChild(element);
        }
    });
}
