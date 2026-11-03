import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {setImmediate} from 'node:timers/promises';
import vm from 'node:vm';

const pageSource = await readFile(new URL('../assets/js/app/pages/storage.js', import.meta.url), 'utf8');

async function loadPage(result) {
  const toasts = [];
  const calls = [];
  let status = {card: {present: true, mounted: true}, effective_mode: 2};
  const context = vm.createContext({window: {addEventListener() {}}, setTimeout() {}, clearTimeout() {}});
  const msg = (key, fallback) => ({key, fallback});
  const resolve = spec => typeof spec === 'string' ? spec : spec?.fallback || spec?.key || '';
  const el = (tag, options = {}, children = []) => ({
    kind: tag, options, children, style: {}, isConnected: true, addEventListener() {},
  });
  const mocks = {
    '../backup-engine.js': {
      createBackupEngine: () => ({capabilities: async () => ({}), attach: async () => false}),
      errorText: error => error.message, formatBytes: String,
    },
    '../data.js': {
      request: async (path, options) => {
        calls.push({path, method: options?.method || 'GET'});
        if (path === '/api/storage/eject') {
          if (result instanceof Error) throw result;
          status = result;
        }
        return status;
      },
      t: (key, options) => options?.defaultValue || key,
    },
    '../../ui/environment.js': {isEmbedded: () => false},
    '../host.js': {postToHost() {}},
    '../saver.js': {createSaver: () => ({refreshChrome() {}})},
    '../../ui/button.js': {button: options => ({kind: 'button', ...options})},
    '../../ui/choice-sheet.js': {choiceSheet() {}},
    '../../ui/confirm.js': {confirmSheet: async () => true},
    '../../ui/dom.js': {el, replace: (node, children) => { node.children = children; }},
    '../../ui/list.js': {
      group: options => ({kind: 'group', children: options.rows}),
      row: options => ({kind: 'row', ...options}),
      screen: children => ({kind: 'screen', children}),
    },
    '../../ui/text.js': {msg, resolve, text: (spec, className) => ({kind: 'text', spec, className})},
    '../../ui/toast.js': {toast: (message, options) => toasts.push({message, options})},
  };
  const module = new vm.SourceTextModule(pageSource, {context});
  await module.link(specifier => {
    const values = mocks[specifier];
    assert.ok(values, `unexpected import: ${specifier}`);
    return new vm.SyntheticModule(Object.keys(values), function () {
      for (const [name, value] of Object.entries(values)) this.setExport(name, value);
    }, {context});
  });
  await module.evaluate();
  const page = await module.namespace.storagePage({});
  await setImmediate();
  return {page, toasts, calls};
}

function find(node, predicate) {
  if (!node) return undefined;
  if (predicate(node)) return node;
  for (const child of node.children || []) {
    const found = find(child, predicate);
    if (found) return found;
  }
  return undefined;
}

for (const scenario of [
  {name: 'clean eject', key: 'storage.ejected'},
  {name: 'flush warning', code: 'storage_flush_failed', warning: true, key: 'storage.ejected_with_warning'},
  {name: 'historical warning', warning: true, key: 'storage.ejected'},
  {name: 'busy mount', error: true, key: 'target busy'},
]) {
  test(`storage page reports ${scenario.name} accurately`, async () => {
    const result = scenario.error ? new Error('target busy') : {
      card: {present: true, mounted: false}, effective_mode: 1,
      warning_code: scenario.code,
      last_warning: scenario.warning ? 'sync /mnt/sdcard: Input/output error' : undefined,
    };
    const {page, toasts, calls} = await loadPage(result);
    const eject = () => find(page, node => node.action === 'eject-card');
    assert.equal(eject().disabled, false);
    await eject().onPress();
    await setImmediate();
    assert.equal(toasts.length, 1);
    const message = toasts[0].message;
    assert.equal(typeof message === 'string' ? message : message.key, scenario.key);
    assert.equal(eject().disabled, !scenario.error);
    assert.ok(calls.some(call => call.path === '/api/storage/eject' && call.method === 'POST'));
    const reminder = find(page, node => node.spec?.key === 'storage.last_flush_warning');
    assert.equal(Boolean(reminder), Boolean(scenario.warning));
    if (scenario.code) assert.equal(toasts[0].options.durationMs, 6000);
    assert.equal(JSON.stringify(page).includes('Input/output error'), false, 'technical warning must not replace localized copy');
  });
}
