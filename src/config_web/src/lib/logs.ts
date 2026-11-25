export type ObjectData = Record<string, unknown>;
export const object = (value: unknown): ObjectData =>
  value && typeof value === 'object' && !Array.isArray(value) ? (value as ObjectData) : {};
export const array = (value: unknown): unknown[] => (Array.isArray(value) ? value : []);
const string = (value: unknown): string => (typeof value === 'string' ? value : '');
export interface ToolCall {
  id?: string;
  type: string;
  function: { name: string; arguments: string };
}
export interface LogMessage {
  role: string;
  content: unknown;
  reasoning_content?: string;
  tool_calls?: ToolCall[];
  tool_call_id?: string;
}
export interface LogEntry {
  kind: 'request' | 'response';
  body: string;
  ts: string;
  status?: number;
}
export interface LogGroup {
  request: LogEntry;
  response: LogEntry | null;
}
export interface LogParser {
  groups: LogGroup[];
  pendingGroup: LogGroup | null;
  buffer: string;
  invalidLines: number;
}
export function parseBody(body: unknown): ObjectData {
  try {
    return object(JSON.parse(string(body)));
  } catch {
    return {};
  }
}
export function isValidLogFileName(name: string) {
  return /^llm-http-.+\.log$/.test(name) && !/[\\/]/.test(name);
}
export function isRenderableImageDataUrl(url: string) {
  return /^data:image\/(?:png|jpeg|jpg|gif|webp|avif|bmp);base64,[a-z\d+/=\s]+$/i.test(url);
}
export function isRenderableRemoteImageUrl(url: string) {
  try {
    const parsed = new URL(url);
    return ['http:', 'https:'].includes(parsed.protocol) && !!parsed.hostname;
  } catch {
    return false;
  }
}
export function imageMimeFromPart(value: unknown) {
  const part = object(value),
    mime = string(part.mime_type) || string(part.mimeType);
  if (mime.startsWith('image/')) return mime;
  const format = string(part.format).toLowerCase();
  return ['jpg', 'jpeg', 'png', 'gif', 'webp'].includes(format)
    ? `image/${format === 'jpg' ? 'jpeg' : format}`
    : '';
}
export function imageUrlFromPart(value: unknown): string {
  const part = object(value),
    source = object(part.source);
  let url = string(part.image_url) || string(object(part.image_url).url);
  if (!url && source.type === 'url') url = string(source.url);
  if (!url && source.type === 'base64' && string(source.media_type).startsWith('image/'))
    url = `data:${source.media_type};base64,${string(source.data)}`;
  if (!url) url = string(part.url);
  if (!url && part.data && imageMimeFromPart(part))
    url = `data:${imageMimeFromPart(part)};base64,${string(part.data)}`;
  return isRenderableImageDataUrl(url) || isRenderableRemoteImageUrl(url) ? url : '';
}
export function imageLabelFromUrl(url: string) {
  if (url.startsWith('data:')) return url.slice(5, url.indexOf(';')) + ' base64 image';
  try {
    const parsed = new URL(url);
    return parsed.pathname.split('/').filter(Boolean).at(-1) || parsed.hostname;
  } catch {
    return 'image';
  }
}
export function contentPartText(value: unknown): string {
  const part = object(value);
  if (part.type === 'text' || part.type === 'input_text' || part.type === 'output_text')
    return string(part.text);
  const url = imageUrlFromPart(part);
  if (url) return `[Image: ${imageLabelFromUrl(url)}]`;
  if (part.type === 'input_audio')
    return `[input_audio${object(part.input_audio).format ? ': ' + object(part.input_audio).format : ''}]`;
  if (part.type === 'tool_result')
    return `[Tool result${part.tool_use_id ? ' ' + part.tool_use_id : ''}]\n${typeof part.content === 'string' ? part.content : JSON.stringify(part.content, null, 2) || ''}`;
  return `[${string(part.type) || 'unknown'}]`;
}
export function messageText(message: LogMessage) {
  const content =
    typeof message.content === 'string'
      ? message.content
      : Array.isArray(message.content)
        ? message.content.map(contentPartText).filter(Boolean).join('\n')
        : message.content == null
          ? ''
          : JSON.stringify(message.content, null, 2);
  return (
    (message.reasoning_content
      ? '[Reasoning]\n' + message.reasoning_content + (content ? '\n' : '')
      : '') + content
  );
}
export function normalizeAnthropicContentPart(value: unknown): unknown {
  const part = object(value);
  return part.type === 'image' && object(part.source).type === 'base64'
    ? { type: 'image_url', image_url: { url: imageUrlFromPart(part) } }
    : value;
}
const tool = (value: unknown): ToolCall => {
  const part = object(value),
    fn = object(part.function);
  return {
    id: string(part.call_id) || string(part.id) || undefined,
    type: 'function',
    function: {
      name: string(fn.name) || string(part.name),
      arguments: string(fn.arguments) || string(part.arguments) || JSON.stringify(part.input || {}),
    },
  };
};
export function normalizeAnthropicMessage(value: unknown): LogMessage {
  const message = object(value);
  const result: LogMessage = {
    role: string(message.role) || 'assistant',
    content: message.content ?? '',
  };
  if (Array.isArray(message.tool_calls)) result.tool_calls = message.tool_calls.map(tool);
  if (typeof message.tool_call_id === 'string') result.tool_call_id = message.tool_call_id;
  if (Array.isArray(message.content)) {
    const content: unknown[] = [],
      calls: ToolCall[] = [];
    let reasoning = '';
    for (const value of message.content) {
      const part = object(value);
      if (part.type === 'thinking') reasoning += string(part.thinking);
      else if (part.type === 'tool_use') calls.push(tool(part));
      else content.push(normalizeAnthropicContentPart(part));
    }
    result.content = content;
    if (calls.length) result.tool_calls = calls;
    if (reasoning) result.reasoning_content = reasoning;
  }
  if (typeof message.reasoning_content === 'string')
    result.reasoning_content = message.reasoning_content;
  return result;
}
export function requestMessages(value: unknown): LogMessage[] {
  const req = object(value);
  const hasMessages = Array.isArray(req.messages) && req.messages.length > 0;
  const messages = hasMessages ? array(req.messages).map(normalizeAnthropicMessage) : [];
  if (!hasMessages && Object.hasOwn(req, 'input')) {
    if (typeof req.input === 'string') messages.push({ role: 'user', content: req.input });
    else
      for (const value of array(req.input)) {
        const item = object(value);
        if (item.type === 'function_call') {
          messages.push({
            role: 'assistant',
            content: '',
            tool_calls: [
              {
                id: string(item.call_id) || string(item.id),
                type: 'function',
                function: {
                  name: string(item.name),
                  arguments:
                    typeof item.arguments === 'string'
                      ? item.arguments
                      : JSON.stringify(item.arguments ?? {}),
                },
              },
            ],
          });
        } else if (item.type === 'function_call_output') {
          messages.push({
            role: 'tool',
            tool_call_id: string(item.call_id),
            content: responseItemText(item.output),
          });
        } else messages.push(normalizeAnthropicMessage(value));
      }
  }
  const system: LogMessage[] = [];
  if (typeof req.system === 'string' && req.system)
    system.push({ role: 'system', content: req.system });
  else if (Array.isArray(req.system) && req.system.length)
    system.push({ role: 'system', content: req.system.map(normalizeAnthropicContentPart) });
  else if (typeof req.instructions === 'string')
    system.push({ role: 'system', content: req.instructions });
  return system.concat(messages);
}
export function responseItemText(value: unknown): string {
  if (value == null) return '';
  if (typeof value === 'string') return value;
  if (Array.isArray(value)) return value.map(responseItemText).join('');
  const part = object(value);
  return (
    string(part.text) || string(part.thinking) || responseItemText(part.content ?? part.summary)
  );
}
export function splitTaggedThinking(content: string) {
  let remaining = content;
  const visible: string[] = [],
    reasoning: string[] = [];
  let found = false;
  while (remaining) {
    const open = remaining.match(/<(think|thinking)>/i),
      close = remaining.match(/<\/(think|thinking)>/i);
    const openAt = open?.index ?? -1,
      closeAt = close?.index ?? -1;
    if (openAt < 0 && closeAt < 0) {
      visible.push(remaining);
      break;
    }
    if (close && closeAt >= 0 && (openAt < 0 || closeAt < openAt)) {
      reasoning.push(remaining.slice(0, closeAt));
      remaining = remaining.slice(closeAt + close[0].length);
      found = true;
      continue;
    }
    if (!open) break;
    const after = remaining.slice(openAt + open[0].length),
      end = after.match(/<\/(think|thinking)>/i);
    if (!end) {
      visible.push(remaining);
      break;
    }
    visible.push(remaining.slice(0, openAt));
    reasoning.push(after.slice(0, end.index));
    remaining = after.slice((end.index ?? 0) + end[0].length);
    found = true;
  }
  return found
    ? {
        reasoning: reasoning
          .map((part) => part.trim())
          .filter(Boolean)
          .join('\n'),
        content: visible.join('').trim(),
      }
    : null;
}
export function addReasoningContent(message: LogMessage, value: unknown) {
  let reasoning = responseItemText(value);
  if (!reasoning && typeof message.content === 'string') {
    const tagged = splitTaggedThinking(message.content);
    if (tagged) {
      reasoning = tagged.reasoning;
      message.content = tagged.content;
    }
  }
  if (reasoning) message.reasoning_content = reasoning;
  return message;
}
export function extractResponsesMessage(value: unknown): LogMessage {
  const obj = object(value);
  let content = '',
    reasoning = '';
  const calls: ToolCall[] = [];
  for (const value of array(obj.output)) {
    const item = object(value);
    if (item.type === 'reasoning')
      reasoning += responseItemText(item.summary ?? item.content ?? item.text);
    else if (item.type === 'message') content += responseItemText(item.content);
    else if (item.type === 'function_call') calls.push(tool(item));
  }
  return addReasoningContent({ role: 'assistant', content, tool_calls: calls }, reasoning);
}
export function extractResponseMessage(body: string): LogMessage | null {
  if (!body) return null;
  const obj = parseBody(body);
  const choices = array(obj.choices);
  if (choices.length) {
    const message = object(object(choices[0]).message);
    const normalized = normalizeAnthropicMessage({ ...message, role: message.role || 'assistant' });
    normalized.tool_calls ||= [];
    return addReasoningContent(normalized, message.reasoning_content);
  }
  if (obj.error)
    return {
      role: 'assistant',
      content:
        'Error: ' +
        (typeof obj.error === 'string' ? obj.error : JSON.stringify(obj.error, null, 2)),
    };
  if (
    Array.isArray(obj.content) &&
    (obj.type === 'message' ||
      (typeof obj.id === 'string' &&
        typeof obj.model === 'string' &&
        Object.hasOwn(obj, 'stop_reason') &&
        obj.usage))
  ) {
    const message = normalizeAnthropicMessage(obj);
    message.content = array(message.content)
      .map((part) => string(object(part).text))
      .join('');
    return message;
  }
  if (Array.isArray(obj.output)) return extractResponsesMessage(obj);
  let hasData = false,
    content = '',
    reasoning = '',
    contentDelta = false,
    reasoningDelta = false;
  const calls: Record<string, ToolCall> = {};
  for (const raw of body.split('\n')) {
    if (!raw.trim().startsWith('data:')) continue;
    const data = raw.trim().slice(5).trim();
    if (!data || data === '[DONE]') continue;
    let event: ObjectData;
    try {
      event = object(JSON.parse(data));
    } catch {
      continue;
    }
    hasData = true;
    if (event.type === 'error') {
      content += (content ? '\n' : '') + string(object(event.error).message);
      continue;
    }
    const block = object(event.content_block);
    if (event.type === 'content_block_start' && block.type === 'tool_use') {
      calls[String(event.index)] = {
        ...tool(block),
        function: { name: string(block.name), arguments: '' },
      };
      continue;
    }
    if (event.type === 'response.output_text.delta' && typeof event.delta === 'string') {
      content += event.delta;
      contentDelta = true;
      continue;
    }
    if (
      ['response.reasoning_summary_text.delta', 'response.reasoning_text.delta'].includes(
        string(event.type),
      ) &&
      typeof event.delta === 'string'
    ) {
      reasoning += event.delta;
      reasoningDelta = true;
      continue;
    }
    if (event.type === 'response.output_item.done') {
      const item = object(event.item);
      if (item.type === 'reasoning' && !reasoningDelta)
        reasoning += responseItemText(item.summary ?? item.content ?? item.text);
      if (item.type === 'message' && !contentDelta) content += responseItemText(item.content);
      if (item.type === 'function_call')
        calls[string(item.call_id) || string(item.id) || String(Object.keys(calls).length)] =
          tool(item);
      continue;
    }
    const delta = object(event.delta);
    if (event.type === 'content_block_delta') {
      if (delta.type === 'text_delta') content += string(delta.text);
      if (['thinking_delta', 'reasoning_delta'].includes(string(delta.type)))
        reasoning += string(delta.thinking);
      const call = calls[String(event.index)];
      if (delta.type === 'input_json_delta' && call)
        call.function.arguments += string(delta.partial_json);
      continue;
    }
    const choice = object(array(event.choices)[0]),
      change = object(choice.delta ?? choice.message);
    reasoning += string(change.reasoning_content);
    content +=
      typeof change.content === 'string'
        ? change.content
        : array(change.content)
            .map((part) => string(object(part).text))
            .join('');
    for (const value of array(change.tool_calls)) {
      const part = object(value),
        key = String(part.index ?? part.id ?? Object.keys(calls).length);
      const call =
        calls[key] ?? (calls[key] = { type: 'function', function: { name: '', arguments: '' } });
      if (part.id) call.id = string(part.id);
      call.function.name += string(object(part.function).name);
      call.function.arguments += string(object(part.function).arguments);
    }
  }
  return hasData
    ? addReasoningContent(
        {
          role: 'assistant',
          content,
          tool_calls: Object.keys(calls)
            .sort((a, b) => +a - +b)
            .map((key) => calls[key]),
        },
        reasoning,
      )
    : null;
}
export function appendGroupedEntry(
  groups: LogGroup[],
  pending: LogGroup | null,
  value: unknown,
): LogGroup | null {
  const item = object(value);
  if (item.kind !== 'request' && item.kind !== 'response') return pending;
  const entry: LogEntry = {
    kind: item.kind,
    body: string(item.body),
    ts: string(item.ts),
    status: typeof item.status === 'number' ? item.status : undefined,
  };
  if (entry.kind === 'request') {
    if (pending) groups.push(pending);
    return { request: entry, response: null };
  }
  if (pending && !pending.response) {
    pending.response = entry;
    groups.push(pending);
    return null;
  }
  return pending;
}
export function processLogChunk(parser: LogParser, chunk: string, isFinal: boolean) {
  parser.buffer += chunk;
  const lines = parser.buffer.split('\n');
  parser.buffer = lines.pop() || '';
  if (isFinal && parser.buffer) {
    lines.push(parser.buffer);
    parser.buffer = '';
  }
  for (const raw of lines) {
    const line = raw.trim();
    if (!line) continue;
    try {
      parser.pendingGroup = appendGroupedEntry(
        parser.groups,
        parser.pendingGroup,
        JSON.parse(line),
      );
    } catch {
      parser.invalidLines++;
    }
  }
}
export async function streamLogEntries(
  name: string,
  onProgress: (loaded: number, total: number, groups: number) => void,
  signal: AbortSignal,
) {
  const response = await fetch('/api/logs/llm/' + encodeURIComponent(name), { signal });
  if (!response.ok) throw new Error(`HTTP ${response.status}`);
  const parser: LogParser = { groups: [], pendingGroup: null, buffer: '', invalidLines: 0 };
  const total = Number(response.headers.get('Content-Length')) || 0;
  let loaded = 0;
  const reader = response.body?.getReader();
  if (!reader) {
    const text = await response.text();
    loaded = text.length;
    processLogChunk(parser, text, true);
  } else {
    const decoder = new TextDecoder();
    let count = 0;
    try {
      for (;;) {
        const part = await reader.read();
        if (part.done) break;
        loaded += part.value.length;
        processLogChunk(parser, decoder.decode(part.value, { stream: true }), false);
        onProgress(loaded, total, parser.groups.length);
        if (++count % 8 === 0) await new Promise((resolve) => setTimeout(resolve, 0));
      }
      processLogChunk(parser, decoder.decode(), true);
    } finally {
      reader.releaseLock();
    }
  }
  if (parser.pendingGroup) parser.groups.push(parser.pendingGroup);
  onProgress(loaded, total || loaded, parser.groups.length);
  return { groups: parser.groups, invalidLines: parser.invalidLines };
}

export type MessageDiffOp = {
  kind: 'same' | 'modified' | 'removed' | 'added';
  oldIndex?: number;
  newIndex?: number;
};
export interface LineDiffRow {
  kind: 'same' | 'add' | 'del' | 'skip';
  text: string;
}
export function buildMessageDiffOps(oldMsgs: LogMessage[], newMsgs: LogMessage[]) {
  // Align whole messages by edit cost so replacements are first-class ops.
  // A modification costs less than delete+add but more than a lone add/delete,
  // preserving anchors around insertions while pairing real same-role edits.
  const a = oldMsgs.map(serializeMsg);
  const b = newMsgs.map(serializeMsg);
  const oldDiffText = oldMsgs.map(messageDiffText);
  const newDiffText = newMsgs.map(messageDiffText);
  const n = a.length,
    mm = b.length;
  const addCost = 2,
    removeCost = 2,
    modifyCost = 3;
  const canLineDiff = (i: number, j: number) =>
    oldMsgs[i] &&
    newMsgs[j] &&
    (oldMsgs[i].role || '') === (newMsgs[j].role || '') &&
    oldDiffText[i] !== newDiffText[j];
  const cost: number[][] = [];
  for (let i = 0; i <= n; i++) {
    cost.push(new Array(mm + 1).fill(0));
  }
  for (let i = n - 1; i >= 0; i--) cost[i][mm] = cost[i + 1][mm] + removeCost;
  for (let j = mm - 1; j >= 0; j--) cost[n][j] = cost[n][j + 1] + addCost;
  for (let i = n - 1; i >= 0; i--) {
    for (let j = mm - 1; j >= 0; j--) {
      if (a[i] === b[j]) {
        cost[i][j] = cost[i + 1][j + 1];
      } else {
        let best = Math.min(cost[i + 1][j] + removeCost, cost[i][j + 1] + addCost);
        if (canLineDiff(i, j)) best = Math.min(best, cost[i + 1][j + 1] + modifyCost);
        cost[i][j] = best;
      }
    }
  }
  let i = 0,
    j = 0;
  const ops: MessageDiffOp[] = [];
  while (i < n && j < mm) {
    if (a[i] === b[j] && cost[i][j] === cost[i + 1][j + 1]) {
      ops.push({ kind: 'same', oldIndex: i, newIndex: j });
      i++;
      j++;
    } else if (canLineDiff(i, j) && cost[i][j] === cost[i + 1][j + 1] + modifyCost) {
      ops.push({ kind: 'modified', oldIndex: i, newIndex: j });
      i++;
      j++;
    } else if (cost[i][j] === cost[i + 1][j] + removeCost) {
      ops.push({ kind: 'removed', oldIndex: i });
      i++;
    } else {
      ops.push({ kind: 'added', newIndex: j });
      j++;
    }
  }
  while (i < n) {
    ops.push({ kind: 'removed', oldIndex: i });
    i++;
  }
  while (j < mm) {
    ops.push({ kind: 'added', newIndex: j });
    j++;
  }
  return ops;
}
export function serializeMsg(m: LogMessage) {
  let s = (m.role || '') + '\u0001';
  if (typeof m.content === 'string') s += m.content;
  else if (Array.isArray(m.content)) s += m.content.map(serializeContentPart).join('\n');
  else if (m.content != null) {
    try {
      s += JSON.stringify(m.content);
    } catch {
      s += String(m.content);
    }
  }
  if (m.tool_calls) s += '\u0002' + JSON.stringify(m.tool_calls);
  if (m.tool_call_id) s += '\u0003' + m.tool_call_id;
  if (m.reasoning_content) s += '\u0004' + m.reasoning_content;
  return s;
}
function serializeContentPart(value: unknown) {
  const p = object(value);
  if (!p) return '';
  if (p.type === 'text') return 'text:' + (p.text || '');
  const url = imageUrlFromPart(p);
  if (url) return (p.type || 'image') + ':' + url;
  try {
    return JSON.stringify(p);
  } catch {
    return String(p);
  }
}
export function messageDiffText(m: LogMessage) {
  let text = messageText(m);
  if (m && m.tool_calls && m.tool_calls.length) {
    text +=
      (text ? '\n' : '') +
      m.tool_calls
        .map(
          (tc) =>
            'tool_call ' +
            (tc.function ? tc.function.name : '') +
            ' ' +
            (tc.function ? tc.function.arguments : ''),
        )
        .join('\n');
  }
  if (m && m.tool_call_id) text += (text ? '\n' : '') + 'tool_call_id ' + m.tool_call_id;
  return text;
}
export function buildLineDiffRows(oldText: string, newText: string) {
  const oldLines = splitDiffLines(oldText);
  const newLines = splitDiffLines(newText);
  let prefix = 0;
  while (
    prefix < oldLines.length &&
    prefix < newLines.length &&
    oldLines[prefix] === newLines[prefix]
  )
    prefix++;
  let suffix = 0;
  while (
    suffix < oldLines.length - prefix &&
    suffix < newLines.length - prefix &&
    oldLines[oldLines.length - 1 - suffix] === newLines[newLines.length - 1 - suffix]
  )
    suffix++;
  const rows: LineDiffRow[] = [];
  for (let i = 0; i < prefix; i++) rows.push({ kind: 'same', text: oldLines[i] });
  const oldMid = oldLines.slice(prefix, oldLines.length - suffix);
  const newMid = newLines.slice(prefix, newLines.length - suffix);
  rows.push(...buildLcsLineDiffRows(oldMid, newMid));
  for (let i = oldLines.length - suffix; i < oldLines.length; i++)
    rows.push({ kind: 'same', text: oldLines[i] });
  return compactDiffContext(rows, 3);
}
function splitDiffLines(text: string) {
  const normalized = String(text == null ? '' : text)
    .replace(/\r\n/g, '\n')
    .replace(/\r/g, '\n');
  if (normalized === '') return [''];
  const lines = normalized.split('\n');
  if (lines.length > 1 && lines[lines.length - 1] === '') lines.pop();
  return lines;
}
function buildLcsLineDiffRows(oldLines: string[], newLines: string[]): LineDiffRow[] {
  if (oldLines.length === 0) return newLines.map<LineDiffRow>((text) => ({ kind: 'add', text }));
  if (newLines.length === 0) return oldLines.map((text) => ({ kind: 'del', text }));
  if (oldLines.length * newLines.length > 200000) {
    return oldLines
      .map<LineDiffRow>((text) => ({ kind: 'del', text }))
      .concat(newLines.map<LineDiffRow>((text) => ({ kind: 'add', text })));
  }
  const lcs: number[][] = [];
  for (let i = 0; i <= oldLines.length; i++) lcs.push(new Array(newLines.length + 1).fill(0));
  for (let i = oldLines.length - 1; i >= 0; i--) {
    for (let j = newLines.length - 1; j >= 0; j--) {
      lcs[i][j] =
        oldLines[i] === newLines[j]
          ? lcs[i + 1][j + 1] + 1
          : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
    }
  }
  const rows: LineDiffRow[] = [];
  let i = 0,
    j = 0;
  while (i < oldLines.length && j < newLines.length) {
    if (oldLines[i] === newLines[j]) {
      rows.push({ kind: 'same', text: oldLines[i] });
      i++;
      j++;
    } else if (lcs[i + 1][j] >= lcs[i][j + 1]) {
      rows.push({ kind: 'del', text: oldLines[i] });
      i++;
    } else {
      rows.push({ kind: 'add', text: newLines[j] });
      j++;
    }
  }
  while (i < oldLines.length) {
    rows.push({ kind: 'del', text: oldLines[i] });
    i++;
  }
  while (j < newLines.length) {
    rows.push({ kind: 'add', text: newLines[j] });
    j++;
  }
  return rows;
}
function compactDiffContext(rows: LineDiffRow[], context: number) {
  const changed = rows.map((row, i) => (row.kind === 'same' ? -1 : i)).filter((i) => i >= 0);
  if (changed.length === 0) return rows;
  const keep = new Set();
  for (const idx of changed) {
    const start = Math.max(0, idx - context);
    const end = Math.min(rows.length - 1, idx + context);
    for (let i = start; i <= end; i++) keep.add(i);
  }
  const compact: LineDiffRow[] = [];
  let hidden = 0;
  const flushHidden = () => {
    if (hidden > 0)
      compact.push({ kind: 'skip', text: hidden + ' unchanged line' + (hidden === 1 ? '' : 's') });
    hidden = 0;
  };
  for (let i = 0; i < rows.length; i++) {
    if (keep.has(i) || rows[i].kind !== 'same') {
      flushHidden();
      compact.push(rows[i]);
    } else {
      hidden++;
    }
  }
  flushHidden();
  return compact;
}
export function computeDiffStats(oldMsgs: LogMessage[], newMsgs: LogMessage[]) {
  let added = 0,
    removed = 0,
    modified = 0;
  const ops = buildMessageDiffOps(oldMsgs, newMsgs);
  for (const op of ops) {
    if (op.kind === 'added') added++;
    else if (op.kind === 'removed') removed++;
    else if (op.kind === 'modified') modified++;
  }
  return { added, removed, modified };
}

export function formatLogTimestamp(timestamp: string) {
  const match = timestamp.match(/(?:^|T)(\d{2}:\d{2}:\d{2})(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?$/);
  return match ? match[1] : timestamp;
}
