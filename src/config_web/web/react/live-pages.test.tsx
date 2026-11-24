import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import type { ReactNode } from 'react';
import { Provider } from 'jotai';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { PageProps } from './types';
import { createDeviceState, type DeviceState } from './state';
import { USBInternet, MCPPage, LocalOverview } from './pages';
import { OTAPage } from './ota';
import { WiFiDetail, WiFiPage } from './wifi';
import { Navigation } from './navigation';
import { ReadyEditor } from './editor';
import { applyLocale } from '../assets/js/app/data.js';

vi.mock('../assets/js/app/wifi-service.js', async (original) => ({
  ...(await original<object>()),
  scan: vi.fn(async () => [{ ssid: 'Cafe', secured: true }]),
}));
const models: DeviceState[] = [];
const flush = async () => {
  for (let n = 0; n < 12; n++) await Promise.resolve();
};
function model(send: Parameters<typeof createDeviceState>[0] = async () => ({}), local = false) {
  const value = createDeviceState(send, { local });
  models.push(value);
  return value;
}
function props(value: DeviceState, path: string): PageProps {
  return {
    model: value,
    route: { path, page: () => document.createElement('div') },
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
});
afterEach(() => {
  cleanup();
  models.splice(0).forEach((value) => value.dispose());
  vi.useRealTimers();
});

describe('live React views', () => {
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
    fireEvent.click(screen.getByRole('switch'));
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
    value.resources.device.publish({ firmware: { current_version: 'old' } });
    const view = mount(value, <Navigation model={value} navigate={vi.fn()} path="/" />);
    act(() => {
      value.resources.storage.publish({ card: { present: true, mounted: true } });
      value.resources.device.publish({ firmware: { current_version: 'new-version' } });
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
    const view = mount(value, <MCPPage {...props(value, '/advanced/mcp')} />);
    const details = view.container.querySelector('details')!;
    details.open = true;
    fireEvent.click(screen.getByText('安装 MCP'));
    const input = screen.getByLabelText('MCP JSON 配置') as HTMLTextAreaElement;
    fireEvent.change(input, { target: { value: '{"mcpServers":{' } });
    input.focus();
    act(() =>
      value.resources.mcp.publish({
        servers: [{ name: 'demo', state: 'connected', enabled: true, tools: ['search'] }],
      }),
    );
    expect(view.container.querySelector('details')).toBe(details);
    expect(details.open).toBe(true);
    expect(input.value).toBe('{"mcpServers":{');
    expect(document.activeElement).toBe(input);
    expect(screen.getByText('search')).toBeTruthy();
  });
  it('retries snapshot failure and keeps the editor mounted on later observations', async () => {
    let offline = true;
    const value = model(async () => {
      if (offline) throw new Error('device offline');
      return { config: {} };
    });
    await value.start();
    const page = props(value, '/model');
    page.route.page = vi.fn(() => {
      const node = document.createElement('input');
      node.setAttribute('aria-label', 'draft');
      return node;
    });
    mount(value, <ReadyEditor {...page} />);
    expect(screen.getByText('device offline')).toBeTruthy();
    offline = false;
    fireEvent.click(screen.getByText('重试'));
    await act(flush);
    const input = screen.getByLabelText('draft') as HTMLInputElement;
    input.value = 'unsaved';
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
    expect(page.route.page).toHaveBeenCalledTimes(1);
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
