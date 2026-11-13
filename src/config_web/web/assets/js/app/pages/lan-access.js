/**
 * Security routes. The two settings are independent and get a page each:
 *
 *   /advanced/lan        the LAN access switch
 *   /advanced/password   the device password (set, change, clear, sign out)
 *
 * The switch and the password are independent:
 *  - LAN access off: only the USB link reaches the settings page (80/443) and
 *    the Agent (8080/8443). On: the local network does too.
 *  - With a device password, every client signs in (the app pairs silently
 *    over USB); without one, nobody does. The password is also the password
 *    of the `aico` login used by the terminal and SSH, which stays locked
 *    until one is set; SSH opens to the LAN only when LAN access is on too.
 * Any client that reached this page may change them. Replacing or clearing a
 * password asks for the current one on every connection, the app's WebView
 * included. A forgotten password is removed on the board itself (the Agent
 * can do it); see docs/03-services/device-management-api.md.
 * Clearing keeps the switch and MCP access tokens.
 */

import {request} from '../data.js';
import {createSaver} from '../saver.js';
import {button} from '../../ui/button.js';
import {el, replace} from '../../ui/dom.js';
import {group, row, screen} from '../../ui/list.js';
import {sheet} from '../../ui/sheet.js';
import {setChecked, switchControl} from '../../ui/switch.js';
import {msg, resolve, text} from '../../ui/text.js';
import {toast} from '../../ui/toast.js';

const LAN_ACCESS_API = '/api/security/lan-access';
const PASSWORD_API = '/api/security/password';
const LOGOUT_API = '/api/auth/logout';

export function fetchLANAccess() {
  return request(LAN_ACCESS_API, {cache: 'no-store'});
}

function updateLANAccess(body) {
  return request(LAN_ACCESS_API, {
    method: 'PUT',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(body),
  });
}

function clearPassword(body) {
  return request(PASSWORD_API, {
    method: 'DELETE',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(body),
  });
}

function currentPasswordError(error) {
  return error && error.error === 'current_password_invalid'
    ? new Error(resolve(msg('lan.password_current_invalid', '当前密码不正确')))
    : error;
}

/**
 * Sheet that asks for the current device password before clearing it.
 *
 * @param {(current: string) => Promise<void>} onSubmit - throws to keep the sheet open.
 */
function openClearPasswordSheet(lanEnabled, onSubmit) {
  const current = passwordInput(msg('lan.password_current', '当前密码'), 'lan-password-current', 'current-password');
  const help = text(lanEnabled
    ? msg('lan.clear_help_lan_on', '清除后所有设备退出登录，SSH 和网页终端无法登录。局域网访问仍开启，同一局域网内的设备无需登录即可访问设置页和 Agent。')
    : msg('lan.clear_help', '清除后所有设备退出登录，SSH 和网页终端无法登录，直到重新设置密码。'), 'ds-field__help');
  const error = el('div', {class: 'ds-field__error', attrs: {'aria-live': 'polite'}});
  error.hidden = true;
  const submit = button({label: msg('lan.clear_password', '清除密码'), variant: 'primary', block: true, action: 'lan-clear-submit'});
  const view = sheet({
    title: msg('lan.clear_password', '清除密码'),
    body: [el('div', {}, [
      el('div', {class: 'ds-field'}, [current, help, error]),
      el('div', {class: 'page__footer'}, [submit]),
    ])],
  });
  submit.addEventListener('click', async () => {
    if (!current.value) {
      error.hidden = false;
      error.textContent = resolve(msg('lan.password_current_required', '请输入当前密码'));
      current.focus();
      return;
    }
    submit.disabled = true;
    try {
      await onSubmit(current.value);
      view.close();
    } catch (failure) {
      error.hidden = false;
      error.textContent = failure && failure.message ? failure.message : resolve(msg('lan.save_failed', '保存失败'));
    } finally {
      submit.disabled = false;
    }
  });
  view.open();
  setTimeout(() => current.focus(), 350);
  return view;
}

function passwordInput(placeholder, action, autocomplete = 'new-password') {
  return el('input', {
    class: 'ds-input',
    attrs: {
      type: 'password',
      placeholder: resolve(placeholder),
      autocomplete,
      autocapitalize: 'none',
      autocorrect: 'off',
      spellcheck: 'false',
      'aria-label': resolve(placeholder),
    },
    data: {action},
  });
}

/**
 * Sheet that asks for a new device password, twice, and the current one when
 * `askCurrent` is set.
 *
 * @param {object} options
 * @param {number} options.minLength
 * @param {boolean} [options.askCurrent]
 * @param {import('../../ui/text.js').TextSpec} options.submitLabel
 * @param {(password: string, current: string) => Promise<void>} options.onSubmit - throws to keep the sheet open.
 * @param {() => void} [options.onCancel] - the sheet closed without a submit.
 */
export function openPasswordSheet({minLength, askCurrent, submitLabel, onSubmit, onCancel}) {
  const current = askCurrent
    ? passwordInput(msg('lan.password_current', '当前密码'), 'lan-password-current', 'current-password')
    : null;
  const password = passwordInput(msg('lan.password_placeholder', '输入新密码'), 'lan-password');
  const confirm = passwordInput(msg('lan.password_confirm', '再次输入密码'), 'lan-password-confirm');
  const rule = msg('lan.password_rule', '至少 6 个字符，区分大小写');
  const help = text(rule, 'ds-field__help');
  const error = el('div', {class: 'ds-field__error', attrs: {'aria-live': 'polite'}});
  error.hidden = true;
  const submit = button({label: submitLabel, variant: 'primary', block: true, action: 'lan-password-submit'});
  let submitted = false;

  function setError(message) {
    error.hidden = message == null;
    error.textContent = message == null ? '' : resolve(message);
    help.hidden = message != null;
  }

  const view = sheet({
    title: msg('lan.password_title', '设备密码'),
    body: [
      el('div', {}, [
        el('div', {class: 'ds-field'}, [current, password, confirm, help, error].filter(Boolean)),
        el('div', {class: 'page__footer'}, [submit]),
      ]),
    ],
  });

  submit.addEventListener('click', async () => {
    const value = password.value;
    if (!current.value) {
      setError(msg('lan.password_current_required', '请输入当前密码'));
      current.focus();
      return;
    }
    if ([...value].length < minLength) {
      setError(rule);
      password.focus();
      return;
    }
    if (value !== confirm.value) {
      setError(msg('lan.password_mismatch', '两次输入的密码不一致'));
      confirm.focus();
      return;
    }
    setError(null);
    submit.disabled = true;
    try {
      await onSubmit(value, current ? current.value : '');
      submitted = true;
      view.close();
    } catch (failure) {
      setError(failure && failure.message ? failure.message : msg('lan.save_failed', '保存失败'));
    } finally {
      submit.disabled = false;
    }
  });
  view.onAfterClose(() => {
    if (!submitted && onCancel) onCancel();
  });
  view.open();
  setTimeout(() => (current || password).focus(), 350);
  return view;
}

/**
 * Shared page state for the two security pages: load, apply a change, and
 * render an error row when the state cannot be read.
 */
async function securityPage(context, title) {
  const body = el('div');
  const saver = createSaver(context, {title, back: '/advanced', root: body});
  const page = {body, saver, state: null, loadError: null, busy: false, render: () => {}};
  try {
    page.state = await fetchLANAccess();
  } catch (error) {
    page.loadError = error;
  }
  page.apply = async (send, done) => {
    page.busy = true;
    page.render();
    try {
      page.state = await send();
      if (page.state.system_synced === false) {
        toast(msg('lan.login_sync_failed', '已保存，SSH 和终端登录将在设备重启后生效'));
      } else if (done) {
        toast(done);
      }
    } finally {
      page.busy = false;
      page.render();
    }
  };
  page.renderUnavailable = () => {
    replace(body, [group({rows: [row({
      label: title,
      value: msg('lan.unavailable', '无法读取'),
      tone: 'danger',
      description: page.loadError && page.loadError.message ? page.loadError.message : null,
    })]})]);
    saver.refreshChrome();
  };
  return page;
}

/** /advanced/lan: only the LAN access switch. */
export async function lanAccessPage(context) {
  const page = await securityPage(context, msg('lan.title', '局域网访问'));

  function toggle(next, control) {
    if (page.busy) {
      setChecked(control, page.state.enabled);
      return;
    }
    page.apply(() => updateLANAccess({enabled: next}), next ? msg('lan.enabled', '已开启局域网访问') : msg('lan.disabled', '已关闭局域网访问'))
      .catch(error => {
        setChecked(control, page.state.enabled);
        toast(error && error.message ? error.message : resolve(msg('lan.save_failed', '保存失败')));
      });
  }

  page.render = () => {
    if (!page.state) {
      page.renderUnavailable();
      return;
    }
    const control = switchControl({
      checked: Boolean(page.state.enabled),
      label: msg('lan.title', '局域网访问'),
      onChange: next => toggle(next, control),
    });
    control.disabled = page.busy;
    replace(page.body, [
      group({
        rows: [row({
          label: msg('lan.title', '局域网访问'),
          description: msg('lan.help', '开启后，同一局域网内的设备也能访问设置页（80 和 443 端口）和 Agent（8080 和 8443 端口）；设置了设备密码时还能用 SSH 和网页终端。关闭时只有 USB 连接可以访问。MCP 使用单独的令牌，不受这个开关影响。'),
          accessory: control,
          action: 'toggle-lan-access',
        })],
        note: page.state.enabled && !page.state.password_set
          ? msg('lan.no_password_warning', '未设置设备密码：同一局域网内的设备无需登录即可访问。可在“设备密码”中设置。')
          : null,
      }),
    ]);
    page.saver.refreshChrome();
  };

  page.render();
  return screen([page.body]);
}

/** /advanced/password: set, change, or clear the device password; sign out. */
export async function devicePasswordPage(context) {
  const page = await securityPage(context, msg('lan.password', '设备密码'));

  function askPassword() {
    // Replacing a password needs the current one, so a stolen session
    // cannot lock the owner out.
    const askCurrent = Boolean(page.state.password_set);
    openPasswordSheet({
      minLength: page.state.min_length || 6,
      askCurrent,
      submitLabel: askCurrent ? msg('lan.change_password', '修改密码') : msg('lan.set_password', '设置密码'),
      onSubmit: async (password, current) => {
        try {
          await page.apply(
            () => updateLANAccess(askCurrent ? {password, current_password: current} : {password}),
            msg('lan.password_saved', '设备密码已更新'),
          );
        } catch (error) {
          throw currentPasswordError(error);
        }
      },
    });
  }

  function askClear() {
    openClearPasswordSheet(Boolean(page.state.enabled), async current => {
      try {
        await page.apply(() => clearPassword({current_password: current}), msg('lan.password_cleared', '已清除设备密码'));
      } catch (error) {
        throw currentPasswordError(error);
      }
    });
  }

  page.render = () => {
    if (!page.state) {
      page.renderUnavailable();
      return;
    }
    const set = Boolean(page.state.password_set);
    const rows = [row({
      label: set ? msg('lan.change_password', '修改密码') : msg('lan.set_password', '设置密码'),
      value: set ? msg('lan.password_set', '已设置') : msg('lan.password_unset', '未设置'),
      chevron: true,
      action: 'open-lan-password',
      onPress: page.busy ? undefined : askPassword,
    })];
    if (set) {
      rows.push(row({
        label: msg('lan.clear_password', '清除密码'),
        tone: 'danger',
        action: 'lan-clear-password',
        onPress: page.busy ? undefined : askClear,
      }));
    }
    const signOut = set && !page.state.via_usb ? group({rows: [row({
      label: msg('lan.sign_out', '退出登录'),
      tone: 'danger',
      action: 'lan-sign-out',
      onPress: async () => {
        try {
          await request(LOGOUT_API, {method: 'POST'});
        } finally {
          window.location.assign('/login');
        }
      },
    })]}) : null;
    replace(page.body, [
      group({
        rows,
        note: msg('lan.password_help', '设置后，所有设备都要先登录：局域网设备输入这个密码，Aico App 通过 USB 自动完成。它同时是 aico 账号的密码，用于网页终端和 SSH；没有设置时这两者无法登录。修改或清除密码会让所有设备退出登录。'),
      }),
      signOut,
    ].filter(Boolean));
    page.saver.refreshChrome();
  };

  page.render();
  return screen([page.body]);
}
