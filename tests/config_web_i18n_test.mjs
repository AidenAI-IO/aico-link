import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import process from 'node:process';
import vm from 'node:vm';
import {fileURLToPath, pathToFileURL} from 'node:url';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const webRoot = path.join(repositoryRoot, 'src/config_web/web');
const i18nPath = path.join(webRoot, 'assets/js/config/i18n.js');

class Element {
  constructor(attributes = {}) {
    this.attributes = new Map(Object.entries(attributes));
    this.dataset = {};
    this.children = [];
    this.textContent = '';
    this.value = '';
    this.disabled = false;
    this._classes = new Set();
    this.className = '';
    this.style = {};
    this.scrollTop = 0;
    this.clientHeight = 0;
    this.scrollHeight = 0;
    this.classList = {
      add: (...names) => names.forEach((name) => this._classes.add(name)),
      remove: (...names) => names.forEach((name) => this._classes.delete(name)),
      contains: (name) => this._classes.has(name),
      toggle: (name, force) => {
        const enabled = force === undefined ? !this._classes.has(name) : !!force;
        if (enabled) this._classes.add(name);
        else this._classes.delete(name);
        return enabled;
      },
    };
  }

  get className() {
    return [...this._classes].join(' ');
  }

  set className(value) {
    this._classes = new Set(String(value || '').split(/\s+/).filter(Boolean));
  }

  get textContent() {
    if (this.children.length) return this.children.map((child) => child.textContent).join('');
    return this._textContent;
  }

  set textContent(value) {
    this._textContent = String(value);
    if (this.children) this.children = [];
  }

  getAttribute(name) {
    return this.attributes.has(name) ? this.attributes.get(name) : null;
  }

  setAttribute(name, value) {
    this.attributes.set(name, String(value));
    if (name === 'class') this.className = value;
    if (name.startsWith('data-')) {
      const key = name.slice(5).replace(/-([a-z])/g, (_match, letter) => letter.toUpperCase());
      this.dataset[key] = String(value);
    }
  }

  removeAttribute(name) {
    this.attributes.delete(name);
  }

  appendChild(child) {
    if (child && child.isFragment) {
      child.children.forEach((item) => { item.parentNode = this; });
      this.children.push(...child.children);
    } else {
      if (child) child.parentNode = this;
      this.children.push(child);
    }
    return child;
  }

  contains(node) {
    for (let current = node; current; current = current.parentNode) {
      if (current === this) return true;
    }
    return false;
  }

  click() {}
  remove() {}
}

const title = new Element({'data-i18n': 'page.title'});
const password = new Element({'data-i18n-placeholder': 'wifi.password_optional'});
const localeSelect = new Element();
const elements = [title, password];
const elementsById = new Map([['localeSelect', localeSelect]]);
const eventListeners = new Map();
const stored = new Map();
let selectionNode = null;
let fetchImpl = async () => { throw new Error('fetch is not configured'); };
const document = {
  title: '',
  documentElement: {lang: ''},
  body: new Element(),
  getElementById(id) {
    return elementsById.get(id) || null;
  },
  querySelectorAll(selector) {
    const attribute = selector.slice(1, -1);
    return elements.filter((element) => element.attributes.has(attribute));
  },
  querySelector() {
    return null;
  },
  addEventListener(type, listener) {
    const listeners = eventListeners.get(type) || [];
    listeners.push(listener);
    eventListeners.set(type, listeners);
  },
  dispatchEvent(event) {
    (eventListeners.get(event.type) || []).forEach((listener) => listener(event));
  },
  createDocumentFragment() {
    return {isFragment: true, children: [], appendChild(child) { this.children.push(child); }};
  },
  createElement() {
    return new Element();
  },
  getSelection() {
    if (!selectionNode) return {rangeCount: 0, isCollapsed: true};
    return {
      rangeCount: 1,
      isCollapsed: false,
      getRangeAt: () => ({commonAncestorContainer: selectionNode}),
    };
  },
};
class CustomEvent {
  constructor(type, options = {}) {
    this.type = type;
    this.detail = options.detail;
  }
}
const context = vm.createContext({
  CustomEvent,
  URL: {
    createObjectURL: () => 'blob:test',
    revokeObjectURL() {},
  },
  console,
  document,
  fetch: (...args) => fetchImpl(...args),
  setTimeout() {},
  localStorage: {
    getItem(key) { return stored.get(key) || null; },
    setItem(key, value) { stored.set(key, value); },
  },
});
const moduleCache = new Map();

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return {promise, resolve, reject};
}

async function loadModule(filePath) {
  const absolutePath = path.resolve(filePath);
  if (moduleCache.has(absolutePath)) return moduleCache.get(absolutePath);
  const source = await fs.readFile(absolutePath, 'utf8');
  const module = new vm.SourceTextModule(source, {
    context,
    identifier: pathToFileURL(absolutePath).href,
  });
  moduleCache.set(absolutePath, module);
  await module.link(async (specifier, referencingModule) => {
    const referencingPath = fileURLToPath(referencingModule.identifier);
    const modulePath = specifier.split('?', 1)[0];
    return loadModule(path.resolve(path.dirname(referencingPath), modulePath));
  });
  return module;
}

const i18nModule = await loadModule(i18nPath);
await i18nModule.evaluate();
const {applyLocale, getActiveLocale, getPersistedLocale, initI18n, saveLocale, t} = i18nModule.namespace;

assert.equal(t('config.save_failed', {section: 'agent'}), 'Save [agent] failed.');
initI18n();
assert.equal(document.documentElement.lang, 'en-US');
assert.equal(document.title, 'Aico Link Setup');
assert.equal(title.textContent, 'Device Settings');
assert.equal(password.getAttribute('placeholder'), 'Open network can leave empty');

applyLocale('zh-CN', false);
assert.equal(t('config.save_failed', {section: 'agent'}), '保存 [agent] 失败。');
assert.equal(t('wifi.connected_to', {ssid: 'Aico Lab'}), '已连接到“Aico Lab”。');
assert.equal(
  t('config.fields.device.device_type.help'),
  'Android 使用 HID touchscreen 模式。iOS、macOS、windows 和 linux 使用 absolute 指针模式。',
);
assert.equal(t('config.fields.device.device_type.label'), '设备类型');
assert.equal(t('config.fields.device.device_type.options.android'), 'Android');
assert.equal(t('config.fields.device.device_type.options.macos'), 'macOS');
assert.equal(t('config.fields.hid.input_backend.label'), '输入后端');
assert.equal(t('config.fields.hid.input_backend.options.hid'), 'USB HID');
assert.equal(t('config.fields.agent.input_mode.label'), '语音模式');
assert.equal(t('config.fields.agent.input_mode.options.realtime'), '实时模式');
assert.equal(t('config.default_value', {value: '16000'}), '默认值：16000');
assert.equal(t('config.fields.model.api_mode.label'), '对话接口');
assert.equal(t('config.fields.model.api_mode.options.responses'), 'Responses（本地上下文）');
assert.equal(t('config.fields.model.responses_context_management.label'), '服务端自动压缩');
assert.equal(t('config.fields.model.responses_truncation.options.auto'), '自动丢弃最早输入');
assert.equal(t('config.fields.model.responses_include.label'), '额外返回字段');
assert.equal(t('config.fields.agent.context_prune_threshold.label'), '历史上下文清理阈值（比例）');
assert.equal(
  t('config.fields.agent.context_prune_threshold.help'),
  '上下文 token 数达到模型可用输入预算的该比例时，清理过期 state 和历史工具结果，并清理至触发值的 6/7。取值需为 0，或大于 0 且小于 1；设为 0 时使用 0.5。该值不会超过 context_compaction_threshold，以保证这个低成本清理先于对话压缩执行。',
);
assert.equal(t('config.fields.agent.context_prune_threshold.placeholder'), '0 = 自动（0.5）');
assert.equal(t('logs.jump_to_bottom'), '跳到底部');
assert.equal(t('system_env.saved'), 'env 已保存。');
assert.equal(t('wifi.proxy_url_help'), '支持的 URL：socks5://、socks5h://、http://、https://');
assert.equal(t('missing.translation.key'), 'missing.translation.key');

applyLocale('en-US', true);
assert.equal(document.documentElement.lang, 'en-US');
assert.equal(title.textContent, 'Device Settings');
assert.equal(password.getAttribute('placeholder'), 'Open network can leave empty');
assert.equal(t('config.fields.model.responses_compact_threshold.label'), 'Compaction threshold (tokens)');
assert.equal(t('wifi.proxy_url_help'), 'Supported URLs: socks5://, socks5h://, http://, https://');
assert.equal(t('config.fields.device.device_type.label'), 'Device type');
assert.equal(t('config.fields.device.device_type.options.windows'), 'Windows');
assert.equal(t('config.fields.hid.input_backend.label'), 'Input backend');
assert.equal(t('config.fields.agent.input_mode.label'), 'Voice mode');
assert.equal(stored.get('aico.config.locale'), 'en-US');

const source = await fs.readFile(i18nPath, 'utf8');
assert.doesNotMatch(source, /MutationObserver/);
assert.doesNotMatch(source, /translateDynamicText/);
assert.doesNotMatch(source, /const\s+zhText\s*=/);

// Every message key the settings pages name literally must exist in both
// catalogues; a missing one would silently show the Chinese fallback text to
// an English reader (or the other way round). Keys built at runtime (schema
// field labels, backup phases) fall back by design and are not listed here.
const sourceRoots = ['assets/js/app', 'assets/js/ui'].map((dir) => path.join(webRoot, dir));
async function sourceFiles(dir) {
  const entries = await fs.readdir(dir, {withFileTypes: true});
  const nested = await Promise.all(entries.map((entry) => {
    const full = path.join(dir, entry.name);
    return entry.isDirectory() ? sourceFiles(full) : [full];
  }));
  return nested.flat().filter((file) => file.endsWith('.js'));
}
const usedKeys = new Set();
for (const root of sourceRoots) {
  for (const file of await sourceFiles(root)) {
    const text = await fs.readFile(file, 'utf8');
    for (const match of text.matchAll(/\b(?:msg|t)\(\s*'([a-z0-9_]+(?:\.[a-z0-9_]+)+)'/g)) usedKeys.add(match[1]);
  }
}
assert.ok(usedKeys.size > 100, `expected the settings pages to name many keys, found ${usedKeys.size}`);
const MISSING = '\u0000missing';
for (const locale of ['en-US', 'zh-CN']) {
  applyLocale(locale, false);
  const missing = [...usedKeys].filter((key) => t(key, {defaultValue: MISSING}) === MISSING).sort();
  assert.deepEqual(missing, [], `${locale} is missing keys used by the settings pages`);
}
applyLocale('en-US', false);

process.stdout.write('config web i18n tests passed\n');
