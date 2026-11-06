/**
 * LAN access route.
 *
 *   /advanced/lan   the LAN access switch and the device password
 *
 * While LAN access is off, the settings page (80), the Agent (8080), the web
 * terminal and SSH answer only the USB link. Turning it on needs a device password, which also
 * becomes the password of the `aico` login used by the terminal and SSH.
 * Both can be changed only over USB (`via_usb`); on any other connection the
 * page shows the state read-only.
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

function passwordInput(placeholder, action) {
  return el('input', {
    class: 'ds-input',
    attrs: {
      type: 'password',
      placeholder: resolve(placeholder),
      autocomplete: 'new-password',
      autocapitalize: 'none',
      autocorrect: 'off',
      spellcheck: 'false',
      'aria-label': resolve(placeholder),
    },
    data: {action},
  });
}

/**
 * Sheet that asks for a new device password, twice.
 *
 * @param {object} options
 * @param {number} options.minLength
 * @param {import('../../ui/text.js').TextSpec} options.submitLabel
 * @param {(password: string) => Promise<void>} options.onSubmit - throws to keep the sheet open.
 * @param {() => void} [options.onCancel] - the sheet closed without a submit.
 */
export function openPasswordSheet({minLength, submitLabel, onSubmit, onCancel}) {
  const password = passwordInput(msg('lan.password_placeholder', '输入新密码'), 'lan-password');
  const confirm = passwordInput(msg('lan.password_confirm', '再次输入密码'), 'lan-password-confirm');
  const help = text(msg('lan.password_rule', '至少 8 个字符，区分大小写'), 'ds-field__help');
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
        el('div', {class: 'ds-field'}, [password, confirm, help, error]),
        el('div', {class: 'page__footer'}, [submit]),
      ]),
    ],
  });

  submit.addEventListener('click', async () => {
    const value = password.value;
    if ([...value].length < minLength) {
      setError(msg('lan.password_rule', '至少 8 个字符，区分大小写'));
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
      await onSubmit(value);
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
  setTimeout(() => password.focus(), 350);
  return view;
}

export async function lanAccessPage(context) {
  const body = el('div');
  const saver = createSaver(context, {title: msg('lan.title', '局域网访问'), back: '/advanced', root: body});
  let state = null;
  let loadError = null;
  let busy = false;

  try {
    state = await fetchLANAccess();
  } catch (error) {
    loadError = error;
  }

  async function apply(change, done) {
    busy = true;
    render();
    try {
      state = await updateLANAccess(change);
      if (state.system_synced === false) {
        toast(msg('lan.login_sync_failed', '已保存，SSH 和终端登录将在设备重启后生效'));
      } else if (done) {
        toast(done);
      }
    } finally {
      busy = false;
      render();
    }
  }

  function askPassword(submitLabel, change, done, onCancel) {
    openPasswordSheet({
      minLength: state.min_length || 8,
      submitLabel,
      onSubmit: password => apply({...change, password}, done),
      onCancel,
    });
  }

  function toggle(next, control) {
    if (busy) {
      setChecked(control, state.enabled);
      return;
    }
    if (next && !state.password_set) {
      // The first switch-on asks for the password the terminal and SSH will use.
      askPassword(msg('lan.set_and_enable', '设置密码并开启'), {enabled: true}, msg('lan.enabled', '已开启局域网访问'),
        () => setChecked(control, state.enabled));
      return;
    }
    apply({enabled: next}, next ? msg('lan.enabled', '已开启局域网访问') : msg('lan.disabled', '已关闭局域网访问'))
      .catch(error => {
        setChecked(control, state.enabled);
        toast(error && error.message ? error.message : resolve(msg('lan.save_failed', '保存失败')));
      });
  }

  function render() {
    if (!state) {
      replace(body, [group({rows: [row({
        label: msg('lan.title', '局域网访问'),
        value: msg('lan.unavailable', '无法读取'),
        tone: 'danger',
        description: loadError && loadError.message ? loadError.message : null,
      })]})]);
      saver.refreshChrome();
      return;
    }
    const editable = Boolean(state.via_usb);
    const control = switchControl({
      checked: Boolean(state.enabled),
      label: msg('lan.title', '局域网访问'),
      onChange: next => toggle(next, control),
    });
    control.disabled = !editable || busy;

    const passwordRow = row({
      label: msg('lan.password', '设备密码'),
      value: state.password_set ? msg('lan.password_set', '已设置') : msg('lan.password_unset', '未设置'),
      chevron: editable,
      action: 'open-lan-password',
      onPress: editable && !busy
        ? () => askPassword(state.password_set ? msg('lan.change_password', '修改密码') : msg('lan.set_password', '设置密码'),
          {}, msg('lan.password_saved', '设备密码已更新'))
        : undefined,
    });

    replace(body, [
      group({
        rows: [row({
          label: msg('lan.title', '局域网访问'),
          description: msg('lan.help', '开启后，同一局域网内的设备可以访问设置页（80 端口）、Agent（8080 端口）、网页终端和 SSH。关闭时只有 USB 连接可以访问。'),
          accessory: control,
          action: 'toggle-lan-access',
        })],
      }),
      group({
        rows: [passwordRow],
        note: msg('lan.password_help', '设备密码同时是 aico 账号的登录密码，用于网页终端和 SSH。'),
      }),
      editable ? null : text(msg('lan.usb_only', '只能在通过 USB 连接时修改这些设置。'), 'page__note'),
    ].filter(Boolean));
    saver.refreshChrome();
  }

  render();
  return screen([body]);
}
