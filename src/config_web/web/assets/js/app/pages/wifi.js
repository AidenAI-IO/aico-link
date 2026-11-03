/**
 * Wi-Fi routes: the network list, one profile's detail, and the two join sheets.
 *
 * Layout follows the design: a titled intro with the app-style blue tile, an
 * inset card per network group, and a full-width black action at the bottom.
 * All transport work is delegated to `wifi-service.js`.
 *
 * Two ways to join, matching the design:
 *   - a discovered network opens the password sheet;
 *   - the "Others…" row opens a manual sheet for a hidden network, where the
 *     name is typed instead of picked. The Agent always writes `scan_ssid=1`
 *     for a new profile, so a hidden network needs no extra field.
 */

import {connectedSsid, fetchSnapshot, savedNetwork, savedNetworks, t} from '../data.js';
import {connect, forget, otherNetworks, proxyChangeRequest, scan, wifiSignalLevel} from '../wifi-service.js';
import {el, replace} from '../../ui/dom.js';
import {group, row, screen} from '../../ui/list.js';
import {button} from '../../ui/button.js';
import {confirmSheet} from '../../ui/confirm.js';
import {setChecked, switchControl} from '../../ui/switch.js';
import {icon, wifiTile} from '../../ui/icon.js';
import {msg, resolve, text} from '../../ui/text.js';
import {sheet} from '../../ui/sheet.js';
import {toast} from '../../ui/toast.js';

function connectionFailureMessage(result, fallback) {
  if (result && result.failureReason === 'wrong_password') {
    return msg('ui.wifi_password_invalid', '密码错误');
  }
  return msg('ui.connect_failed', fallback);
}

/**
 * The intro block: tile, title, and the explanation from the design.
 *
 * @param {boolean} [inCard] - tighten the padding when it sits inside a card.
 */
function intro(inCard) {
  return el('div', {class: inCard ? 'page__intro page__intro--in-card' : 'page__intro'}, [
    wifiTile(),
    text('Wi-Fi', 'page__title'),
    text(
      msg(
        'ui.wifi_intro',
        '连接后，Aico 可实现消息推送、信息捕捉，还能学习你的操作方式，借助自动化能力简化复杂任务。',
      ),
      'page__description',
    ),
  ]);
}

/** The proxy controls shared by every join form. */
function proxyControls() {
  let enabled = false;
  let url = '';

  const urlField = el('div', {class: 'ds-field', attrs: {hidden: 'hidden'}}, [
    text(msg('wifi.proxy_url', '代理 URL'), 'ds-field__label'),
    el('input', {
      class: 'ds-input',
      attrs: {
        type: 'url',
        placeholder: resolve(msg('wifi.proxy_url_placeholder', '请输入代理 URL')),
        inputmode: 'url',
        autocapitalize: 'none',
        autocorrect: 'off',
        spellcheck: 'false',
        'aria-label': 'proxy URL',
      },
      data: {action: 'wifi-proxy-url'},
      on: {input: event => (url = event.target.value.trim())},
    }),
  ]);

  const control = switchControl({
    checked: false,
    label: msg('ui.proxy_row_label', '配置代理'),
    onChange: next => {
      enabled = next;
      urlField.hidden = !next;
    },
  });

  return {
    /** The switch row plus its conditional URL field, ready to append. */
    nodes: [
      row({
        label: msg('ui.proxy_row_label', '配置代理'),
        labelTone: 'strong',
        extraClass: 'ds-row--plain',
        accessory: control,
      }),
      urlField,
    ],
    /** A manual submit must also flip the switch's painted state. */
    setEnabled: next => {
      setChecked(control, next);
      enabled = next;
      urlField.hidden = !next;
    },
    mode: () => (enabled ? 'proxy' : 'system'),
    url: () => url,
  };
}

/**
 * The form body both sheets share: an optional name field, the password field
 * with its helper line, the proxy controls, and the submit button.
 *
 * @param {object} options
 * @param {boolean} options.withName - include the manually typed network name.
 * @param {boolean} [options.withPasswordLabel] - caption the password field; the
 *   password sheet omits it because its title already names the field.
 * @param {boolean} [options.withHelp] - show the password rule under the field.
 * @param {boolean} [options.withRule] - divide the fields from the proxy controls.
 */
function joinForm({withName, withPasswordLabel = true, withHelp = false, withRule = false}) {
  const nameInput = withName
    ? el('input', {
        class: 'ds-input',
        attrs: {
          type: 'text',
          placeholder: resolve(msg('ui.network_name_placeholder', '输入网络名称')),
          autocapitalize: 'none',
          autocorrect: 'off',
          spellcheck: 'false',
          'aria-label': resolve(msg('ui.network_name_label', '名称')),
        },
        data: {action: 'wifi-network-name'},
      })
    : null;

  const passwordInput = el('input', {
    class: 'ds-input',
    attrs: {
      type: 'password',
      placeholder: resolve(msg('ui.password_enter', '输入密码')),
      autocomplete: 'off',
      autocapitalize: 'none',
      autocorrect: 'off',
      spellcheck: 'false',
      'aria-label': resolve(msg('wifi.password', '密码')),
    },
    data: {action: 'wifi-password'},
  });

  // Only the discovered-network sheet explains the rule; the manual sheet's
  // design keeps the form to its two fields.
  const helpNode = withHelp
    ? text(msg('ui.wifi_password_rule', '密码至少 8 个字符，区分大小写'), 'ds-field__help')
    : el('span', {attrs: {hidden: 'hidden'}});
  const errorNode = el('div', {class: 'ds-field__error', attrs: {'aria-live': 'polite'}});
  errorNode.hidden = true;

  function setError(message) {
    if (message == null) {
      errorNode.hidden = true;
      errorNode.textContent = '';
      passwordInput.classList.remove('ds-input--invalid');
      passwordInput.removeAttribute('aria-invalid');
      helpNode.hidden = false;
      return;
    }
    errorNode.textContent = resolve(message);
    errorNode.hidden = false;
    passwordInput.classList.add('ds-input--invalid');
    passwordInput.setAttribute('aria-invalid', 'true');
    helpNode.hidden = true;
  }

  const submitButton = button({
    label: msg('ui.join_network', '加入网络'),
    variant: 'primary',
    block: true,
    action: 'wifi-join',
  });
  // The design shows the action muted until a hidden network has a name.
  submitButton.disabled = Boolean(withName);

  const proxy = proxyControls();

  const fields = [];
  if (nameInput) {
    fields.push(
      el('div', {class: 'ds-field'}, [
        text(msg('ui.network_name_label', '名称'), 'ds-field__label'),
        nameInput,
      ]),
    );
  }
  fields.push(
    el('div', {class: 'ds-field'}, [
      withPasswordLabel ? text(msg('wifi.password', '密码'), 'ds-field__label') : null,
      passwordInput,
      helpNode,
      errorNode,
    ]),
  );

  const body = el('div', {}, [
    ...fields,
    withRule ? el('div', {class: 'ds-sheet__rule'}) : null,
    ...proxy.nodes,
    el('div', {class: 'page__footer'}, [submitButton]),
  ]);

  if (nameInput) {
    nameInput.addEventListener('input', () => {
      submitButton.disabled = nameInput.value.trim() === '';
    });
  }

  return {
    body,
    name: () => (nameInput ? nameInput.value.trim() : ''),
    password: () => passwordInput.value,
    proxy,
    submitButton,
    setError,
    focus: () => (nameInput || passwordInput).focus(),
  };
}

/* ------------------------------------------------------------------ list --- */

/**
 * @param {{snapshot: object|null, navigate: Function}} context
 */
export async function wifiPage(context) {
  // The shell already read the snapshot for the rail; only fetch when this page
  // is reached without one.
  let snapshot = context.snapshot || null;
  if (!snapshot) {
    snapshot = await fetchSnapshot().catch(error => {
      console.warn('[wifi] snapshot unavailable:', error && error.message);
      return null;
    });
  }

  /** Discovered networks, filled in by the scan that runs on entry. */
  let discovered = [];
  let scanning = false;
  /** SSID of the saved network being joined from the list, if any. */
  let switching = '';

  const listHost = el('div');
  const rescanButton = button({
    label: msg('ui.rescan_networks', '重新扫描网络'),
    variant: 'primary',
    block: true,
    action: 'wifi-rescan',
    onPress: () => runScan(),
  });

  function render() {
    const connected = connectedSsid(snapshot);
    const saved = savedNetworks(snapshot);
    const connectedStatus = (snapshot && snapshot.wifi_status) || {};
    const discoveredBySsid = new Map(discovered.map(network => [network.ssid, network]));
    const savedSsids = saved.map(network => network.ssid);
    const priorityOrdered = [
      ...(connected ? saved.filter(network => network.ssid === connected) : []),
      ...saved.filter(network => !connected || network.ssid !== connected),
    ];
    const others = otherNetworks(discovered, savedSsids, connected);

    const sections = [];

    // The design keeps the title block and the connected profile inside one card,
    // divided by a hairline, with no section caption above them.
    sections.push(
      group({
        rows: [
          intro(true),
          ...priorityOrdered.map(network =>
            row({
              label: network.ssid,
              icon: 'wifi',
              signalLevel: wifiSignalLevel(
                network.ssid === connected
                  ? {
                      ...network,
                      ...(discoveredBySsid.get(network.ssid) || {}),
                      ...(connectedStatus.signal_dbm != null ? {signalDbm: connectedStatus.signal_dbm} : {}),
                    }
                  : {...network, ...(discoveredBySsid.get(network.ssid) || {})},
              ),
              // As on iOS: tapping a saved network joins it, and the ⓘ opens
              // its details. The connected one has nothing to join.
              action: `wifi-open-${network.ssid}`,
              onPress: network.ssid === connected
                ? () => openDetails(network.ssid)
                : () => switchTo(network.ssid, connected),
              accessory: savedAccessory(network, connected),
              value: network.ssid === connected || network.ssid === switching ? null : disabledLabel(network),
            }),
          ),
        ],
      }),
    );

    const otherRows = others.map(network =>
      row({
        label: network.ssid,
        icon: 'wifi',
        signalLevel: wifiSignalLevel(network),
        action: `wifi-join-${network.ssid}`,
        onPress: () => openJoinSheet(network.ssid, network.secured, refresh),
        accessory: network.secured ? icon('lock', {class: 'ds-row__value', size: 18}) : null,
      }),
    );
    // Always last: manual entry for a network the scan cannot see.
    otherRows.push(
      row({
        label: msg('ui.other_networks_more', '其他…'),
        labelTone: 'accent',
        action: 'wifi-custom-network',
        onPress: () => openCustomNetworkSheet(refresh),
      }),
    );

    sections.push(
      group({
        caption: msg('ui.section_other_networks', '其他网络'),
        rows: otherRows,
      }),
    );

    replace(listHost, sections);
    rescanButton.disabled = scanning;
  }

  function openDetails(ssid) {
    context.navigate(`/wifi/${encodeURIComponent(ssid)}`);
  }

  /** Connected check or joining spinner, then the details button. */
  function savedAccessory(network, connected) {
    const info = el('button', {
      class: 'ds-row__info',
      attrs: {type: 'button', 'aria-label': resolve(msg('wifi.details', '网络详情'))},
      data: {action: `wifi-info-${network.ssid}`},
      on: {click: event => {
        // The row itself joins the network; this button only opens details.
        event.stopPropagation();
        openDetails(network.ssid);
      }},
    }, [icon('info', {size: 22})]);
    let state = null;
    if (network.ssid === switching) state = el('span', {class: 'ds-row__spinner', attrs: {role: 'status', 'aria-label': resolve(msg('wifi.joining', '正在连接…'))}});
    else if (network.ssid === connected) state = icon('checkCircle', {class: 'ds-row__icon ds-row__icon--success', size: 22});
    return el('span', {class: 'ds-row__accessory-group'}, [state, info]);
  }

  /** Join a saved network with its stored password and proxy setting. */
  async function switchTo(ssid, connected) {
    if (switching) return;
    // Reached over the board's own Wi-Fi address, this page loses its
    // connection the moment the board leaves that network.
    const status = (snapshot && snapshot.wifi_status) || {};
    if (connected && status.ip_address && status.ip_address === window.location.hostname) {
      const confirmed = await confirmSheet({
        title: t('wifi.switch_confirm_title', {ssid, defaultValue: '切换到“{{ssid}}”？'}),
        body: msg('wifi.switch_confirm_body', '本页面正通过当前 Wi-Fi 访问设备，切换后连接会断开。请通过 USB 或新网络中的设备地址重新打开。'),
        confirmLabel: msg('wifi.switch', '切换'),
        action: 'confirm-wifi-switch',
      });
      if (!confirmed) return;
    }
    switching = ssid;
    render();
    try {
      const result = await connect({ssid, keepProxy: true});
      if (result.ok) {
        toast(t('wifi.connected_to', {ssid, defaultValue: '已连接到“{{ssid}}”。'}));
      } else if (result.failureReason === 'wrong_password') {
        // The stored password no longer works; ask for a new one.
        toast(msg('ui.wifi_password_invalid', '密码错误'));
        switching = '';
        openJoinSheet(ssid, true, refresh);
      } else if (connected) {
        toast(t('wifi.switch_failed', {ssid, defaultValue: '无法连接到“{{ssid}}”，已保持原来的网络。'}), {durationMs: 5000});
      } else {
        // Nothing was connected, so there is no previous network to keep.
        toast(t('wifi.join_failed', {ssid, defaultValue: '无法连接到“{{ssid}}”。'}), {durationMs: 5000});
      }
    } catch (error) {
      toast(error && error.message ? error.message : resolve(msg('ui.connect_failed', '连接失败')), {durationMs: 5000});
    } finally {
      switching = '';
      await refresh();
    }
  }

  /** A saved profile that is disabled shows as such instead of as connected. */
  function disabledLabel(network) {
    return network && network.disabled ? msg('wifi.disabled', '已停用') : null;
  }

  async function refresh() {
    snapshot = await fetchSnapshot().catch(() => snapshot);
    render();
  }

  async function runScan() {
    if (scanning) return;
    scanning = true;
    render();
    try {
      discovered = await scan();
      // Refresh the profiles too: a successful association shows up here.
      snapshot = await fetchSnapshot().catch(() => snapshot);
    } catch (error) {
      toast(error && error.message ? error.message : resolve(msg('wifi.scan_failed', '扫描失败')));
    } finally {
      scanning = false;
      render();
    }
  }

  // The scan action belongs at the bottom of the screen, per the design.
  const host = screen(
    [listHost, el('div', {class: 'page__footer page__footer--bottom'}, [rescanButton])],
    'ds-screen--sticky',
  );
  render();
  // Scanning takes a few seconds; render what is already known first.
  runScan();

  return host;
}

/* ---------------------------------------------------------------- detail --- */

/**
 * The detail view for one profile: its identity, then its proxy settings.
 *
 * @param {{params: {ssid: string}, navigate: Function}} context
 */
export async function wifiDetailPage(context) {
  const ssid = context.params.ssid;
  let snapshot = context.snapshot || null;
  if (!snapshot) {
    snapshot = await fetchSnapshot().catch(error => {
      console.warn('[wifi] snapshot unavailable:', error && error.message);
      return null;
    });
  }

  const profile = savedNetwork(snapshot, ssid) || {ssid};
  const connected = connectedSsid(snapshot) === ssid;

  const savedProxyUrl = profile.proxy_url || '';
  let proxyEnabled = (profile.proxy_mode || 'system') === 'proxy';
  let proxyUrl = savedProxyUrl;

  const proxyErrorNode = el('div', {class: 'ds-field__error', attrs: {'aria-live': 'polite'}});
  proxyErrorNode.hidden = true;

  const proxyInput = el('input', {
    class: 'ds-input',
    attrs: {
      type: 'url',
      value: proxyUrl,
      placeholder: resolve(msg('wifi.proxy_url_placeholder', '请输入代理 URL')),
      inputmode: 'url',
      autocapitalize: 'none',
      autocorrect: 'off',
      spellcheck: 'false',
      'aria-label': 'proxy URL',
    },
    data: {action: 'wifi-proxy-url'},
  });

  const proxyUrlField = el(
    'div',
    {class: 'ds-field', attrs: {hidden: proxyEnabled ? null : 'hidden'}},
    [text(msg('wifi.proxy_url', '代理 URL'), 'ds-field__label'), proxyInput, proxyErrorNode],
  );

  function setProxyError(message) {
    if (message == null) {
      proxyErrorNode.hidden = true;
      proxyErrorNode.textContent = '';
      proxyInput.classList.remove('ds-input--invalid');
      proxyInput.removeAttribute('aria-invalid');
      return;
    }
    proxyErrorNode.textContent = resolve(message);
    proxyErrorNode.hidden = false;
    proxyInput.classList.add('ds-input--invalid');
    proxyInput.setAttribute('aria-invalid', 'true');
  }

  // A committed URL, or switching the proxy off, is what talks to the Agent.
  proxyInput.addEventListener('change', () => {
    if (!proxyEnabled) return;
    commitProxy({enabled: true, url: proxyInput.value});
  });
  proxyInput.addEventListener('keydown', event => {
    if (event.key === 'Enter') proxyInput.blur();
  });

  const proxySwitch = switchControl({
    checked: proxyEnabled,
    label: msg('ui.proxy_row_label', '配置代理'),
    onChange: next => {
      proxyEnabled = next;
      proxyUrlField.hidden = !next;
      setProxyError(null);
      if (next) {
        // Reveal the field first. Sending `proxy_mode=proxy` now would be
        // rejected: the Agent requires a `proxy_url` unless the profile already
        // stores one, and asking for it up front is what made this look broken.
        if (!savedProxyUrl) proxyInput.focus();
        return;
      }
      commitProxy({enabled: false, url: ''});
    },
  });

  /**
   * Persist a proxy choice.
   *
   * @param {{enabled: boolean, url: string}} choice
   */
  async function commitProxy({enabled, url}) {
    // `proxyChangeRequest` owns the rule that a proxy needs a URL, so the UI can
    // never send a combination the Agent rejects.
    const choice = proxyChangeRequest({enabled, url, savedUrl: savedProxyUrl});
    if (!choice.ok) {
      setProxyError(msg('wifi.proxy_required', '请输入代理 URL'));
      return;
    }

    setProxyError(null);
    proxyInput.disabled = true;
    try {
      const result = await connect({
        ssid,
        proxyMode: choice.proxyMode,
        proxyUrl: choice.proxyUrl,
      });
      if (!result.ok) throw new Error(resolve(msg('wifi.connect_failed', '连接失败')));
      proxyEnabled = enabled;
      proxyUrl = choice.proxyUrl;
      proxyUrlField.hidden = !enabled;
      toast(msg('action.saved', '已保存'));
    } catch (error) {
      // The switch already moved; put it back so the control never claims a
      // state the device did not accept.
      setChecked(proxySwitch, proxyEnabled);
      proxyUrlField.hidden = !proxyEnabled;
      setProxyError({key: 'ui.proxy_apply_failed', fallback: error && error.message ? error.message : '保存失败'});
    } finally {
      proxyInput.disabled = false;
    }
  }

  const infoRows = [row({label: msg('ui.network_name', '网络名称'), value: ssid, tone: 'strong'})];
  // Only the connected profile has an address; a saved one out of range has none.
  const ipAddress = connected ? ((snapshot && snapshot.wifi_status) || {}).ip_address : '';
  if (ipAddress) {
    infoRows.push(row({label: msg('ui.ip_address', 'IP 地址'), value: ipAddress}));
  }
  if (profile.has_psk) {
    infoRows.push(row({label: msg('wifi.password', '密码'), value: '••••••••'}));
  }

  const forgetRow = row({
    label: msg('ui.forget_network', '忽略此网络'),
    labelTone: 'accent',
    action: 'wifi-forget',
    onPress: async () => {
      // The title names the network, so it needs `t` with the SSID; `msg`
      // resolves without parameters and left the quotes empty.
      const confirmed = await confirmSheet({
        title: t('wifi.forget_confirm', {ssid, defaultValue: '忘记“{{ssid}}”？'}),
        confirmLabel: msg('action.forget', '忘记'),
        danger: true,
        action: 'confirm-wifi-forget',
      });
      if (!confirmed) return;
      try {
        await forget(ssid);
        context.navigate('/wifi', {replace: true});
      } catch (error) {
        toast(error && error.message ? error.message : resolve(msg('wifi.forget_failed', '忘记网络失败')));
      }
    },
  });

  // The design's detail page carries no title block: the top bar (native or
  // standalone) names the network, and the "Network info" card repeats it.
  if (context.header) context.header({title: ssid, onBack: () => context.navigate('/wifi')});
  return screen([
    // The design leads with the destructive action as a blue row, not a button
    // parked at the bottom of the page.
    group({rows: [forgetRow]}),
    group({caption: msg('ui.section_network_info', '网络信息'), rows: infoRows}),
    group({
      joined: true,
      rows: [row({label: msg('ui.proxy_row_label', '配置代理'), accessory: proxySwitch}), proxyUrlField],
    }),
  ]);
}

/* ----------------------------------------------------------------- sheets --- */

/**
 * Sheet for joining a network the scan found.
 *
 * @param {string} ssid
 * @param {boolean} secured
 * @param {() => void} onJoined
 */
export function openJoinSheet(ssid, secured, onJoined) {
  // The design titles this sheet after the field it asks for, so the field
  // itself carries no caption and a rule separates the form from the proxy row.
  const form = joinForm({withName: false, withPasswordLabel: false, withHelp: true, withRule: true});
  const view = sheet({
    title: msg('ui.wifi_password_title', 'Wi-Fi 密码'),
    body: [form.body],
  });

  form.submitButton.addEventListener('click', async () => {
    const password = form.password();
    if (secured && password.length < 8) {
      form.setError(msg('ui.wifi_password_rule', '密码至少 8 个字符，区分大小写'));
      form.focus();
      return;
    }

    form.submitButton.disabled = true;
    form.setError(null);
    try {
      const result = await connect({
        ssid,
        psk: password,
        proxyMode: form.proxy.mode(),
        proxyUrl: form.proxy.url(),
      });
      if (!result.ok) {
        form.setError(connectionFailureMessage(result, '连接失败'));
        return;
      }
      view.close();
      toast(t('wifi.connected_to', {ssid, defaultValue: '已连接到“{{ssid}}”。'}));
      if (onJoined) onJoined();
    } catch (error) {
      form.setError({key: 'ui.connect_failed', fallback: error && error.message ? error.message : '连接失败'});
    } finally {
      form.submitButton.disabled = false;
    }
  });

  view.onAfterClose(() => form.setError(null));
  view.open();
  // Focus the field the user came here to fill in.
  setTimeout(() => form.focus(), 350);
  return view;
}

/**
 * Sheet for a hidden network: the name is typed rather than picked.
 *
 * @param {() => void} onJoined
 */
export function openCustomNetworkSheet(onJoined) {
  const form = joinForm({withName: true});
  const view = sheet({
    title: msg('ui.custom_network_title', '其他网络'),
    body: [form.body],
  });

  form.submitButton.addEventListener('click', async () => {
    const ssid = form.name();
    if (!ssid) return;

    form.submitButton.disabled = true;
    form.setError(null);
    try {
      const result = await connect({
        ssid,
        psk: form.password(),
        proxyMode: form.proxy.mode(),
        proxyUrl: form.proxy.url(),
      });
      if (!result.ok) {
        form.setError(connectionFailureMessage(result, '无法加入该网络，请检查名称与密码'));
        return;
      }
      view.close();
      toast(t('wifi.connected_to', {ssid, defaultValue: '已连接到“{{ssid}}”。'}));
      if (onJoined) onJoined();
    } catch (error) {
      form.setError({key: 'ui.connect_failed', fallback: error && error.message ? error.message : '连接失败'});
    } finally {
      form.submitButton.disabled = false;
    }
  });

  view.onAfterClose(() => form.setError(null));
  view.open();
  setTimeout(() => form.focus(), 350);
  return view;
}
