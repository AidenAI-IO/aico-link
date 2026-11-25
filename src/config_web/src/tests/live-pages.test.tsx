import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import type { ReactNode } from 'react';
import { Provider } from 'jotai';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Configuration, PageProps } from '../types';
import { createDeviceState, type DeviceState } from '../state/device';
import { USBInternet, MCPPage, LocalOverview } from '../pages/Device';
import { OTAPage } from '../pages/OTA';
import { WiFiDetail, WiFiPage } from '../pages/WiFi';
import { Navigation } from '../app/Navigation';
import { RequireConfig } from '../pages/Settings';
import { ValueField } from '../components/fields';
import { App } from '../app/App';
import { applyLocale } from '../lib/data';
import { AgentLogsPage } from '../pages/Advanced';
import { VoicePage } from '../pages/Voice';
import { useSchema } from '../hooks/query';
import { ApplicationStatus } from '../components/ApplicationStatus';
import { SwipeRow } from '../components/SwipeRow';
import { OverlayHost } from '../components/overlays';
import * as wifiAPI from '../api/wifi';

vi.mock('../api/wifi', async (original) => ({
  ...(await original<object>()),
  scan: vi.fn(async () => [{ ssid: 'Cafe', secured: true }]),
}));
const models: DeviceState[] = [];
it.each([
  { profile: 'device', local: false, container: false },
  { profile: 'native', local: true, container: false },
  { profile: 'container', local: false, container: true },
])(
  'hides confirmed success while retaining unresolved application states ($profile)',
  async ({ local, container }) => {
    const value = model(
      async () => {
        throw new Error('Agent unavailable');
      },
      local,
      container,
    );
    const applied = {
      state: 'applied',
      applied: true,
      revision: 'two',
      persisted_revision: 'two',
    };
    value.resources.application.publish({ ...applied, revision: 'one' });
    mount(value, <ApplicationStatus model={value} />);
    expect(screen.getByText('已保存到本机，尚未应用到 Agent')).toBeTruthy();
    for (let poll = 0; poll < 2; poll++) {
      act(() => value.resources.application.publish({ ...applied }));
      expect(screen.queryByRole('status')).toBe(null);
    }
    act(() => value.resources.application.publish({ ...applied, state: 'pending', pending: true }));
    expect(screen.getByText('配置已保存，正在等待 Agent 应用')).toBeTruthy();
    act(() =>
      value.resources.application.publish({
        ...applied,
        state: 'reboot_required',
        reboot_required: true,
      }),
    );
    expect(screen.getByText('已保存，重启后生效')).toBeTruthy();
    act(() =>
      value.resources.application.publish({ ...applied, state: 'failed', error: 'Rejected' }),
    );
    expect(screen.getByText('配置尚未生效，请检查 Agent 日志')).toBeTruthy();
    expect(screen.getByRole('button', { name: '重试应用' })).toBeTruthy();
    act(() => value.resources.application.publish(applied));
    expect(screen.queryByRole('status')).toBe(null);
    await act(() => value.resources.application.refresh());
    expect(screen.getByText('暂时无法确认配置状态；Agent 启动后会读取已保存的配置')).toBeTruthy();
  },
);
it('shares schema reads across concurrent consumers and route remounts, with retry after failure', async () => {
  let failed = true;
  const send = vi.fn(async () => {
    if (failed) throw new Error('schema offline');
    return { sections: [] };
  });
  const value = model(send);
  function SchemaView() {
    const query = useSchema(value);
    return (
      <button onClick={query.retry}>{query.error || (query.data ? 'ready' : 'loading')}</button>
    );
  }
  let view = mount(
    value,
    <>
      <SchemaView />
      <SchemaView />
    </>,
  );
  await act(flush);
  expect(send).toHaveBeenCalledTimes(1);
  expect(screen.getAllByText('schema offline')).toHaveLength(2);
  failed = false;
  fireEvent.click(screen.getAllByText('schema offline')[0]);
  await act(flush);
  expect(screen.getAllByText('ready')).toHaveLength(2);
  view.unmount();
  view = mount(value, <SchemaView />);
  await act(flush);
  expect(screen.getByText('ready')).toBeTruthy();
  expect(send).toHaveBeenCalledTimes(2);
  view.unmount();
});
const flush = async () => {
  for (let n = 0; n < 12; n++) await Promise.resolve();
};
function model(
  send: Parameters<typeof createDeviceState>[0] = async () => ({}),
  local = false,
  container = false,
) {
  const value = createDeviceState(send, { local, container });
  models.push(value);
  return value;
}
function props(value: DeviceState, path: string): PageProps {
  return {
    model: value,
    route: { path },
    navigate: vi.fn(),
    header: vi.fn(),
  };
}
function mount(value: DeviceState, node: ReactNode) {
  return render(<Provider store={value.store}>{node}</Provider>);
}
const wifi = (ssid: string, networks = ['Home', 'Work']) => ({
  wifi_status: { connected: true, state: 'COMPLETED', ssid },
  wifi: { networks: networks.map((name) => ({ ssid: name, has_psk: true, proxy_mode: 'system' })) },
});
beforeEach(() => {
  vi.useFakeTimers();
  applyLocale('zh-CN', false);
  window.history.replaceState({}, '', '/');
});
afterEach(() => {
  cleanup();
  models.splice(0).forEach((value) => value.dispose());
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe('live React views', () => {
  it('keeps delete reachable by keyboard while retaining swipe presentation', () => {
    const remove = vi.fn();
    const open = vi.fn();
    const view = render(
      <SwipeRow onDelete={remove}>
        <button onClick={open}>Provider</button>
      </SwipeRow>,
    );
    const action = screen.getByRole('button', { name: '删除' });
    expect(action.tabIndex).toBe(0);
    act(() => action.focus());
    expect(view.container.querySelector<HTMLElement>('.ds-swipe__content')?.style.transform).toBe(
      'translateX(-80px)',
    );
    const provider = screen.getByRole('button', { name: 'Provider' });
    act(() => provider.focus());
    fireEvent.click(provider, { detail: 0 });
    expect(open).toHaveBeenCalledOnce();
    act(() => action.focus());
    fireEvent.click(provider, { detail: 1 });
    expect(open).toHaveBeenCalledOnce();
    expect(view.container.querySelector<HTMLElement>('.ds-swipe__content')?.style.transform).toBe(
      '',
    );
    fireEvent.click(action);
    expect(remove).toHaveBeenCalledOnce();
  });
  it('keeps a voice model visible on schema failure and retries without implying a fixed provider', async () => {
    let failed = true;
    const value = model(async () => {
      if (failed) throw new Error('schema unavailable');
      return { sections: [{ name: 'voice_model_providers', fields: [{ key: 'model' }] }] };
    });
    value.store.set(value.config, {
      agent: { input_mode: 'realtime' },
      voice_model: { provider: 'demo' },
      voice_model_providers: { demo: { type: 'openai', model: 'saved-model' } },
    });
    mount(value, <VoicePage {...props(value, '/voice')} />);
    await act(flush);
    expect(screen.getByText('saved-model')).toBeTruthy();
    expect(screen.queryByText('由提供商决定')).toBe(null);
    failed = false;
    fireEvent.click(screen.getByRole('button', { name: '重试' }));
    await act(flush);
    expect(screen.getByRole('button', { name: '模型 saved-model' })).toBeTruthy();
  });
  it.each<{ mode: string; config: Configuration }>([
    { mode: 'realtime', config: { agent: { input_mode: 'stt' } } },
    {
      mode: 'realtime',
      config: {
        agent: { input_mode: 'stt' },
        voice_model: { provider: 'qwen', has_api_key: false },
        voice_model_providers: { qwen: { type: 'qwen', model: 'qwen-realtime' } },
      },
    },
    {
      mode: 'stt',
      config: { agent: { input_mode: 'realtime' }, stt: { provider: 'speech' } },
    },
  ])('previews $mode immediately without persisting the selection', async ({ mode, config }) => {
    const send = vi.fn(async () => ({ sections: [] }));
    const value = model(send);
    value.store.set(value.config, config);
    const page = props(value, '/voice');
    mount(value, <VoicePage {...page} />);
    await act(flush);
    send.mockClear();
    fireEvent.click(screen.getByRole('tab', { name: mode === 'stt' ? '经典模式' : '实时模式' }));
    expect(page.navigate).not.toHaveBeenCalled();
    await act(flush);
    expect(value.store.get(value.voiceMode)).toBe(mode);
    expect(value.store.get(value.config)?.agent?.input_mode).toBe(config.agent?.input_mode);
    expect(screen.getByText(/此模式尚未启用/).closest('[role="status"]')).toBeTruthy();
    expect(
      screen
        .getByRole('tab', { name: mode === 'stt' ? '经典模式' : '实时模式' })
        .getAttribute('aria-selected'),
    ).toBe('true');
  });
  it.each([false, true])(
    'retains voice tabs across subpages and navigation without saving (container: %s)',
    async (container) => {
      const config: Configuration = {
        agent: { input_mode: 'stt' },
        voice_model: { provider: 'demo' },
        voice_model_providers: {
          demo: { type: 'qwen', has_api_key: true, model: 'realtime-model' },
        },
        stt: { provider: 'speech' },
        tts: { provider: 'voice' },
      };
      const send = vi.fn(async (path: string, options?: RequestInit) => {
        if (options?.method) throw new Error('preview must not write');
        if (path === '/api/config') return { config, config_valid: true };
        if (path === '/api/config/schema')
          return { sections: [{ name: 'voice_model_providers', fields: [{ key: 'model' }] }] };
        return { state: 'applied' };
      });
      window.history.replaceState({}, '', '/voice');
      const value = model(send, false, container);
      const view = mount(value, <App model={value} />);
      await act(flush);
      fireEvent.click(screen.getByRole('tab', { name: '实时模式' }));
      const selected = () =>
        expect(screen.getByRole('tab', { name: '实时模式' }).getAttribute('aria-selected')).toBe(
          'true',
        );
      for (const label of ['提供商 demo (Qwen)', '模型 realtime-model', '更多设置']) {
        fireEvent.click(screen.getByRole('button', { name: label }));
        await act(flush);
        if (label.startsWith('提供商')) expect(screen.getByText('提供商')).toBeTruthy();
        fireEvent.click(screen.getByRole('button', { name: '返回' }));
        await act(flush);
        selected();
      }
      fireEvent.click(view.container.querySelector('[data-action="open-model"]')!);
      await act(flush);
      fireEvent.click(view.container.querySelector('[data-action="open-voice"]')!);
      await act(flush);
      selected();
      await act(() => value.resources.snapshot.refresh());
      selected();
      fireEvent.click(screen.getByRole('tab', { name: '经典模式' }));
      fireEvent.click(view.container.querySelector('[data-action="open-stt-providers"]')!);
      await act(flush);
      fireEvent.click(screen.getByRole('button', { name: '返回' }));
      await act(flush);
      expect(screen.getByRole('tab', { name: '经典模式' }).getAttribute('aria-selected')).toBe(
        'true',
      );
      expect(send.mock.calls.some(([, options]) => options?.method)).toBe(false);
      expect(value.store.get(value.config)?.agent?.input_mode).toBe('stt');
      fireEvent.click(screen.getByRole('tab', { name: '实时模式' }));
      selected();
      view.unmount();
      const fresh = model(send, false, container);
      fresh.store.set(fresh.config, config);
      mount(fresh, <VoicePage {...props(fresh, '/voice')} />);
      expect(screen.getByRole('tab', { name: '经典模式' }).getAttribute('aria-selected')).toBe(
        'true',
      );
    },
  );
  it.each([false, true])(
    'applies a previewed mode only after credentials are saved (container: %s)',
    async (container) => {
      let config: Configuration = {
        agent: { input_mode: 'stt' },
        voice_model: { provider: 'demo' },
        voice_model_providers: { demo: { type: 'qwen', has_api_key: false } },
      };
      const send = vi.fn(async (path: string, options?: RequestInit) => {
        if (path === '/api/config/plan') return { apply: 'live' };
        if (path === '/api/config' && options?.method === 'PATCH') {
          const patch = JSON.parse(String(options.body)).config;
          if (patch.voice_model_providers)
            config = {
              ...config,
              voice_model_providers: { demo: { type: 'qwen', has_api_key: true } },
            };
          if (patch.agent) config = { ...config, agent: patch.agent };
          return { config, state: 'applied', apply: 'live' };
        }
        if (path === '/api/config') return { config, config_valid: true };
        if (path === '/api/config/schema')
          return {
            sections: [
              { name: 'voice_model_providers', fields: [{ key: 'api_key', secret: true }] },
            ],
          };
        return { state: 'applied' };
      });
      window.history.replaceState({}, '', '/voice');
      const value = model(send, false, container);
      mount(value, <App model={value} />);
      await act(flush);
      fireEvent.click(screen.getByRole('tab', { name: '实时模式' }));
      fireEvent.click(screen.getByRole('button', { name: '提供商 demo (Qwen)' }));
      await act(flush);
      fireEvent.click(screen.getByRole('button', { name: 'demo (Qwen)' }));
      await act(flush);
      expect(send.mock.calls.some(([, options]) => options?.method)).toBe(false);
      expect(screen.queryByText('删除配置')).toBe(null);
      fireEvent.change(screen.getByLabelText('实时语音 API 密钥'), {
        target: { value: 'test-key' },
      });
      fireEvent.click(screen.getByRole('button', { name: '保存' }));
      await act(flush);
      expect(value.store.get(value.config)?.agent?.input_mode).toBe('realtime');
      expect(send.mock.calls.filter(([, options]) => options?.method === 'PATCH')).toHaveLength(2);
    },
  );
  it('retains direct Wi-Fi mode and provides a close action in the join sheet', async () => {
    const value = model();
    value.publishWiFi(wifi('Home'));
    const view = mount(value, <WiFiPage {...props(value, '/wifi')} />);
    await act(flush);
    const cafe = view.container.querySelector<HTMLElement>('[data-action="wifi-network-Cafe"]')!;
    fireEvent.click(cafe);
    fireEvent.click(screen.getByRole('button', { name: '取消' }));
    expect(screen.queryByRole('dialog')).toBe(null);
    const connect = vi
      .spyOn(wifiAPI, 'connect')
      .mockResolvedValue({ ok: true, failureReason: '', payload: {} });
    fireEvent.click(cafe);
    fireEvent.change(screen.getByLabelText('密码'), { target: { value: 'password123' } });
    fireEvent.click(screen.getByRole('switch', { name: '直连' }));
    fireEvent.click(screen.getByRole('button', { name: '加入网络' }));
    await act(flush);
    expect(connect).toHaveBeenCalledWith(expect.objectContaining({ proxyMode: 'direct' }));
  });
  it('uses live OTA activity to block a duplicate start without interpreting an old log as current', () => {
    const value = model();
    value.resources.otaLogs.publish({ ota_update_running: false, ota_log: { log: '' } });
    value.resources.ota.publish({ ota_update_running: true });
    mount(value, <OTAPage {...props(value, '/firmware')} />);
    expect(
      (screen.getByRole('button', { name: '检查并安装更新' }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });
  it('keeps unread summaries loading independently until their observations arrive', async () => {
    const responses = new Map<string, (data: unknown) => void>();
    const value = model((path) => new Promise((resolve) => responses.set(path, resolve)));
    const view = mount(value, <App model={value} />);
    await act(flush);
    const rows = (name: string) => [
      ...view.container.querySelectorAll<HTMLElement>(`[data-action="open-${name}"]`),
    ];
    for (const name of ['wifi', 'voice', 'storage', 'firmware']) {
      expect(rows(name).length).toBeGreaterThan(0);
      rows(name).forEach((row) => expect(row.getAttribute('aria-busy')).toBe('true'));
    }
    expect(screen.queryByText(/暂时无法确认配置状态/)).toBe(null);
    expect(screen.queryByText('未连接')).toBe(null);
    const row = rows('wifi')[0];
    row.focus();
    await act(async () => {
      responses.get('/api/config')!({ config: { agent: { input_mode: 'realtime' } } });
      await flush();
    });
    expect(rows('voice')[0].textContent).toContain('实时模式');
    expect(rows('wifi')[0].getAttribute('aria-busy')).toBe('true');
    await act(async () => {
      responses.get('/api/network/wifi/status')!(wifi('Home'));
      responses.get('/api/storage/status')!({ card: { present: true, mounted: true } });
      responses.get('/api/device/firmware')!({ firmware: { current_version: 'new-version' } });
      responses.get('/api/config/application')!({ state: 'applied' });
      responses.get('/api/ota/status?logs=0')!({ ota_update_running: false });
      await flush();
    });
    expect(rows('wifi')[0]).toBe(row);
    expect(document.activeElement).toBe(row);
    expect(row.textContent).toContain('已连接');
    expect(rows('storage')[0].textContent).toContain('已连接');
    expect(rows('firmware')[0].textContent).toContain('new-version');
    expect(view.container.querySelector('.ds-loading')).toBe(null);
  });
  it.each([
    {
      name: 'USB',
      page: (value: DeviceState) => <USBInternet model={value} />,
      resource: 'usb' as const,
      premature: '无法读取状态',
      failure: '无法读取状态',
    },
    {
      name: 'Agent status and logs',
      page: (value: DeviceState) => <AgentLogsPage {...props(value, '/advanced/logs/agent')} />,
      resource: 'device' as const,
      premature: '未运行',
      failure: '未知',
    },
    {
      name: 'OTA logs',
      page: (value: DeviceState) => <OTAPage {...props(value, '/firmware/log')} logOnly />,
      resource: 'otaLogs' as const,
      premature: '还没有更新记录',
      failure: '未知',
    },
    {
      name: 'Wi-Fi summary',
      page: (value: DeviceState) => <Navigation model={value} navigate={vi.fn()} path="/" />,
      resource: 'wifi' as const,
      premature: '未连接',
      failure: '未知',
    },
  ])('distinguishes initial loading from a failed $name read', async (test) => {
    const value = model(async () => {
      throw new Error('read failed');
    });
    const view = mount(value, test.page(value));
    expect(view.container.querySelector('.ds-loading')).toBeTruthy();
    expect(screen.queryByText(test.premature)).toBe(null);
    await act(async () => {
      await value.resources[test.resource].refresh();
      await flush();
    });
    expect(screen.getAllByText(test.failure).length).toBeGreaterThan(0);
    if (test.resource === 'otaLogs' || test.resource === 'usb')
      expect(view.container.querySelector('.ds-loading')).toBe(null);
  });
  it('waits for local configuration before declaring an API key missing', () => {
    const value = model(undefined, true);
    const view = mount(value, <LocalOverview {...props(value, '/')} />);
    expect(screen.queryByText('未配置')).toBe(null);
    expect(view.container.querySelector('.ds-loading')).toBeTruthy();
    act(() =>
      value.publishSave({
        config: {
          model: { provider: 'demo' },
          model_providers: { demo: { type: 'openai', has_api_key: true } },
        },
      }),
    );
    expect(screen.getByText('已配置')).toBeTruthy();
    expect(screen.queryByText('未配置')).toBe(null);
  });
  it('waits for the speaker query before showing an empty enrollment list', async () => {
    let profiles!: (data: unknown) => void;
    const value = model((path) =>
      path === '/api/speaker/profiles'
        ? new Promise((resolve) => {
            profiles = resolve;
          })
        : Promise.resolve(path === '/api/speaker/status' ? { gate_enabled: false } : {}),
    );
    value.publishSave({ config: { agent: { input_mode: 'stt' } } });
    const view = mount(value, <VoicePage {...props(value, '/voice')} />);
    await act(flush);
    const panel = view.container.querySelector('[data-action="speaker-panel"]')!;
    expect(panel.querySelector('.ds-loading')).toBeTruthy();
    expect(screen.queryByText('还没有声纹')).toBe(null);
    await act(async () => {
      profiles({ profiles: [] });
      await flush();
    });
    expect(screen.getByText('还没有声纹')).toBeTruthy();
    expect(panel.querySelector('.ds-loading')).toBe(null);
  });
  it('routes container voice, memory and logs with the PC capabilities passed to editors', async () => {
    window.history.replaceState({}, '', '/voice');
    const value = model(
      async (path) =>
        path === '/api/config'
          ? { config: { agent: { input_mode: 'stt' } } }
          : path === '/api/device/status'
            ? { agent_status: { classic_voice_enabled: false } }
            : { state: 'applied' },
      false,
      true,
    );
    const view = mount(value, <App model={value} />);
    await act(flush);
    expect(view.container.querySelector('[data-action="voice-mode-realtime"]')).toBeTruthy();
    expect(screen.getByText(/当前容器未启用 Classic 语音/)).toBeTruthy();
    expect(view.container.querySelector('[data-action="speaker-panel"]')).toBe(null);
    fireEvent.click(view.container.querySelector('[data-action="open-memory"]')!);
    await act(flush);
    fireEvent.click(screen.getByRole('button', { name: /屏幕记忆保留期/ }));
    expect(screen.getByRole('dialog')).toBeTruthy();
    expect(screen.getByRole('button', { name: '永久' })).toBeTruthy();
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    expect(screen.queryByRole('dialog')).toBe(null);
    expect(view.container.querySelector('[data-action="reset-memory"]')).toBeTruthy();
    expect(screen.queryByRole('switch')).toBe(null);
    fireEvent.click(view.container.querySelector('[data-action="open-logs"]')!);
    await act(flush);
    expect(view.container.querySelector('[data-action="export-logs"]')).toBeTruthy();
  });
  it('updates published Agent and optional container terminal links from shared observations', () => {
    const value = model(undefined, false, true);
    value.resources.device.publish({ agent_status: { url: 'http://127.0.0.1:51278' } });
    mount(value, <LocalOverview {...props(value, '/')} />);
    const link = screen.getByRole('link', { name: '打开 Agent 界面' });
    expect(link.getAttribute('href')).toBe('http://127.0.0.1:51278');
    expect(screen.queryByRole('link', { name: '打开容器终端' })).toBe(null);
    act(() =>
      value.resources.device.publish({
        agent_status: {
          url: 'http://127.0.0.1:51279',
          terminal_url: 'http://127.0.0.1:51279/webtty/',
        },
      }),
    );
    expect(screen.getByRole('link', { name: '打开 Agent 界面' })).toBe(link);
    expect(link.getAttribute('href')).toBe('http://127.0.0.1:51279');
    expect(screen.getByRole('link', { name: '打开容器终端' }).getAttribute('href')).toBe(
      'http://127.0.0.1:51279/webtty/',
    );
    act(() => value.resources.device.publish({ agent_status: { url: 'http://127.0.0.1:51279' } }));
    expect(screen.queryByRole('link', { name: '打开容器终端' })).toBe(null);
  });
  it('keeps the focused USB switch while its confirmed status changes', () => {
    const value = model();
    value.resources.usb.publish({ enabled: true, via_usb: true, active: false });
    mount(value, <USBInternet model={value} />);
    const control = screen.getByRole('switch');
    control.focus();
    act(() => {
      value.resources.usb.publish({ enabled: true, via_usb: true, active: true, host: 'android' });
    });
    expect(screen.getByRole('switch')).toBe(control);
    expect(document.activeElement).toBe(control);
    expect(screen.getByText('通过 Android 联网')).toBeTruthy();
  });
  it('preserves Wi-Fi row controls through reordering and join-sheet credentials through polls', async () => {
    const value = model();
    value.publishWiFi(wifi('Home'));
    const view = mount(value, <WiFiPage {...props(value, '/wifi')} />);
    await act(flush);
    const info = screen.getByRole('button', { name: /Home 网络信息/ });
    info.focus();
    act(() => value.publishWiFi(wifi('Work')));
    expect(screen.getByRole('button', { name: /Home 网络信息/ })).toBe(info);
    expect(document.activeElement).toBe(info);
    const cafe = view.container.querySelector<HTMLElement>('[data-action="wifi-network-Cafe"]')!;
    cafe.focus();
    fireEvent.click(cafe);
    const password = screen.getByLabelText('密码') as HTMLInputElement;
    fireEvent.change(password, { target: { value: 'my-password' } });
    password.focus();
    act(() => value.publishWiFi(wifi('Cafe', ['Home', 'Work', 'Cafe'])));
    expect(screen.getByLabelText('密码')).toBe(password);
    expect(password.value).toBe('my-password');
    expect(document.activeElement).toBe(password);
    fireEvent.keyDown(password, { key: 'Escape' });
    expect(screen.queryByRole('dialog')).toBe(null);
  });
  it('restores the equivalent control when a network changes group with focus in the list', async () => {
    const value = model();
    value.publishWiFi(wifi('Home'));
    const view = mount(value, <WiFiPage {...props(value, '/wifi')} />);
    await act(flush);
    const cafe = view.container.querySelector<HTMLElement>('[data-action="wifi-network-Cafe"]')!;
    cafe.focus();
    act(() => value.publishWiFi(wifi('Cafe', ['Home', 'Cafe'])));
    const replacement = view.container.querySelector<HTMLElement>(
      '[data-action="wifi-network-Cafe"]',
    )!;
    expect(replacement).not.toBe(cafe);
    expect(document.activeElement).toBe(replacement);
  });
  it('adopts newly saved proxy settings without erasing a typed URL draft', () => {
    const value = model();
    value.publishWiFi(wifi('Home'));
    const page = props(value, '/wifi/Home');
    page.route.params = { ssid: 'Home' };
    mount(value, <WiFiDetail {...page} />);
    fireEvent.click(screen.getByRole('switch', { name: '配置代理' }));
    const input = screen.getByLabelText('代理 URL') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'http://draft:8080' } });
    act(() =>
      value.publishWiFi({
        ...wifi('Home'),
        wifi: { networks: [{ ssid: 'Home', proxy_mode: 'proxy', proxy_url: 'http://saved:8080' }] },
      }),
    );
    expect(input.value).toBe('http://draft:8080');
    expect(screen.getByRole('switch').getAttribute('aria-checked')).toBe('true');
  });
  it('updates navigation summaries from shared storage, firmware and saved configuration', () => {
    const value = model();
    value.resources.storage.publish({ card: { present: false, mounted: false } });
    value.resources.firmware.publish({ firmware: { current_version: 'old' } });
    const view = mount(value, <Navigation model={value} navigate={vi.fn()} path="/" />);
    act(() => {
      value.resources.storage.publish({ card: { present: true, mounted: true } });
      value.resources.firmware.publish({ firmware: { current_version: 'new-version' } });
      value.publishSave({
        config: { agent: { input_mode: 'realtime' } },
        revision: 'new',
        state: 'applied',
      });
    });
    expect(view.container.querySelector('[data-action="open-storage"]')!.textContent).toContain(
      '已连接',
    );
    expect(view.container.querySelector('[data-action="open-voice"]')!.textContent).toContain(
      '实时模式',
    );
    expect(view.container.querySelector('[data-action="open-firmware"]')!.textContent).toContain(
      'new-version',
    );
  });
  it('continues polling after an idle OTA log mounts and discovers an external update', async () => {
    let running = false;
    const value = model(async (path) =>
      path === '/api/ota/status'
        ? {
            ota_update_running: running,
            ota_log: { log: running ? 'message="ota check: start"' : '' },
          }
        : {},
    );
    value.activate('/firmware/log');
    mount(value, <OTAPage {...props(value, '/firmware/log')} logOnly />);
    await act(flush);
    expect(screen.getByText('还没有更新记录')).toBeTruthy();
    running = true;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2000);
    });
    expect(screen.getByText('message="ota check: start"')).toBeTruthy();
  });
  it('retains open MCP details and installation JSON while server status refreshes', () => {
    const value = model();
    value.resources.mcp.publish({
      servers: [{ name: 'demo', state: 'disconnected', enabled: true, tools: [] }],
    });
    mount(value, <MCPPage {...props(value, '/advanced/mcp')} />);
    fireEvent.click(screen.getByRole('button', { name: /demo/ }));
    const details = screen.getByRole('dialog');
    act(() =>
      value.resources.mcp.publish({
        servers: [{ name: 'demo', state: 'connected', enabled: true, tools: ['search'] }],
      }),
    );
    expect(screen.getByRole('dialog')).toBe(details);
    expect(screen.getByText('search')).toBeTruthy();
    fireEvent.keyDown(details, { key: 'Escape' });
    fireEvent.click(screen.getByRole('button', { name: '安装 MCP' }));
    const input = screen.getByLabelText('MCP JSON 配置') as HTMLTextAreaElement;
    fireEvent.change(input, { target: { value: '{"mcpServers":{' } });
    input.focus();
    act(() => value.resources.mcp.publish({ servers: [] }));
    expect(input.value).toBe('{"mcpServers":{');
    expect(document.activeElement).toBe(input);
  });
  it('keeps MCP details closed when an uninstalled server is installed again', async () => {
    const server = { name: 'demo', state: 'connected', enabled: true, tools: [] };
    let servers = [server];
    const value = model(
      async (path, options) => {
        if (options?.method === 'DELETE') {
          servers = [];
          return { ok: true };
        }
        if (path === '/api/mcp/servers') return { servers };
        return { state: 'applied' };
      },
      false,
      true,
    );
    value.activate('/advanced/mcp');
    await act(flush);
    mount(
      value,
      <>
        <OverlayHost />
        <MCPPage {...props(value, '/advanced/mcp')} />
      </>,
    );
    fireEvent.click(screen.getByRole('button', { name: /demo/ }));
    fireEvent.click(screen.getByRole('button', { name: '卸载' }));
    await act(flush);
    fireEvent.click(screen.getAllByRole('button', { name: '卸载' }).at(-1)!);
    await act(flush);
    expect(screen.queryByRole('dialog')).toBe(null);
    servers = [server];
    await act(() => value.resources.mcp.refresh());
    expect(screen.getByRole('button', { name: /demo/ })).toBeTruthy();
    expect(screen.queryByRole('dialog')).toBe(null);
  });
  it('retries snapshot failure and keeps the editor mounted on later observations', async () => {
    let offline = true;
    const value = model(async () => {
      if (offline) throw new Error('device offline');
      return { config: {} };
    });
    await value.start();
    const page = props(value, '/model');
    mount(
      value,
      <RequireConfig props={page}>
        <ValueField label="draft" saved="" onCommit={async () => 'saved'} />
      </RequireConfig>,
    );
    expect(screen.getByText('device offline')).toBeTruthy();
    offline = false;
    fireEvent.click(screen.getByText('重试'));
    await act(flush);
    const input = screen.getByLabelText('draft') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'unsaved' } });
    input.focus();
    act(() =>
      value.publishSave({
        config: { model: { model: 'external' } },
        state: 'applied',
        revision: 'two',
      }),
    );
    expect(screen.getByLabelText('draft')).toBe(input);
    expect(input.value).toBe('unsaved');
  });
  it('keeps local mobile shortcuts available during configuration read failure', async () => {
    const value = model(async () => {
      throw new Error('cannot read TOML');
    }, true);
    await value.start();
    mount(value, <LocalOverview {...props(value, '/')} />);
    expect(screen.getByText('cannot read TOML')).toBeTruthy();
    expect(screen.getByText('模型设置')).toBeTruthy();
    expect(screen.getByText('日志')).toBeTruthy();
  });
});
