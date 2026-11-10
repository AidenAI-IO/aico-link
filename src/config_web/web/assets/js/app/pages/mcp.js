/**
 * MCP route.
 *
 *   /advanced/mcp   the MCP switch, how to connect, and MCP access tokens
 *
 * MCP clients are usually agents on a computer (Claude Code, Cursor). Over the
 * USB cable they connect to 192.168.42.1 with no credentials. Over Wi-Fi they
 * need LAN access on and an MCP access token: a token reaches only the Agent's
 * /mcp, does not expire, survives a password change, and is revoked here on its
 * own. The secret is shown once, when it is created, together with a
 * ready-to-paste client configuration.
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
const USB_MCP_ENDPOINT = 'http://192.168.42.1:8080/mcp';

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
 * The MCP URL a Wi-Fi client uses: the board's Wi-Fi address when it is known,
 * otherwise the address this page was opened on unless that is the USB link.
 * Empty when neither is known.
 */
export function wifiEndpoint(location = window.location, wifiAddress = '') {
  const host = wifiAddress || (location.hostname !== '192.168.42.1' ? location.hostname : '');
  return host ? `http://${host}:8080/mcp` : '';
}

/** Client configuration snippets; `secret` is omitted for the USB link. */
export function mcpClientSnippets(endpoint, secret = '') {
  const header = secret ? ` --header "Authorization: Bearer ${secret}"` : '';
  const server = {type: 'http', url: endpoint};
  if (secret) server.headers = {Authorization: `Bearer ${secret}`};
  return {
    claudeCode: `claude mcp add --transport http aico ${endpoint}${header}`,
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

function configSheet({title, intro, endpoint, secret}) {
  const snippets = mcpClientSnippets(endpoint, secret);
  sheet({
    title,
    body: [el('div', {}, [
      text(intro, 'page__note'),
      secret ? codeBlock(secret, 'copy-mcp-token') : null,
      text(msg('mcp.claude_code', 'Claude Code'), 'ds-field__help'),
      codeBlock(snippets.claudeCode, 'copy-mcp-claude'),
      text(msg('mcp.json_config', '其他客户端（JSON 配置）'), 'ds-field__help'),
      codeBlock(snippets.json, 'copy-mcp-json'),
    ].filter(Boolean))],
  }).open();
}

function showSecret(secret, endpoint) {
  configSheet({
    title: msg('mcp.created_title', '令牌已创建'),
    intro: msg('mcp.created_help', '令牌只显示这一次，请现在复制保存。'),
    endpoint,
    secret,
  });
}

function showUSBConfig() {
  configSheet({
    title: msg('mcp.usb_title', '通过 USB 连接'),
    intro: msg('mcp.usb_help', '用数据线把电脑连上设备，不需要令牌。'),
    endpoint: USB_MCP_ENDPOINT,
  });
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
        showSecret(created.secret, wifiEndpoint(window.location, wifiAddress) || 'http://<board-ip>:8080/mcp');
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
    const caption = msg('mcp.wifi_caption', '通过 Wi-Fi 连接');
    if (!state) {
      return group({
        caption,
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
      caption,
      rows,
      note: state.lan_access
        ? msg('mcp.help', '电脑和设备在同一个 Wi-Fi 下时，用令牌连接。令牌只能访问 MCP，不会过期，修改设备密码也不会失效；不再使用时请在这里撤销。')
        : msg('mcp.lan_off', '通过 Wi-Fi 连接需要先开启局域网访问。令牌可以先创建，开启后生效。'),
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
      mcpDraft ? group({
        caption: msg('mcp.usb_caption', '通过 USB 连接'),
        rows: [row({
          label: msg('mcp.usb_config', '连接配置'),
          value: '192.168.42.1',
          chevron: true,
          action: 'show-mcp-usb-config',
          onPress: showUSBConfig,
        })],
        note: msg('mcp.usb_note', '数据线连接时不需要令牌。'),
      }) : null,
      mcpDraft ? tokenSection() : null,
    ].filter(Boolean));
    saver.refreshChrome();
  }

  await load();
  render();
  return screen([body]);
}
