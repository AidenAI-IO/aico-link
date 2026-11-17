let previousCoreServicesSnapshot = null;
let coreServicesRequestInFlight = false;
let coreIncidentsRequestInFlight = false;
let selectedCoreIncidentUnit = '';
const coreIncidentEventsByUnit = new Map();
const coreIncidentLogCache = new Map();

const CORE_SERVICES_POLL_MS = 5000;

const CORE_SERVICE_LABELS = {
    'aico-agent.service': 'Agent',
    'aico-frame.service': 'Frame',
    'aico-audio.service': 'Audio',
    'aico-ble.service': 'BLE',
    'aico-config-web.service': 'Config Web',
    'aico-mcu-event.service': 'MCU',
    'aico-wifi-proxy.service': 'Wi-Fi Proxy',
    'aico-ttyd.service': 'Terminal',
};

function coreServiceBytes(value) {
    if (!Number.isFinite(value) || value < 0) return '—';
    const units = ['B', 'KiB', 'MiB', 'GiB'];
    let unit = 0;
    while (value >= 1024 && unit < units.length - 1) {
        value /= 1024;
        unit += 1;
    }
    return value.toFixed(unit === 0 ? 0 : 1) + ' ' + units[unit];
}

function coreServiceCPUPercent(service, previous, seconds) {
    const current = service?.proc?.cpu_total;
    const before = previous?.proc?.cpu_total;
    if (!Number.isFinite(current)) return null;
    if (!Number.isFinite(before) || seconds <= 0 || current < before) return null;
    // cpu_total is in clock ticks; assume USER_HZ=100 like the Linux default.
    const percent = 100 * ((current - before) / 100) / seconds;
    if (!Number.isFinite(percent) || percent < 0) return null;
    return Math.min(percent, 999);
}

function coreServiceSeverityLabel(severity) {
    switch (severity) {
        case 'failed': return 'Failed';
        case 'degraded': return 'Watch';
        default: return 'OK';
    }
}

function coreServiceLabelForUnit(unit) {
    if (!unit) return '';
    if (CORE_SERVICE_LABELS[unit]) return CORE_SERVICE_LABELS[unit];
    return String(unit).replace(/\.service$/, '').replace(/^aico-/, '');
}

function formatCoreIncidentTime(ts) {
    if (!ts) return '';
    const date = new Date(ts);
    if (Number.isNaN(date.getTime())) return String(ts);
    // Keep English digits/order regardless of browser locale.
    const pad = value => String(value).padStart(2, '0');
    return date.getUTCFullYear()
        + '-' + pad(date.getUTCMonth() + 1)
        + '-' + pad(date.getUTCDate())
        + ' ' + pad(date.getUTCHours())
        + ':' + pad(date.getUTCMinutes())
        + ':' + pad(date.getUTCSeconds())
        + ' UTC';
}

function formatCoreIncidentBoard(board) {
    if (!board || typeof board !== 'object') return '';
    const parts = [];
    if (Array.isArray(board.loadavg) && board.loadavg.length) {
        parts.push('load ' + board.loadavg.map(v => Number(v).toFixed(2)).join('/'));
    }
    if (Number.isFinite(board.cpu_percent)) {
        parts.push('cpu ' + Math.round(board.cpu_percent) + '%');
    }
    if (Number.isFinite(board.mem_available_kb) && Number.isFinite(board.mem_total_kb) && board.mem_total_kb > 0) {
        parts.push('mem free ' + coreServiceBytes(board.mem_available_kb * 1024));
    }
    return parts.join(' · ');
}

function coreIncidentReason(event) {
    if (!event) return '';
    if (event.result && event.result !== 'success') return event.result;
    if (event.active_state && event.active_state !== 'active') return event.active_state;
    return coreServiceSeverityLabel(event.severity);
}

function formatCoreIncidentExit(event) {
    if (!event || event.exec_main_status == null || event.exec_main_status === '') return '';
    const code = Number(event.exec_main_status);
    if (!Number.isFinite(code)) return 'exit ' + event.exec_main_status;
    const signalNames = {
        1: 'SIGHUP',
        2: 'SIGINT',
        9: 'SIGKILL',
        15: 'SIGTERM',
    };
    if (event.result === 'signal' && signalNames[code]) {
        return signalNames[code] + ' (' + code + ')';
    }
    if (event.result === 'signal') return 'signal ' + code;
    return 'exit ' + code;
}

async function loadCoreIncidentLog(unit, logNode) {
    if (!unit || !logNode) return;
    if (coreIncidentLogCache.has(unit)) {
        logNode.textContent = coreIncidentLogCache.get(unit);
        logNode.scrollTop = logNode.scrollHeight;
        return;
    }
    logNode.textContent = 'Loading…';
    try {
        const response = await fetch('/api/diagnostics/incident-log?unit=' + encodeURIComponent(unit), {
            cache: 'no-store',
        });
        if (!response.ok) throw new Error('incident log failed');
        const payload = await response.json();
        const text = (payload.log || '').trim()
            || ('No log tail captured for ' + coreServiceLabelForUnit(unit) + '.');
        coreIncidentLogCache.set(unit, text);
        logNode.textContent = text;
        logNode.scrollTop = logNode.scrollHeight;
    } catch (_) {
        logNode.textContent = 'Failed to load log tail for ' + coreServiceLabelForUnit(unit) + '.';
    }
}

async function copyCoreIncidentLog(logNode, button) {
    const text = (logNode?.textContent || '').trim();
    if (!text || text === 'Loading…') return;
    const label = button.textContent;
    try {
        if (navigator.clipboard?.writeText) {
            await navigator.clipboard.writeText(text);
        } else {
            const area = document.createElement('textarea');
            area.value = text;
            area.setAttribute('readonly', '');
            area.style.position = 'fixed';
            area.style.opacity = '0';
            document.body.appendChild(area);
            area.select();
            document.execCommand('copy');
            area.remove();
        }
        button.textContent = 'Copied';
    } catch (_) {
        button.textContent = 'Failed';
    }
    window.setTimeout(() => {
        button.textContent = label || 'Copy';
    }, 1200);
}

function renderCoreServices(snapshot) {
    const list = document.getElementById('coreServicesList');
    const summary = document.getElementById('coreServicesSummary');
    if (!list || !summary) return;

    const services = snapshot.services || [];
    const previousByUnit = new Map((previousCoreServicesSnapshot?.services || []).map(item => [item.unit, item]));
    const seconds = previousCoreServicesSnapshot &&
        snapshot.sampled_at_ms > previousCoreServicesSnapshot.sampled_at_ms
        ? (snapshot.sampled_at_ms - previousCoreServicesSnapshot.sampled_at_ms) / 1000
        : 0;

    list.replaceChildren();
    let failed = 0;
    let degraded = 0;
    for (const service of services) {
        if (service.severity === 'failed') failed += 1;
        else if (service.severity === 'degraded') degraded += 1;

        const previous = previousByUnit.get(service.unit);
        const cpu = coreServiceCPUPercent(service, previous, seconds);
        const mem = service.proc?.mem_rss_bytes;
        const row = document.createElement('button');
        row.type = 'button';
        row.className = 'core-service-row';
        row.dataset.unit = service.unit;
        row.dataset.severity = service.severity || 'ok';

        const copy = document.createElement('span');
        copy.className = 'core-service-copy';

        const name = document.createElement('span');
        name.className = 'core-service-name';
        name.textContent = service.label || service.unit;

        const metrics = document.createElement('span');
        metrics.className = 'core-service-metrics';
        const cpuText = cpu === null ? (service.proc ? 'CPU …' : 'CPU —') : ('CPU ' + Math.round(cpu) + '%');
        const memText = Number.isFinite(mem) ? ('MEM ' + coreServiceBytes(mem)) : 'MEM —';
        metrics.textContent = cpuText + ' · ' + memText;
        if (service.n_restarts > 0) {
            metrics.textContent += ' · R' + service.n_restarts;
        }
        copy.append(name, metrics);

        const state = document.createElement('span');
        state.className = 'core-service-state';
        state.textContent = coreServiceSeverityLabel(service.severity);
        state.title = [service.active_state, service.sub_state, service.result]
            .filter(Boolean).join(' · ') || service.load_error || '';

        row.append(copy, state);
        row.addEventListener('click', () => selectCoreServiceIncident(service.unit));
        list.append(row);
    }

    if (!services.length) {
        list.textContent = 'Unavailable';
    }

    const parts = [];
    if (failed) parts.push(failed + ' failed');
    if (degraded) parts.push(degraded + ' watch');
    if (!parts.length && services.length) parts.push(services.length + ' ok');
    summary.textContent = parts.join(' · ');
    previousCoreServicesSnapshot = snapshot;
}

function isRealCoreIncident(event) {
    if (!event || (event.severity !== 'failed' && event.severity !== 'degraded')) return false;
    // Drop spurious snapshots taken while the unit was still healthy.
    if (event.result === 'success' && event.active_state === 'active') return false;
    return true;
}

function renderCoreIncidents(events) {
    const list = document.getElementById('coreIncidentsList');
    const empty = document.getElementById('coreIncidentsEmpty');
    if (!list || !empty) return;
    const visible = (events || []).filter(isRealCoreIncident);
    const previouslyOpen = new Set(
        [...list.querySelectorAll('details.core-incident-item[open]')].map(node => node.dataset.unit)
    );
    if (selectedCoreIncidentUnit) previouslyOpen.add(selectedCoreIncidentUnit);

    coreIncidentEventsByUnit.clear();
    for (const event of visible) {
        if (event.unit && !coreIncidentEventsByUnit.has(event.unit)) {
            coreIncidentEventsByUnit.set(event.unit, event);
        }
    }
    list.replaceChildren();
    if (!visible.length) {
        empty.hidden = false;
        selectedCoreIncidentUnit = '';
        return;
    }
    empty.hidden = true;
    for (const event of visible) {
        const item = document.createElement('details');
        item.className = 'core-incident-item';
        item.dataset.unit = event.unit || '';
        item.dataset.severity = event.severity || 'failed';
        item.open = previouslyOpen.has(event.unit);

        const summary = document.createElement('summary');
        summary.className = 'core-incident-summary';

        const main = document.createElement('span');
        main.className = 'core-incident-main';

        const title = document.createElement('span');
        title.className = 'core-incident-title';
        title.textContent = coreServiceLabelForUnit(event.unit);

        const meta = document.createElement('span');
        meta.className = 'core-incident-meta';
        meta.textContent = formatCoreIncidentTime(event.ts);
        main.append(title, meta);

        const badge = document.createElement('span');
        badge.className = 'core-incident-badge';
        badge.textContent = coreIncidentReason(event) || coreServiceSeverityLabel(event.severity);

        summary.append(main, badge);
        summary.title = [
            coreIncidentReason(event),
            event.active_state,
            event.sub_state,
            formatCoreIncidentBoard(event.board),
        ].filter(Boolean).join(' · ');

        const body = document.createElement('div');
        body.className = 'core-incident-body';

        const facts = document.createElement('dl');
        facts.className = 'core-incident-facts';
        const factRows = [
            ['Board', formatCoreIncidentBoard(event.board)],
            ['State', [
                event.active_state,
                event.sub_state,
                formatCoreIncidentExit(event),
            ].filter(Boolean).join(' · ')],
        ];
        for (const [label, value] of factRows) {
            if (!value) continue;
            const row = document.createElement('div');
            row.className = 'core-incident-fact';
            const dt = document.createElement('dt');
            dt.textContent = label;
            const dd = document.createElement('dd');
            dd.textContent = value;
            row.append(dt, dd);
            facts.append(row);
        }
        if (facts.childElementCount) body.append(facts);

        const logHead = document.createElement('div');
        logHead.className = 'core-incident-log-head';
        const logLabel = document.createElement('div');
        logLabel.className = 'core-incident-log-label';
        logLabel.textContent = 'Log tail';
        const copyBtn = document.createElement('button');
        copyBtn.type = 'button';
        copyBtn.className = 'core-incident-copy';
        copyBtn.textContent = 'Copy';
        logHead.append(logLabel, copyBtn);
        body.append(logHead);

        const logNode = document.createElement('pre');
        logNode.className = 'core-incident-log';
        if (coreIncidentLogCache.has(event.unit)) {
            logNode.textContent = coreIncidentLogCache.get(event.unit);
        } else {
            logNode.textContent = 'Loading…';
        }
        copyBtn.addEventListener('click', (ev) => {
            ev.preventDefault();
            ev.stopPropagation();
            copyCoreIncidentLog(logNode, copyBtn);
        });
        body.append(logNode);

        item.append(summary, body);
        item.addEventListener('toggle', () => {
            if (item.open) {
                selectedCoreIncidentUnit = event.unit || '';
                // Keep only one incident expanded for a quieter panel.
                list.querySelectorAll('details.core-incident-item[open]').forEach(node => {
                    if (node !== item) node.open = false;
                });
                loadCoreIncidentLog(event.unit, logNode);
            } else if (selectedCoreIncidentUnit === event.unit) {
                selectedCoreIncidentUnit = '';
            }
        });
        list.append(item);
        if (item.open) loadCoreIncidentLog(event.unit, logNode);
    }
}

function selectCoreServiceIncident(unit) {
    if (!unit) return;
    const section = document.getElementById('coreIncidentsDetails');
    if (section && !section.open) section.open = true;
    const item = document.querySelector('.core-incident-item[data-unit="' + unit.replace(/"/g, '\\"') + '"]');
    if (!item) return;
    item.open = true;
    item.scrollIntoView({block: 'nearest'});
}

function coreServicesPanelOpen() {
    const panel = document.getElementById('coreServicesPanel');
    return Boolean(panel && panel.open && !document.hidden);
}

async function refreshCoreServices() {
    if (!coreServicesPanelOpen() || coreServicesRequestInFlight) return;
    coreServicesRequestInFlight = true;
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 4000);
    try {
        const response = await fetch('/api/diagnostics/services', {cache: 'no-store', signal: controller.signal});
        if (!response.ok) throw new Error('services failed');
        renderCoreServices(await response.json());
    } catch (_) {
        renderCoreServices({});
        previousCoreServicesSnapshot = null;
    } finally {
        clearTimeout(timeout);
        coreServicesRequestInFlight = false;
    }
}

async function refreshCoreIncidents() {
    if (!coreServicesPanelOpen() || coreIncidentsRequestInFlight) return;
    coreIncidentsRequestInFlight = true;
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 4000);
    try {
        const response = await fetch('/api/diagnostics/incidents?limit=20', {
            cache: 'no-store',
            signal: controller.signal,
        });
        if (!response.ok) throw new Error('incidents failed');
        const payload = await response.json();
        renderCoreIncidents(payload.events || []);
    } catch (_) {
        renderCoreIncidents([]);
    } finally {
        clearTimeout(timeout);
        coreIncidentsRequestInFlight = false;
    }
}

function startCoreServicesPolling() {
    const panel = document.getElementById('coreServicesPanel');
    const tick = () => {
        if (!coreServicesPanelOpen()) return;
        refreshCoreServices();
        refreshCoreIncidents();
    };
    // Only poll while the Core services panel is expanded.
    setInterval(tick, CORE_SERVICES_POLL_MS);
    if (panel) {
        panel.addEventListener('toggle', () => {
            previousCoreServicesSnapshot = null;
            if (panel.open) tick();
        });
    }
    document.addEventListener('visibilitychange', () => {
        previousCoreServicesSnapshot = null;
        if (!document.hidden) tick();
    });
}
