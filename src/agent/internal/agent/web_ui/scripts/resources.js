let previousResourceSnapshot = null;
let resourceRequestInFlight = false;

function resourceBytes(value) {
    if (!Number.isFinite(value) || value < 0) return 'Unavailable';
    const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
    let unit = 0;
    while (value >= 1024 && unit < units.length - 1) {
        value /= 1024;
        unit++;
    }
    return value.toFixed(unit === 0 ? 0 : 1) + ' ' + units[unit];
}

function resourcePercent(value) {
    return Number.isFinite(value) && value >= 0 && value <= 100 ? Math.round(value) + '%' : 'Unavailable';
}

function resourceUsageText(usage) {
    if (!usage || !Number.isFinite(usage.used_bytes) || !Number.isFinite(usage.total_bytes) ||
        usage.total_bytes <= 0 || usage.used_bytes < 0 || usage.used_bytes > usage.total_bytes) return 'Unavailable';
    return resourceBytes(usage.used_bytes) + ' / ' + resourceBytes(usage.total_bytes) +
        ' (' + resourcePercent(100 * usage.used_bytes / usage.total_bytes) + ')';
}

function resourceRate(current, previous, seconds) {
    if (!Number.isFinite(current)) return 'Unavailable';
    if (!Number.isFinite(previous) || seconds <= 0 || current < previous) return 'Sampling…';
    return resourceBytes((current - previous) / seconds) + '/s';
}

function resourceUsagePercent(usage) {
    if (!usage || !Number.isFinite(usage.used_bytes) || !Number.isFinite(usage.total_bytes) ||
        usage.total_bytes <= 0 || usage.used_bytes < 0 || usage.used_bytes > usage.total_bytes) return null;
    return 100 * usage.used_bytes / usage.total_bytes;
}

function updateResourceMeter(meter, value, description) {
    const valid = Number.isFinite(value) && value >= 0 && value <= 100;
    meter.hidden = !valid;
    meter.value = valid ? value : 0;
    meter.setAttribute('aria-valuetext', valid ? description : 'Unavailable');
    meter.title = valid ? description : 'Unavailable';
}

function renderResourceDisks(disks) {
    const container = document.getElementById('resourceDisks');
    container.replaceChildren();
    if (!disks.length) {
        container.textContent = 'Unavailable';
        return;
    }
    const labels = {'/': 'System', '/userdata': 'Data', '/userdata/ota': 'OTA'};
    for (const disk of disks) {
        const row = document.createElement('div');
        row.className = 'resource-gauge';
        const heading = document.createElement('div');
        heading.className = 'resource-reading';
        const label = document.createElement('span');
        label.textContent = labels[disk.path] || disk.path;
        label.title = disk.path;
        const value = document.createElement('span');
        const percent = resourceUsagePercent(disk.usage);
        value.textContent = resourcePercent(percent);
        heading.append(label, value);
        const meter = document.createElement('meter');
        meter.min = 0;
        meter.max = 100;
        meter.setAttribute('aria-label', label.textContent + ' disk usage');
        updateResourceMeter(meter, percent, resourceUsageText(disk.usage));
        row.append(heading, meter);
        container.append(row);
    }
}

function renderResources(snapshot) {
    const set = (name, value) => { document.getElementById('resource' + name).textContent = value; };
    const gauge = (name, value, description = resourcePercent(value)) => {
        set(name, description);
        updateResourceMeter(document.getElementById('resource' + name + 'Meter'), value, description);
    };
    const previous = previousResourceSnapshot && snapshot.boot_id === previousResourceSnapshot.boot_id &&
        snapshot.sampled_at_ms > previousResourceSnapshot.sampled_at_ms ? previousResourceSnapshot : null;
    const seconds = previous ? (snapshot.sampled_at_ms - previous.sampled_at_ms) / 1000 : 0;
    const wifi = snapshot.wifi || [];
    const addresses = wifi.map(item => item.name + ': ' + (item.ips.length ? item.ips.join(', ') : 'No IP')).join('\n') || 'Unavailable';
    set('WiFi', wifi.map(item => (wifi.length > 1 ? item.name + ': ' : '') +
        (item.ips.find(ip => !ip.includes(':')) || item.ips[0] || 'No IP')).join(', ') || 'Unavailable');
    document.getElementById('resourceWiFi').title = addresses;
    set('Addresses', addresses);
    for (const direction of ['RX', 'TX']) {
        const key = direction.toLowerCase() + '_bytes';
        set(direction, wifi.map(item => {
            const before = previous && (previous.wifi || []).find(old => old.name === item.name && old.index === item.index);
            return (wifi.length > 1 ? item.name + ': ' : '') + resourceRate(item[key], before?.[key], seconds);
        }).join('\n') || 'Unavailable');
    }
    set('Totals', wifi.map(item => item.name + ': ' + resourceBytes(item.rx_bytes) + ' / ' + resourceBytes(item.tx_bytes)).join('\n') || 'Unavailable');
    const battery = snapshot.battery;
    gauge('Battery', battery?.percent);
    set('Voltage', Number.isFinite(battery?.millivolts) ? ' · ' + (battery.millivolts / 1000).toFixed(2) + ' V' : ' · Voltage unavailable');
    let cpu = null;
    if (snapshot.cpu && previous?.cpu) {
        const total = snapshot.cpu.total - previous.cpu.total;
        const idle = snapshot.cpu.idle - previous.cpu.idle;
        if (total > 0 && idle >= 0 && idle <= total) cpu = 100 * (total - idle) / total;
    }
    gauge('CPU', cpu, cpu === null && snapshot.cpu ? 'Sampling…' : resourcePercent(cpu));
    gauge('NPU', snapshot.npu_percent);
    gauge('Memory', resourceUsagePercent(snapshot.memory));
    document.getElementById('resourceMemoryMeter').title = resourceUsageText(snapshot.memory);
    set('MemoryDetail', resourceUsageText(snapshot.memory));
    renderResourceDisks(snapshot.disks || []);
    set('DiskDetails', (snapshot.disks || []).map(disk => disk.path + ': ' + resourceUsageText(disk.usage)).join('\n') || 'Unavailable');
    set('Status', 'Live · 1s');
    previousResourceSnapshot = snapshot;
}

async function refreshResources() {
    if (document.hidden || resourceRequestInFlight) return;
    resourceRequestInFlight = true;
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 2500);
    try {
        const response = await fetch('/api/resources', {cache: 'no-store', signal: controller.signal});
        if (!response.ok) throw new Error('Resource request failed');
        renderResources(await response.json());
    } catch (_) {
        renderResources({});
        previousResourceSnapshot = null;
        document.getElementById('resourceStatus').textContent = 'Offline · retrying';
    } finally {
        clearTimeout(timeout);
        resourceRequestInFlight = false;
    }
}

function startResourcePolling() {
    refreshResources();
    setInterval(refreshResources, 1000);
    document.addEventListener('visibilitychange', () => {
        previousResourceSnapshot = null;
        if (!document.hidden) refreshResources();
    });
}
