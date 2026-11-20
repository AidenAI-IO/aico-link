const taskHistoryURL = '/api/agent-tasks/history';

function taskHistoryStatus(task) {
    return { tool_error: 'Tool error', interrupted: 'Interrupted' }[task.status] || taskStatusLabel(task);
}

function taskHistoryDuration(ms) {
    if (!Number.isFinite(ms) || ms < 0) return '—';
    if (ms > 0 && ms < 1000) return '<1s';
    const seconds = Math.floor(ms / 1000);
    const hours = Math.floor(seconds / 3600);
    const minutes = Math.floor(seconds % 3600 / 60);
    return [hours ? hours + 'h' : '', minutes ? minutes + 'm' : '', seconds % 60 + 's'].filter(Boolean).join(' ');
}

function taskHistoryDate(value) {
    const date = new Date(value);
    if (!value || Number.isNaN(date.getTime()) || date.getFullYear() < 1970) return '—';
    const time = date.toLocaleTimeString([], {hour: '2-digit', minute: '2-digit'});
    if (date.toDateString() === new Date().toDateString()) return time;
    return date.toLocaleDateString([], {month: 'short', day: 'numeric'}) + ' · ' + time;
}

function taskHistoryTokens(value) {
    return Number.isFinite(value) ? value.toLocaleString() : '—';
}

function taskHistoryCacheHitRate(input, cached) {
    if (!Number.isFinite(input) || input <= 0 || !Number.isFinite(cached)) return '—';
    return (cached / input).toLocaleString(undefined, {style: 'percent', maximumFractionDigits: 2});
}

function taskHistoryCost(cost) {
    if (!cost || !Number.isFinite(cost.usd)) return '—';
    const value = cost.usd > 0 && cost.usd < 0.00001 ? '<$0.00001' : '$' + cost.usd.toFixed(5);
    return value + (cost.partial ? '+' : '');
}

function renderTaskHistoryDetail(body, task) {
    body.replaceChildren();
    if (task.error || task.result) {
        const outcome = inspectorElement('p', 'task-history-outcome', task.error || task.result);
        outcome.dataset.status = task.status;
        body.append(outcome);
    }
    const metrics = inspectorElement('dl', 'task-history-metrics');
    const costSource = {provider: 'Provider reported', telemetry: 'Telemetry estimate', catalog: 'Catalog estimate', mixed: 'Mixed sources'};
    const costCaption = [costSource[task.cost?.source], task.cost?.partial ? 'Partial amount' : ''].filter(Boolean).join(' · ');
    const entries = [
        ['Duration', taskHistoryDuration(task.duration_ms), 'Execution ' + taskHistoryDuration(task.execution_ms)],
        ['Backend tokens', taskHistoryTokens(task.tokens?.total)],
        ['Cost', taskHistoryCost(task.cost), costCaption],
    ];
    for (const [label, value, caption] of entries) {
        const metric = inspectorElement('div', '');
        metric.append(inspectorElement('dt', '', label), inspectorElement('dd', '', value));
        if (caption) metric.append(inspectorElement('small', '', caption));
        metrics.append(metric);
    }
    body.append(metrics);
    if (task.pricing?.length) {
        const rates = inspectorElement('div', 'task-history-usage');
        for (const price of task.pricing) rates.append(inspectorElement('span', '', price));
        body.append(rates);
    }
    if (task.tokens) {
        const usage = inspectorElement('div', 'task-history-usage');
        for (const [label, value] of [
            ['Cached input', task.tokens.cached_input], ['Uncached input', task.tokens.input - task.tokens.cached_input],
            ['Output', task.tokens.output],
        ]) {
            const item = inspectorElement('span', '', label + ' ');
            item.append(inspectorElement('strong', '', taskHistoryTokens(value)));
            usage.append(item);
        }
        body.append(usage);
    }
    body.append(renderTaskTimeline({
        omitted: task.omitted,
        steps: (task.steps || []).map(step => ({
            kind: 'tool', title: step.tool, at: step.at, status: step.status, detail: step.detail,
        })),
    }, 'Agent call path'));
}

function startTaskHistory() {
    const get = id => document.getElementById('taskHistory' + id);
    const dialog = get('Dialog');
    const title = get('Title');
    const metadata = get('Metadata');
    const list = get('ListView');
    const detail = get('Detail');
    const rows = get('Rows');
    const table = get('Table');
    const columnLabels = Array.from(table.querySelectorAll('th'), header => header.textContent);
    const notice = get('Notice');
    const more = get('More');
    const back = get('Back');
    let controller;
    let cursor = '';
    let selectedButton;

    function setNotice(message) {
        notice.textContent = message;
        notice.classList.toggle('hidden', !message);
    }

    async function openDetail(task, button) {
        selectedButton = button;
        list.classList.add('hidden');
        detail.classList.remove('hidden');
        back.classList.remove('hidden');
        metadata.classList.remove('hidden');
        title.textContent = taskTitle(task);
        metadata.textContent = taskHistoryStatus(task);
        detail.replaceChildren(inspectorElement('p', 'inspector-empty', 'Loading…'));
        back.focus();
        const signal = controller.signal;
        try {
            const response = await inspectorFetchJSON(taskHistoryURL + '/' + encodeURIComponent(task.id), {signal});
            if (signal.aborted || selectedButton !== button) return;
            metadata.textContent = [taskHistoryStatus(response), formatTaskTime(response.started_at),
                ...(response.providers || []), ...(response.models || [])].filter(Boolean).join(' · ');
            renderTaskHistoryDetail(detail, response);
        } catch (_) {
            if (!signal.aborted && selectedButton === button) {
                detail.replaceChildren(inspectorElement('p', 'inspector-empty', 'Task details are unavailable.'));
            }
        }
    }

    function appendRow(task) {
        const row = inspectorElement('tr', 'task-history-row');
        row.setAttribute('role', 'row');
        row.dataset.status = task.status;
        const titleCell = inspectorElement('td', '');
        titleCell.setAttribute('role', 'cell');
        const button = inspectorElement('button', 'inspector-row-main', taskTitle(task));
        button.type = 'button';
        titleCell.append(button);
        const status = inspectorElement('td', '');
        status.setAttribute('role', 'cell');
        status.append(inspectorElement('span', 'task-status', taskHistoryStatus(task)));
        const {input, cached_input: cached, output, total} = task.tokens || {};
        row.append(titleCell, status);
        const values = [
            taskHistoryDate(task.started_at),
            taskHistoryDuration(task.duration_ms), taskHistoryTokens(total), taskHistoryTokens(cached),
            taskHistoryTokens(input - cached), taskHistoryTokens(output), taskHistoryCacheHitRate(input, cached),
            taskHistoryCost(task.cost),
        ];
        for (const [index, value] of values.entries()) {
            const cell = inspectorElement('td', '', value);
            cell.setAttribute('role', 'cell');
            const label = inspectorElement('span', 'task-history-cell-label', columnLabels[index + 2]);
            label.setAttribute('aria-hidden', 'true');
            cell.prepend(label);
            row.append(cell);
        }
        row.addEventListener('click', () => { void openDetail(task, button); });
        rows.append(row);
    }

    async function loadPage() {
        more.disabled = true;
        const signal = controller.signal;
        try {
            const url = taskHistoryURL + (cursor ? '?before=' + encodeURIComponent(cursor) : '');
            const response = await inspectorFetchJSON(url, {signal});
            if (signal.aborted) return;
            for (const task of response.tasks || []) {
                appendRow(task);
            }
            cursor = response.next_cursor || '';
            table.classList.toggle('hidden', !rows.children.length);
            more.classList.toggle('hidden', !cursor);
            setNotice(rows.children.length ? '' : 'No task history.');
        } catch (_) {
            if (!signal.aborted) setNotice('Task history is unavailable.');
        } finally {
            if (!signal.aborted) more.disabled = false;
        }
    }

    get('Open').addEventListener('click', () => {
        controller?.abort();
        controller = new AbortController();
        cursor = '';
        selectedButton = undefined;
        rows.replaceChildren();
        table.classList.add('hidden');
        more.classList.add('hidden');
        back.classList.add('hidden');
        metadata.classList.add('hidden');
        detail.classList.add('hidden');
        list.classList.remove('hidden');
        title.textContent = 'Task history';
        setNotice('Loading…');
        dialog.showModal();
        void loadPage();
    });
    more.addEventListener('click', () => { void loadPage(); });
    back.addEventListener('click', () => {
        detail.classList.add('hidden');
        list.classList.remove('hidden');
        back.classList.add('hidden');
        metadata.classList.add('hidden');
        title.textContent = 'Task history';
        const button = selectedButton;
        selectedButton = undefined;
        button?.focus({preventScroll: true});
    });
    dialog.addEventListener('close', () => {
        // A close event can arrive after the dialog has already reopened.
        if (dialog.open) return;
        controller?.abort();
        get('Open').focus({preventScroll: true});
    });
    dialog.addEventListener('keydown', event => {
        if (event.key === 'Escape') event.stopPropagation();
    });
}
