import type { ReactNode } from 'react';
import type { TextSpec } from '../types';
import { atom, createStore, useAtomValue } from 'jotai';
import { createPortal } from 'react-dom';
import { useEffect, useId, useRef } from 'react';
import { msg, resolve } from '../lib/i18n';
import { Button, Text } from './ui';
const store = createStore();
interface Confirmation {
  title: TextSpec;
  body?: TextSpec;
  confirmLabel?: TextSpec;
  danger?: boolean;
  action?: string;
  done: (value: boolean) => void;
}
const confirmation = atom<Confirmation | null>(null);
const notice = atom('');
const progress = atom<{ title: TextSpec; body: TextSpec; failed?: boolean } | null>(null);
let noticeID = 0;
let noticeTimer: ReturnType<typeof setTimeout> | undefined;
let openModalCount = 0;
let modalApp: HTMLElement | null = null;
let modalAppWasInert = false;

function acquireModalInert() {
  if (openModalCount === 0) {
    modalApp = document.getElementById('app');
    modalAppWasInert = modalApp?.inert || false;
    if (modalApp) modalApp.inert = true;
  }
  openModalCount += 1;
  return () => {
    openModalCount -= 1;
    if (openModalCount === 0) {
      if (modalApp) modalApp.inert = modalAppWasInert;
      modalApp = null;
    }
  };
}

export function toast(value: TextSpec, { durationMs = 3500 }: { durationMs?: number } = {}) {
  const text = resolve(value);
  store.set(notice, text);
  const id = ++noticeID;
  if (noticeTimer) clearTimeout(noticeTimer);
  noticeTimer = undefined;
  if (durationMs > 0)
    noticeTimer = setTimeout(() => {
      if (noticeID === id) store.set(notice, '');
    }, durationMs);
}
export function confirmSheet(spec: Omit<Confirmation, 'done'>): Promise<boolean> {
  if (store.get(confirmation)) return Promise.resolve(false);
  return new Promise((done) => store.set(confirmation, { ...spec, done }));
}
export function showProgress(title: TextSpec, body: TextSpec) {
  store.set(progress, { title, body });
  return {
    remove: () => store.set(progress, null),
    fail: (message: string) =>
      store.set(progress, {
        title: msg('apply.restart_failed', '重启未完成'),
        body: message,
        failed: true,
      }),
  };
}
export function Modal({
  title,
  children,
  onClose,
  busy = false,
  hideTitle = false,
}: {
  title: TextSpec;
  children: ReactNode;
  onClose: () => void;
  busy?: boolean;
  hideTitle?: boolean;
}) {
  const panel = useRef<HTMLDivElement>(null);
  const titleID = useId();
  useEffect(() => {
    const previous = document.activeElement;
    const releaseModalInert = acquireModalInert();
    const first = panel.current?.querySelector<HTMLElement>(
      'input:not(:disabled),button:not(:disabled),textarea:not(:disabled),select:not(:disabled),[tabindex="0"]',
    );
    (first || panel.current?.querySelector<HTMLElement>('h2') || panel.current)?.focus();
    return () => {
      releaseModalInert();
      if (previous instanceof HTMLElement && previous.isConnected) previous.focus();
    };
  }, []);
  return createPortal(
    <div
      className="ds-sheet"
      role="dialog"
      aria-modal="true"
      aria-labelledby={titleID}
      onKeyDown={(event) => {
        if (event.key === 'Escape' && !busy) {
          event.stopPropagation();
          onClose();
        }
        if (event.key !== 'Tab') return;
        const controls = [
          ...(panel.current?.querySelectorAll<HTMLElement>(
            'input:not(:disabled),button:not(:disabled),textarea:not(:disabled),select:not(:disabled),[tabindex="0"]',
          ) || []),
        ];
        const first = controls[0],
          last = controls.at(-1);
        if (!controls.length) {
          event.preventDefault();
          panel.current?.focus();
        } else if (event.shiftKey && document.activeElement === first) {
          event.preventDefault();
          last?.focus();
        } else if (!event.shiftKey && document.activeElement === last) {
          event.preventDefault();
          first?.focus();
        }
      }}
    >
      <div className="ds-sheet__scrim ds-sheet__scrim--open" onClick={() => !busy && onClose()} />
      <div ref={panel} tabIndex={-1} className="ds-sheet__panel ds-sheet__panel--open">
        <div className="ds-sheet__grabber">
          <div className="ds-sheet__handle" />
        </div>
        <h2 id={titleID} tabIndex={-1} className={hideTitle ? 'ds-sr-only' : 'ds-sheet__title'}>
          <Text value={title} />
        </h2>
        <div className="ds-sheet__body">{children}</div>
      </div>
    </div>,
    document.body,
  );
}
export function OverlayHost() {
  useEffect(
    () => () => {
      const pending = store.get(confirmation);
      store.set(confirmation, null);
      pending?.done(false);
      store.set(progress, null);
      store.set(notice, '');
    },
    [],
  );

  const ask = useAtomValue(confirmation, { store });
  const text = useAtomValue(notice, { store });
  const wait = useAtomValue(progress, { store });
  const finish = (value: boolean) => {
    store.set(confirmation, null);
    ask?.done(value);
  };
  return (
    <>
      {ask && (
        <Modal title={ask.title} onClose={() => finish(false)}>
          <p>
            <Text value={ask.body} />
          </p>
          <div className="ds-button-pair">
            <Button variant="secondary" onClick={() => finish(false)}>
              <Text value={msg('action.cancel', '取消')} />
            </Button>
            <Button
              action={ask.action}
              variant={ask.danger ? 'danger' : 'primary'}
              onClick={() => finish(true)}
            >
              <Text value={ask.confirmLabel || msg('action.confirm', '确认')} />
            </Button>
          </div>
        </Modal>
      )}
      {text &&
        createPortal(
          <div className="ds-toast" role="status">
            {text}
          </div>,
          document.body,
        )}
      {wait && (
        <Modal
          title={wait.title}
          onClose={() => {
            if (wait.failed) store.set(progress, null);
          }}
          busy={!wait.failed}
        >
          <p>
            <Text value={wait.body} />
          </p>
          {wait.failed && (
            <Button onClick={() => store.set(progress, null)}>
              <Text value={msg('action.close', '关闭')} />
            </Button>
          )}
        </Modal>
      )}
    </>
  );
}
