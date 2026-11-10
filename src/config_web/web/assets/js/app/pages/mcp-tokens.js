/**
 * MCP access tokens route.
 *
 *   /advanced/lan/mcp   named tokens that let an MCP client on the LAN reach /mcp
 *
 * A token reaches only the Agent's /mcp, and only while LAN access is on. It
 * does not expire and survives a password change; each one is revoked here on
 * its own. The secret is shown once, when it is created, together with a
 * ready-to-paste client configuration. Clients connected over USB need no
 * token.
 */

import {ensureSnapshot, request} from '../data.js';
import {createSaver} from '../saver.js';
import {button} from '../../ui/button.js';
import {confirmSheet} from '../../ui/confirm.js';
import {el, replace} from '../../ui/dom.js';
import {group, row, screen} from '../../ui/list.js';
import {sheet} from '../../ui/sheet.js';
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
 * The MCP URL a LAN client uses. Over USB the page is on 192.168.42.1, which a
 * LAN client cannot reach, so the board's Wi-Fi address is used instead when
 * it is known.
 */
export function mcpEndpoint(location = window.location, wifiAddress = '') {
  const host = location.hostname === '192.168.42.1' && wifiAddress ? wifiAddress : location.hostname;
  return `http://${host}:8080/mcp`;
}

/** Client configuration snippets for a new token. */
export function mcpClientSnippets(endpoint, secret) {
  return {
    claudeCode: `claude mcp add --transport http aico ${endpoint} --header "Authorization: Bearer ${secret}"`,
    json: JSON.stringify({mcpServers: {aico: {type: 'http', url: endpoint, headers: {Authorization: `Bearer ${secret}`}}}}, null, 2),
  };
}

async function copy(value) {
  try {
    await navigator.clipboard.writeText(value);
    toast(msg('mcp.copied', '已复制'));
  } catch (_) {
    toast(msg('mcp.copy_failed', '无法复制，请手动选择'));
  }
}

function codeBlock(value, action) {
  const block = el('pre', {class: 'ds-code', text: value});
  const copyButton = button({label: msg('mcp.copy', '复制'), variant: 'secondary', action});
  copyButton.addEventListener('click', () => copy(value));
  return el('div', {class: 'ds-code-block'}, [block, copyButton]);
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
    body: [el('div', {}, [
      text(msg('mcp.created_help', '令牌只显示这一次，请现在复制保存。'), 'page__note'),
      codeBlock(secret, 'copy-mcp-token'),
      text(msg('mcp.claude_code', 'Claude Code'), 'ds-field__help'),
      codeBlock(snippets.claudeCode, 'copy-mcp-claude'),
      text(msg('mcp.json_config', '其他客户端（JSON 配置）'), 'ds-field__help'),
      codeBlock(snippets.json, 'copy-mcp-json'),
    ])],
  }).open();
}

export async function mcpTokensPage(context) {
  const body = el('div');
  const saver = createSaver(context, {title: msg('mcp.title', 'MCP 访问令牌'), back: '/advanced/lan', root: body});
  let state = null;
  let loadError = null;
  let busy = false;
  const snapshot = await ensureSnapshot(context);
  const wifiAddress = ((snapshot && snapshot.wifi_status) || {}).ip_address || '';

  async function load() {
    try {
      state = await fetchTokens();
      loadError = null;
    } catch (error) {
      loadError = error;
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
        showSecret(created.secret, mcpEndpoint(window.location, wifiAddress));
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

  function render() {
    if (!state) {
      replace(body, [group({rows: [row({
        label: msg('mcp.title', 'MCP 访问令牌'),
        value: msg('lan.unavailable', '无法读取'),
        tone: 'danger',
        description: loadError && loadError.message ? loadError.message : null,
      })]})]);
      saver.refreshChrome();
      return;
    }
    const tokens = Array.isArray(state.tokens) ? state.tokens : [];
    const rows = tokens.map(token => row({
      label: token.name,
      description: new Date(token.created_at).toLocaleString(),
      value: msg('mcp.revoke', '撤销'),
      tone: 'danger',
      action: 'revoke-mcp-token',
      onPress: busy ? undefined : () => revoke(token),
    }));
    const full = tokens.length >= (state.max_tokens || 20);
    rows.push(row({
      label: msg('mcp.create', '创建令牌'),
      labelTone: 'accent',
      action: 'create-mcp-token',
      onPress: busy ? undefined : () => (full ? toast(msg('mcp.limit', '令牌数量已达上限，请先撤销一个')) : askName()),
    }));
    replace(body, [
      group({
        rows,
        note: msg('mcp.help', '局域网里的 MCP 客户端用令牌连接 Agent 的 /mcp，不需要设备密码。令牌只能访问 MCP，不会过期，修改设备密码也不会失效；不再使用时请在这里撤销。通过 USB 连接的客户端不需要令牌。'),
      }),
      state.lan_access ? null : text(msg('mcp.lan_off', '局域网访问未开启，令牌暂时不能使用。'), 'page__note'),
    ].filter(Boolean));
    saver.refreshChrome();
  }

  await load();
  render();
  return screen([body]);
}
