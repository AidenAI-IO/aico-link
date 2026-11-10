/**
 * MCP route.
 *
 *   /advanced/mcp   the MCP switch and MCP access tokens
 *
 * Every MCP client needs an MCP access token, whether the computer is plugged
 * straight into Aico Link or on the same Wi-Fi, so this page is the one place
 * to set a client up. Over Wi-Fi, LAN access must also be on. A token reaches
 * only the Agent's /mcp, does not expire, survives a password change, and is
 * revoked here on its own. The secret is shown once, when it is created,
 * together with a ready-to-paste client configuration.
 */

import {adoptSaved, ensureSnapshot, request} from '../data.js';
import {createSaver} from '../saver.js';
import {button} from '../../ui/button.js';
import {confirmSheet} from '../../ui/confirm.js';
import {el, replace} from '../../ui/dom.js';
import {icon} from '../../ui/icon.js';
import {group, row, screen} from '../../ui/list.js';
import {sheet} from '../../ui/sheet.js';
import {setChecked, switchControl} from '../../ui/switch.js';
import {msg, resolve, text} from '../../ui/text.js';
import {toast} from '../../ui/toast.js';

const MCP_TOKENS_API = '/api/security/mcp-tokens';

function fetchTokens() {
  return request(MCP_TOKENS_API, {cache: 'no-store'});
}

function createToken(name) {
  return request(MCP_TOKENS_API, {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({name}),
  });
}

function revokeToken(id) {
  return request(`${MCP_TOKENS_API}/${encodeURIComponent(id)}`, {method: 'DELETE'});
}

/**
 * The MCP URL shown with a new token: the board's Wi-Fi address when it is
 * known, otherwise the address this page was opened on. A computer plugged
 * straight into Aico Link uses 192.168.42.1 instead; the sheet says so.
 */
export function wifiEndpoint(location = window.location, wifiAddress = '') {
  const host = wifiAddress || location.hostname;
  return `http://${host}:8080/mcp`;
}

/** Client configuration snippets for a new token. */
export function mcpClientSnippets(endpoint, secret) {
  const server = {type: 'http', url: endpoint, headers: {Authorization: `Bearer ${secret}`}};
  return {
    claudeCode: `claude mcp add --transport http aico ${endpoint} --header "Authorization: Bearer ${secret}"`,
    json: JSON.stringify({mcpServers: {aico: server}}, null, 1),
  };
}

async function copy(value) {
  try {
    await navigator.clipboard.writeText(value);
    return true;
  } catch (_) {
    toast(msg('mcp.copy_failed', '无法复制，请手动选择'));
    return false;
  }
}

/** A code block with a small copy button in its top-right corner. */
function codeBlock(value, action) {
  const copyButton = el('button', {
    class: 'ds-code__copy',
    attrs: {type: 'button', 'aria-label': resolve(msg('mcp.copy', '复制')), title: resolve(msg('mcp.copy', '复制'))},
    data: {action},
  }, [icon('copy', {size: 16})]);
  let reset = null;
  copyButton.addEventListener('click', async () => {
    if (!(await copy(value))) return;
    replace(copyButton, [icon('check', {size: 14})]);
    copyButton.classList.add('ds-code__copy--done');
    clearTimeout(reset);
    reset = setTimeout(() => {
      replace(copyButton, [icon('copy', {size: 16})]);
      copyButton.classList.remove('ds-code__copy--done');
    }, 1500);
  });
  return el('div', {class: 'ds-code'}, [el('pre', {class: 'ds-code__text', text: value}), copyButton]);
}

function nameInput() {
  return el('input', {
    class: 'ds-input',
    attrs: {
      type: 'text',
      maxlength: '64',
      placeholder: resolve(msg('mcp.name_placeholder', '例如：MacBook Claude Code')),
      autocapitalize: 'none',
      autocorrect: 'off',
      spellcheck: 'false',
      'aria-label': resolve(msg('mcp.name', '名称')),
    },
    data: {action: 'mcp-token-name'},
  });
}

function showSecret(secret, endpoint) {
  const snippets = mcpClientSnippets(endpoint, secret);
  sheet({
    title: msg('mcp.created_title', '令牌已创建'),
    body: [el('div', {class: 'ds-code-sheet'}, [
      text(msg('mcp.created_help', '令牌只显示这一次，请现在复制保存。'), 'ds-code-sheet__intro'),
      codeBlock(secret, 'copy-mcp-token'),
      text(msg('mcp.claude_code', 'Claude Code'), 'ds-code-sheet__label'),
      codeBlock(snippets.claudeCode, 'copy-mcp-claude'),
      text(msg('mcp.json_config', '其他客户端（JSON 配置）'), 'ds-code-sheet__label'),
      codeBlock(snippets.json, 'copy-mcp-json'),
      text(msg('mcp.direct_hint', '电脑用数据线直连 Aico Link 时，把地址换成 192.168.42.1。'), 'ds-code-sheet__label'),
    ])],
  }).open();
}

export async function mcpPage(context) {
  const snapshot = await ensureSnapshot(context);
  const body = el('div');
  const saver = createSaver(context, {
    title: msg('advanced.mcp_enabled', 'MCP'),
    back: '/advanced',
    root: body,
    onSaved: adoptSaved(snapshot, () => render()),
  });
  const mcpSaved = () => snapshot?.config?.agent?.mcp_enabled !== false;
  const wifiAddress = ((snapshot && snapshot.wifi_status) || {}).ip_address || '';
  let mcpDraft = mcpSaved();
  let mcpSaving = false;
  let state = null;
  let loadError = null;
  let busy = false;

  async function load() {
    try {
      state = await fetchTokens();
      loadError = null;
    } catch (error) {
      loadError = error;
    }
  }

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

  function askName() {
    const input = nameInput();
    const error = el('div', {class: 'ds-field__error', attrs: {'aria-live': 'polite'}});
    error.hidden = true;
    const submit = button({label: msg('mcp.create', '创建令牌'), variant: 'primary', block: true, action: 'mcp-token-submit'});
    const view = sheet({
      title: msg('mcp.new_title', '新建 MCP 访问令牌'),
      body: [el('div', {}, [
        el('div', {class: 'ds-field'}, [input, text(msg('mcp.name_help', '用来区分不同的设备或客户端'), 'ds-field__help'), error]),
        el('div', {class: 'page__footer'}, [submit]),
      ])],
    });
    submit.addEventListener('click', async () => {
      const name = input.value.trim();
      if (!name) {
        error.hidden = false;
        error.textContent = resolve(msg('mcp.name_required', '请输入名称'));
        input.focus();
        return;
      }
      submit.disabled = true;
      try {
        const created = await createToken(name);
        view.close();
        await load();
        render();
        showSecret(created.secret, wifiEndpoint(window.location, wifiAddress));
      } catch (failure) {
        error.hidden = false;
        error.textContent = failure && failure.message ? failure.message : resolve(msg('lan.save_failed', '保存失败'));
      } finally {
        submit.disabled = false;
      }
    });
    view.open();
    setTimeout(() => input.focus(), 350);
  }

  async function revoke(token) {
    const confirmed = await confirmSheet({
      title: msg('mcp.revoke_title', '撤销这个令牌？'),
      body: msg('mcp.revoke_body', '使用它的 MCP 客户端会立即无法连接。'),
      confirmLabel: msg('mcp.revoke', '撤销'),
      danger: true,
      action: 'confirm-mcp-revoke',
    });
    if (!confirmed) return;
    busy = true;
    render();
    try {
      await revokeToken(token.id);
      toast(msg('mcp.revoked', '已撤销'));
    } catch (failure) {
      toast(failure && failure.message ? failure.message : resolve(msg('lan.save_failed', '保存失败')));
    } finally {
      busy = false;
      await load();
      render();
    }
  }

  function tokenSection() {
    if (!state) {
      return group({
        rows: [row({
          label: msg('mcp.title', 'MCP 访问令牌'),
          value: msg('lan.unavailable', '无法读取'),
          tone: 'danger',
          description: loadError && loadError.message ? loadError.message : null,
        })],
      });
    }
    const tokens = Array.isArray(state.tokens) ? state.tokens : [];
    const rows = [];
    if (!state.lan_access) {
      rows.push(row({
        label: msg('lan.title', '局域网访问'),
        value: msg('lan.off', '已关闭'),
        tone: 'muted',
        chevron: true,
        action: 'open-lan-access',
        onPress: () => context.navigate('/advanced/lan'),
      }));
    }
    tokens.forEach(token => rows.push(row({
      label: token.name,
      description: new Date(token.created_at).toLocaleString(),
      value: msg('mcp.revoke', '撤销'),
      tone: 'danger',
      action: 'revoke-mcp-token',
      onPress: busy ? undefined : () => revoke(token),
    })));
    const full = tokens.length >= (state.max_tokens || 20);
    rows.push(row({
      label: msg('mcp.create', '创建令牌'),
      labelTone: 'accent',
      action: 'create-mcp-token',
      onPress: busy ? undefined : () => (full ? toast(msg('mcp.limit', '令牌数量已达上限，请先撤销一个')) : askName()),
    }));
    return group({
      rows,
      note: state.lan_access
        ? msg('mcp.help', '所有 MCP 客户端都用令牌连接，直连 Aico Link 和通过 Wi-Fi 都一样。令牌只能访问 MCP，不会过期，修改设备密码也不会失效；不再使用时请在这里撤销。')
        : msg('mcp.lan_off', '通过 Wi-Fi 连接还需要开启局域网访问；直连 Aico Link 时不需要。'),
    });
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
        rows: [row({
          label: msg('advanced.mcp_enabled', 'MCP'),
          description: msg('advanced.mcp_enabled_help', '让电脑上的 AI 助手（如 Claude Code、Cursor）调用设备工具，例如截屏、点击和输入文字。'),
          accessory: mcpToggle,
          action: 'toggle-mcp-enabled',
        })],
      }),
      mcpDraft ? tokenSection() : null,
    ].filter(Boolean));
    saver.refreshChrome();
  }

  await load();
  render();
  return screen([body]);
}
