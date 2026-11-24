import { useEffect, useRef, useState } from 'react';
import { useAtomValue } from 'jotai';
import type { ApplicationData, ConfigPatch, JsonObject, PageProps, TextSpec } from '../types';
import { apiError, json } from '../api/client';
import { confirmRestart, needsRestart, performRestart, saveLabel } from '../lib/apply';
import { postToHost } from '../lib/host';
import { msg, resolve } from '../lib/i18n';
import { toast } from '../components/overlays';
import { Button, Text } from '../components/ui';
export type SaveOutcome = 'saved' | 'staged' | 'invalid' | 'failed' | 'superseded';
const rank: Record<string, number> = { live: 0, agent_restart: 1, reboot: 2 };
export function mergePatch(target: JsonObject, patch: JsonObject): JsonObject {
  for (const [key, value] of Object.entries(patch)) {
    if (value && typeof value === 'object' && !Array.isArray(value))
      target[key] = mergePatch(
        target[key] && typeof target[key] === 'object' && !Array.isArray(target[key])
          ? target[key]
          : {},
        value,
      );
    else target[key] = value;
  }
  return target;
}
export function usePageHeader(
  props: PageProps,
  title: TextSpec,
  dirty = false,
  submit?: () => unknown,
  level = 'live',
  back?: string,
  enabled = true,
) {
  const { header, navigate, route } = props;
  const submitRef = useRef(submit);
  submitRef.current = submit;
  const label = resolve(title);
  useEffect(() => {
    if (!enabled) return;
    header({
      title: label,
      onBack: () => navigate(back || route.path.slice(0, route.path.lastIndexOf('/')) || '/'),
      action: dirty ? (
        <Button variant="quiet" action="save-form" onClick={() => void submitRef.current?.()}>
          <Text value={saveLabel(level)} />
        </Button>
      ) : null,
    });
    postToHost({
      type: 'aico_config_save_state',
      title: label,
      dirty,
      apply: level,
      action_label: resolve(saveLabel(level)),
    });
    const onSave = () => {
      if (dirty) void submitRef.current?.();
    };
    window.addEventListener('aico:save-request', onSave);
    return () => window.removeEventListener('aico:save-request', onSave);
  }, [header, navigate, route.path, label, dirty, level, back, enabled]);
}
export function useSettings(props: PageProps, title: TextSpec, back?: string, header = true) {
  const config = useAtomValue(props.model.config) || {};
  const staged = useRef(new Map<string, { patch: ConfigPatch; level: string }>());
  const sequence = useRef(new Map<string, number>());
  const mounted = useRef(true);
  const [, update] = useState(0);
  const [busy, setBusy] = useState(false);
  const committing = useRef(false);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  const repaint = () => {
    if (mounted.current) update((value) => value + 1);
  };
  const level = [...staged.current.values()].reduce(
    (max, item) => (rank[item.level] > rank[max] ? item.level : max),
    'live',
  );
  const save = async (patch: ConfigPatch) => {
    const result = await props.model.send<ApplicationData & { apply?: string }>('/api/config', {
      method: 'PATCH',
      ...json({ config: patch }),
    });
    props.model.publishSave(result);
    return result;
  };
  async function change(
    key: string,
    patch: ConfigPatch,
    { quietInvalid = false } = {},
  ): Promise<SaveOutcome> {
    if (committing.current) return 'failed';
    const id = (sequence.current.get(key) || 0) + 1;
    sequence.current.set(key, id);
    let apply = 'live';
    try {
      apply =
        (
          await props.model.send<{ apply?: string }>('/api/config/plan', {
            method: 'POST',
            ...json({ config: patch }),
          })
        ).apply || 'live';
    } catch (caught) {
      if (sequence.current.get(key) !== id || !mounted.current) return 'superseded';
      const error = apiError(caught);
      if (error.status === 400) {
        if (!quietInvalid) toast(error.message);
        return 'invalid';
      }
    }
    if (sequence.current.get(key) !== id || !mounted.current) return 'superseded';
    if (needsRestart(apply)) {
      staged.current.set(key, { patch, level: apply });
      repaint();
      return 'staged';
    }
    staged.current.delete(key);
    repaint();
    try {
      const result = await save(patch);
      if (sequence.current.get(key) !== id || !mounted.current) return 'superseded';
      if (needsRestart(result.apply)) {
        if (await confirmRestart(result.apply!)) await performRestart(result.apply!);
        else toast(msg('apply.saved_pending_restart', '已保存，重启后生效'));
      } else toast(msg('basic.saved', '已保存'));
      return 'saved';
    } catch (caught) {
      const error = apiError(caught);
      if (error.persisted) props.model.publishSave(error as ApplicationData);
      if (sequence.current.get(key) !== id) return 'superseded';
      if (props.model.local && error.persisted) {
        toast(msg('local.saved_unapplied', '已保存到本机，尚未应用到 Agent'));
        return 'saved';
      }
      toast(error.message);
      return 'failed';
    }
  }
  async function commit(): Promise<boolean> {
    if (!staged.current.size || committing.current) return false;
    committing.current = true;
    if (mounted.current) setBusy(true);
    try {
      const restart = [...staged.current.values()].reduce(
        (max, item) => ((rank[item.level] || 0) > (rank[max] || 0) ? item.level : max),
        'live',
      );
      if (!(await confirmRestart(restart)) || !mounted.current) return false;
      const entries = new Map(staged.current);
      const result = await save(
        [...entries.values()].reduce((patch, item) => mergePatch(patch, item.patch), {}),
      );
      for (const [key, item] of entries)
        if (staged.current.get(key) === item) staged.current.delete(key);
      repaint();
      if (needsRestart(result.apply)) await performRestart(result.apply!);
      return true;
    } catch (caught) {
      const error = apiError(caught);
      if (error.persisted) props.model.publishSave(error);
      toast(error.message);
      return false;
    } finally {
      committing.current = false;
      if (mounted.current) setBusy(false);
    }
  }
  async function apply(
    key: string,
    patch: ConfigPatch,
    options?: { quietInvalid?: boolean },
  ): Promise<SaveOutcome> {
    const outcome = await change(key, patch, options);
    return outcome === 'staged' ? ((await commit()) ? 'saved' : 'staged') : outcome;
  }
  usePageHeader(props, title, staged.current.size > 0 && !busy, commit, level, back, header);
  return {
    config,
    change,
    apply,
    commit,
    busy,
    isStaged: (key: string) => staged.current.has(key),
  };
}
export type Settings = ReturnType<typeof useSettings>;
export const section = (
  config: import('../types').Configuration,
  key: string,
): Record<string, unknown> => {
  const value = config[key];
  return value && typeof value === 'object' && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
};
