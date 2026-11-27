import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { Provider } from 'jotai';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { createDeviceState } from '../state/device';
import type { Configuration, PageProps, Transport } from '../types';
import { DecisionPage, ModelPicker, ProviderForm } from '../pages/Models';
import { VoiceOptions } from '../pages/Voice';
import { RawConfigPage } from '../pages/Advanced';
import { ValueField } from '../components/fields';
import { ChoiceRow, Dropdown } from '../components/choices';
import { OverlayHost, Modal, confirmSheet, showProgress, toast } from '../components/overlays';
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
  localStorage.clear();
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
it('supports keyboard navigation in dropdown options', () => {
  render(
    <Dropdown
      label="Provider"
      selected="first"
      options={[
        { value: 'first', label: 'First' },
        { value: 'second', label: 'Second' },
        { value: 'third', label: 'Third' },
      ]}
      onSelect={vi.fn()}
    />,
  );
  fireEvent.click(screen.getByRole('button', { name: 'Provider First' }));
  expect(document.activeElement).toBe(screen.getByRole('option', { name: 'First' }));
  fireEvent.keyDown(document.activeElement!, { key: 'ArrowDown' });
  expect(document.activeElement).toBe(screen.getByRole('option', { name: 'Second' }));
  fireEvent.keyDown(document.activeElement!, { key: 'End' });
  expect(document.activeElement).toBe(screen.getByRole('option', { name: 'Third' }));
  fireEvent.keyDown(document.activeElement!, { key: 'Escape' });
  expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Provider First' }));
});
it('expires only the toast invocation that scheduled its timer', async () => {
  vi.useFakeTimers();
  render(<OverlayHost />);
  try {
    act(() => toast('Same message', { durationMs: 1000 }));
    await act(async () => vi.advanceTimersByTimeAsync(500));
    act(() => toast('Same message', { durationMs: 1000 }));
    await act(async () => vi.advanceTimersByTimeAsync(500));
    expect(screen.getByRole('status').textContent).toBe('Same message');
    await act(async () => vi.advanceTimersByTimeAsync(500));
    expect(screen.queryByRole('status')).toBe(null);
  } finally {
    vi.useRealTimers();
  }
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
it('submits a reverted field value while its earlier save is pending', async () => {
  const finishes: ((result: 'saved') => void)[] = [];
  const commit = vi.fn(() => new Promise<'saved'>((resolve) => finishes.push(resolve)));
  render(<ValueField label="Model" saved="A" onCommit={commit} />);
  const input = screen.getByLabelText('Model') as HTMLInputElement;
  fireEvent.change(input, { target: { value: 'B' } });
  fireEvent.blur(input);
  fireEvent.change(input, { target: { value: 'A' } });
  fireEvent.blur(input);
  expect(commit).toHaveBeenCalledTimes(2);
  await act(async () => {
    finishes.forEach((finish) => finish('saved'));
    await flush();
  });
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
  const input = screen.getByRole('combobox', { name: 'Decision model' }) as HTMLInputElement;
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
it('retains saved custom main models after switching, recreating the app and a catalog failure', async () => {
  let config = {
    model: { provider: 'account', model: 'listed' },
    model_providers: { account: { type: 'openai', base_url: 'https://one.example' } },
  };
  let offline = false;
  const send: Transport = async (path, options) => {
    if (path.startsWith('/api/models')) {
      if (offline) throw new Error('catalog offline');
      return { models: [{ id: 'listed', name: 'Listed model' }] };
    }
    if (path === '/api/config/plan') return { apply: 'live' };
    if (path === '/api/config/test') return { ok: true };
    if (options?.method === 'PATCH')
      config = {
        ...config,
        model: { ...config.model, ...JSON.parse(String(options.body)).config.model },
      };
    return { config, config_valid: true, apply: 'live' };
  };
  const app = setup(send, config);
  app.props.route.path = '/model/models';
  const view = app.mount(<ModelPicker {...app.props} />);
  await act(flush);
  expect(screen.getAllByRole('button', { name: 'Listed model listed' })).toHaveLength(1);
  fireEvent.change(screen.getByLabelText('Other model'), { target: { value: 'custom/saved' } });
  fireEvent.click(screen.getByRole('button', { name: 'Use' }));
  await act(flush);
  expect(screen.getByRole('button', { name: 'custom/saved custom/saved' })).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Listed model listed' }));
  await act(flush);
  view.unmount();
  app.model.dispose();

  offline = true;
  const fresh = setup(send, config);
  fresh.props.route.path = '/model/models';
  fresh.mount(<ModelPicker {...fresh.props} />);
  await act(flush);
  const custom = screen.getByRole('button', { name: 'custom/saved custom/saved' });
  expect(screen.getByText('catalog offline')).toBeTruthy();
  fireEvent.click(custom);
  await act(flush);
  expect(config.model.model).toBe('custom/saved');
  config = {
    ...config,
    model_providers: { account: { type: 'openai', base_url: 'https://two.example' } },
    model: { ...config.model, model: 'different-endpoint-model' },
  };
  act(() => fresh.model.publishSave({ config }));
  await act(flush);
  expect(screen.queryByRole('button', { name: 'custom/saved custom/saved' })).toBe(null);
  fresh.model.dispose();
});
it('excludes rejected custom models from saved choices', async () => {
  const config = { model: { provider: 'demo', model: 'original' } };
  const send: Transport = async (path) => {
    if (path.startsWith('/api/models')) return { models: [] };
    if (path === '/api/config/plan') throw { status: 400, error: 'rejected' };
    return { config, config_valid: true };
  };
  const app = setup(send, config);
  app.mount(<ModelPicker {...app.props} />);
  await act(flush);
  fireEvent.change(screen.getByLabelText('Other model'), { target: { value: 'custom/rejected' } });
  fireEvent.click(screen.getByRole('button', { name: 'Use' }));
  await act(flush);
  expect(screen.queryByRole('button', { name: 'custom/rejected custom/rejected' })).toBe(null);
  expect(JSON.stringify(app.model.store.get(app.model.modelHistory))).not.toContain(
    'custom/rejected',
  );
  app.model.dispose();
});
it('warns after saving a model when the connection probe fails', async () => {
  let config: Configuration = {
    model: { provider: 'demo', model: 'original' },
    model_providers: { demo: { type: 'openai' } },
  };
  const send: Transport = async (path, options) => {
    if (path.startsWith('/api/models')) return { models: [{ id: 'listed', recommended: true }] };
    if (path === '/api/config/plan') return { apply: 'live' };
    if (path === '/api/config/test')
      return { ok: false, results: [{ passed: false, detail: '401' }] };
    if (options?.method === 'PATCH') {
      config = {
        ...config,
        model: { ...config.model, ...JSON.parse(String(options.body)).config.model },
      };
    }
    return { config, config_valid: true, apply: 'live' };
  };
  const app = setup(send, config);
  app.props.route.path = '/model/models';
  app.mount(<ModelPicker {...app.props} />);
  await act(flush);
  fireEvent.click(screen.getByRole('button', { name: 'listed Recommended · listed' }));
  await act(flush);
  expect(config.model?.model).toBe('listed');
  expect(app.props.navigate).toHaveBeenCalledWith('/model/main');
  await act(flush);
  expect(screen.getByText('Connection failed: 401')).toBeTruthy();
  app.model.dispose();
});
it.each(['voice', 'decision'])(
  'adds confirmed custom %s models to suggestions and keeps them after switching',
  async (kind) => {
    let config: Configuration =
      kind === 'voice'
        ? {
            voice_model: { provider: 'demo' },
            voice_model_providers: { demo: { type: 'qwen', model: 'custom/first' } },
          }
        : { decision: { provider: 'openrouter', model: 'custom/first' } };
    const app = setup(async (path) => {
      if (path.startsWith('/api/models')) return { models: [] };
      if (path === '/api/config/schema')
        return {
          sections: [
            {
              name: 'voice_model_providers',
              fields: [{ key: 'model', label: 'Voice model', widget: 'text' }],
            },
          ],
        };
      return { config, config_valid: true };
    }, config);
    app.props.route =
      kind === 'voice'
        ? { path: '/voice/model', query: { kind: 'realtime' } }
        : { path: '/model/decision' };
    app.mount(kind === 'voice' ? <VoiceOptions {...app.props} /> : <DecisionPage {...app.props} />);
    await act(flush);
    config =
      kind === 'voice'
        ? { ...config, voice_model_providers: { demo: { type: 'qwen', model: 'custom/second' } } }
        : { decision: { provider: 'openrouter', model: 'custom/second' } };
    act(() => app.model.publishSave({ config }));
    await act(flush);
    const input = screen.getByRole('combobox');
    fireEvent.focus(input);
    fireEvent.change(input, { target: { value: '' } });
    expect(screen.getByRole('option', { name: 'custom/first' })).toBeTruthy();
    expect(screen.getByRole('option', { name: 'custom/second' })).toBeTruthy();
    app.model.dispose();
  },
);
it.each(['click', 'keyboard'] as const)(
  'selects an inline decision model by %s while keeping custom input and focus',
  async (method) => {
    let config = { decision: { provider: 'openrouter', model: 'old' } };
    const writes: unknown[] = [];
    const { model, props, mount } = setup(async (path, options) => {
      if (path.startsWith('/api/models'))
        return {
          models: [
            { id: '~typesafe/jev-latest', name: 'Jev Latest' },
            { id: '~typesafe/jev-1.14', name: 'Jev 1.14' },
          ],
        };
      if (path === '/api/config/plan') return { apply: 'live' };
      if (path === '/api/config' && options?.method === 'PATCH') {
        const patch = JSON.parse(String(options.body)).config;
        writes.push(patch);
        config = { decision: { ...config.decision, ...patch.decision } };
      }
      return { config, config_valid: true, apply: 'live' };
    }, config);
    mount(<DecisionPage {...props} />);
    const user = userEvent.setup();
    await act(flush);
    const input = screen.getByRole('combobox', { name: 'Decision model' }) as HTMLInputElement;
    await user.click(input);
    await user.clear(input);
    await user.type(input, 'Jev');
    const list = screen.getByRole('listbox', { name: 'Decision model' });
    expect(list.classList.contains('ds-suggestions')).toBe(true);
    expect(screen.getByRole('option', { name: 'Jev Latest ~typesafe/jev-latest' })).toBeTruthy();
    fireEvent.keyDown(input, { key: 'ArrowDown', isComposing: true });
    fireEvent.keyDown(input, { key: 'Enter', isComposing: true });
    expect(writes).toEqual([]);
    expect(input.getAttribute('aria-activedescendant')).toBe(null);
    if (method === 'click') {
      await user.click(screen.getByRole('option', { name: 'Jev 1.14 ~typesafe/jev-1.14' }));
    } else {
      await user.keyboard('{ArrowDown}{ArrowDown}{Enter}');
    }
    await act(flush);
    expect(writes).toEqual([{ decision: { model: '~typesafe/jev-1.14' } }]);
    expect(input.value).toBe('~typesafe/jev-1.14');
    expect(document.activeElement).toBe(input);
    expect(screen.queryByRole('listbox')).toBe(null);
    await user.clear(input);
    await user.type(input, 'Jev');
    await user.keyboard('{ArrowUp}');
    expect(screen.getByRole('option', { selected: true }).textContent).toContain('Jev 1.14');
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('listbox')).toBe(null);
    expect(document.activeElement).toBe(input);
    await user.clear(input);
    await user.type(input, 'custom/model{Enter}');
    await act(flush);
    expect(writes).toEqual([
      { decision: { model: '~typesafe/jev-1.14' } },
      { decision: { model: 'custom/model' } },
    ]);
    model.dispose();
  },
);
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
it('locks provider and schema choices while a provider save is pending', async () => {
  let rejectSave!: (error: Error) => void;
  let notifySave!: () => void;
  const saveStarted = new Promise<void>((resolve) => {
    notifySave = resolve;
  });
  const { model, props, mount } = setup(
    async (path, options) => {
      if (path === '/api/config/schema')
        return {
          sections: [
            {
              name: 'model_providers',
              fields: [
                {
                  key: 'region',
                  widget: 'select',
                  enum: [
                    { value: 'one', label: 'One' },
                    { value: 'two', label: 'Two' },
                  ],
                },
                { key: 'base_url' },
              ],
            },
          ],
        };
      if (path === '/api/config/plan') return { apply: 'live' };
      if (path === '/api/config' && options?.method === 'PATCH') {
        notifySave();
        return new Promise((_resolve, reject) => {
          rejectSave = reject;
        });
      }
      return { config: {} };
    },
    {
      model_providers: {
        existing: { type: 'custom', region: 'one', base_url: 'https://old' },
      },
    },
  );
  const view = mount(
    <ProviderForm
      props={props}
      recordSection="model_providers"
      type="custom"
      existingName="existing"
      back="/model/providers"
    />,
  );
  await act(flush);
  fireEvent.change(screen.getByLabelText('Base URL'), { target: { value: 'https://new' } });
  fireEvent.submit(view.container.querySelector('form')!);
  await act(async () => {
    await saveStarted;
    await flush();
  });

  const choiceRows = () => [
    ...view.container.querySelectorAll<HTMLElement>('.ds-form-card .ds-row--outlined'),
  ];
  expect(choiceRows()).toHaveLength(2);
  choiceRows().forEach((row) => fireEvent.click(row));
  expect(screen.queryByRole('dialog')).toBe(null);

  await act(async () => {
    rejectSave(new Error('write failed'));
    await flush();
  });
  fireEvent.click(choiceRows()[0]);
  expect(screen.getByRole('dialog')).toBeTruthy();
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
it('saves only the staged entries whose restart level was confirmed', async () => {
  let releasePlan!: (value: { apply: string }) => void;
  const send = vi.fn(async (path: string, options?: RequestInit) => {
    if (path === '/api/config/plan') {
      const patch = JSON.parse(String(options?.body)).config;
      if (patch.hid) return new Promise<{ apply: string }>((resolve) => (releasePlan = resolve));
      return { apply: 'agent_restart' };
    }
    return { apply: 'live', config: {} };
  });
  const { model, props, mount } = setup(send);
  function Form() {
    const saver = useSettings(props, 'Settings');
    return (
      <>
        <button onClick={() => void saver.change('agent', { agent: { input_mode: 'stt' } })}>
          Stage agent
        </button>
        <button onClick={() => void saver.change('hid', { hid: { keyboard_layout: 'azerty' } })}>
          Stage reboot
        </button>
        <button onClick={() => void saver.commit()}>Commit</button>
      </>
    );
  }
  mount(<Form />);
  fireEvent.click(screen.getByText('Stage agent'));
  await act(flush);
  fireEvent.click(screen.getByText('Stage reboot'));
  fireEvent.click(screen.getByText('Commit'));
  await act(flush);
  expect(screen.getByRole('dialog')).toBeTruthy();
  await act(async () => {
    releasePlan({ apply: 'reboot' });
    await flush();
  });
  fireEvent.click(screen.getByText('Save & Restart'));
  await act(flush);
  const writes = send.mock.calls
    .filter(([path, options]) => path === '/api/config' && options?.method === 'PATCH')
    .map(([, options]) => JSON.parse(String(options?.body)).config);
  expect(writes).toEqual([{ agent: { input_mode: 'stt' } }]);
  fireEvent.click(screen.getByText('Commit'));
  await act(flush);
  expect(screen.getByRole('dialog')).toBeTruthy();
  expect(screen.getByText(/reboot/i)).toBeTruthy();
  model.dispose();
});
it('replaces a staged field after a duplicate submission and a revert during another commit', async () => {
  let releasePlan!: (value: { apply: string }) => void;
  const send = vi.fn(async (path: string, options?: RequestInit) => {
    const patch = JSON.parse(String(options?.body)).config;
    if (path === '/api/config/plan') {
      if (patch.model?.model === 'B')
        return new Promise<{ apply: string }>((resolve) => (releasePlan = resolve));
      return { apply: patch.agent ? 'agent_restart' : 'live' };
    }
    return { apply: 'live', config: { model: { model: 'A' }, ...patch } };
  });
  const { model, props, mount } = setup(send, { model: { model: 'A' } });
  function Form() {
    const saver = useSettings(props, 'Settings');
    return (
      <>
        <ValueField
          label="Model"
          saved={saver.config.model?.model}
          onCommit={(value) => saver.change('model', { model: { model: String(value) } })}
        />
        <button onClick={() => void saver.change('agent', { agent: { input_mode: 'stt' } })}>
          Stage agent
        </button>
        <button onClick={() => void saver.commit()}>Commit</button>
      </>
    );
  }
  mount(<Form />);
  fireEvent.click(screen.getByText('Stage agent'));
  await act(flush);
  const input = screen.getByLabelText('Model') as HTMLInputElement;
  fireEvent.change(input, { target: { value: 'B' } });
  fireEvent.blur(input);
  fireEvent.change(input, { target: { value: 'C' } });
  fireEvent.change(input, { target: { value: 'B' } });
  fireEvent.blur(input);
  expect(send.mock.calls.filter(([path]) => path === '/api/config/plan')).toHaveLength(2);
  fireEvent.click(screen.getByText('Commit'));
  await act(flush);
  await act(async () => {
    releasePlan({ apply: 'live' });
    await flush();
  });
  fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
  await act(flush);
  fireEvent.change(input, { target: { value: 'A' } });
  fireEvent.blur(input);
  await act(flush);
  expect(input.value).toBe('A');
  fireEvent.click(screen.getByText('Commit'));
  await act(flush);
  fireEvent.click(screen.getByRole('button', { name: 'Save & Restart' }));
  await act(flush);
  const writes = send.mock.calls
    .filter(([path, options]) => path === '/api/config' && options?.method === 'PATCH')
    .map(([, options]) => JSON.parse(String(options?.body)).config);
  expect(writes).toEqual([{ model: { model: 'A' } }, { agent: { input_mode: 'stt' } }]);
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
it.each(['cancel', 'escape', 'scrim', 'confirm'])(
  'keeps confirmation focus contained and settles through %s',
  async (action) => {
    render(
      <>
        <button>Open confirmation</button>
        <OverlayHost />
      </>,
    );
    const opener = screen.getByRole('button', { name: 'Open confirmation' });
    opener.focus();
    let result!: Promise<boolean>;
    act(() => {
      result = confirmSheet({
        title: 'Update?',
        body: 'Services may restart.',
        confirmLabel: 'Update',
      });
    });
    const dialog = screen.getByRole('dialog', { name: 'Update?' });
    expect(document.getElementById(dialog.getAttribute('aria-describedby')!)?.textContent).toBe(
      'Services may restart.',
    );
    const cancel = screen.getByRole('button', { name: 'Cancel' });
    const confirm = screen.getByRole('button', { name: 'Update' });
    expect(document.activeElement).toBe(cancel);
    fireEvent.keyDown(cancel, { key: 'Tab' });
    expect(document.activeElement).toBe(confirm);
    fireEvent.keyDown(confirm, { key: 'Tab', shiftKey: true });
    expect(document.activeElement).toBe(cancel);
    if (action === 'escape') fireEvent.keyDown(cancel, { key: 'Escape' });
    else if (action === 'scrim') fireEvent.click(dialog.querySelector('.ds-action-sheet__scrim')!);
    else fireEvent.click(action === 'confirm' ? confirm : cancel);
    await expect(result).resolves.toBe(action === 'confirm');
    expect(screen.queryByRole('dialog')).toBe(null);
    expect(document.activeElement).toBe(opener);
  },
);
it('keeps restart progress modal until completion or a dismissible failure', () => {
  render(
    <>
      <button>Restart</button>
      <OverlayHost />
    </>,
  );
  const opener = screen.getByRole('button', { name: 'Restart' });
  opener.focus();
  let progress!: ReturnType<typeof showProgress>;
  act(() => {
    progress = showProgress('Restarting', 'Waiting for the device.');
  });
  fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
  expect(screen.getByRole('dialog', { name: 'Restarting' })).toBeTruthy();
  act(() => progress.fail('Device unavailable.'));
  expect(screen.getByRole('status').textContent).toBe('Device unavailable.');
  fireEvent.click(screen.getByRole('button', { name: 'Close' }));
  expect(screen.queryByRole('dialog')).toBe(null);
  expect(document.activeElement).toBe(opener);
  act(() => {
    progress = showProgress('Restarting', 'Waiting for the device.');
  });
  act(() => progress.remove());
  expect(screen.queryByRole('dialog')).toBe(null);
});
it('restores app interactivity only after the last overlapping modal closes', () => {
  const app = document.createElement('div');
  app.id = 'app';
  document.body.append(app);
  let firstOpen = true;
  let secondOpen = false;
  const content = () => (
    <>
      {firstOpen && (
        <Modal title="Progress" onClose={() => {}}>
          <button>Progress action</button>
        </Modal>
      )}
      {secondOpen && (
        <Modal title="Confirm" onClose={() => {}}>
          <button>Confirm action</button>
        </Modal>
      )}
    </>
  );
  const view = render(content(), { container: app });
  expect(app.inert).toBe(true);
  secondOpen = true;
  view.rerender(content());
  firstOpen = false;
  view.rerender(content());
  expect(app.inert).toBe(true);
  secondOpen = false;
  view.rerender(content());
  expect(app.inert).toBe(false);
  app.remove();
});
it('does not save settings when apply planning is unavailable', async () => {
  const send = vi.fn(async (path: string, _options?: RequestInit) => {
    if (path === '/api/config/plan')
      throw Object.assign(new Error('plan unavailable'), { status: 503 });
    return { apply: 'reboot', config: {} };
  });
  const { model, props, mount } = setup(send);
  function Form() {
    const saver = useSettings(props, 'Settings');
    const [result, setResult] = useState('');
    return (
      <>
        <button
          onClick={async () => setResult(await saver.change('wifi', { wifi: { enabled: true } }))}
        >
          Save setting
        </button>
        <output>{result}</output>
      </>
    );
  }
  mount(<Form />);
  fireEvent.click(screen.getByText('Save setting'));
  await act(flush);
  expect(screen.getByText('failed')).toBeTruthy();
  expect(
    send.mock.calls.some(
      ([path, options]) => path === '/api/config' && options?.method === 'PATCH',
    ),
  ).toBe(false);
  model.dispose();
});
