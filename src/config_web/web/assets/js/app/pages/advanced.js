/**
 * Advanced routes.
 *
 *   /advanced             entries: logs, manual configuration
 *   /advanced/logs        live logs, raw model HTTP logging, log level, retention, export
 *   /advanced/logs/agent  the Agent log, tailed live, with the Agent's run state
 *   /advanced/logs/level  log level choice
 *   /advanced/config      the whole grouped Agent TOML, edited in place
 *
 * Log settings apply live and save on change. The TOML editor is a form: its
 * 保存 appears in the top bar (or the app's native bar) once the text differs
 * from what was loaded, and saving goes through `PUT /api/config/backup`,
 * which validates the file before replacing it — the same path the classic
 * page's manual editor uses.
 */

import {adoptSaved, ensureSnapshot, request, t} from '../data.js';
import {isEmbedded} from '../../ui/environment.js';
import {postToHost} from '../host.js';
import {createSaver} from '../saver.js';
import {choiceSheet} from '../../ui/choice-sheet.js';
import {el, replace} from '../../ui/dom.js';
import {icon} from '../../ui/icon.js';
import {group, row, screen} from '../../ui/list.js';
import {setChecked, switchControl} from '../../ui/switch.js';
import {msg, resolve, text} from '../../ui/text.js';
import {toast} from '../../ui/toast.js';
import {tomlEditor} from '../../ui/toml-editor.js';

/** `log.level` values, as the Agent's schema enumerates them. */
export const LOG_LEVELS = [
  {value: 'debug', label: msg('advanced.level_debug', '调试')},
  {value: 'info', label: msg('advanced.level_info', '信息')},
  {value: 'warn', label: msg('advanced.level_warn', '警告')},
  {value: 'error', label: msg('advanced.level_error', '错误')},
];

/** Raw model HTTP log retention presets, in days (`log.llm_http_retention_days`). */
export const RETENTION_PRESETS = [1, 3, 7, 14, 30];

const days = n => t('memory.days', {n, defaultValue: '{{n}} 天'});

/** How often the live Agent log is re-read while its page is open. */
const AGENT_LOG_POLL_MS = 2000;

/**
 * The Agent status is re-read every this many log polls (about ten seconds):
 * `/api/device/status` runs the Agent init script's `status` on the board, which
 * is too heavy to repeat every two seconds.
 */
const AGENT_STATUS_EVERY_POLLS = 5;

/** A card followed by a footnote, the pattern the log settings use. */
function noted(card, note) {
  card.classList.add('ds-group--noted');
  return el('div', {}, [card, text(note, 'page__note page__note--below')]);
}

/**
 * The board's web terminal (ttyd). Over USB it is served on port 3000, which
 * the companion app recognises and opens in its own terminal screen; any
 * other address reaches it through the Agent on 8080, as the classic page did.
 */
export function terminalUrl(location = window.location) {
  const url = new URL(location.href);
  url.port = url.hostname === '192.168.42.1' ? '3000' : '8080';
  url.pathname = '/webtty/';
  url.search = '';
  url.hash = '';
  return url.href;
}

function terminalRow() {
  const node = row({
    label: msg('advanced.terminal', '终端'),
    value: msg('advanced.terminal_hint', '设备命令行'),
    chevron: true,
    action: 'open-terminal',
    onPress: () => {
      // A new tab in a browser; inside the app this navigation is intercepted
      // and opens the native terminal screen.
      if (isEmbedded()) window.location.href = terminalUrl();
      else window.open(terminalUrl(), '_blank', 'noopener');
    },
  });
  return node;
}

export async function advancedPage(context) {
  const snapshot = await ensureSnapshot(context);
  const body = el('div');
  const saver = createSaver(context, {
    title: msg('ui.nav_advanced', '高级设置'),
    back: '/',
    root: body,
    onSaved: adoptSaved(snapshot, () => render()),
  });
  const mcpSaved = () => snapshot?.config?.agent?.mcp_enabled !== false;
  let mcpDraft = mcpSaved();
  let mcpSaving = false;

  async function saveMCP(next) {
    if (mcpSaving) return;
    const previous = mcpDraft;
    mcpDraft = next;
    mcpSaving = true;
    render();
    try {
      const outcome = await saver.change('agent.mcp_enabled', {agent: {mcp_enabled: next}});
      if (outcome === 'invalid' || outcome === 'failed') mcpDraft = previous;
      else mcpDraft = mcpSaved();
    } finally {
      mcpSaving = false;
      render();
    }
  }

  function render() {
    const mcpToggle = switchControl({
      checked: mcpDraft,
      label: msg('advanced.mcp_enabled', 'MCP'),
      onChange: next => {
        if (mcpSaving) {
          setChecked(mcpToggle, mcpDraft);
          return;
        }
        saveMCP(next);
      },
    });
    if (mcpSaving) mcpToggle.disabled = true;
    replace(body, [
      group({
        rows: [
          row({
            label: msg('advanced.logs', '日志'),
            value: msg('advanced.logs_hint', '管理日志记录和导出'),
            chevron: true,
            action: 'open-logs',
            onPress: () => context.navigate('/advanced/logs'),
          }),
          row({
            label: msg('advanced.manual_config', '手动编辑配置'),
            value: msg('advanced.manual_config_hint', '编辑 Agent TOML 配置文件'),
            chevron: true,
            action: 'open-manual-config',
            onPress: () => context.navigate('/advanced/config'),
          }),
          terminalRow(),
        ],
      }),
      group({
        rows: [
          row({
            label: msg('advanced.mcp_enabled', 'MCP'),
            description: msg('advanced.mcp_enabled_help', '开启后可通过 MCP 调用 Agent 工具。'),
            accessory: mcpToggle,
            action: 'toggle-mcp-enabled',
          }),
        ],
      }),
    ]);
    saver.refreshChrome();
  }
  render();
  return screen([body]);
}

export async function logsPage(context) {
  const snapshot = await ensureSnapshot(context);
  const body = el('div');
  const saver = createSaver(context, {
    title: msg('advanced.logs', '日志'),
    back: '/advanced',
    root: body,
    onSaved: adoptSaved(snapshot, () => render()),
  });
  const config = () => snapshot.config || {};

  async function exportLogs(trigger) {
    if (isEmbedded()) {
      // The archive is binary and a WebView cannot save a download, so the app
      // opens the board URL in the system browser, which can.
      postToHost({type: 'aico_open_url', url: new URL('/api/logs/support', window.location.href).href});
      return;
    }
    trigger.disabled = true;
    try {
      const response = await fetch('/api/logs/support', {cache: 'no-store'});
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const match = (response.headers.get('Content-Disposition') || '').match(/filename="([^"]+)"/i);
      const url = URL.createObjectURL(await response.blob());
      const link = el('a', {attrs: {href: url, download: match ? match[1] : 'aico-logs.tar.gz'}});
      document.body.appendChild(link);
      link.click();
      link.remove();
      // WebKit starts the download after this tick; revoking now can cancel it.
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    } catch (error) {
      toast(error && error.message ? error.message : resolve(msg('advanced.export_failed', '导出日志失败')));
    } finally {
      trigger.disabled = false;
    }
  }

  function render() {
    const raw = Boolean((config().model || {}).log_raw_http);
    const level = (config().log || {}).level || 'info';
    const retention = (config().log || {}).llm_http_retention_days;
    const levelEntry = LOG_LEVELS.find(entry => entry.value === level);

    const toggle = switchControl({
      checked: raw,
      label: msg('advanced.raw_http', '记录详细模型请求'),
      onChange: async next => {
        const outcome = await saver.change('model.log_raw_http', {model: {log_raw_http: next}});
        if (outcome === 'invalid' || outcome === 'failed') setChecked(toggle, !next);
      },
    });
    const presets = retention != null && !RETENTION_PRESETS.includes(retention) ? [retention, ...RETENTION_PRESETS] : RETENTION_PRESETS;
    const exportButton = el('button', {class: 'ds-card-action', attrs: {type: 'button'}, data: {action: 'export-logs'}, on: {click: () => exportLogs(exportButton)}},
      [text(msg('advanced.export_logs', '导出日志'))]);

    replace(body, [
      group({rows: [
        row({
          label: msg('advanced.agent_log', '实时 Agent 日志'),
          chevron: true,
          action: 'open-agent-log',
          onPress: () => context.navigate('/advanced/logs/agent'),
        }),
        row({
          label: msg('advanced.llm_logs', '模型请求日志'),
          chevron: true,
          action: 'open-llm-logs',
          // A standalone page outside the settings shell, as on the classic page.
          onPress: () => { window.location.href = '/llm-logs'; },
        }),
      ]}),
      noted(group({rows: [row({label: msg('advanced.raw_http', '记录详细模型请求'), accessory: toggle})]}),
        msg('advanced.raw_http_help', '将模型原始 HTTP 请求和响应写入 Agent 日志目录，仅建议排查问题时启用。')),
      noted(group({rows: [row({
        label: msg('advanced.log_level', '日志等级'),
        value: levelEntry ? levelEntry.label : level,
        chevron: true,
        action: 'open-log-level',
        onPress: () => context.navigate('/advanced/logs/level'),
      })]}), msg('advanced.log_level_help', '只记录该等级及以上的 Agent 日志。')),
      noted(group({rows: [row({
        label: msg('advanced.retention', '日志保留天数'),
        value: retention == null ? '' : days(retention),
        chevron: true,
        action: 'open-log-retention',
        onPress: () => choiceSheet({
          options: presets.map(value => ({value, label: days(value)})),
          selected: retention,
          onSelect: value => saver.change('log.llm_http_retention_days', {log: {llm_http_retention_days: value}}),
        }),
      })]}), msg('advanced.retention_help', '详细模型请求日志在自动清理前保留的天数。')),
      el('div', {class: 'ds-group'}, [exportButton]),
    ]);
    saver.refreshChrome();
  }

  render();
  return screen([body]);
}

/** `2026-09-30T08:34:41Z [INFO][agent][server] message`, the Agent's compact record. */
const AGENT_LOG_RECORD = /^(\S+Z) \[(DEBUG|INFO|WARN|ERROR)\]((?:\[[^\]]*\])*)(.*)$/;

/**
 * Agent log text as coloured lines. A line that is not a record (a wrapped
 * message, a stack trace) keeps the severity of the record above it.
 */
export function agentLogLines(logText) {
  let severity = 'info';
  return String(logText).split(/\r?\n/).map(line => {
    const record = line.match(AGENT_LOG_RECORD);
    if (!record) {
      if (/\bpanic\b|^goroutine \d+/.test(line)) severity = 'error';
      return el('span', {class: `ds-log__line ds-log__line--${severity}`}, [line]);
    }
    severity = record[2].toLowerCase();
    return el('span', {class: `ds-log__line ds-log__line--${severity}`}, [
      el('span', {class: 'ds-log__time'}, [record[1]]),
      ' ',
      el('span', {class: `ds-log__level ds-log__level--${severity}`}, [`[${record[2]}]`]),
      el('span', {class: 'ds-log__scope'}, [record[3]]),
      record[4],
    ]);
  });
}

/** The Agent's run state from `GET /api/device/status`, as one row. */
function agentStatusRow(status) {
  const running = Boolean(status.process_running);
  const detail = status.startup_error || (running && !status.port_reachable ? status.port_detail : '');
  return row({
    label: msg('advanced.agent_status', 'Agent 状态'),
    value: running ? msg('advanced.agent_running', '运行中') : msg('advanced.agent_stopped', '未运行'),
    tone: running ? 'success' : 'danger',
    description: detail || (running && status.pid ? `PID ${status.pid}` : null),
    descriptionTone: detail ? 'code' : undefined,
  });
}

export async function agentLogPage(context) {
  const body = el('div', {class: 'page__fill'});
  const saver = createSaver(context, {title: msg('advanced.agent_log', '实时 Agent 日志'), back: '/advanced/logs', root: body});
  const statusGroup = el('div');
  const pre = el('pre', {class: 'ds-log', data: {action: 'agent-log'}});
  let timer = null;
  let shown = null;
  let polls = 0;
  let statusInFlight = false;

  // Replacing the text would drop a selection the user is making to copy it.
  const selecting = () => {
    const selection = window.getSelection ? window.getSelection() : null;
    return Boolean(selection && !selection.isCollapsed && pre.contains(selection.anchorNode));
  };

  // The page is attached a few frames after it renders; wait for a laid-out box.
  function toBottom(frames = 60) {
    if (pre.isConnected && pre.clientHeight) pre.scrollTop = pre.scrollHeight;
    else if (frames > 0) requestAnimationFrame(() => toBottom(frames - 1));
  }

  async function loadStatus() {
    if (statusInFlight) return;
    statusInFlight = true;
    const payload = await request('/api/device/status').catch(() => null);
    statusInFlight = false;
    // A failed refresh keeps the last known state rather than blanking the row.
    if (payload) replace(statusGroup, [group({rows: [agentStatusRow(payload.agent_status || {})]})]);
  }

  // The shell attaches the page as soon as this function returns, so by the
  // first tick a detached page has been left (or was never shown): stop.
  function schedule() {
    timer = setTimeout(() => {
      if (!body.isConnected) return;
      polls += 1;
      if (polls % AGENT_STATUS_EVERY_POLLS === 0) loadStatus();
      load();
    }, AGENT_LOG_POLL_MS);
  }

  async function load() {
    clearTimeout(timer);
    try {
      const snapshot = (await request('/api/logs/agent')).agent_log || {};
      const logText = (snapshot.log || '').trim();
      if (logText !== shown && !selecting()) {
        // Nothing is measurable before the first render: open on the newest line.
        const stick = shown === null || pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 16;
        if (logText) replace(pre, agentLogLines(logText));
        else pre.textContent = snapshot.error || resolve(msg('advanced.agent_log_empty', '暂无日志'));
        shown = logText;
        if (stick) toBottom();
      }
    } catch (error) {
      if (!pre.textContent) pre.textContent = error && error.message ? error.message : resolve(msg('advanced.agent_log_failed', '读取日志失败'));
    }
    schedule();
  }

  replace(body, [statusGroup, el('div', {class: 'ds-group'}, [el('div', {class: 'ds-log-frame ds-log-frame--bounded'}, [pre])])]);
  saver.refreshChrome();
  // Mount first: waiting here would hold the page off screen for as long as
  // the slower of the two requests takes.
  loadStatus();
  load();
  return screen([body], 'ds-screen--sticky');
}

export async function logLevelPage(context) {
  const snapshot = await ensureSnapshot(context);
  const body = el('div');
  const saver = createSaver(context, {title: msg('advanced.log_level', '日志等级'), back: '/advanced/logs', root: body, onSaved: adoptSaved(snapshot)});
  const current = ((snapshot.config || {}).log || {}).level || 'info';
  replace(body, [group({
    rows: LOG_LEVELS.map(entry => row({
      label: entry.label,
      action: `choose-log-level-${entry.value}`,
      accessory: entry.value === current ? icon('check', {class: 'ds-choice-row__check', size: 22}) : null,
      onPress: async () => {
        if (entry.value === current) {
          context.navigate('/advanced/logs', {replace: true});
          return;
        }
        const outcome = await saver.change('log.level', {log: {level: entry.value}});
        if (outcome === 'saved' || outcome === 'staged') context.navigate('/advanced/logs', {replace: true});
      },
    })),
  })]);
  saver.refreshChrome();
  return screen([body]);
}

export async function manualConfigPage(context) {
  const body = el('div', {class: 'page__fill'});
  let loaded = '';
  let submitting = false;
  let editor = null;
  // Set only once the saved file was read. Saving replaces the whole
  // configuration, so an editor that failed to load must never offer it.
  let editable = false;
  const dirty = () => editable && Boolean(editor) && editor.textarea.value !== loaded;
  const saver = createSaver(context, {
    title: msg('advanced.manual_config', '手动编辑配置'),
    back: '/advanced',
    root: body,
    form: {ready: () => !submitting && dirty(), submit},
  });

  async function submit() {
    if (submitting || !dirty()) return;
    submitting = true;
    saver.refreshChrome();
    const value = editor.textarea.value;
    try {
      const payload = await request('/api/config/backup', {
        method: 'PUT',
        headers: {'Content-Type': 'application/toml; charset=utf-8'},
        body: value,
      });
      loaded = value;
      toast(payload.pending ? msg('manual_config.saved_pending', 'Agent TOML 配置已保存，正在等待应用。') : msg('manual_config.saved', 'Agent TOML 配置已保存。'));
    } catch (error) {
      if (error && error.persisted) {
        loaded = value;
        toast(msg('manual_config.saved_not_applied', 'Agent TOML 配置已保存，但未能应用。'), {durationMs: 5000});
      } else {
        toast(error && error.message ? error.message : resolve(msg('manual_config.save_failed', '保存 Agent TOML 配置失败。')), {durationMs: 6000});
      }
    } finally {
      submitting = false;
      saver.refreshChrome();
    }
  }

  let source = '';
  try {
    const response = await fetch('/api/config/backup', {cache: 'no-store'});
    if (!response.ok) {
      const payload = await response.json().catch(() => ({}));
      throw new Error(payload.error || `HTTP ${response.status}`);
    }
    source = await response.text();
    editable = true;
  } catch (error) {
    toast(error && error.message ? error.message : resolve(msg('manual_config.load_failed', '加载配置失败。')), {durationMs: 5000});
  }
  loaded = source;
  editor = tomlEditor({value: source, label: resolve(msg('advanced.manual_config', '手动编辑配置')), onInput: () => saver.refreshChrome()});
  editor.textarea.readOnly = !editable;

  replace(body, [
    el('div', {class: 'ds-group ds-group--fill'}, [
      text(editable
        ? msg('advanced.manual_config_help', '编辑完整的分组式 Agent TOML，保存前会先校验配置。')
        : msg('manual_config.load_failed_help', '当前配置未能读取，为避免覆盖现有配置，此处不可编辑。请返回后重试。'), 'ds-group__intro'),
      el('div', {class: 'ds-toml-frame'}, [editor.el]),
    ]),
  ]);
  saver.refreshChrome();
  return screen([body], 'ds-screen--sticky');
}
