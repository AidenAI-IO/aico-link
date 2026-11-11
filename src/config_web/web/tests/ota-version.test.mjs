import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import vm from 'node:vm';

const pageSource = await readFile(new URL('../assets/js/app/pages/ota.js', import.meta.url), 'utf8');

test('firmware page reads the installed business version after an update finishes', async () => {
  let statusReads = 0;
  let updateFinished = false;
  let poll;
  const calls = [];
  const context = vm.createContext({
    setTimeout(callback) { poll = callback; return 1; },
    clearTimeout() {},
  });
  const el = (kind, options = {}, children = []) => ({kind, children, isConnected: true, ...options});
  const mocks = {
    '../data.js': {
      async request(path) {
        assert.equal(path, '/api/ota/status');
        calls.push('status');
        statusReads++;
        if (statusReads === 1) return {ota_update_running: true};
        await Promise.resolve();
        updateFinished = true;
        return {ota_update_running: false};
      },
      async fetchSnapshot() {
        calls.push('snapshot');
        return {firmware: {
          current_version: 'dev-v0.0.77',
          business_version: updateFinished ? '0.0.78-1' : '',
        }};
      },
      t: (_key, options) => options.defaultValue,
    },
    '../apply.js': {waitForReboot() {}},
    '../host.js': {postToHost() {}},
    '../ota-progress.js': {
      businessReleaseVersion: version => String(version || '').replace(/-\d+$/, ''),
      parseOtaLog: () => ({state: 'completed'}),
      shortVersion: value => value || '',
    },
    '../saver.js': {createSaver: () => ({refreshChrome() {}})},
    '../../ui/button.js': {button: options => ({kind: 'button', ...options})},
    '../../ui/confirm.js': {confirmSheet() {}},
    '../../ui/dom.js': {el, replace: (node, children) => { node.children = children; }},
    '../../ui/icon.js': {icon: () => ({kind: 'icon'})},
    '../../ui/list.js': {
      group: options => ({kind: 'group', children: options.rows}),
      row: options => ({kind: 'row', ...options}),
      screen: children => ({kind: 'screen', children}),
    },
    '../../ui/text.js': {
      msg: (_key, fallback) => fallback,
      resolve: value => value,
      text: (value, className) => ({kind: 'text', value, className}),
    },
    '../../ui/toast.js': {toast() {}},
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

  const page = await module.namespace.otaPage({});
  assert.ok(!JSON.stringify(page).includes('aico-business-v0.0.78'));
  assert.equal(typeof poll, 'function');
  await poll();
  assert.deepEqual(calls, ['status', 'snapshot', 'status', 'snapshot']);
  assert.ok(JSON.stringify(page).includes('dev-v0.0.77 + aico-business-v0.0.78'));
});
