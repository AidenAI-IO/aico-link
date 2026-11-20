import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import vm from 'node:vm';

const pageSource = await readFile(new URL('../assets/js/app/pages/model.js', import.meta.url), 'utf8');
const fieldsSource = await readFile(new URL('../assets/js/app/fields.js', import.meta.url), 'utf8');

async function loadDecisionPage({catalogAvailable = true} = {}) {
  const snapshot = {config: {decision: {
    provider: 'openrouter', enabled: true, has_api_key: true,
    model: 'vendor/original', base_url: 'https://router.example/v1',
  }}};
  const patches = [];
  const calls = [];
  const context = vm.createContext({});
  const msg = (key, fallback) => ({key, fallback});
  const resolve = spec => typeof spec === 'string' ? spec : spec?.fallback || spec?.key || '';
  const el = (tag, options = {}, children = []) => ({
    tag, options, children, value: options.attrs?.value || '', listeners: {}, dataset: {},
    addEventListener(name, listener) { this.listeners[name] = listener; },
    setAttribute(name, value) { (this.options.attrs ||= {})[name] = value; },
    removeAttribute(name) { delete this.options.attrs?.[name]; },
    classList: {toggle() {}},
    scrollIntoView() {},
  });
  const mocks = {
    '../data.js': {
      ensureSnapshot: async () => snapshot,
      adoptSaved: target => payload => { target.config = payload.config; },
      request: async (path, options) => {
        calls.push({path, body: options?.body ? JSON.parse(options.body) : null});
        if (path.startsWith('/api/models?')) {
          if (!catalogAvailable) throw new Error('catalog unavailable');
          return {models: [
            {id: 'vendor/jev-special', name: 'Vendor: Jev'},
            {id: 'vendor/other-decision', name: 'Other Decision'},
            {id: 'vendor/jev-next', name: 'Vendor: Jev Next'},
          ]};
        }
        return {ok: true};
      },
      t: (key, options) => options?.defaultValue || key,
    },
    '../../config/i18n.js': {getActiveLocale: () => 'en'},
    '../saver.js': {createSaver: (_, options) => ({
      refreshChrome() {},
      async change(key, patch) {
        patches.push({key, patch});
        options.onSaved({config: {decision: {...snapshot.config.decision, ...patch.decision}}});
        return 'saved';
      },
    })},
    '../../ui/brand.js': {providerMark() {}},
    '../../ui/button.js': {button: options => ({...options})},
    '../../ui/choice-sheet.js': {choiceSheet() {}},
    '../../ui/confirm.js': {confirmSheet: async () => true},
    '../../ui/swipe.js': {swipeRow() {}},
    '../../ui/dom.js': {el, replace: (node, children) => { node.children = children; }},
    '../../ui/icon.js': {icon() {}},
    '../../ui/list.js': {
      group: options => ({children: options.rows}),
      row: options => ({...el('row'), ...options}),
      screen: children => ({children}),
    },
    '../../ui/switch.js': {setChecked() {}, switchControl: options => ({...options})},
    '../../ui/text.js': {msg, resolve, text: spec => ({spec})},
    '../../ui/toast.js': {toast() {}},
  };
  const synthetic = specifier => {
    const values = mocks[specifier];
    assert.ok(values, `unexpected import: ${specifier}`);
    return new vm.SyntheticModule(Object.keys(values), function () {
      for (const [name, value] of Object.entries(values)) this.setExport(name, value);
    }, {context});
  };
  const fields = new vm.SourceTextModule(fieldsSource, {context});
  await fields.link(specifier => synthetic(specifier.replace('../ui/', '../../ui/')));
  const page = new vm.SourceTextModule(pageSource, {context});
  await page.link(specifier => specifier === '../fields.js' ? fields : synthetic(specifier));
  await page.evaluate();
  return {page: await page.namespace.decisionPage({}), patches, calls};
}

function find(node, action) {
  if (!node) return undefined;
  if (typeof action === 'function' ? action(node) : node.options?.data?.action === action || node.action === action) return node;
  for (const child of [...(node.children || []), node.accessory]) {
    const found = find(child, action);
    if (found) return found;
  }
  return undefined;
}

test('OpenRouter model suggestions assist free input without changing the saved key', async () => {
  const {page, patches, calls} = await loadDecisionPage();
  const model = find(page, 'edit-decision.model');
  model.value = 'jev';
  await model.listeners.focus();
  assert.equal(new URL(calls[0].path, 'https://device.local').searchParams.get('output_modalities'), 'decisions');
  const suggestions = find(page, node => node.options?.attrs?.id === model.options.attrs['aria-controls']);
  assert.deepEqual(suggestions.children.map(option => option.options.data.value), ['vendor/jev-special', 'vendor/jev-next']);
  assert.equal(model.value, 'jev', 'loading suggestions must not replace typed text');
  assert.equal(patches.length, 0, 'suggestions must not save a model automatically');
  await suggestions.children[0].options.on.click();
  const baseURL = find(page, 'edit-decision.base_url');
  baseURL.value = 'https://new-router.example/v1';
  await baseURL.listeners.change();
  assert.deepEqual(JSON.parse(JSON.stringify(patches)), [
    {key: 'decision.model', patch: {decision: {model: 'vendor/jev-special'}}},
    {key: 'decision.base_url', patch: {decision: {base_url: 'https://new-router.example/v1'}}},
  ]);
  await find(page, 'test-decision-connection').options.on.click();
  assert.equal(calls.at(-1).path, '/api/config/test');
  assert.equal(calls.at(-1).body.section, 'decision');
  assert.equal(calls.at(-1).body.values.model, 'vendor/jev-special');
  assert.equal(calls.at(-1).body.values.base_url, 'https://new-router.example/v1');
  assert.equal(calls.at(-1).body.values.has_api_key, true);
  assert.equal(Object.hasOwn(calls.at(-1).body.values, 'api_key'), false);
  const customModel = find(page, 'edit-decision.model');
  customModel.value = 'vendor/custom-decision';
  await customModel.listeners.change();
  assert.equal(patches.at(-1).patch.decision.model, 'vendor/custom-decision');
  const resetModel = find(page, 'edit-decision.model');
  resetModel.value = '';
  await resetModel.listeners.change();
  assert.equal(patches.at(-1).patch.decision.model, null, 'clearing the input must restore Jev');
});

test('model suggestions support keyboard navigation, Escape, blur, and input composition', async () => {
  const {page, patches} = await loadDecisionPage();
  const model = find(page, 'edit-decision.model');
  model.value = 'jev';
  await model.listeners.focus();
  const list = find(page, node => node.options?.attrs?.role === 'listbox');
  const key = (value, options = {}) => model.listeners.keydown({key: value, preventDefault() {}, ...options});
  key('ArrowDown', {isComposing: true});
  assert.equal(model.options.attrs['aria-activedescendant'], undefined);
  key('ArrowDown');
  assert.equal(list.children[0].options.attrs['aria-selected'], 'true');
  key('Escape');
  assert.equal(list.hidden, true);
  assert.equal(patches.length, 0);
  key('ArrowUp');
  assert.equal(list.children[1].options.attrs['aria-selected'], 'true');
  key('Enter');
  assert.equal(patches.at(-1).patch.decision.model, 'vendor/jev-next');
  assert.equal(list.hidden, true);
  const current = find(page, 'edit-decision.model');
  await current.listeners.focus();
  current.listeners.blur();
  assert.equal(current.options.attrs['aria-expanded'], 'false');
  current.value = 'unknown';
  current.listeners.input();
  assert.equal(current.options.attrs['aria-expanded'], 'false');
});

test('a failed model catalog does not prevent saving a model ID or clearing the Base URL', async () => {
  const {page, patches} = await loadDecisionPage({catalogAvailable: false});
  const model = find(page, 'edit-decision.model');
  await model.listeners.focus();
  model.value = '  vendor/custom-decision  ';
  await model.listeners.change();
  const baseURL = find(page, 'edit-decision.base_url');
  baseURL.value = '';
  await baseURL.listeners.change();
  assert.deepEqual(JSON.parse(JSON.stringify(patches)), [
    {key: 'decision.model', patch: {decision: {model: 'vendor/custom-decision'}}},
    {key: 'decision.base_url', patch: {decision: {base_url: null}}},
  ]);
});
