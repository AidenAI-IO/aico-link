import { IntroTile } from '../components/marks';
import type { FormEvent, KeyboardEvent } from 'react';
import type { DeviceState } from '../state/device';
import { validWiFi } from '../state/device';
const selectWiFi = (result: { payload: unknown }) =>
  validWiFi(result.payload) ? result.payload : null;
import type { Network, PageProps } from '../types';
import { useEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { useAtomValue } from 'jotai';
import {
  connect,
  currentNetworks,
  forget,
  otherNetworks,
  proxyChangeRequest,
  scan,
  wifiSignalLevel,
} from '../api/wifi';
import { connectedSsid, savedNetworks, savedNetwork } from '../lib/data';
import { confirmSheet } from '../components/overlays';
import { msg, resolve } from '../lib/i18n';
import { toast } from '../components/overlays';
import { Button, FocusList, Group, Icon, ResourceError, Row, Switch, Text } from '../components/ui';
import { Loading } from '../components/Loading';
import { isInitialLoading } from '../state/resource';

const failureText = (result: { failureReason?: string }) =>
  resolve(
    result.failureReason === 'wrong_password'
      ? msg('ui.wifi_password_invalid', '密码错误')
      : msg('wifi.connect_failed', '连接失败'),
  );

function JoinSheet({
  model,
  network,
  onClose,
}: {
  model: DeviceState;
  network: Partial<Network>;
  onClose: () => void;
}) {
  const [ssid, setSSID] = useState(network.ssid || '');
  const [password, setPassword] = useState('');
  const [mode, setMode] = useState<'system' | 'direct' | 'proxy'>('system');
  const [url, setURL] = useState('');
  const [error, setError] = useState('');
  const { busy } = useAtomValue(model.resources.wifi.view);
  const panel = useRef<HTMLDivElement>(null);
  const drag = useRef<number | null>(null);
  const busyRef = useRef(busy);
  busyRef.current = busy;
  useEffect(() => {
    const previous = document.activeElement;
    const app = document.getElementById('app');
    const wasInert = app?.inert;
    if (app) app.inert = true;
    panel.current?.querySelector('input')?.focus();
    return () => {
      if (app) app.inert = !!wasInert;
      if (previous instanceof HTMLElement && previous.isConnected) previous.focus();
    };
  }, []);
  async function submit(event: FormEvent) {
    event.preventDefault();
    if (busy || !ssid.trim()) return;
    if (network.secured && password.length < 8) {
      setError(resolve(msg('ui.wifi_password_rule', '密码至少 8 个字符，区分大小写')));
      return;
    }
    setError('');
    try {
      const result = await model.resources.wifi.mutate(
        () => connect({ ssid: ssid.trim(), psk: password, proxyMode: mode, proxyUrl: url }),
        selectWiFi,
      );
      if (!result.ok) throw new Error(failureText(result));
      onClose();
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : String(failure));
    }
  }
  function keyDown(event: KeyboardEvent) {
    if (event.key === 'Escape' && !busy) {
      event.stopPropagation();
      onClose();
    }
    if (event.key !== 'Tab') return;
    const controls = [
      ...(panel.current?.querySelectorAll<HTMLInputElement | HTMLButtonElement | HTMLSelectElement>(
        'input,button,select',
      ) || []),
    ].filter((node) => !node.disabled);
    const first = controls[0];
    const last = controls.at(-1);
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last?.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first?.focus();
    }
  }
  return createPortal(
    <div
      className="ds-sheet"
      role="dialog"
      aria-modal="true"
      aria-label={resolve(
        network.ssid
          ? msg('ui.wifi_password_title', 'Wi-Fi 密码')
          : msg('ui.custom_network_title', '其他网络'),
      )}
      onKeyDown={keyDown}
    >
      <div className="ds-sheet__scrim ds-sheet__scrim--open" onClick={() => !busy && onClose()} />
      <div className="ds-sheet__panel ds-sheet__panel--open" ref={panel}>
        <div
          className="ds-sheet__grabber"
          onPointerDown={(event) => {
            if (busyRef.current) return;
            drag.current = event.clientY;
            event.currentTarget.setPointerCapture?.(event.pointerId);
          }}
          onPointerUp={(event) => {
            const start = drag.current;
            drag.current = null;
            if (start !== null && !busyRef.current && event.clientY - start > 80) onClose();
          }}
          onPointerCancel={() => {
            drag.current = null;
          }}
        >
          <div className="ds-sheet__handle" />
        </div>
        <h2 className="ds-sheet__title">
          <Text
            value={
              network.ssid
                ? msg('ui.wifi_password_title', 'Wi-Fi 密码')
                : msg('ui.custom_network_title', '其他网络')
            }
          />
        </h2>
        <form className="ds-sheet__body" onSubmit={submit}>
          {!network.ssid && (
            <label className="ds-field">
              <Text value={msg('ui.network_name_label', '名称')} className="ds-field__label" />
              <input
                className="ds-input"
                value={ssid}
                onChange={(event) => setSSID(event.target.value)}
                autoComplete="off"
                autoCapitalize="none"
                spellCheck={false}
                disabled={busy}
                placeholder={resolve(msg('ui.network_name_placeholder', '输入网络名称'))}
                data-action="wifi-network-name"
              />
            </label>
          )}
          <label className="ds-field">
            {!network.ssid && (
              <Text value={msg('wifi.password', '密码')} className="ds-field__label" />
            )}
            <input
              className={`ds-input ${error ? 'ds-input--invalid' : ''}`}
              type="password"
              value={password}
              onChange={(event) => setPassword(event.target.value)}
              autoComplete="off"
              disabled={busy}
              aria-label={resolve(msg('wifi.password', '密码'))}
              aria-invalid={!!error}
              placeholder={resolve(msg('ui.password_enter', '输入密码'))}
              data-action="wifi-password"
            />
            {network.ssid && !error && (
              <Text
                value={msg('ui.wifi_password_rule', '密码至少 8 个字符，区分大小写')}
                className="ds-field__help"
              />
            )}
          </label>
          {network.ssid && <div className="ds-sheet__rule" />}
          <Row
            label={msg('ui.proxy_row_label', '配置代理')}
            labelTone="strong"
            className="ds-row--plain"
          >
            <Switch
              checked={mode === 'proxy'}
              disabled={busy}
              label={msg('ui.proxy_row_label', '配置代理')}
              onChange={(enabled) => setMode(enabled ? 'proxy' : 'system')}
            />
          </Row>
          {mode !== 'proxy' && (
            <Row label={msg('wifi.proxy_direct', '直连')} className="ds-row--plain">
              <Switch
                checked={mode === 'direct'}
                disabled={busy}
                label={msg('wifi.proxy_direct', '直连')}
                onChange={(direct) => setMode(direct ? 'direct' : 'system')}
              />
            </Row>
          )}
          {mode === 'proxy' && (
            <label className="ds-field">
              <Text value={msg('wifi.proxy_url', '代理 URL')} className="ds-field__label" />
              <input
                className="ds-input"
                type="url"
                value={url}
                onChange={(event) => setURL(event.target.value)}
                disabled={busy}
                required
                placeholder={resolve(msg('wifi.proxy_url_placeholder', '请输入代理 URL'))}
                data-action="wifi-proxy-url"
              />
            </label>
          )}
          {error && (
            <p role="alert" className="ds-field__error">
              {error}
            </p>
          )}
          <div className="page__footer">
            <Button block variant="quiet" disabled={busy} onClick={onClose}>
              <Text value={msg('action.cancel', '取消')} />
            </Button>
            <button
              type="submit"
              className="ds-btn ds-btn--primary ds-btn--block"
              data-action="wifi-join"
              disabled={busy || !ssid.trim()}
            >
              <Text
                value={
                  busy ? msg('wifi.connecting', '正在连接…') : msg('ui.join_network', '加入网络')
                }
              />
            </button>
          </div>
        </form>
      </div>
    </div>,
    document.body,
  );
}

export function WiFiPage({ model, navigate }: PageProps) {
  const snapshot = useAtomValue(model.current);
  const resource = useAtomValue(model.resources.wifi.view);
  const { busy } = resource;
  const [discovered, setDiscovered] = useState<Network[]>([]);
  const [scanning, setScanning] = useState(true);
  const [joining, setJoining] = useState<Partial<Network> | null>(null);
  const mounted = useRef(true);
  const scanningRef = useRef(false);
  async function runScan() {
    if (scanningRef.current) return;
    scanningRef.current = true;
    setScanning(true);
    try {
      const result = await scan();
      if (mounted.current) setDiscovered(result);
    } catch (error) {
      if (mounted.current) toast(error instanceof Error ? error.message : String(error));
    } finally {
      scanningRef.current = false;
      if (mounted.current) setScanning(false);
    }
  }
  useEffect(() => {
    mounted.current = true;
    void runScan();
    return () => {
      mounted.current = false;
    };
  }, []);
  const connected = connectedSsid(snapshot);
  const saved = savedNetworks(snapshot);
  const profiles = currentNetworks(
    [
      ...(connected && !saved.some((entry) => entry.ssid === connected)
        ? [{ ssid: connected }]
        : []),
      ...saved,
    ],
    connected,
  );
  const others = otherNetworks(
    discovered,
    saved.map((entry) => entry.ssid),
    connected,
  );
  async function joinSaved(ssid: string) {
    if (busy || ssid === connected) return;
    try {
      const result = await model.resources.wifi.mutate(
        () => connect({ ssid, keepProxy: true }),
        selectWiFi,
      );
      if (!result.ok) {
        if (result.failureReason === 'wrong_password') setJoining({ ssid, secured: true });
        else toast(failureText(result));
      }
    } catch (error) {
      toast(error instanceof Error ? error.message : String(error));
    }
  }
  const networkRow = (network: Network, known: boolean) => {
    const active = network.ssid === connected;
    const observation = discovered.find((value) => value.ssid === network.ssid) || network;
    const signal = wifiSignalLevel(
      active && snapshot.wifi_status?.signal_dbm != null
        ? { signalDbm: snapshot.wifi_status?.signal_dbm }
        : observation,
    );
    return (
      <Row
        key={network.ssid}
        label={network.ssid}
        tone={active ? 'success' : undefined}
        action={`wifi-network-${network.ssid}`}
        leading={<Icon name="wifi" signalLevel={signal} className="ds-row__icon" />}
        onPress={!busy ? () => (known ? joinSaved(network.ssid) : setJoining(network)) : undefined}
      >
        {active && !known && (
          <Icon name="checkCircle" className="ds-row__icon ds-row__icon--success" size={22} />
        )}
        {!known && network.secured && <Icon name="lock" className="ds-row__value" size={18} />}
        {known && (
          <span className="ds-row__accessory-group">
            {active && (
              <Icon name="checkCircle" className="ds-row__icon ds-row__icon--success" size={22} />
            )}
            <button
              type="button"
              className="ds-row__info"
              data-action={`wifi-info-${network.ssid}`}
              aria-label={`${network.ssid} ${resolve(msg('ui.section_network_info', '网络信息'))}`}
              onClick={(event) => {
                event.stopPropagation();
                navigate(`/wifi/${encodeURIComponent(network.ssid)}`);
              }}
            >
              <Icon name="info" size={22} />
            </button>
          </span>
        )}
      </Row>
    );
  };
  return (
    <FocusList className="ds-screen ds-screen--sticky">
      <Group>
        <div className="page__intro page__intro--in-card">
          <IntroTile kind="wifi" />
          <h1 className="page__title">Wi-Fi</h1>
          <Text
            value={msg('ui.wifi_intro', '选择网络以连接设备。')}
            className="page__description"
          />
        </div>
        {isInitialLoading(resource) && <Loading block />}
        {profiles.map((network) => networkRow({ ...network, secured: !!network.has_psk }, true))}
      </Group>
      <ResourceError model={model} name="wifi" />
      <Group caption={msg('ui.section_other_networks', '其他网络')}>
        {scanning && !discovered.length && <Loading block />}
        {others.map((network) => networkRow(network, false))}
        <Row
          label={msg('ui.other_networks_more', '其他…')}
          labelTone="accent"
          action="wifi-custom-network"
          onPress={() => setJoining({ secured: false })}
        />
      </Group>
      <div className="page__footer page__footer--bottom">
        <Button block disabled={scanning} action="wifi-rescan" onClick={runScan}>
          <Text
            value={
              scanning
                ? msg('wifi.scanning', '正在扫描…')
                : msg('ui.rescan_networks', '重新扫描网络')
            }
          />
        </Button>
      </div>
      {joining && <JoinSheet model={model} network={joining} onClose={() => setJoining(null)} />}
    </FocusList>
  );
}

export function WiFiDetail({ model, route, navigate }: PageProps) {
  const ssid = route.params?.ssid || '';
  const snapshot = useAtomValue(model.current);
  const { busy } = useAtomValue(model.resources.wifi.view);
  const profile = savedNetwork(snapshot, ssid);
  const [urlDraft, setURLDraft] = useState<string | null>(null);
  const [modeDraft, setModeDraft] = useState<boolean | null>(null);
  const [directDraft, setDirectDraft] = useState<boolean | null>(null);
  const [error, setError] = useState('');
  const input = useRef<HTMLInputElement>(null);
  const enabled = modeDraft ?? profile?.proxy_mode === 'proxy';
  const url = urlDraft ?? profile?.proxy_url ?? '';
  async function save(
    next: boolean,
    value: string,
    direct = directDraft ?? profile?.proxy_mode === 'direct',
  ) {
    if (!profile || busy) return;
    const choice = proxyChangeRequest({ enabled: next, url: value, savedUrl: profile.proxy_url });
    if (!choice.ok) {
      setError(resolve(msg('wifi.proxy_required', '请输入代理 URL')));
      return;
    }
    setError('');
    try {
      const result = await model.resources.wifi.mutate(
        () =>
          connect({
            ssid,
            proxyMode: !next && direct ? 'direct' : (choice.proxyMode as 'system' | 'proxy'),
            proxyUrl: choice.proxyUrl,
          }),
        selectWiFi,
      );
      if (!result.ok) throw new Error(failureText(result));
      setURLDraft(null);
      setModeDraft(null);
      setDirectDraft(null);
    } catch (failure) {
      setModeDraft(null);
      setDirectDraft(null);
      setError(failure instanceof Error ? failure.message : String(failure));
    }
  }
  async function remove() {
    if (
      busy ||
      !profile ||
      !(await confirmSheet({
        title: ssid,
        body: msg('ui.forget_network', '忽略此网络'),
        confirmLabel: msg('action.forget', '忘记'),
        danger: true,
        action: 'confirm-wifi-forget',
      }))
    )
      return;
    try {
      await model.resources.wifi.mutate(() => forget(ssid), selectWiFi);
      navigate('/wifi');
    } catch (failure) {
      toast(failure instanceof Error ? failure.message : String(failure));
    }
  }
  return (
    <div className="ds-screen">
      {profile && (
        <Group>
          <Row
            label={msg('ui.forget_network', '忽略此网络')}
            labelTone="accent"
            onPress={remove}
            action="wifi-forget"
          />
        </Group>
      )}
      <Group caption={msg('ui.section_network_info', '网络信息')}>
        <Row label={msg('ui.network_name', '网络名称')} value={ssid} tone="strong" />
        {connectedSsid(snapshot) === ssid && snapshot.wifi_status?.ip_address && (
          <Row label={msg('ui.ip_address', 'IP 地址')} value={snapshot.wifi_status?.ip_address} />
        )}
        {profile?.has_psk && <Row label={msg('wifi.password', '密码')} value="••••••••" />}
      </Group>
      <ResourceError model={model} name="wifi" />
      {profile && (
        <Group joined>
          <Row label={msg('ui.proxy_row_label', '配置代理')}>
            <Switch
              checked={enabled}
              disabled={busy}
              label={msg('ui.proxy_row_label', '配置代理')}
              onChange={(value) => {
                setModeDraft(value);
                setError('');
                if (!value || profile.proxy_url) void save(value, url);
                else queueMicrotask(() => input.current?.focus());
              }}
            />
          </Row>
          {!enabled && (
            <Row label={msg('wifi.proxy_direct', '直连')}>
              <Switch
                checked={directDraft ?? profile.proxy_mode === 'direct'}
                disabled={busy}
                label={msg('wifi.proxy_direct', '直连')}
                onChange={(value) => {
                  setDirectDraft(value);
                  void save(false, url, value);
                }}
              />
            </Row>
          )}
          <label className="ds-field" hidden={!enabled}>
            <Text value={msg('wifi.proxy_url', '代理 URL')} className="ds-field__label" />
            <input
              ref={input}
              className="ds-input"
              type="url"
              value={url}
              disabled={busy}
              data-action="wifi-proxy-url"
              onChange={(event) => setURLDraft(event.target.value)}
              onBlur={() => {
                if (enabled && urlDraft !== null) void save(true, url);
              }}
              onKeyDown={(event) => {
                if (event.key === 'Enter') event.currentTarget.blur();
              }}
            />
          </label>
          {error && (
            <p className="ds-field__error" role="alert">
              {error}
            </p>
          )}
        </Group>
      )}
    </div>
  );
}
