import type { PageProps, EditorOptions, EditorContext } from './types';
import { useEffect, useRef, useState } from 'react';
import { useAtomValue } from 'jotai';
import { initI18n } from '../assets/js/app/data.js';
import { Button, ResourceError, Text } from './ui';
import { msg } from '../assets/js/ui/text.js';

/**
 * Editors own their drafts until unmount. Shared observations are delivered as
 * subscriptions, never by rebuilding the form with a new snapshot as a prop.
 */
export function Editor({
  route,
  model,
  navigate,
  header,
  options = {},
}: PageProps & { options?: EditorOptions }) {
  const host = useRef<HTMLDivElement>(null);
  const [error, setError] = useState<string | null>(null);
  const initial = useRef({ route, navigate, header, options });
  useEffect(() => {
    let cancelled = false;
    const subscriptions: (() => void)[] = [];
    const target = host.current;
    const spec = initial.current;
    const context: EditorContext = {
      isCurrent: () => !cancelled,
      ...spec.route,
      ...spec.options,
      localAgent: model.local,
      containerAgent: model.container,
      snapshot: structuredClone(model.store.get(model.current)),
      navigate: spec.navigate,
      header: (value) => {
        if (!cancelled) spec.header(value);
      },
      onWiFiChange: (listener) => {
        if (cancelled) return;
        subscriptions.push(
          model.store.sub(model.resources.wifi.atom, () =>
            listener(model.store.get(model.current)),
          ),
        );
      },
      sharedState: model,
    };
    Promise.resolve()
      .then(() => (cancelled ? null : spec.route.page(context)))
      .then((node) => {
        if (cancelled || !node) return;
        target?.replaceChildren(node);
        initI18n();
      })
      .catch((failure) => {
        if (!cancelled) setError(failure.message);
      });
    return () => {
      cancelled = true;
      subscriptions.forEach((stop) => stop());
      target?.replaceChildren();
    };
  }, [model]);
  return (
    <div ref={host} className="editor-host">
      {error && <p role="alert">{error}</p>}
    </div>
  );
}

export function ReadyEditor(props: PageProps & { options?: EditorOptions }) {
  const state = useAtomValue(props.model.resources.snapshot.atom);
  // The raw editor reads the file independently and remains a recovery route.
  if (!state.data && state.error && props.route.path === '/advanced/config')
    return <Editor {...props} />;
  if (!state.data)
    return (
      <div role="status">
        <ResourceError model={props.model} name="snapshot" />
        {state.error ? (
          <Button onClick={() => props.model.resources.snapshot.refresh()}>
            <Text value={msg('ui.retry', '重试')} />
          </Button>
        ) : (
          <p>
            <Text value={msg('status.loading', '正在读取设备配置…')} />
          </p>
        )}
      </div>
    );
  return <Editor {...props} />;
}
