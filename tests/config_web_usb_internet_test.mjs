import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import vm from 'node:vm';
import {test} from 'node:test';

const context = vm.createContext({console});
const source = await fs.readFile(new URL('../src/config_web/web/assets/js/app/pages/usb-internet.js', import.meta.url), 'utf8');
const module = new vm.SourceTextModule(source, {context});
const names = ['request', 'el', 'replace', 'group', 'row', 'switchControl', 'msg', 'resolve', 'toast'];
await module.link(() => new vm.SyntheticModule(names, function() {
  for (const name of names) this.setExport(name, () => {});
}, {context}));
await module.evaluate();
const {usbInternetState} = module.namespace;

test('USB Internet uses confirmed board state and restores it after a failed save', async () => {
  let fail = true;
  const model = usbInternetState(async (_url, options) => {
    if (options.method === 'PUT') {
      if (fail) throw new Error('offline');
      return {enabled: JSON.parse(options.body).enabled, via_usb: true};
    }
    return {enabled: false, via_usb: true};
  }, () => {});
  assert.equal(model.state, null);
  await model.load();
  assert.equal(model.state.enabled, false);
  await assert.rejects(model.save(true), /offline/);
  assert.equal(model.state.enabled, false);
  assert.equal(model.busy, false);
  fail = false;
  await model.save(true);
  assert.equal(model.state.enabled, true);
  await model.save(false);
  assert.equal(model.state.enabled, false);
});

test('a stale status read cannot undo a completed toggle and LAN reads cannot write', async () => {
  let delayed;
  let calls = 0;
  const model = usbInternetState(async (_url, options) => {
    if (options.method === 'PUT') return {enabled: true, via_usb: true};
    if (++calls === 2) return new Promise(resolve => { delayed = resolve; });
    return {enabled: false, via_usb: true};
  }, () => {});
  await model.load();
  const read = model.load();
  await model.save(true);
  delayed({enabled: false, via_usb: true});
  await read;
  assert.equal(model.state.enabled, true);
  let writes = 0;
  const readonly = usbInternetState(async (_url, options) => {
    if (options.method) writes++;
    return {enabled: false, via_usb: false};
  }, () => {});
  await readonly.load();
  await readonly.save(true);
  assert.equal(writes, 0);
});

test('unchanged polling preserves the rendered control while visible changes and outages update it', async () => {
  let result = {enabled: true, active: true, host: 'mac', via_usb: true, expires_in_seconds: 29};
  let offline = false;
  let renders = 0;
  const model = usbInternetState(async () => {
    if (offline) throw new Error('offline');
    return {...result};
  }, () => { renders++; });
  await model.load();
  assert.equal(renders, 1);
  result.expires_in_seconds = 24;
  await model.load();
  await model.load();
  assert.equal(renders, 1, 'lease countdown must not replace the focused control');
  for (const change of [{host: 'android'}, {active: false}, {enabled: false}, {via_usb: false}]) {
    result = {...result, ...change};
    const before = renders;
    await model.load();
    assert.equal(renders, before + 1);
  }
  offline = true;
  const before = renders;
  await assert.rejects(model.load(), /offline/);
  assert.equal(model.state, null);
  assert.equal(renders, before + 1);
  await assert.rejects(model.load(), /offline/);
  assert.equal(renders, before + 1, 'repeated errors must not replace the unavailable control');
  offline = false;
  await model.load();
  assert.equal(renders, before + 2);
});
