// Inspector sidebar: connected device, background tasks and shared files.
// Hardware resources live in resources.js.
const screenRefreshMs = 5000;
const taskRefreshMs = 2000;
const fileRefreshMs = 5000;

let inspectorTasks = [];
let inspectorTasksEnabled = false;
let inspectorDraggingTaskId = '';
let inspectorReorderInFlight = false;
let inspectorScreenInFlight = false;
let inspectorScreenUrl = '';
let activeTaskModalId = '';

// Lists are rebuilt on every poll. A rebuilt list would drop keyboard focus,
// so remember which row and which control had it and focus the new copy.
function rememberListFocus(list) {
    const active = document.activeElement;
    const row = active && list.contains(active) ? active.closest('[data-key]') : null;
    if (!row) return null;
    return {key: row.dataset.key, control: active.classList.contains('inspector-row-remove') ? 'remove' : 'main'};
}

function restoreListFocus(list, focus) {
    if (!focus) return;
    const row = Array.from(list.querySelectorAll('[data-key]')).find(node => node.dataset.key === focus.key);
    const target = row && row.querySelector(focus.control === 'remove' ? '.inspector-row-remove' : '.inspector-row-main');
    if (target && !target.disabled) target.focus({preventScroll: true});
}

function inspectorElement(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
}

async function inspectorFetchJSON(url, options) {
    const response = await fetch(url, Object.assign({cache: 'no-store'}, options || {}));
    if (!response.ok) {
        const error = new Error((await response.text()).trim() || response.statusText);
        error.status = response.status;
        throw error;
    }
    return response.json();
}

function refreshInspector() {
    refreshConnectedDevice();
    refreshDeviceScreen();
    refreshAgentTasks();
    refreshSharedFiles();
}

// ---- Connected device ----------------------------------------------------

function deviceDisplayName(status) {
    const env = status.environment || {};
    return env.device_name || [env.brand || env.manufacturer, env.model].filter(Boolean).join(' ') ||
        (status.device_type ? status.device_type + ' device' : 'Phone');
}

function renderConnectedDevice(status) {
    const name = document.getElementById('deviceName');
    const subtitle = document.getElementById('deviceSubtitle');
    const facts = document.getElementById('deviceFacts');
    facts.replaceChildren();
    if (!status || !status.connected) {
        name.textContent = '';
        subtitle.textContent = '';
        return;
    }
    name.textContent = deviceDisplayName(status);
    const env = status.environment || {};
    const battery = env.battery || {};
    subtitle.textContent = 'Connected · ' + ([env.system_name || status.platform, env.system_version].filter(Boolean).join(' ') || 'phone');
    const rows = [
        ['Battery', Number.isFinite(battery.level)
            ? Math.round(battery.level <= 1 ? battery.level * 100 : battery.level) + '%' + (battery.charging ? ' · charging' : '')
            : ''],
        ['App', status.app_state || ''],
        ['Control', status.pointer_mode || ''],
    ];
    for (const [label, value] of rows) {
        if (!value) continue;
        const pair = inspectorElement('div', 'device-fact');
        pair.append(inspectorElement('dt', '', label), inspectorElement('dd', '', value));
        facts.append(pair);
    }
}

async function refreshConnectedDevice() {
    if (document.hidden) return;
    try {
        renderConnectedDevice(await inspectorFetchJSON('/api/phone-bridge/status'));
    } catch (_) {
        renderConnectedDevice(null);
    }
}

function setDeviceScreen(state, caption) {
    const figure = document.getElementById('deviceScreen');
    figure.dataset.state = state;
    document.getElementById('deviceScreenCaption').textContent = caption;
    document.getElementById('deviceScreenImage').hidden = state !== 'live';
}

async function refreshDeviceScreen() {
    if (document.hidden || inspectorScreenInFlight) return;
    inspectorScreenInFlight = true;
    try {
        const result = await inspectorFetchJSON('/api/providers/screenshot', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({format: 'jpeg', quality: 55, crop_black: true}),
        });
        const data = result && result.data;
        if (!data || !data.image) throw new Error('No frame');
        const bytes = Uint8Array.from(atob(data.image), c => c.charCodeAt(0));
        const url = URL.createObjectURL(new Blob([bytes], {type: 'image/jpeg'}));
        const image = document.getElementById('deviceScreenImage');
        image.src = url;
        if (inspectorScreenUrl) URL.revokeObjectURL(inspectorScreenUrl);
        inspectorScreenUrl = url;
        setDeviceScreen('live', data.meta && data.meta.stale ? 'Stale frame' : 'Updated ' + formatTime(new Date().toISOString()));
    } catch (_) {
        setDeviceScreen('empty', 'No screen');
    } finally {
        inspectorScreenInFlight = false;
    }
}

// ---- Background tasks ----------------------------------------------------

function taskStatusLabel(task) {
    if (task.status === 'running' && task.pending_user_action) return 'Needs you';
    return {
        created: 'Queued', queued: 'Queued', running: 'Working', cancelling: 'Cancelling',
        completed: 'Done', failed: 'Failed', cancelled: 'Cancelled',
    }[task.status] || task.status;
}

function taskTitle(task) {
    const firstLine = String(task.prompt || '').trim().split('\n')[0];
    return firstLine.length > 80 ? firstLine.slice(0, 79) + '…' : firstLine || task.id;
}

function queuedTaskIds() {
    return inspectorTasks.filter(task => task.status === 'queued').map(task => task.id);
}

function renderAgentTasks() {
    const list = document.getElementById('taskList');
    const empty = document.getElementById('taskEmpty');
    const focus = rememberListFocus(list);
    list.replaceChildren();
    const queued = queuedTaskIds();
    for (const task of inspectorTasks) {
        const item = inspectorElement('li', 'inspector-row task-row');
        item.dataset.taskId = task.id;
        item.dataset.key = task.id;
        item.dataset.status = task.pending_user_action ? 'action' : task.status;
        const open = inspectorElement('button', 'inspector-row-main', '');
        open.type = 'button';
        open.title = 'Show task details';
        open.append(inspectorElement('span', 'inspector-row-name', taskTitle(task)),
            inspectorElement('span', 'task-status', taskStatusLabel(task)));
        open.addEventListener('click', () => openTaskModal(task.id));
        const cancel = inspectorElement('button', 'inspector-row-remove', '×');
        cancel.type = 'button';
        cancel.title = 'Cancel task';
        cancel.setAttribute('aria-label', 'Cancel ' + taskTitle(task));
        cancel.disabled = task.status === 'cancelling';
        cancel.addEventListener('click', () => cancelAgentTask(task));
        item.append(open, cancel);
        if (task.status === 'queued' && queued.length > 1) {
            item.draggable = true;
            item.classList.add('draggable');
            item.setAttribute('aria-roledescription', 'Reorderable task');
            item.addEventListener('dragstart', handleTaskDragStart);
            item.addEventListener('dragend', handleTaskDragEnd);
            open.addEventListener('keydown', handleTaskReorderKey);
        }
        list.append(item);
    }
    empty.hidden = inspectorTasks.length > 0;
    empty.textContent = inspectorTasksEnabled ? 'No background tasks.' : 'Background tasks run in Realtime mode.';
    document.getElementById('taskHint').hidden = queued.length < 2;
    restoreListFocus(list, focus);
    renderInspectorSummary();
}

function renderInspectorSummary() {
    const running = inspectorTasks.filter(task => task.status === 'running' || task.status === 'cancelling').length;
    const queued = inspectorTasks.length - running;
    const parts = [];
    if (running) parts.push(running + ' working');
    if (queued) parts.push(queued + ' queued');
    document.getElementById('inspectorSummary').textContent = parts.join(' · ');
}

async function refreshAgentTasks() {
    if (document.hidden || inspectorDraggingTaskId || inspectorReorderInFlight) return;
    try {
        const result = await inspectorFetchJSON('/api/agent-tasks');
        inspectorTasksEnabled = result.enabled === true;
        inspectorTasks = Array.isArray(result.tasks) ? result.tasks : [];
    } catch (_) {
        inspectorTasksEnabled = false;
        inspectorTasks = [];
    }
    if (!inspectorDraggingTaskId) renderAgentTasks();
}

async function cancelAgentTask(task) {
    if (task.status === 'running' && !window.confirm('Stop "' + taskTitle(task) + '"? Steps it already completed are not undone.')) return;
    try {
        await inspectorFetchJSON('/api/agent-tasks/' + encodeURIComponent(task.id), {method: 'DELETE'});
    } catch (err) {
        console.error('Failed to cancel task:', err);
        showErrorToast('Cancel task failed', err);
    }
    refreshAgentTasks();
}

async function submitTaskOrder(order) {
    inspectorReorderInFlight = true;
    try {
        const result = await inspectorFetchJSON('/api/agent-tasks/order', {
            method: 'PUT',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({task_ids: order}),
        });
        inspectorTasks = Array.isArray(result.tasks) ? result.tasks : inspectorTasks;
    } catch (err) {
        // 409 means a task started or finished meanwhile; the refresh shows the real order.
        if (err.status === 409) {
            showToast('The queue changed before your reorder was saved.', {kind: 'info'});
        } else {
            console.error('Failed to reorder tasks:', err);
            showErrorToast('Reorder failed', err);
        }
    } finally {
        inspectorReorderInFlight = false;
    }
    await refreshAgentTasks();
}

function handleTaskDragStart(event) {
    inspectorDraggingTaskId = event.currentTarget.dataset.taskId;
    event.currentTarget.classList.add('dragging');
    event.dataTransfer.effectAllowed = 'move';
    event.dataTransfer.setData('text/plain', inspectorDraggingTaskId);
}

function handleTaskDragOver(event) {
    if (!inspectorDraggingTaskId) return;
    const list = document.getElementById('taskList');
    const dragging = list.querySelector('.dragging');
    const target = event.target.closest && event.target.closest('.task-row.draggable');
    if (!dragging || !target || target === dragging) {
        event.preventDefault();
        return;
    }
    event.preventDefault();
    const box = target.getBoundingClientRect();
    const after = event.clientY > box.top + box.height / 2;
    list.insertBefore(dragging, after ? target.nextSibling : target);
}

function handleTaskDragEnd(event) {
    event.currentTarget.classList.remove('dragging');
    const order = Array.from(document.querySelectorAll('#taskList .task-row.draggable'), node => node.dataset.taskId);
    const changed = order.join('\n') !== queuedTaskIds().join('\n');
    inspectorDraggingTaskId = '';
    if (changed) submitTaskOrder(order);
    else renderAgentTasks();
}

// Alt+ArrowUp / Alt+ArrowDown moves a focused queued task without a mouse.
function handleTaskReorderKey(event) {
    if (!event.altKey || (event.key !== 'ArrowUp' && event.key !== 'ArrowDown')) return;
    event.preventDefault();
    const order = queuedTaskIds();
    const id = event.currentTarget.closest('.task-row').dataset.taskId;
    const index = order.indexOf(id);
    const next = index + (event.key === 'ArrowUp' ? -1 : 1);
    if (index < 0 || next < 0 || next >= order.length) return;
    [order[index], order[next]] = [order[next], order[index]];
    submitTaskOrder(order).then(() => {
        const row = document.querySelector('#taskList .task-row[data-task-id="' + CSS.escape(id) + '"] .inspector-row-main');
        if (row) row.focus();
    });
}

// ---- Task details modal --------------------------------------------------

function taskModalEl() {
    return document.getElementById('taskModal');
}

function formatTaskTime(value) {
    if (!value) return '';
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? '' : date.toLocaleTimeString([], {hour: '2-digit', minute: '2-digit', second: '2-digit'});
}

function renderTaskModal(detail) {
    const task = detail.task;
    const timeline = detail.timeline || {steps: []};
    document.getElementById('taskModalStatus').textContent = taskStatusLabel(task) + (task.started_at ? ' · started ' + formatTaskTime(task.started_at) : '');
    document.getElementById('taskModalTitle').textContent = taskTitle(task);
    const body = document.getElementById('taskModalBody');
    body.replaceChildren();

    const goal = inspectorElement('section', 'task-modal-section');
    goal.append(inspectorElement('div', 'message-role', 'Goal'), inspectorElement('p', 'state-detail-copy', task.prompt || ''));
    body.append(goal);

    if (task.pending_user_action) {
        const action = inspectorElement('section', 'task-modal-section task-modal-action');
        action.append(inspectorElement('div', 'message-role', 'Waiting for you'),
            inspectorElement('p', 'state-detail-copy', [task.pending_user_action.reason,
                task.pending_user_action.suggested_action].filter(Boolean).join('\n')));
        body.append(action);
    }

    const steps = inspectorElement('section', 'task-modal-section');
    steps.append(inspectorElement('div', 'message-role', 'Recent steps'));
    const list = inspectorElement('ol', 'task-timeline');
    if (timeline.omitted) list.append(inspectorElement('li', 'task-step omitted', timeline.omitted + ' earlier steps omitted'));
    for (const step of timeline.steps || []) {
        const item = inspectorElement('li', 'task-step');
        item.dataset.kind = step.kind || '';
        item.dataset.status = step.is_error ? 'failed' : step.status || '';
        const head = inspectorElement('div', 'task-step-head');
        head.append(inspectorElement('span', 'task-step-time', formatTaskTime(step.at)),
            inspectorElement('span', 'task-step-title', step.title || step.kind));
        if (step.kind === 'tool' && step.status) head.append(inspectorElement('span', 'task-step-status', step.is_error ? 'failed' : step.status));
        item.append(head);
        if (step.detail) item.append(inspectorElement('pre', 'task-step-detail', step.detail));
        list.append(item);
    }
    if (!list.children.length) list.append(inspectorElement('li', 'task-step omitted', 'No steps yet.'));
    steps.append(list);
    body.append(steps);

    if (task.result || task.error) {
        const result = inspectorElement('section', 'task-modal-section');
        result.append(inspectorElement('div', 'message-role', task.error ? 'Error' : 'Result'),
            inspectorElement('p', 'state-detail-copy', task.error || task.result));
        body.append(result);
    }
}

async function refreshTaskModal() {
    if (!activeTaskModalId || document.hidden) return;
    const id = activeTaskModalId;
    try {
        const detail = await inspectorFetchJSON('/api/agent-tasks/' + encodeURIComponent(id));
        if (id === activeTaskModalId) renderTaskModal(detail);
    } catch (_) {
        if (id !== activeTaskModalId) return;
        document.getElementById('taskModalStatus').textContent = 'Unavailable';
        document.getElementById('taskModalBody').replaceChildren(
            inspectorElement('p', 'inspector-empty', 'This task is no longer available.'));
    }
}

function openTaskModal(taskId) {
    activeTaskModalId = taskId;
    taskModalEl().classList.remove('hidden');
    document.body.classList.add('state-modal-open');
    document.getElementById('taskModalBody').replaceChildren(inspectorElement('p', 'inspector-empty', 'Loading…'));
    refreshTaskModal();
    document.getElementById('taskModalClose').focus();
}

function closeTaskModal() {
    if (!activeTaskModalId) return;
    activeTaskModalId = '';
    taskModalEl().classList.add('hidden');
    document.body.classList.remove('state-modal-open');
}

// ---- Shared files --------------------------------------------------------

function formatFileSize(bytes) {
    return typeof resourceBytes === 'function' ? resourceBytes(bytes) : bytes + ' B';
}

function renderSharedFiles(files) {
    const list = document.getElementById('fileList');
    const focus = rememberListFocus(list);
    list.replaceChildren();
    const entries = files.slice().sort((a, b) => String(b.modified).localeCompare(String(a.modified)));
    for (const file of entries) {
        const item = inspectorElement('li', 'inspector-row file-row');
        item.dataset.key = file.path;
        const directory = file.kind === 'directory';
        const link = inspectorElement(directory ? 'a' : 'button', 'inspector-row-main');
        if (directory) {
            link.href = '/share/' + encodeURIComponent(file.name) + '/';
            link.target = '_blank';
            link.rel = 'noopener noreferrer';
            link.title = 'Open folder';
        } else {
            link.type = 'button';
            link.title = 'Preview · ' + formatFileSize(file.size);
            link.addEventListener('click', () => openFileModal(file));
        }
        link.append(inspectorElement('span', 'inspector-row-name', file.name + (directory ? '/' : '')),
            inspectorElement('span', 'file-source', directory ? 'Folder'
                : file.shared_by === 'user' ? 'Shared by you' : 'Shared by Aico'));
        item.append(link);
        if (!directory) {
            const remove = inspectorElement('button', 'inspector-row-remove', '×');
            remove.type = 'button';
            remove.title = 'Delete file';
            remove.setAttribute('aria-label', 'Delete ' + file.name);
            remove.addEventListener('click', () => deleteSharedFile(file));
            item.append(remove);
        }
        list.append(item);
    }
    document.getElementById('fileEmpty').hidden = entries.length > 0;
    restoreListFocus(list, focus);
}

async function refreshSharedFiles() {
    if (document.hidden) return;
    try {
        const result = await inspectorFetchJSON('/api/files');
        renderSharedFiles(Array.isArray(result.files) ? result.files : []);
    } catch (_) {
        renderSharedFiles([]);
        document.getElementById('fileEmpty').textContent = 'Shared files are unavailable.';
    }
}

// ---- Shared file preview -------------------------------------------------

// /share serves every file as an opaque download, so previews fetch the bytes
// and render them here. Only formats the browser displays without running
// page code are rendered; HTML, SVG and anything unknown are shown as text or
// as details with the download button.
const previewMaxBytes = 20 << 20;
const previewTextMaxBytes = 512 << 10;
const previewImageTypes = {png: 'image/png', jpg: 'image/jpeg', jpeg: 'image/jpeg', gif: 'image/gif', webp: 'image/webp', bmp: 'image/bmp'};
const previewMediaTypes = {mp4: 'video/mp4', webm: 'video/webm', mov: 'video/mp4', mp3: 'audio/mpeg', wav: 'audio/wav', m4a: 'audio/mp4', ogg: 'audio/ogg'};
const previewTextExtensions = new Set(['txt', 'md', 'markdown', 'log', 'csv', 'tsv', 'json', 'jsonl', 'yaml', 'yml', 'toml',
    'ini', 'conf', 'cfg', 'xml', 'html', 'htm', 'svg', 'css', 'js', 'mjs', 'ts', 'py', 'go', 'rs', 'c', 'h', 'cc', 'cpp',
    'hpp', 'java', 'kt', 'swift', 'sh', 'bash', 'zsh', 'sql', 'diff', 'patch']);
let activeFilePreviewUrl = '';
let activeFilePreviewController = null;

function fileModalEl() {
    return document.getElementById('fileModal');
}

function fileExtension(name) {
    const match = /\.([^.]+)$/.exec(String(name || '').toLowerCase());
    return match ? match[1] : '';
}

function filePreviewKind(file) {
    const ext = fileExtension(file.name);
    if (previewImageTypes[ext]) return 'image';
    if (ext === 'pdf') return 'pdf';
    if (previewMediaTypes[ext]) return previewMediaTypes[ext].startsWith('video/') ? 'video' : 'audio';
    if (previewTextExtensions.has(ext) || String(file.mime_type || '').startsWith('text/')) return 'text';
    return '';
}

function previewMimeType(file, kind) {
    const ext = fileExtension(file.name);
    if (kind === 'image') return previewImageTypes[ext];
    if (kind === 'pdf') return 'application/pdf';
    return previewMediaTypes[ext] || 'application/octet-stream';
}

function setFilePreviewMessage(text) {
    document.getElementById('fileModalBody').replaceChildren(inspectorElement('p', 'file-preview-message', text));
}

async function renderFilePreview(file, signal) {
    const kind = filePreviewKind(file);
    const body = document.getElementById('fileModalBody');
    if (!kind) {
        setFilePreviewMessage('No preview for this file type. Download it to open it.');
        return;
    }
    if (file.size > (kind === 'text' ? previewTextMaxBytes : previewMaxBytes)) {
        setFilePreviewMessage('This file is too large to preview. Download it to open it.');
        return;
    }
    setFilePreviewMessage('Loading preview…');
    const response = await fetch(file.download_url, {signal});
    if (!response.ok) throw new Error(response.status === 404 ? 'This file no longer exists.' : 'Preview failed.');
    if (kind === 'text') {
        const text = await response.text();
        body.replaceChildren(inspectorElement('pre', 'file-preview-text', text));
        return;
    }
    const blob = new Blob([await response.arrayBuffer()], {type: previewMimeType(file, kind)});
    activeFilePreviewUrl = URL.createObjectURL(blob);
    let node;
    if (kind === 'image') {
        node = document.createElement('img');
        node.alt = file.name;
    } else if (kind === 'pdf') {
        node = document.createElement('iframe');
        node.title = file.name;
    } else {
        node = document.createElement(kind);
        node.controls = true;
    }
    node.className = 'file-preview-' + kind;
    node.src = activeFilePreviewUrl;
    body.replaceChildren(node);
}

function openFileModal(file) {
    closeFileModal();
    document.getElementById('fileModalTitle').textContent = file.name;
    document.getElementById('fileModalMeta').textContent = [formatFileSize(file.size),
        file.shared_by === 'user' ? 'Shared by you' : 'Shared by Aico',
        file.modified ? formatTaskTime(file.modified) : ''].filter(Boolean).join(' · ');
    const download = document.getElementById('fileModalDownload');
    download.href = file.download_url;
    download.setAttribute('download', file.name);
    fileModalEl().classList.remove('hidden');
    document.body.classList.add('state-modal-open');
    document.getElementById('fileModalClose').focus();
    const controller = new AbortController();
    activeFilePreviewController = controller;
    renderFilePreview(file, controller.signal).catch(err => {
        if (controller.signal.aborted) return;
        setFilePreviewMessage(err.message || 'Preview failed.');
    });
}

function closeFileModal() {
    if (activeFilePreviewController) activeFilePreviewController.abort();
    activeFilePreviewController = null;
    if (activeFilePreviewUrl) URL.revokeObjectURL(activeFilePreviewUrl);
    activeFilePreviewUrl = '';
    if (fileModalEl().classList.contains('hidden')) return;
    fileModalEl().classList.add('hidden');
    document.getElementById('fileModalBody').replaceChildren();
    if (!activeTaskModalId) document.body.classList.remove('state-modal-open');
}

async function deleteSharedFile(file) {
    if (!window.confirm('Delete "' + file.name + '"? Links to it will stop working.')) return;
    const rel = file.path.replace(/^\/share\/?/, '');
    try {
        const response = await fetch('/api/files?path=' + encodeURIComponent(rel), {method: 'DELETE'});
        if (!response.ok && response.status !== 404) throw new Error(await response.text());
    } catch (err) {
        console.error('Failed to delete file:', err);
        showErrorToast('Delete failed', err);
    }
    refreshSharedFiles();
}

// Keeps Tab and Shift+Tab inside the open task or file dialog, as the
// aria-modal dialogs promise.
function trapDialogFocus(event) {
    const dialog = [taskModalEl(), fileModalEl()].find(el => !el.classList.contains('hidden'));
    if (!dialog) return;
    const focusable = Array.from(dialog.querySelectorAll(
        'button:not([disabled]), a[href], audio[controls], video[controls], iframe, [tabindex]:not([tabindex="-1"])'))
        .filter(node => node.offsetParent !== null || node === document.activeElement);
    if (focusable.length === 0) {
        event.preventDefault();
        return;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (!dialog.contains(document.activeElement)) {
        event.preventDefault();
        first.focus();
    } else if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
    }
}

// ---- Startup -------------------------------------------------------------

function startInspector() {
    document.getElementById('taskList').addEventListener('dragover', handleTaskDragOver);
    document.getElementById('taskModalClose').addEventListener('click', closeTaskModal);
    taskModalEl().addEventListener('click', event => {
        if (event.target === taskModalEl()) closeTaskModal();
    });
    document.getElementById('fileModalClose').addEventListener('click', closeFileModal);
    fileModalEl().addEventListener('click', event => {
        if (event.target === fileModalEl()) closeFileModal();
    });
    document.addEventListener('keydown', event => {
        if (event.key === 'Tab') trapDialogFocus(event);
        if (event.key !== 'Escape') return;
        closeTaskModal();
        closeFileModal();
    });
    document.addEventListener('visibilitychange', () => {
        if (!document.hidden) refreshInspector();
    });
    setInterval(refreshDeviceScreen, screenRefreshMs);
    setInterval(refreshConnectedDevice, screenRefreshMs);
    setInterval(() => { refreshAgentTasks(); refreshTaskModal(); }, taskRefreshMs);
    setInterval(refreshSharedFiles, fileRefreshMs);
    refreshInspector();
}
