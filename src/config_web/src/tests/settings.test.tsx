import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { Provider } from 'jotai';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { createDeviceState } from '../state/device';
import type { PageProps, Transport } from '../types';
import { DecisionPage, ProviderForm } from '../pages/Models';
import { RawConfigPage } from '../pages/Advanced';
import { ValueField } from '../components/fields';
import { ChoiceRow, Dropdown } from '../components/choices';
import { OverlayHost, Modal } from '../components/overlays';
import { useSettings } from '../hooks/settings';
import { applyLocale } from '../lib/i18n';
const flush = async () => {
  for (let index = 0; index < 15; index++) await Promise.resolve();
};
function setup(send: Transport, config: import('../types').Configuration = {}) {
  const model = createDeviceState(send);
  model.publishSave({ config });
  const props: PageProps = {
    model,
    route: { path: '/model/decision' },
    header: vi.fn(),
    navigate: vi.fn(),
  };
  return {
    model,
    props,
    mount: (node: React.ReactNode) =>
      render(
        <Provider store={model.store}>
          <OverlayHost />
          {node}
        </Provider>,
      ),
  };
}
beforeEach(() => {
  applyLocale('en-US');
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});
it('blocks open dropdown choices when disabled and allows them again when enabled', () => {
  const select = vi.fn();
  const picker = (disabled: boolean) => (
    <Dropdown
      label="Provider"
      selected="first"
      options={[
        { value: 'first', label: 'First' },
        { value: 'second', label: 'Second' },
      ]}
      onSelect={select}
      disabled={disabled}
    />
  );
  const view = render(picker(false));
  fireEvent.click(screen.getByRole('button', { name: 'Provider First' }));
  view.rerender(picker(true));
  const second = screen.getByRole('option', { name: 'Second' });
  expect((second as HTMLButtonElement).disabled).toBe(true);
  fireEvent.click(second);
  expect(select).not.toHaveBeenCalled();
  view.rerender(picker(false));
  fireEvent.click(second);
  expect(select).toHaveBeenCalledWith('second');
});
it('keeps staged choices through observations, restores focus and rolls back a rejected choice', async () => {
  let outcome: 'staged' | 'failed' = 'staged';
  const select = vi.fn(async () => outcome);
  const choices = [
    { value: 'first', label: 'First' },
    { value: 'second', label: 'Second' },
  ];
  const picker = (selected: string) => (
    <ChoiceRow label="Retention" selected={selected} options={choices} onSelect={select} />
  );
  const view = render(picker('first'));
  const trigger = screen.getByRole('button', { name: 'Retention First' });
  trigger.focus();
  fireEvent.click(trigger);
  fireEvent.click(screen.getByRole('button', { name: 'Second' }));
  await act(flush);
  expect(document.activeElement).toBe(trigger);
  view.rerender(picker('first'));
  expect(screen.getByRole('button', { name: 'Retention Second' })).toBe(trigger);
  view.rerender(picker('second'));
  view.rerender(picker('first'));
  expect(screen.getByRole('button', { name: 'Retention First' })).toBe(trigger);
  outcome = 'failed';
  fireEvent.click(trigger);
  fireEvent.click(screen.getByRole('button', { name: 'Second' }));
  await act(flush);
  expect(screen.getByRole('button', { name: 'Retention First' })).toBe(trigger);
  fireEvent.click(trigger);
  fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
  expect(screen.queryByRole('dialog')).toBe(null);
  expect(document.activeElement).toBe(trigger);
});
it('keeps a newer draft focused when an older save completes and allows a rejected value to retry', async () => {
  let finish!: (result: 'saved' | 'failed') => void;
  const commit = vi.fn(
    () =>
      new Promise<'saved' | 'failed'>((resolve) => {
        finish = resolve;
      }),
  );
  render(<ValueField label="Model" saved="original" onCommit={commit} />);
  const input = screen.getByLabelText('Model') as HTMLInputElement;
  fireEvent.change(input, { target: { value: 'first' } });
  fireEvent.blur(input);
  fireEvent.change(input, { target: { value: 'new draft' } });
  input.focus();
  await act(async () => {
    finish('saved');
    await flush();
  });
  expect(input.value).toBe('new draft');
  expect(document.activeElement).toBe(input);
  fireEvent.blur(input);
  await act(async () => {
    finish('failed');
    await flush();
  });
  expect(input.value).toBe('new draft');
  fireEvent.blur(input);
  expect(commit).toHaveBeenCalledTimes(3);
});
it('resumes following confirmed values after a staged field is persisted', async () => {
  const commit = vi.fn(async () => 'staged' as const);
  const { rerender } = render(<ValueField label="Model" saved="original" onCommit={commit} />);
  const input = screen.getByLabelText('Model') as HTMLInputElement;
  fireEvent.change(input, { target: { value: 'pending' } });
  fireEvent.blur(input);
  await act(flush);
  rerender(<ValueField label="Model" saved="external" onCommit={commit} />);
  expect(input.value).toBe('pending');
  rerender(<ValueField label="Model" saved="pending" onCommit={commit} />);
  rerender(<ValueField label="Model" saved="later external" onCommit={commit} />);
  expect(input.value).toBe('later external');
});
it('keeps model suggestions optional and publishes persisted values without erasing another field draft', async () => {
  let finish!: (value: unknown) => void;
  const writes: Record<string, unknown>[] = [];
  const { model, props, mount } = setup(
    async (path, options) => {
      if (path.startsWith('/api/models')) throw new Error('catalog offline');
      if (path === '/api/config/plan') return { apply: 'live' };
      if (path === '/api/config') {
        const patch = JSON.parse(String(options?.body)).config;
        writes.push(patch);
        return new Promise((resolve) => {
          finish = resolve;
        });
      }
      return {};
    },
    {
      decision: {
        provider: 'openrouter',
        enabled: true,
        has_api_key: true,
        model: 'old',
        base_url: 'https://old',
      },
    },
  );
  mount(<DecisionPage {...props} />);
  await act(flush);
  const input = screen.getByLabelText('Decision model') as HTMLInputElement;
  const url = screen.getByLabelText('Decision model Base URL') as HTMLInputElement;
  fireEvent.change(input, { target: { value: 'custom/model' } });
  fireEvent.blur(input);
  await act(flush);
  fireEvent.change(url, { target: { value: '' } });
  url.focus();
  await act(async () => {
    finish({
      config: {
        decision: {
          provider: 'openrouter',
          has_api_key: true,
          model: 'custom/model',
          base_url: 'https://old',
        },
      },
      apply: 'live',
    });
    await flush();
  });
  expect(url.value).toBe('');
  expect(document.activeElement).toBe(url);
  expect(writes[0]).toEqual({ decision: { model: 'custom/model' } });
  fireEvent.blur(url);
  await act(flush);
  expect(writes[1]).toEqual({ decision: { base_url: null } });
  model.dispose();
});
it('omits blank stored secrets and follows schema visibility in provider forms', async () => {
  const writes: unknown[] = [];
  const { model, props, mount } = setup(
    async (path, options) => {
      if (path === '/api/config/schema')
        return {
          sections: [
            {
              name: 'model_providers',
              fields: [
                { key: 'api_key', secret: true },
                { key: 'base_url' },
                {
                  key: 'secret_key',
                  secret: true,
                  visibleWhen: {
                    all: [{ field: 'model_providers.type', op: 'eq', value: 'other' }],
                  },
                },
              ],
            },
          ],
        };
      if (path === '/api/config/plan') return { apply: 'live' };
      if (path === '/api/config') {
        writes.push(JSON.parse(String(options?.body)).config);
        return {
          config: {
            model_providers: {
              existing: { type: 'openai', has_api_key: true, base_url: 'https://new' },
            },
          },
          apply: 'live',
        };
      }
      return {};
    },
    {
      model_providers: { existing: { type: 'openai', has_api_key: true, base_url: 'https://old' } },
    },
  );
  mount(
    <ProviderForm
      props={props}
      recordSection="model_providers"
      type="openai"
      existingName="existing"
      back="/model/providers"
    />,
  );
  await act(flush);
  expect(screen.queryByLabelText('secret_key')).toBe(null);
  const input = screen.getByLabelText('Base URL');
  fireEvent.change(input, { target: { value: 'https://new' } });
  fireEvent.submit(input.closest('form')!);
  await act(flush);
  expect(writes).toEqual([
    { model_providers: { existing: { type: 'openai', base_url: 'https://new' } } },
  ]);
  model.dispose();
});
it('computes the current restart plan before committing and guards native saves against duplicates', async () => {
  const send = vi.fn(async (path: string, _options?: RequestInit) =>
    path === '/api/config/plan'
      ? { apply: 'agent_restart' }
      : { apply: 'live', config: { agent: { input_mode: 'stt' } } },
  );
  const { model, props, mount } = setup(send);
  function Form() {
    const saver = useSettings(props, 'Settings');
    return (
      <button
        onClick={async () => {
          await saver.change('mode', { agent: { input_mode: 'stt' } });
          await saver.commit();
        }}
      >
        Change
      </button>
    );
  }
  mount(<Form />);
  fireEvent.click(screen.getByText('Change'));
  await act(flush);
  expect(screen.getByRole('dialog')).toBeTruthy();
  const writes = () =>
    send.mock.calls.filter(
      ([path, options]) => path === '/api/config' && options?.method === 'PATCH',
    );
  expect(writes()).toHaveLength(0);
  window.dispatchEvent(new Event('aico:save-request'));
  fireEvent.click(screen.getByText('Save & Restart'));
  await act(flush);
  expect(writes()).toHaveLength(1);
  model.dispose();
});
it('keeps raw TOML recovery writable only after a successful read and preserves failed edits', async () => {
  let offline = true;
  vi.stubGlobal('fetch', async () => {
    if (offline) throw new Error('TOML offline');
    return new Response('[agent]\nlocale="en-US"');
  });
  const send = vi.fn(async () => {
    throw new Error('invalid TOML');
  });
  const { model, props, mount } = setup(send);
  props.route.path = '/advanced/config';
  mount(<RawConfigPage {...props} />);
  await act(flush);
  const input = screen.getByLabelText('Edit configuration') as HTMLTextAreaElement;
  expect(input.disabled).toBe(true);
  offline = false;
  fireEvent.click(screen.getByText('Retry'));
  await act(flush);
  expect(input.disabled).toBe(false);
  fireEvent.change(input, { target: { value: 'draft = invalid' } });
  fireEvent(window, new Event('aico:save-request'));
  await act(flush);
  expect(send).toHaveBeenCalledWith(
    '/api/config/backup',
    expect.objectContaining({
      method: 'PUT',
      body: 'draft = invalid',
    }),
  );
  expect(input.value).toBe('draft = invalid');
  model.dispose();
});
it('traps dialog focus and restores its opener on close', () => {
  const view = render(
    <>
      <button>Open</button>
    </>,
  );
  const opener = screen.getByText('Open');
  opener.focus();
  view.rerender(
    <>
      <button>Open</button>
      <Modal title="Join" onClose={() => view.rerender(<button>Open</button>)}>
        <input aria-label="Password" />
        <button>Join</button>
      </Modal>
    </>,
  );
  const input = screen.getByLabelText('Password');
  expect(document.activeElement).toBe(input);
  fireEvent.keyDown(input, { key: 'Tab', shiftKey: true });
  expect(document.activeElement).toBe(screen.getByText('Join', { selector: 'button' }));
  fireEvent.keyDown(input, { key: 'Escape' });
  expect(screen.queryByRole('dialog')).toBe(null);
  expect(document.activeElement).toBe(opener);
});
