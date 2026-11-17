let previousCoreServicesSnapshot = null;
let coreServicesRequestInFlight = false;
let coreIncidentsRequestInFlight = false;
let selectedCoreIncidentKey = '';
const coreIncidentEventsByUnit = new Map();
// Cache log tails by incident identity (unit|ts) so a newer failure invalidates older text.
const coreIncidentLogCache = new Map();
const coreIncidentLogInFlight = new Map();

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
    const currentPid = service?.proc?.pid ?? service?.main_pid;
    const beforePid = previous?.proc?.pid ?? previous?.main_pid;
    if (!Number.isFinite(current)) return null;
    if (!Number.isFinite(before) || seconds <= 0 || current < before) return null;
    // After a restart the counter baseline belongs to a different process.
    if (Number.isFinite(currentPid) && Number.isFinite(beforePid) && currentPid !== beforePid) {
        return null;
    }
    // cpu_total is in clock ticks; assume USER_HZ=100 like the Linux default.
    const percent = 100 * ((current - before) / 100) / seconds;
    if (!Number.isFinite(percent) || percent < 0) return null;
    return Math.min(percent, 999);
}

function coreServiceSeverityLabel(severity) {
    switch (severity) {
        case 'failed': return 'Failed';
        case 'degraded': return 'Watch';
        case 'unavailable': return 'Unavailable';
        default: return 'OK';
    }
}

function coreServiceDisplaySeverity(service) {
    if (!service) return 'unavailable';
    if (service.load_error) return 'unavailable';
    return service.severity || 'ok';
}

function coreServiceLabelForUnit(unit) {
    if (!unit) return '';
    if (CORE_SERVICE_LABELS[unit]) return CORE_SERVICE_LABELS[unit];
    return String(unit).replace(/\.service$/, '').replace(/^aico-/, '');
}

function coreIncidentKey(event) {
    if (!event) return '';
    return String(event.unit || '') + '|' + String(event.ts || '');
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
    const raw = event.exec_main_status;
    if (typeof raw === 'string' && /[A-Za-z_]/.test(raw)) {
        const name = raw.startsWith('SIG') ? raw : ('SIG' + raw);
        return name;
    }
    const code = Number(raw);
    if (!Number.isFinite(code)) return 'exit ' + raw;
    const signalNames = {
        1: 'SIGHUP',
        2: 'SIGINT',
        9: 'SIGKILL',
        15: 'SIGTERM',
    };
    if ((event.result === 'signal' || event.exec_main_code === 'killed' || event.exec_main_code === 'dumped')
        && signalNames[code]) {
        return signalNames[code] + ' (' + code + ')';
    }
    if (event.result === 'signal' || event.exec_main_code === 'killed' || event.exec_main_code === 'dumped') {
        return 'signal ' + code;
    }
    return 'exit ' + code;
}

async function loadCoreIncidentLog(event, logNode) {
    if (!event?.unit || !logNode) return;
    const key = coreIncidentKey(event);
    if (coreIncidentLogCache.has(key)) {
        logNode.textContent = coreIncidentLogCache.get(key);
        logNode.scrollTop = logNode.scrollHeight;
        return;
    }
    if (coreIncidentLogInFlight.get(key)) return;
    coreIncidentLogInFlight.set(key, true);
    logNode.textContent = 'Loading…';
    try {
        const response = await fetch('/api/diagnostics/incident-log?unit=' + encodeURIComponent(event.unit), {
            cache: 'no-store',
        });
        if (!response.ok) throw new Error('incident log failed');
        const payload = await response.json();
        const text = (payload.log || '').trim()
            || ('No log tail captured for ' + coreServiceLabelForUnit(event.unit) + '.');
        // Ignore late responses for an older incident of the same unit.
        if (coreIncidentKey(event) !== key) return;
        coreIncidentLogCache.set(key, text);
        logNode.textContent = text;
        logNode.scrollTop = logNode.scrollHeight;
    } catch (_) {
        if (coreIncidentKey(event) !== key) return;
        logNode.textContent = 'Failed to load log tail for ' + coreServiceLabelForUnit(event.unit) + '.';
    } finally {
        coreIncidentLogInFlight.delete(key);
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
    let unavailable = 0;
    for (const service of services) {
        const displaySeverity = coreServiceDisplaySeverity(service);
        if (displaySeverity === 'failed') failed += 1;
        else if (displaySeverity === 'degraded') degraded += 1;
        else if (displaySeverity === 'unavailable') unavailable += 1;

        const previous = previousByUnit.get(service.unit);
        const cpu = coreServiceCPUPercent(service, previous, seconds);
        const mem = service.proc?.mem_rss_bytes;
        const row = document.createElement('button');
        row.type = 'button';
        row.className = 'core-service-row';
        row.dataset.unit = service.unit;
        row.dataset.severity = displaySeverity;

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
        state.textContent = coreServiceSeverityLabel(displaySeverity);
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
    if (unavailable) parts.push(unavailable + ' unavailable');
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

function renderCoreIncidents(events, options = {}) {
    const list = document.getElementById('coreIncidentsList');
    const empty = document.getElementById('coreIncidentsEmpty');
    if (!list || !empty) return;
    const errored = Boolean(options.error);
    const visible = errored ? [] : (events || []).filter(isRealCoreIncident);
    const previouslyOpen = new Set(
        [...list.querySelectorAll('details.core-incident-item[open]')].map(node => node.dataset.incidentKey)
    );
    if (selectedCoreIncidentKey) previouslyOpen.add(selectedCoreIncidentKey);

    coreIncidentEventsByUnit.clear();
    for (const event of visible) {
        if (event.unit && !coreIncidentEventsByUnit.has(event.unit)) {
            coreIncidentEventsByUnit.set(event.unit, event);
        }
    }
    list.replaceChildren();
    if (errored) {
        empty.hidden = false;
        empty.textContent = 'Failure history unavailable.';
        selectedCoreIncidentKey = '';
        return;
    }
    if (!visible.length) {
        empty.hidden = false;
        empty.textContent = 'No recorded failures.';
        selectedCoreIncidentKey = '';
        return;
    }
    empty.hidden = true;
    empty.textContent = 'No recorded failures.';
    for (const event of visible) {
        const key = coreIncidentKey(event);
        const item = document.createElement('details');
        item.className = 'core-incident-item';
        item.dataset.unit = event.unit || '';
        item.dataset.incidentKey = key;
        item.dataset.severity = event.severity || 'failed';
        item.open = previouslyOpen.has(key);

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
        if (coreIncidentLogCache.has(key)) {
            logNode.textContent = coreIncidentLogCache.get(key);
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
                selectedCoreIncidentKey = key;
                // Keep only one incident expanded for a quieter panel.
                list.querySelectorAll('details.core-incident-item[open]').forEach(node => {
                    if (node !== item) node.open = false;
                });
                loadCoreIncidentLog(event, logNode);
            } else if (selectedCoreIncidentKey === key) {
                selectedCoreIncidentKey = '';
            }
        });
        list.append(item);
        if (item.open) loadCoreIncidentLog(event, logNode);
    }
}

function selectCoreServiceIncident(unit) {
    if (!unit) return;
    const section = document.getElementById('coreIncidentsDetails');
    if (section && !section.open) section.open = true;
    const event = coreIncidentEventsByUnit.get(unit);
    const key = event ? coreIncidentKey(event) : '';
    const item = key
        ? document.querySelector('.core-incident-item[data-incident-key="' + key.replace(/"/g, '\\"') + '"]')
        : document.querySelector('.core-incident-item[data-unit="' + unit.replace(/"/g, '\\"') + '"]');
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
        renderCoreIncidents([], {error: true});
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
