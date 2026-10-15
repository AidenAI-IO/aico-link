let state = {files: [], selectedFile: null, groups: [], selectedRequest: null, fileLoadToken: 0, fileLoadAbort: null};
async function request(url, options) {
  const res = await fetch(url, options || {});
  const text = await res.text();
  let body = {};
  try { body = text ? JSON.parse(text) : {}; } catch(err) { body = {ok: false, error: text || err.message}; }
  if (!res.ok) throw new Error(body.error || ('HTTP ' + res.status));
  return body;
}
function setActionStatus(message, isError) {
  const el = document.getElementById('actionStatus');
  if (!el) return;
  el.textContent = message || '';
  el.className = 'action-status' + (isError ? ' error' : '');
}
function updateSelectedFileControls() {
  const title = document.getElementById('detailTitle');
  if (title) title.textContent = state.selectedFile ? ('Request Detail - ' + state.selectedFile.name) : 'Request Detail';
  const btn = document.getElementById('exportRawBtn');
  if (btn) btn.disabled = !state.selectedFile;
}
function isValidLogFileName(name) {
  return /^llm-http-.+\.log$/.test(String(name || '')) && String(name).indexOf('/') === -1 && String(name).indexOf('\\') === -1;
}
async function readErrorText(response) {
  const text = await response.text();
  if (!text) return 'HTTP ' + response.status;
  try {
    const body = JSON.parse(text);
    return body.error || body.message || text;
  } catch(err) {
    return text;
  }
}
function formatBytes(bytes) {
  const value = Number(bytes) || 0;
  if (value < 1024) return value + ' B';
  if (value < 1024 * 1024) return (value / 1024).toFixed(value < 10 * 1024 ? 1 : 0) + ' KB';
  return (value / (1024 * 1024)).toFixed(value < 10 * 1024 * 1024 ? 1 : 0) + ' MB';
}
function formatLogTimestamp(timestamp) {
  const value = String(timestamp || '');
  const match = value.match(/(?:^|T)(\d{2}:\d{2}:\d{2})(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?$/);
  return match ? match[1] : value;
}
function pauseForUi() {
  return new Promise(resolve => setTimeout(resolve, 0));
}
function appendGroupedEntry(groups, pendingGroup, entry) {
  if (!entry || typeof entry !== 'object') return pendingGroup;
  if (entry.kind === 'request') {
    if (pendingGroup) groups.push(pendingGroup);
    return {request: entry, response: null};
  }
  if (entry.kind === 'response' && pendingGroup && !pendingGroup.response) {
    pendingGroup.response = entry;
    groups.push(pendingGroup);
    return null;
  }
  return pendingGroup;
}
function processLogLine(parser, rawLine) {
  const line = String(rawLine == null ? '' : rawLine).trim();
  if (!line) return;
  try {
    const entry = JSON.parse(line);
    parser.pendingGroup = appendGroupedEntry(parser.groups, parser.pendingGroup, entry);
  } catch(err) {
    parser.invalidLines++;
  }
}
function processLogChunk(parser, chunk, isFinal) {
  parser.buffer += chunk || '';
  let newlineIndex = parser.buffer.indexOf('\n');
  while (newlineIndex >= 0) {
    const line = parser.buffer.slice(0, newlineIndex);
    parser.buffer = parser.buffer.slice(newlineIndex + 1);
    processLogLine(parser, line);
    newlineIndex = parser.buffer.indexOf('\n');
  }
  if (isFinal && parser.buffer) {
    processLogLine(parser, parser.buffer);
    parser.buffer = '';
  }
}
async function streamLogEntries(name, onProgress, signal) {
  const response = signal
    ? await fetch('/api/logs/llm/' + encodeURIComponent(name), {signal: signal})
    : await fetch('/api/logs/llm/' + encodeURIComponent(name));
  if (!response.ok) throw new Error(await readErrorText(response));
  const parser = {groups: [], pendingGroup: null, buffer: '', invalidLines: 0};
  const totalBytes = parseInt(response.headers.get('Content-Length') || '0', 10) || 0;
  let loadedBytes = 0;
  if (!response.body || typeof response.body.getReader !== 'function') {
    const text = await response.text();
    loadedBytes = text.length;
    processLogChunk(parser, text, true);
  } else {
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let chunksSinceYield = 0;
    while (true) {
      const part = await reader.read();
      if (part.done) break;
      if (!part.value || part.value.length === 0) continue;
      loadedBytes += part.value.length;
      processLogChunk(parser, decoder.decode(part.value, {stream: true}), false);
      if (onProgress) onProgress(loadedBytes, totalBytes, parser.groups.length);
      chunksSinceYield++;
      if (chunksSinceYield >= 8) {
        chunksSinceYield = 0;
        await pauseForUi();
      }
    }
    processLogChunk(parser, decoder.decode(), true);
  }
  if (parser.pendingGroup) parser.groups.push(parser.pendingGroup);
  if (onProgress) onProgress(totalBytes || loadedBytes, totalBytes || loadedBytes, parser.groups.length);
  return {groups: parser.groups, invalidLines: parser.invalidLines};
}
async function loadFileList(preferredName) {
  try {
    const data = await request('/api/logs/llm');
    state.files = data.files || [];
    const targetName = preferredName || (state.selectedFile ? state.selectedFile.name : '');
    state.selectedFile = targetName ? (state.files.find(f => f.name === targetName) || null) : null;
    renderFileList();
    updateSelectedFileControls();
    if (state.files.length === 0) {
      state.groups = [];
      state.selectedRequest = null;
      document.getElementById('requestList').innerHTML = '<div class="empty" style="padding:12px">No log files available.</div>';
      document.getElementById('requestDetail').innerHTML = '<div class="empty">Select a request to view details.</div>';
      return;
    }
    if (state.selectedFile) {
      const index = state.files.findIndex(f => f.name === state.selectedFile.name);
      if (index >= 0) {
        await loadFile(index);
        return;
      }
    }
    if (state.files.length > 0) {
      await loadFile(0);
    }
  } catch(err) {
    document.getElementById('fileList').innerHTML = '<div class="empty" style="padding:12px">Failed to load: ' + esc(err.message) + '</div>';
    setActionStatus('Failed to refresh log list: ' + err.message, true);
  }
}
function triggerImport() {
  const input = document.getElementById('importFileInput');
  if (input) input.click();
}
function hasFileNamed(name) {
  return state.files.some(f => f && f.name === name);
}
async function handleImportSelection(event) {
  const input = event && event.target ? event.target : document.getElementById('importFileInput');
  const file = input && input.files && input.files[0] ? input.files[0] : null;
  if (!file) return;
  try {
    if (!isValidLogFileName(file.name)) throw new Error('file name must match llm-http-*.log');
    if (hasFileNamed(file.name) && !confirm('A log file named ' + file.name + ' already exists. Replace it?')) {
      setActionStatus('Import cancelled for ' + file.name + '.', false);
      return;
    }
    const payload = await request('/api/logs/llm/' + encodeURIComponent(file.name), {method: 'PUT', headers: {'Content-Type': 'text/plain; charset=utf-8'}, body: file});
    setActionStatus('Imported ' + (payload.name || file.name) + ' (' + (payload.size_bytes || file.size) + ' bytes).', false);
    await loadFileList(file.name);
  } catch(err) {
    setActionStatus('Import failed: ' + err.message, true);
  } finally {
    if (input) input.value = '';
  }
}
async function downloadSelectedFile() {
  if (!state.selectedFile) return;
  try {
    const name = state.selectedFile.name;
    const link = document.createElement('a');
    link.href = '/api/logs/llm/' + encodeURIComponent(name);
    link.download = name;
    document.body.appendChild(link);
    link.click();
    link.remove();
    setActionStatus('Exported ' + name + '.', false);
  } catch(err) {
    setActionStatus('Export failed: ' + err.message, true);
  }
}
function renderFileList() {
  const el = document.getElementById('fileList');
  if (state.files.length === 0) {
    el.innerHTML = '<div class="empty" style="padding:12px">No log files available.</div>';
    return;
  }
  el.innerHTML = state.files.map((f, i) => {
    const active = state.selectedFile && state.selectedFile.name === f.name ? ' active' : '';
    const kb = Math.round((f.size_bytes || 0) / 1024);
    return '<button class="file-row' + active + '" data-action="load-file" data-file-index="' + i + '">' +
           '<div class="file-name">' + esc(f.name) + '</div>' +
           '<div class="file-meta">' + kb + ' KB</div>' +
           '</button>';
  }).join('');
}
async function loadFile(index) {
  const f = state.files[index];
  if (!f) return;
  if (state.fileLoadAbort) state.fileLoadAbort.abort();
  const controller = typeof AbortController === 'function' ? new AbortController() : null;
  const loadToken = state.fileLoadToken + 1;
  state.fileLoadToken = loadToken;
  state.fileLoadAbort = controller;
  state.selectedFile = f;
  state.selectedRequest = null;
  state.groups = [];
  renderFileList();
  updateSelectedFileControls();
  document.getElementById('requestList').innerHTML = '<div class="empty" style="padding:12px">Loading...</div>';
  document.getElementById('requestDetail').innerHTML = '<div class="empty">Select a request to view details.</div>';
  try {
    let lastProgressAt = 0;
    const parsed = await streamLogEntries(f.name, function(loadedBytes, totalBytes, groupCount) {
      if (loadToken !== state.fileLoadToken) return;
      const now = Date.now();
      if (loadedBytes !== totalBytes && now - lastProgressAt < 120) return;
      lastProgressAt = now;
      const progress = totalBytes > 0
        ? formatBytes(loadedBytes) + ' / ' + formatBytes(totalBytes)
        : formatBytes(loadedBytes);
      const suffix = groupCount > 0 ? ' (' + groupCount + ' requests)' : '';
      document.getElementById('requestList').innerHTML = '<div class="empty" style="padding:12px">Loading ' + esc(f.name) + '... ' + esc(progress) + suffix + '</div>';
    }, controller && controller.signal ? controller.signal : null);
    if (loadToken !== state.fileLoadToken) return;
    state.groups = parsed.groups;
    renderGroups();
    if (parsed.invalidLines > 0) {
      setActionStatus('Opened ' + f.name + ' with ' + parsed.invalidLines + ' invalid JSONL lines skipped.', false);
    }
  } catch(err) {
    if (loadToken !== state.fileLoadToken) return;
    if (err && err.name === 'AbortError') return;
    document.getElementById('requestList').innerHTML = '<div class="empty" style="padding:12px">Failed to load: ' + esc(err.message) + '</div>';
    setActionStatus('Failed to open ' + f.name + ': ' + err.message, true);
  } finally {
    if (loadToken === state.fileLoadToken) state.fileLoadAbort = null;
  }
}
function renderGroups() {
  const listEl = document.getElementById('requestList');
  const detailEl = document.getElementById('requestDetail');
  if (state.groups.length === 0) {
    listEl.innerHTML = '<div class="empty" style="padding:12px">No valid requests in this file.</div>';
    detailEl.innerHTML = '<div class="empty">Select a request to view details.</div>';
    return;
  }
  listEl.innerHTML = state.groups.map((g, i) => renderRequestItem(g, i)).join('');
  if (state.selectedRequest === null) {
    state.selectedRequest = 0;
  }
  renderRequestDetail();
}
function renderRequestItem(g, idx) {
  const req = parseBody(g.request.body);
  const messages = requestMessages(req);
  const lastMsg = messages.length > 0 ? messages[messages.length - 1] : null;
  const preview = lastMsg ? (messageText(lastMsg) || JSON.stringify(lastMsg).substring(0, 100)) : 'No message content';
  const status = !g.response ? 'none' : (g.response.status >= 200 && g.response.status < 300 ? 'ok' : 'err');
  const badgeText = !g.response ? 'No Response' : (status === 'ok' ? g.response.status : 'Error ' + g.response.status);
  const badgeClass = 'badge ' + status;
  const active = state.selectedRequest === idx ? ' active' : '';
  let html = '<button class="request-item' + active + '" data-action="select-request" data-request-index="' + idx + '">';
  html += '<div class="request-item-head">';
  html += '<span class="request-item-index">#' + (idx + 1) + '</span>';
  html += '<span class="' + badgeClass + '">' + esc(badgeText) + '</span>';
  html += '<span style="font-size:11px;color:#9ca3af;margin-left:auto">' + esc(formatLogTimestamp(g.request.ts)) + '</span>';
  html += '</div>';
  html += '<div class="request-item-preview">' + esc(preview) + '</div>';
  html += '</button>';
  return html;
}
function selectRequest(idx) {
  state.selectedRequest = idx;
  renderGroups();
}
function renderRequestDetail() {
  const detailEl = document.getElementById('requestDetail');
  if (state.selectedRequest === null || !state.groups[state.selectedRequest]) {
    detailEl.innerHTML = '<div class="empty">Select a request to view details.</div>';
    return;
  }
  const idx = state.selectedRequest;
  const g = state.groups[idx];
  const req = parseBody(g.request.body);
  const status = !g.response ? 'none' : (g.response.status >= 200 && g.response.status < 300 ? 'ok' : 'err');
  const badgeText = !g.response ? 'No Response' : (status === 'ok' ? g.response.status : 'Error ' + g.response.status);
  const badgeClass = 'badge ' + status;
  let html = '<div class="group">';
  html += '<div class="group-head">';
  html += '<span class="group-title">Request #' + (idx + 1) + '</span>';
  html += '<span class="' + badgeClass + '">' + esc(badgeText) + '</span>';
  html += '<span style="font-size:12px;color:#6b7280">' + esc(g.request.ts) + '</span>';
  if (idx > 0) {
    const prevReq = parseBody(state.groups[idx - 1].request.body);
    const diffStats = computeDiffStats(requestMessages(prevReq), requestMessages(req));
    html += '<div class="diff-stats">';
    if (diffStats.added > 0) html += '<div class="diff-stat added"><span class="icon">+</span><span>' + diffStats.added + '</span></div>';
    if (diffStats.removed > 0) html += '<div class="diff-stat removed"><span class="icon">-</span><span>' + diffStats.removed + '</span></div>';
    if (diffStats.modified > 0) html += '<div class="diff-stat modified"><span class="icon">~</span><span>' + diffStats.modified + '</span></div>';
    html += '</div>';
  }
  html += '<div class="group-actions">';
  html += '<button class="button" id="msg-btn" data-action="show-detail" data-detail-view="messages">Messages</button>';
  if (idx > 0) {
    html += '<button class="button active" id="diff-btn" data-action="show-detail" data-detail-view="diff">Diff</button>';
  }
  html += '<button class="button" id="raw-btn" data-action="show-detail" data-detail-view="raw">Raw</button>';
  html += '</div></div>';
  const bodyContent = idx > 0 ? (function() {
    const prevReq = parseBody(state.groups[idx - 1].request.body);
    return renderDiffView(g, prevReq, req);
  })() : renderMessagesView(g, req);
  html += '<div class="group-body" id="detail-body">' + bodyContent + '</div>';
  html += '</div>';
  detailEl.innerHTML = html;
}
function showDetailView(view) {
  const idx = state.selectedRequest;
  if (idx === null) return;
  const g = state.groups[idx];
  if (!g) return;
  document.getElementById('msg-btn').className = view === 'messages' ? 'button active' : 'button';
  const diffBtn = document.getElementById('diff-btn');
  if (diffBtn) diffBtn.className = view === 'diff' ? 'button active' : 'button';
  document.getElementById('raw-btn').className = view === 'raw' ? 'button active' : 'button';
  const body = document.getElementById('detail-body');
  const req = parseBody(g.request.body);
  if (view === 'messages') {
    body.innerHTML = renderMessagesView(g, req);
  } else if (view === 'diff' && idx > 0) {
    const prevReq = parseBody(state.groups[idx - 1].request.body);
    body.innerHTML = renderDiffView(g, prevReq, req);
  } else if (view === 'raw') {
    body.innerHTML = '<pre class="raw">' + esc(JSON.stringify(req, null, 2)) + '</pre>';
  }
}
function renderMessages(msgs, showIndex, idPrefix) {
  showIndex = showIndex !== false;
  idPrefix = idPrefix || 'msg';
  if (!msgs || msgs.length === 0) return '<div style="color:#6b7280;font-size:13px">No messages.</div>';
  return msgs.map((m, i) => {
    const uid = idPrefix + '-' + i;
    let html = '<div class="msg">';
    const contentText = messageText(m);
    const needsCollapse = messageNeedsCollapse(contentText);
    html += renderMessageHead('msg-head role-' + cssToken(m.role || 'unknown'), m.role, showIndex ? i : null, '', needsCollapse ? uid : null);
    html += '<div class="msg-content' + (needsCollapse ? ' collapsed' : '') + '" id="' + uid + '">';
    html += renderMessageContent(m);
    html += renderToolCalls(m);
    html += '</div>';
    html += '</div>';
    return html;
  }).join('');
}
function renderToolCalls(m) {
  if (!m || !m.tool_calls || m.tool_calls.length === 0) return '';
  return m.tool_calls.map(tc => {
    const fn = (tc && tc.function) || {};
    const rawArgs = fn.arguments == null ? '' : fn.arguments;
    let args = '';
    try { args = JSON.stringify(JSON.parse(rawArgs), null, 2); } catch(e) { args = String(rawArgs); }
    return '<div class="tool-call"><div class="tc-name">🔧 ' + esc(fn.name || '') + '</div><pre style="margin:4px 0 0;font-size:12px">' + esc(args) + '</pre></div>';
  }).join('');
}
function renderMessageContent(m) {
  if (!m) return '';
  let html = '';
  if (m.reasoning_content) {
    html += '<div class="msg-part msg-reasoning"><div class="msg-part-label">Reasoning</div><div class="msg-text">' + esc(m.reasoning_content) + '</div></div>';
  }
  if (typeof m.content === 'string') return html + '<div class="msg-part msg-text">' + esc(m.content) + '</div>';
  if (Array.isArray(m.content)) return html + m.content.map((p, i) => renderContentPart(p, i)).join('');
  if (m.content == null) return html;
  return html + '<div class="msg-part msg-text">' + esc(JSON.stringify(m.content, null, 2)) + '</div>';
}
function renderContentPart(p, index) {
  const url = imageUrlFromPart(p);
  if (url) {
    const label = imageLabelFromUrl(url);
    return '<div class="msg-part"><img src="' + escAttr(url) + '" class="msg-image" alt="Message image ' + (index + 1) + '" loading="lazy"><div class="msg-image-meta">' + esc(label) + '</div></div>';
  }
  if (p && p.type === 'text') return '<div class="msg-part msg-text">' + esc(p.text || '') + '</div>';
  if (p && p.type === 'tool_result') {
    const result = typeof p.content === 'string' ? p.content : JSON.stringify(p.content, null, 2);
    const label = p.tool_use_id ? 'Tool result ' + p.tool_use_id : 'Tool result';
    return '<div class="msg-part tool-call"><div class="tc-name">' + esc(label) + '</div><pre style="margin:4px 0 0;font-size:12px">' + esc(result || '') + '</pre></div>';
  }
  return '<div class="msg-part msg-text">' + esc(contentPartText(p)) + '</div>';
}
function renderMessageHead(headClass, role, index, marker, collapseUid) {
  let html = '<div class="' + headClass + '">';
  if (index !== null && index !== undefined) html += '<span class="msg-index">#' + index + '</span>';
  html += '<span class="msg-role">' + esc(role || 'unknown') + '</span>';
  if (marker) html += '<span class="diff-mark">' + esc(marker) + '</span>';
  if (collapseUid) html += '<span class="msg-head-action"><button class="msg-collapse" title="Toggle collapse" data-action="toggle-message" data-message-id="' + escAttr(collapseUid) + '">↓</button></span>';
  html += '</div>';
  return html;
}
function messageText(m) {
  let contentText = '';
  if (m && m.reasoning_content) contentText = '[Reasoning]\n' + String(m.reasoning_content);
  if (typeof m.content === 'string') {
    contentText += (contentText ? '\n' : '') + m.content;
  } else if (Array.isArray(m.content)) {
    const parts = m.content.map(contentPartText).filter(t => t !== '').join('\n');
    contentText += (contentText && parts ? '\n' : '') + parts;
  } else if (m.content != null) {
    const value = JSON.stringify(m.content, null, 2);
    contentText += (contentText && value ? '\n' : '') + value;
  }
  return contentText;
}
function contentPartText(p) {
  if (!p) return '';
  if (p.type === 'text') return p.text || '';
  const url = imageUrlFromPart(p);
  if (url) return '[Image: ' + imageLabelFromUrl(url) + ']';
  if (p.type === 'input_audio') {
    const format = p.input_audio && p.input_audio.format ? ': ' + p.input_audio.format : '';
    return '[input_audio' + format + ']';
  }
  if (p.type === 'tool_result') {
    const result = typeof p.content === 'string' ? p.content : JSON.stringify(p.content, null, 2);
    return '[Tool result' + (p.tool_use_id ? ' ' + p.tool_use_id : '') + ']\n' + (result || '');
  }
  return '[' + (p.type || 'unknown') + ']';
}
function imageUrlFromPart(p) {
  if (!p || typeof p !== 'object') return '';
  let url = '';
  if (typeof p.image_url === 'string') url = p.image_url;
  else if (p.image_url && typeof p.image_url.url === 'string') url = p.image_url.url;
  else if (p.source && p.source.type === 'url' && typeof p.source.url === 'string') url = p.source.url;
  else if (p.source && p.source.type === 'base64' && typeof p.source.data === 'string') {
    const mime = String(p.source.media_type || '');
    if (mime.toLowerCase().indexOf('image/') === 0) url = 'data:' + mime + ';base64,' + p.source.data;
  }
  if (!url && typeof p.url === 'string') url = p.url;
  if (!url && typeof p.data === 'string') {
    const mime = imageMimeFromPart(p);
    if (mime) url = 'data:' + mime + ';base64,' + p.data;
  }
  return isRenderableImageDataUrl(url) || isRenderableRemoteImageUrl(url) ? url : '';
}

function normalizeAnthropicContentPart(part) {
  if (!part || typeof part !== 'object') return part;
  if (part.type === 'image' && part.source && part.source.type === 'base64') {
    const mime = String(part.source.media_type || '');
    if (mime.toLowerCase().indexOf('image/') === 0 && typeof part.source.data === 'string') {
      return {type: 'image_url', image_url: {url: 'data:' + mime + ';base64,' + part.source.data}};
    }
  }
  return part;
}

function normalizeAnthropicMessage(message) {
  if (!message || !Array.isArray(message.content)) return message;
  const content = [];
  const toolCalls = [];
  let reasoning = '';
  for (const part of message.content) {
    if (part && part.type === 'thinking') {
      reasoning += typeof part.thinking === 'string' ? part.thinking : '';
      continue;
    }
    if (part && part.type === 'tool_use') {
      toolCalls.push({id: part.id, type: 'function', function: {
        name: part.name || '', arguments: JSON.stringify(part.input || {})
      }});
      continue;
    }
    content.push(normalizeAnthropicContentPart(part));
  }
  const normalized = {role: message.role || 'unknown', content: content};
  if (reasoning) normalized.reasoning_content = reasoning;
  if (toolCalls.length > 0) normalized.tool_calls = toolCalls;
  return normalized;
}

function requestMessages(req) {
  if (!req || typeof req !== 'object') return [];
  const hasSystemPrompt = Array.isArray(req.system) || (typeof req.system === 'string' && req.system !== '');
  const isAnthropic = hasSystemPrompt || (req.messages || []).some(function(message) {
    return Array.isArray(message && message.content) && message.content.some(function(part) {
      return part && (part.type === 'tool_use' || part.type === 'tool_result' || (part.type === 'image' && part.source));
    });
  });
  if (!isAnthropic) return req.messages || [];
  const messages = [];
  if (Array.isArray(req.system) && req.system.length > 0) {
    messages.push({role: 'system', content: req.system.map(normalizeAnthropicContentPart)});
  } else if (typeof req.system === 'string' && req.system) {
    messages.push({role: 'system', content: req.system});
  }
  for (const message of req.messages || []) messages.push(normalizeAnthropicMessage(message));
  return messages;
}
function imageMimeFromPart(p) {
  const rawMime = String(p.mime_type || p.mimeType || '');
  if (rawMime.toLowerCase().indexOf('image/') === 0) return rawMime;
  const format = String(p.format || '').toLowerCase();
  if (format === 'jpg') return 'image/jpeg';
  if (format === 'jpeg' || format === 'png' || format === 'gif' || format === 'webp') return 'image/' + format;
  return '';
}
function isRenderableImageDataUrl(url) {
  const lower = String(url || '').toLowerCase();
  return lower.indexOf('data:image/') === 0 && lower.indexOf(';base64,') > 0;
}
function isRenderableRemoteImageUrl(url) {
  try {
    const parsed = new URL(String(url || ''));
    return (parsed.protocol === 'http:' || parsed.protocol === 'https:') && parsed.hostname !== '';
  } catch (_) {
    return false;
  }
}
function imageLabelFromUrl(url) {
  const s = String(url || '');
  const end = s.toLowerCase().indexOf(';base64,');
  if (end > 5) return s.substring(5, end) + ' base64 image';
  if (isRenderableRemoteImageUrl(s)) {
    try {
      const parsed = new URL(s);
      return parsed.pathname.split('/').filter(Boolean).pop() || parsed.hostname || 'remote image';
    } catch (_) {
      return 'remote image';
    }
  }
  return 'image';
}
function messageNeedsCollapse(text) {
  const value = String(text == null ? '' : text);
  return value.split('\n').length > 8 || value.length > 3000;
}
function toggleMsgCollapse(uid, btn) {
  const el = document.getElementById(uid);
  if (!el) return;
  if (el.classList.contains('collapsed')) {
    el.classList.remove('collapsed');
    btn.textContent = '↑';
  } else {
    el.classList.add('collapsed');
    btn.textContent = '↓';
  }
}
function renderMessagesView(g, req) {
  return renderMessages(requestMessages(req), true, 'request-msg') + renderResponseBlock(g);
}
function renderDiffView(g, prevReq, req) {
  return renderDiff(requestMessages(prevReq), requestMessages(req)) + renderResponseBlock(g);
}
function renderResponseBlock(g) {
  let head = '<div class="response-section"><div class="response-section-head"><span>Response</span>';
  if (!g.response) {
    head += '<span class="badge none">No Response</span></div>';
    head += '<div style="color:#6b7280;font-size:13px">No response was recorded for this request.</div></div>';
    return head;
  }
  const status = g.response.status;
  const ok = status >= 200 && status < 300;
  const badgeClass = 'badge ' + (ok ? 'ok' : (status === 0 ? 'none' : 'err'));
  const badgeText = status === 0 ? 'No Status' : (ok ? status : 'Error ' + status);
  head += '<span class="' + badgeClass + '">' + esc(badgeText) + '</span>';
  if (g.response.ts) head += '<span style="font-size:12px;color:#6b7280;font-weight:400">' + esc(g.response.ts) + '</span>';
  head += '</div>';
  const msg = extractResponseMessage(g.response.body);
  let bodyHtml;
  if (msg) {
    bodyHtml = renderMessages([msg], false, 'response-msg');
  } else {
    // Body is not OpenAI-shaped (transport error, empty stream, plain text
    // error from the upstream). Show it raw so failures stay diagnosable.
    const text = String(g.response.body == null ? '' : g.response.body);
    bodyHtml = '<pre class="raw">' + esc(text) + '</pre>';
  }
  return head + bodyHtml + '</div>';
}
function extractResponseMessage(body) {
  // OpenAI-compatible responses come in three shapes in the log:
  //   1. Non-streaming JSON with choices[0].message.
  //   2. SSE stream of `data: {delta: ...}` lines (terminated by [DONE]).
  //   3. Plain error text written by the agent itself (transport error,
  //      `(empty stream response)`, etc.) — caller falls back to raw.
  // Returns a synthetic assistant message {role, content, tool_calls} or null.
  if (!body) return null;
  const text = String(body);
  // Try non-streaming JSON first.
  try {
    const obj = JSON.parse(text);
    if (obj && Array.isArray(obj.choices) && obj.choices.length > 0) {
      const m = obj.choices[0].message || {};
      const result = {role: m.role || 'assistant', content: m.content || '', tool_calls: m.tool_calls || []};
      return addReasoningContent(result, m.reasoning_content);
    }
    if (obj && obj.error) {
      const errStr = typeof obj.error === 'string' ? obj.error : JSON.stringify(obj.error, null, 2);
      return {role: 'assistant', content: 'Error: ' + errStr};
    }
    const isAnthropicMessage = obj && Array.isArray(obj.content) && (
      obj.type === 'message' ||
      (typeof obj.id === 'string' && typeof obj.model === 'string' &&
       Object.prototype.hasOwnProperty.call(obj, 'stop_reason') && obj.usage && typeof obj.usage === 'object')
    );
    if (isAnthropicMessage) {
      let content = '';
      let reasoning = '';
      const toolCalls = [];
      for (const block of obj.content) {
        if (!block) continue;
        if (block.type === 'text' && typeof block.text === 'string') content += block.text;
        if (block.type === 'thinking' && typeof block.thinking === 'string') reasoning += block.thinking;
        if (block.type === 'tool_use') {
          toolCalls.push({id: block.id, type: 'function', function: {
            name: block.name || '', arguments: JSON.stringify(block.input || {})
          }});
        }
      }
      return addReasoningContent({role: obj.role || 'assistant', content: content, tool_calls: toolCalls}, reasoning);
    }
    if (obj && Array.isArray(obj.output)) {
      return extractResponsesMessage(obj);
    }
  } catch(e) {}
  // Try SSE stream — accumulate content and tool_calls across `data:` events.
  const lines = text.split('\n');
  let hasData = false;
  let content = '';
  let reasoning = '';
  let sawResponsesContentDelta = false;
  let sawResponsesReasoningDelta = false;
  const toolCalls = {};
  for (const rawLine of lines) {
    const line = rawLine.trim();
    if (!line.startsWith('data:')) continue;
    const data = line.substring(5).trim();
    if (!data || data === '[DONE]') continue;
    let event;
	try { event = JSON.parse(data); } catch(e) { continue; }
	hasData = true;
	if (event.type === 'error' && event.error && typeof event.error.message === 'string') {
	  if (content) content += '\n';
	  content += event.error.message;
	  continue;
	}
	if (event.type === 'content_block_start' && event.content_block && event.content_block.type === 'tool_use') {
      const block = event.content_block;
      toolCalls[event.index] = {id: block.id, type: 'function', function: {name: block.name || '', arguments: ''}};
      continue;
    }
    if (event.type === 'response.output_text.delta' && typeof event.delta === 'string') {
      content += event.delta;
      sawResponsesContentDelta = true;
      continue;
    }
    if ((event.type === 'response.reasoning_summary_text.delta' || event.type === 'response.reasoning_text.delta') && typeof event.delta === 'string') {
      reasoning += event.delta;
      sawResponsesReasoningDelta = true;
      continue;
    }
    if (event.type === 'response.output_item.done' && event.item) {
      const item = event.item;
      if (item.type === 'reasoning' && !sawResponsesReasoningDelta) reasoning += responseItemText(item.summary || item.content || item.text);
      if (item.type === 'message' && !sawResponsesContentDelta) content += responseItemText(item.content);
      if (item.type === 'function_call') {
        const k = item.call_id || item.id || Object.keys(toolCalls).length;
        toolCalls[k] = {id: item.call_id || item.id, type: 'function', function: {name: item.name || '', arguments: item.arguments || '{}'}};
      }
      continue;
    }
    if (event.type === 'content_block_delta' && event.delta) {
      if (event.delta.type === 'text_delta' && typeof event.delta.text === 'string') content += event.delta.text;
      if ((event.delta.type === 'thinking_delta' || event.delta.type === 'reasoning_delta') && typeof event.delta.thinking === 'string') reasoning += event.delta.thinking;
      if (event.delta.type === 'input_json_delta') {
        const k = event.index;
        if (toolCalls[k]) toolCalls[k].function.arguments += event.delta.partial_json || '';
      }
      continue;
    }
    const choice = (event.choices && event.choices[0]) || null;
    if (!choice) continue;
    const delta = choice.delta || choice.message || {};
    if (typeof delta.reasoning_content === 'string') reasoning += delta.reasoning_content;
    if (typeof delta.content === 'string') content += delta.content;
    else if (Array.isArray(delta.content)) {
      for (const p of delta.content) {
        if (p && p.type === 'text' && typeof p.text === 'string') content += p.text;
      }
    }
    if (Array.isArray(delta.tool_calls)) {
      for (const tc of delta.tool_calls) {
        const k = tc.index != null ? tc.index : (tc.id || Object.keys(toolCalls).length);
        if (!toolCalls[k]) toolCalls[k] = {type: 'function', function: {name: '', arguments: ''}};
        if (tc.id) toolCalls[k].id = tc.id;
        if (tc.function) {
          if (tc.function.name) toolCalls[k].function.name += tc.function.name;
          if (tc.function.arguments) toolCalls[k].function.arguments += tc.function.arguments;
        }
      }
    }
  }
  if (!hasData) return null;
  const tcArr = Object.keys(toolCalls).sort((a, b) => (+a) - (+b)).map(k => toolCalls[k]);
  return addReasoningContent({role: 'assistant', content: content, tool_calls: tcArr}, reasoning);
}
function responseItemText(value) {
  if (value == null) return '';
  if (typeof value === 'string') return value;
  if (Array.isArray(value)) return value.map(responseItemText).join('');
  if (typeof value === 'object') {
    if (typeof value.text === 'string') return value.text;
    if (typeof value.thinking === 'string') return value.thinking;
    if (value.content != null) return responseItemText(value.content);
    if (value.summary != null) return responseItemText(value.summary);
  }
  return '';
}
function addReasoningContent(message, value) {
  let reasoning = responseItemText(value);
  if (!reasoning && message && typeof message.content === 'string') {
    const tagged = splitTaggedThinking(message.content);
    if (tagged) {
      reasoning = tagged.reasoning;
      message.content = tagged.content;
    }
  }
  if (reasoning) message.reasoning_content = reasoning;
  return message;
}
function splitTaggedThinking(content) {
  let remaining = String(content || '');
  const visible = [];
  const reasoning = [];
  let found = false;
  const openPattern = /<(think|thinking)>/i;
  const closePattern = /<\/(think|thinking)>/i;
  while (remaining) {
    const open = remaining.match(openPattern);
    const close = remaining.match(closePattern);
    const openAt = open ? open.index : -1;
    const closeAt = close ? close.index : -1;
    if (openAt < 0 && closeAt < 0) {
      visible.push(remaining);
      break;
    }
    if (closeAt >= 0 && (openAt < 0 || closeAt < openAt)) {
      reasoning.push(remaining.slice(0, closeAt));
      remaining = remaining.slice(closeAt + close[0].length);
      found = true;
      continue;
    }
    const afterOpen = remaining.slice(openAt + open[0].length);
    const end = afterOpen.match(closePattern);
    if (!end) {
      // An unclosed block cannot be split. Keep the raw text — including the
      // open tag — visible so a truncated response stays readable in the
      // viewer. The runtime does the opposite and hides it, because there a
      // leaked partial thought would reach TTS.
      visible.push(remaining);
      break;
    }
    visible.push(remaining.slice(0, openAt));
    found = true;
    reasoning.push(afterOpen.slice(0, end.index));
    remaining = afterOpen.slice(end.index + end[0].length);
  }
  if (!found) return null;
  return {reasoning: reasoning.map(function(part) { return part.trim(); }).filter(Boolean).join('\n'), content: visible.join('').trim()};
}
function extractResponsesMessage(obj) {
  let content = '';
  let reasoning = '';
  const toolCalls = [];
  for (const item of obj.output || []) {
    if (!item) continue;
    if (item.type === 'reasoning') reasoning += responseItemText(item.summary || item.content || item.text);
    else if (item.type === 'message') content += responseItemText(item.content);
    else if (item.type === 'function_call') {
      toolCalls.push({id: item.call_id || item.id, type: 'function', function: {name: item.name || '', arguments: item.arguments || '{}'}});
    }
  }
  return addReasoningContent({role: 'assistant', content, tool_calls: toolCalls}, reasoning);
}
function renderDiff(oldMsgs, newMsgs) {
  const ops = buildMessageDiffOps(oldMsgs, newMsgs);
  let blockIndex = 0, html = '';
  for (const op of ops) {
    if (op.kind === 'same') html += diffMsgBlock(newMsgs[op.newIndex], 'same', blockIndex++, op.newIndex);
    else if (op.kind === 'removed') html += diffMsgBlock(oldMsgs[op.oldIndex], 'removed', blockIndex++, op.oldIndex);
    else if (op.kind === 'added') html += diffMsgBlock(newMsgs[op.newIndex], 'added', blockIndex++, op.newIndex);
    else if (op.kind === 'modified') html += diffMsgBlock(newMsgs[op.newIndex], 'modified', blockIndex++, op.newIndex, oldMsgs[op.oldIndex]);
  }
  return html || '<div style="color:#6b7280;font-size:13px">No differences.</div>';
}
function buildMessageDiffOps(oldMsgs, newMsgs) {
  // Align whole messages by edit cost so replacements are first-class ops.
  // A modification costs less than delete+add but more than a lone add/delete,
  // preserving anchors around insertions while pairing real same-role edits.
  const a = oldMsgs.map(serializeMsg);
  const b = newMsgs.map(serializeMsg);
  const oldDiffText = oldMsgs.map(messageDiffText);
  const newDiffText = newMsgs.map(messageDiffText);
  const n = a.length, mm = b.length;
  const addCost = 2, removeCost = 2, modifyCost = 3;
  const canLineDiff = (i, j) => oldMsgs[i] && newMsgs[j] && (oldMsgs[i].role || '') === (newMsgs[j].role || '') && oldDiffText[i] !== newDiffText[j];
  const cost = [];
  for (let i = 0; i <= n; i++) { cost.push(new Array(mm + 1).fill(0)); }
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
  let i = 0, j = 0;
  const ops = [];
  while (i < n && j < mm) {
    if (a[i] === b[j] && cost[i][j] === cost[i + 1][j + 1]) {
      ops.push({kind: 'same', oldIndex: i, newIndex: j}); i++; j++;
    } else if (canLineDiff(i, j) && cost[i][j] === cost[i + 1][j + 1] + modifyCost) {
      ops.push({kind: 'modified', oldIndex: i, newIndex: j}); i++; j++;
    } else if (cost[i][j] === cost[i + 1][j] + removeCost) {
      ops.push({kind: 'removed', oldIndex: i}); i++;
    } else {
      ops.push({kind: 'added', newIndex: j}); j++;
    }
  }
  while (i < n) { ops.push({kind: 'removed', oldIndex: i}); i++; }
  while (j < mm) { ops.push({kind: 'added', newIndex: j}); j++; }
  return ops;
}
function serializeMsg(m) {
  let s = (m.role || '') + '\u0001';
  if (typeof m.content === 'string') s += m.content;
  else if (Array.isArray(m.content)) s += m.content.map(serializeContentPart).join('\n');
  else if (m.content != null) { try { s += JSON.stringify(m.content); } catch(e) { s += String(m.content); } }
  if (m.tool_calls) s += '\u0002' + JSON.stringify(m.tool_calls);
  if (m.tool_call_id) s += '\u0003' + m.tool_call_id;
  return s;
}
function serializeContentPart(p) {
  if (!p) return '';
  if (p.type === 'text') return 'text:' + (p.text || '');
  const url = imageUrlFromPart(p);
  if (url) return (p.type || 'image') + ':' + url;
  try { return JSON.stringify(p); } catch(e) { return String(p); }
}
function diffMsgBlock(m, kind, index, messageIndex, oldMsg) {
  const mark = kind === 'added' ? '+' : (kind === 'removed' ? '-' : (kind === 'modified' ? '~' : '='));
  const cls = kind === 'added' ? 'diff-add' : (kind === 'removed' ? 'diff-del' : (kind === 'modified' ? 'diff-mod' : 'diff-same'));
  const text = kind === 'modified' && oldMsg ? messageDiffText(oldMsg) + '\n' + messageDiffText(m) : messageDiffText(m);
  const uid = 'diff-msg-' + index;
  const needsCollapse = messageNeedsCollapse(text);
  let html = '<div class="diff-msg ' + kind + '">';
  html += renderMessageHead('diff-msg-head role-' + cssToken(m.role || 'unknown'), m.role, messageIndex, mark, needsCollapse ? uid : null);
  const content = kind === 'modified' && oldMsg ? renderLineDiffContent(oldMsg, m) : renderMessageContent(m);
  html += '<div class="msg-content diff-line ' + cls + (kind === 'modified' ? ' line-diff' : '') + (needsCollapse ? ' collapsed' : '') + '" id="' + uid + '">' + content + renderToolCalls(m) + '</div>';
  html += '</div>';
  return html;
}
function messageDiffText(m) {
  let text = messageText(m);
  if (m && m.tool_calls && m.tool_calls.length) {
    text += (text ? '\n' : '') + m.tool_calls.map(tc => 'tool_call ' + (tc.function ? tc.function.name : '') + ' ' + (tc.function ? tc.function.arguments : '')).join('\n');
  }
  if (m && m.tool_call_id) text += (text ? '\n' : '') + 'tool_call_id ' + m.tool_call_id;
  return text;
}
function renderLineDiffContent(oldMsg, newMsg) {
  const rows = buildLineDiffRows(messageDiffText(oldMsg), messageDiffText(newMsg));
  return rows.map(row => {
    if (row.kind === 'skip') return '<div class="line-diff-row skip"><span class="line-diff-prefix">...</span><span class="line-diff-text">' + esc(row.text) + '</span></div>';
    const prefix = row.kind === 'add' ? '+' : (row.kind === 'del' ? '-' : ' ');
    return '<div class="line-diff-row ' + row.kind + '"><span class="line-diff-prefix">' + prefix + '</span><span class="line-diff-text">' + esc(row.text) + '</span></div>';
  }).join('');
}
function buildLineDiffRows(oldText, newText) {
  const oldLines = splitDiffLines(oldText);
  const newLines = splitDiffLines(newText);
  let prefix = 0;
  while (prefix < oldLines.length && prefix < newLines.length && oldLines[prefix] === newLines[prefix]) prefix++;
  let suffix = 0;
  while (suffix < oldLines.length - prefix && suffix < newLines.length - prefix && oldLines[oldLines.length - 1 - suffix] === newLines[newLines.length - 1 - suffix]) suffix++;
  const rows = [];
  for (let i = 0; i < prefix; i++) rows.push({kind: 'same', text: oldLines[i]});
  const oldMid = oldLines.slice(prefix, oldLines.length - suffix);
  const newMid = newLines.slice(prefix, newLines.length - suffix);
  rows.push(...buildLcsLineDiffRows(oldMid, newMid));
  for (let i = oldLines.length - suffix; i < oldLines.length; i++) rows.push({kind: 'same', text: oldLines[i]});
  return compactDiffContext(rows, 3);
}
function splitDiffLines(text) {
  const normalized = String(text == null ? '' : text).replace(/\r\n/g, '\n').replace(/\r/g, '\n');
  if (normalized === '') return [''];
  const lines = normalized.split('\n');
  if (lines.length > 1 && lines[lines.length - 1] === '') lines.pop();
  return lines;
}
function buildLcsLineDiffRows(oldLines, newLines) {
  if (oldLines.length === 0) return newLines.map(text => ({kind: 'add', text}));
  if (newLines.length === 0) return oldLines.map(text => ({kind: 'del', text}));
  if (oldLines.length * newLines.length > 200000) {
    return oldLines.map(text => ({kind: 'del', text})).concat(newLines.map(text => ({kind: 'add', text})));
  }
  const lcs = [];
  for (let i = 0; i <= oldLines.length; i++) lcs.push(new Array(newLines.length + 1).fill(0));
  for (let i = oldLines.length - 1; i >= 0; i--) {
    for (let j = newLines.length - 1; j >= 0; j--) {
      lcs[i][j] = oldLines[i] === newLines[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
    }
  }
  const rows = [];
  let i = 0, j = 0;
  while (i < oldLines.length && j < newLines.length) {
    if (oldLines[i] === newLines[j]) { rows.push({kind: 'same', text: oldLines[i]}); i++; j++; }
    else if (lcs[i + 1][j] >= lcs[i][j + 1]) { rows.push({kind: 'del', text: oldLines[i]}); i++; }
    else { rows.push({kind: 'add', text: newLines[j]}); j++; }
  }
  while (i < oldLines.length) { rows.push({kind: 'del', text: oldLines[i]}); i++; }
  while (j < newLines.length) { rows.push({kind: 'add', text: newLines[j]}); j++; }
  return rows;
}
function compactDiffContext(rows, context) {
  const changed = rows.map((row, i) => row.kind === 'same' ? -1 : i).filter(i => i >= 0);
  if (changed.length === 0) return rows;
  const keep = new Set();
  for (const idx of changed) {
    const start = Math.max(0, idx - context);
    const end = Math.min(rows.length - 1, idx + context);
    for (let i = start; i <= end; i++) keep.add(i);
  }
  const compact = [];
  let hidden = 0;
  const flushHidden = () => {
    if (hidden > 0) compact.push({kind: 'skip', text: hidden + ' unchanged line' + (hidden === 1 ? '' : 's')});
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
function parseBody(body) {
  try { return JSON.parse(body); } catch(e) { return {}; }
}
function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}
function escAttr(s) {
  return esc(s);
}
function cssToken(s) {
  return String(s == null ? '' : s).replace(/[^a-zA-Z0-9_-]/g, '_') || 'unknown';
}
function computeDiffStats(oldMsgs, newMsgs) {
  let added = 0, removed = 0, modified = 0;
  const ops = buildMessageDiffOps(oldMsgs, newMsgs);
  for (const op of ops) {
    if (op.kind === 'added') added++;
    else if (op.kind === 'removed') removed++;
    else if (op.kind === 'modified') modified++;
  }
  return {added, removed, modified};
}
document.addEventListener('click', function(event) {
  const target = event.target.closest('[data-action]');
  if (!target) return;
  const action = target.dataset.action;
  if (action === 'refresh-files') loadFileList();
  else if (action === 'trigger-import') triggerImport();
  else if (action === 'export-raw') downloadSelectedFile();
  else if (action === 'load-file') loadFile(Number(target.dataset.fileIndex));
  else if (action === 'select-request') selectRequest(Number(target.dataset.requestIndex));
  else if (action === 'show-detail') showDetailView(target.dataset.detailView);
  else if (action === 'toggle-message') toggleMsgCollapse(target.dataset.messageId, target);
});
document.addEventListener('change', function(event) {
  if (event.target.dataset.action === 'import-file') handleImportSelection(event);
});

loadFileList();
