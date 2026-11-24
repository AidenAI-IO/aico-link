(() => {
    const el = (id) => document.getElementById(id);
    const tabs = [...document.querySelectorAll('.debug-tabs [role="tab"]')];
    const status = el('utgStatus');
    const refresh = el('refreshUTG');
    const query = el('utgQuery');
    const intent = el('utgIntent');
    let snapshot;

    function appendRow(body, values, codeColumns = []) {
        const row = body.insertRow();
        values.forEach((value, i) => {
            const cell = row.insertCell();
            const target = codeColumns.includes(i) ? cell.appendChild(document.createElement('code')) : cell;
            target.textContent = value;
        });
    }

    function renderTables() {
        if (!snapshot) return;
        const term = query.value.trim().toLowerCase();
        const matches = (...values) => !term || values.some((value) => (value || '').toLowerCase().includes(term));
        const searchableNodes = new Set(snapshot.nodes.filter((node) => matches(node.label, node.summary, node.pitfall)).map((node) => node.label));
        const edges = snapshot.edges.filter((edge) => (
            (!intent.value || edge.intent === intent.value) &&
            (matches(edge.from, edge.to, edge.guide) || searchableNodes.has(edge.from) || searchableNodes.has(edge.to))
        ));
        const labels = new Set(edges.flatMap((edge) => [edge.from, edge.to]));
        const nodes = snapshot.nodes.filter((node) => labels.has(node.label) || (!intent.value && searchableNodes.has(node.label)));
        const nodeBody = el('utgNodes');
        const edgeBody = el('utgEdges');
        nodeBody.replaceChildren();
        edgeBody.replaceChildren();
        for (const n of nodes) appendRow(nodeBody, [n.label, n.summary, n.samples, n.pitfall || '—'], [0]);
        for (const e of edges) appendRow(edgeBody, [e.from, e.to, e.intent, e.successes, e.guide], [0, 1, 2]);
        el('utgNodeStatus').textContent = nodes.length ? `${nodes.length} of ${snapshot.nodes.length}` : 'No matching landmarks.';
        el('utgEdgeStatus').textContent = edges.length ? `${edges.length} of ${snapshot.edges.length}` : 'No matching transitions.';
    }

    function renderSnapshot(data) {
        snapshot = data;
        const populated = data.nodes.length > 0 || data.edges.length > 0;
        el('utgContent').hidden = false;
        el('utgPlatform').textContent = data.platform;
        el('utgNodeCount').textContent = data.nodes.length;
        el('utgEdgeCount').textContent = data.edges.length;
        el('utgMatch').textContent = data.current_label || (data.has_recent_frame ? 'No confident match' : 'No recent frame');
        const note = !data.available ? 'Persistent UTG is not available for this runtime.' :
            populated ? '' : 'No learned landmarks yet. The Agent can learn after a completed visual task.';
        status.textContent = [data.enabled ? 'ConAct-UTG enabled.' : 'ConAct-UTG disabled.', note].filter(Boolean).join(' ');
        const selectedIntent = intent.value;
        intent.replaceChildren(new Option('All intents', ''));
        for (const value of data.intents) intent.add(new Option(value, value));
        if (data.intents.includes(selectedIntent)) intent.value = selectedIntent;
        el('utgFilters').hidden = !populated;
        el('utgLandmarks').hidden = !populated;
        el('utgTransitions').hidden = !populated;
        el('utgTipCard').hidden = !data.tips.length;
        el('utgTips').replaceChildren(...data.tips.map((tip) => {
            const li = document.createElement('li');
            li.textContent = tip;
            return li;
        }));
        renderTables();
    }

    async function loadUTG() {
        if (refresh.disabled) return;
        refresh.disabled = true;
        status.textContent = 'Loading UTG…';
        try {
            const response = await fetch('/api/debug/utg', { cache: 'no-store' });
            if (!response.ok) throw new Error(`HTTP ${response.status}`);
            renderSnapshot(await response.json());
        } catch (error) {
            status.textContent = `Could not load UTG (${error.message}).${snapshot ? ' Showing the last successful snapshot.' : ''}`;
        } finally {
            refresh.disabled = false;
        }
    }

    function showTab(name) {
        for (const tab of tabs) {
            const active = tab.dataset.tab === name;
            tab.setAttribute('aria-selected', String(active));
            tab.tabIndex = active ? 0 : -1;
            el(tab.getAttribute('aria-controls')).hidden = !active;
        }
        el('coordinatePanel').dispatchEvent(new Event('debug-tab-change'));
        if (name === 'utg') loadUTG();
    }

    function activate(tab) {
        history.replaceState(null, '', '#' + tab.dataset.tab);
        showTab(tab.dataset.tab);
    }

    tabs.forEach((tab, i) => {
        tab.addEventListener('click', () => activate(tab));
        tab.addEventListener('keydown', (event) => {
            const offsets = { ArrowLeft: -1, ArrowRight: 1 };
            const next = event.key === 'Home' ? tabs[0] : event.key === 'End' ? tabs[tabs.length - 1] :
                offsets[event.key] ? tabs[(i + offsets[event.key] + tabs.length) % tabs.length] : null;
            if (!next) return;
            event.preventDefault();
            next.focus();
            activate(next);
        });
    });
    refresh.addEventListener('click', loadUTG);
    query.addEventListener('input', renderTables);
    intent.addEventListener('change', renderTables);
    const syncHash = () => showTab(location.hash === '#utg' ? 'utg' : 'coordinate');
    window.addEventListener('hashchange', syncHash);
    syncHash();
})();
