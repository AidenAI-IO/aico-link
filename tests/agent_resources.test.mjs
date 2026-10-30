import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const ui = new URL('../src/agent/internal/agent/web_ui/', import.meta.url);
const script = readFileSync(new URL('scripts/resources.js', ui), 'utf8');

class Element {
    children = [];
    attributes = new Map();
    text = '';
    set textContent(value) { this.text = value; this.children = []; }
    get textContent() { return this.text + this.children.map(child => child.textContent).join(''); }
    append(...children) { this.children.push(...children); }
    replaceChildren(...children) { this.text = ''; this.children = children; }
    setAttribute(name, value) { this.attributes.set(name, value); }
}

function harness(fetch) {
    const elements = new Map();
    const listeners = new Map();
    const intervals = [];
    const timeouts = new Map();
    let nextTimer = 0;
    const document = {
        hidden: false,
        createElement() { return new Element(); },
        getElementById(id) {
            if (!elements.has(id)) elements.set(id, new Element());
            return elements.get(id);
        },
        addEventListener(event, callback) { listeners.set(event, callback); },
    };
    const context = vm.createContext({
        document, fetch, AbortController,
        setInterval(callback, ms) { intervals.push({callback, ms}); },
        setTimeout(callback, ms) { timeouts.set(++nextTimer, {callback, ms}); return nextTimer; },
        clearTimeout(id) { timeouts.delete(id); },
    });
    vm.runInContext(script, context);
    return {context, document, listeners, intervals, timeouts,
        text: name => document.getElementById('resource' + name).textContent};
}

function sample(overrides = {}) {
    return {
        sampled_at_ms: 1000, boot_id: 'boot-a',
        wifi: [{name: 'wlan0', index: 3, ips: ['192.168.1.12'], rx_bytes: 1024, tx_bytes: 2048}],
        battery: {percent: 0, millivolts: 3700},
        cpu: {total: 100, idle: 50}, npu_percent: 0,
        memory: {used_bytes: 1024, total_bytes: 2048},
        disks: [{path: '/userdata', usage: {used_bytes: 1024, total_bytes: 4096}}],
        ...overrides,
    };
}

test('resources render actual utilization, rates, totals and unavailable metrics', () => {
    const h = harness();
    h.context.renderResources(sample());
    assert.equal(h.text('Battery'), '0%');
    assert.equal(h.text('Voltage'), ' · 3.70 V');
    assert.equal(h.text('NPU'), '0%');
    assert.equal(h.text('CPU'), 'Sampling…');
    assert.equal(h.text('RX'), 'Sampling…');
    assert.equal(h.text('Totals'), 'wlan0: 1.0 KiB / 2.0 KiB');
    assert.equal(h.text('Memory'), '50%');
    assert.equal(h.text('MemoryDetail'), '1.0 KiB / 2.0 KiB (50%)');
    assert.equal(h.document.getElementById('resourceMemoryMeter').value, 50);
    assert.equal(h.document.getElementById('resourceBatteryMeter').hidden, false);
    assert.equal(h.document.getElementById('resourceBatteryMeter').value, 0);
    assert.equal(h.text('Disks'), 'Data25%');
    assert.equal(h.text('DiskDetails'), '/userdata: 1.0 KiB / 4.0 KiB (25%)');
    h.context.renderResources(sample({sampled_at_ms: 3000, cpu: {total: 300, idle: 100},
        wifi: [{name: 'wlan0', index: 3, ips: ['192.168.1.12'], rx_bytes: 5120, tx_bytes: 4096}]}));
    assert.equal(h.text('CPU'), '75%');
    assert.equal(h.text('RX'), '2.0 KiB/s');
    assert.equal(h.text('TX'), '1.0 KiB/s');
    h.context.renderResources(sample({sampled_at_ms: 4000, battery: null, cpu: null,
        npu_percent: null, memory: null, wifi: [], disks: []}));
    for (const name of ['Battery', 'CPU', 'NPU', 'Memory', 'WiFi', 'RX', 'TX', 'Disks']) {
        assert.equal(h.text(name), 'Unavailable', name);
    }
});

test('counter rollback, interface replacement and device reboot reset rate sampling', () => {
    const h = harness();
    h.context.renderResources(sample());
    h.context.renderResources(sample({sampled_at_ms: 2000, cpu: {total: 20, idle: 5},
        wifi: [{name: 'wlan0', index: 3, ips: [], rx_bytes: 1, tx_bytes: 0}]}));
    assert.equal(h.text('CPU'), 'Sampling…');
    assert.equal(h.text('RX'), 'Sampling…');
    assert.equal(h.text('TX'), 'Sampling…');
    h.context.renderResources(sample({sampled_at_ms: 3000,
        wifi: [{name: 'wlan0', index: 4, ips: [], rx_bytes: 2000, tx_bytes: 4000}]}));
    assert.equal(h.text('RX'), 'Sampling…');
    assert.equal(h.text('TX'), 'Sampling…');
    h.context.renderResources(sample({sampled_at_ms: 4000, boot_id: 'boot-b'}));
    assert.equal(h.text('CPU'), 'Sampling…');
    assert.equal(h.text('RX'), 'Sampling…');
    assert.equal(h.text('TX'), 'Sampling…');
});

test('poll every second without overlapping; pause when hidden and recover after errors', async () => {
    let resolve, calls = 0;
    const h = harness(() => { calls++; return new Promise(done => { resolve = done; }); });
    h.context.startResourcePolling();
    assert.equal(h.intervals.length, 1);
    assert.equal(h.intervals[0].ms, 1000);
    await h.intervals[0].callback();
    assert.equal(calls, 1);
    resolve({ok: true, json: async () => sample()});
    await new Promise(done => setImmediate(done));
    assert.equal(h.text('Status'), 'Live · 1s');
    assert.equal(h.timeouts.size, 0);
    h.document.hidden = true;
    h.listeners.get('visibilitychange')();
    await h.intervals[0].callback();
    assert.equal(calls, 1);
    h.document.hidden = false;
    h.listeners.get('visibilitychange')();
    assert.equal(calls, 2);
    resolve({ok: false});
    await new Promise(done => setImmediate(done));
    assert.equal(h.text('CPU'), 'Unavailable');
    assert.equal(h.text('Status'), 'Offline · retrying');
    assert.equal(h.document.getElementById('resourceMemoryMeter').hidden, true);
    const pending = h.intervals[0].callback();
    resolve({ok: true, json: async () => sample({sampled_at_ms: 3000})});
    await pending;
    assert.equal(h.text('Status'), 'Live · 1s');
    assert.equal(h.text('CPU'), 'Sampling…');
});

test('a hung request is aborted and polling can resume', async () => {
    const h = harness((url, options) => new Promise((resolve, reject) => {
        assert.equal(url, '/api/resources');
        assert.equal(options.cache, 'no-store');
        options.signal.addEventListener('abort', () => reject(new Error('aborted')));
    }));
    const pending = h.context.refreshResources();
    const timeout = [...h.timeouts.values()][0];
    assert.equal(timeout.ms, 2500);
    timeout.callback();
    await pending;
    assert.equal(h.text('Status'), 'Offline · retrying');
    assert.equal(h.timeouts.size, 0);
    h.context.fetch = async () => ({ok: true, json: async () => sample()});
    await h.context.refreshResources();
    assert.equal(h.text('Status'), 'Live · 1s');
});

test('terminal is a standalone link and resource script loads before bootstrap', () => {
    const index = readFileSync(new URL('index.html', ui), 'utf8');
    assert.doesNotMatch(index, /<iframe|terminalFrame/);
    assert.match(index, /href="\/webtty\/" target="_blank" rel="noopener noreferrer">Open terminal/);
    assert.ok(index.indexOf('scripts/resources.js') < index.indexOf('scripts/bootstrap.js'));
});

test('main Wi-Fi reading prefers IPv4 while Details retains all addresses', () => {
    const h = harness();
    const ipv6 = '2001:db8::122';
    h.context.renderResources(sample({wifi: [{name: 'wlan0', index: 3, ips: [ipv6, '192.0.2.122'], rx_bytes: 1, tx_bytes: 2}]}));
    assert.equal(h.text('WiFi'), '192.0.2.122');
    assert.ok(h.text('Addresses').includes(ipv6));
    assert.ok(h.text('Addresses').includes('192.0.2.122'));
    h.context.renderResources(sample({wifi: [{name: 'wlan0', index: 3, ips: [ipv6], rx_bytes: 1, tx_bytes: 2}]}));
    assert.equal(h.text('WiFi'), ipv6);
});
