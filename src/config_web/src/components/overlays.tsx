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
interface Progress {
  title: TextSpec;
  body: TextSpec;
  failed?: boolean;
}
const progress = atom<Progress | null>(null);
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
function Dialog({
  children,
  onClose,
  busy = false,
  titleID,
  descriptionID,
  className,
  panelClassName,
  scrimClassName,
  initialFocus,
}: {
  children: ReactNode;
  onClose: () => void;
  busy?: boolean;
  titleID: string;
  descriptionID?: string;
  className: string;
  panelClassName: string;
  scrimClassName?: string;
  initialFocus?: string;
}) {
  const panel = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const previous = document.activeElement;
    const releaseModalInert = acquireModalInert();
    const first = panel.current?.querySelector<HTMLElement>(
      initialFocus ||
        'input:not(:disabled),button:not(:disabled),textarea:not(:disabled),select:not(:disabled),[tabindex="0"]',
    );
    (first || panel.current?.querySelector<HTMLElement>('h2') || panel.current)?.focus();
    return () => {
      releaseModalInert();
      if (previous instanceof HTMLElement && previous.isConnected) previous.focus();
    };
  }, [initialFocus]);
  return createPortal(
    <div
      className={className}
      role="dialog"
      aria-modal="true"
      aria-labelledby={titleID}
      aria-describedby={descriptionID}
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
      {scrimClassName && <div className={scrimClassName} onClick={() => !busy && onClose()} />}
      <div ref={panel} tabIndex={-1} className={panelClassName}>
        {children}
      </div>
    </div>,
    document.body,
  );
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
  const titleID = useId();
  return (
    <Dialog
      titleID={titleID}
      onClose={onClose}
      busy={busy}
      className="ds-sheet"
      scrimClassName="ds-sheet__scrim ds-sheet__scrim--open"
      panelClassName="ds-sheet__panel ds-sheet__panel--open"
    >
      <div className="ds-sheet__grabber">
        <div className="ds-sheet__handle" />
      </div>
      <h2 id={titleID} tabIndex={-1} className={hideTitle ? 'ds-sr-only' : 'ds-sheet__title'}>
        <Text value={title} />
      </h2>
      <div className="ds-sheet__body">{children}</div>
    </Dialog>
  );
}
function ConfirmationDialog({
  ask,
  finish,
}: {
  ask: Confirmation;
  finish: (value: boolean) => void;
}) {
  const titleID = useId();
  const bodyID = useId();
  return (
    <Dialog
      titleID={titleID}
      descriptionID={ask.body ? bodyID : undefined}
      onClose={() => finish(false)}
      className="ds-action-sheet"
      scrimClassName="ds-action-sheet__scrim ds-action-sheet__scrim--open"
      panelClassName="ds-action-sheet__panel ds-action-sheet__panel--open"
      initialFocus='[data-action="cancel-confirm"]'
    >
      <div className="ds-action-sheet__group">
        <div className="ds-action-sheet__header">
          <h2 id={titleID} tabIndex={-1} className="ds-action-sheet__title">
            <Text value={ask.title} />
          </h2>
          {ask.body && (
            <p id={bodyID} className="ds-action-sheet__message">
              <Text value={ask.body} />
            </p>
          )}
        </div>
        <button
          type="button"
          data-action={ask.action || 'confirm'}
          className={`ds-action-sheet__button ${ask.danger ? 'ds-action-sheet__button--danger' : ''}`}
          onClick={() => finish(true)}
        >
          <Text value={ask.confirmLabel || msg('action.confirm', '确认')} />
        </button>
      </div>
      <div className="ds-action-sheet__group ds-action-sheet__group--cancel">
        <button
          type="button"
          data-action="cancel-confirm"
          className="ds-action-sheet__button ds-action-sheet__button--cancel"
          onClick={() => finish(false)}
        >
          <Text value={msg('action.cancel', '取消')} />
        </button>
      </div>
    </Dialog>
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
      {ask && <ConfirmationDialog ask={ask} finish={finish} />}
      {text &&
        createPortal(
          <div className="ds-toast" role="status">
            {text}
          </div>,
          document.body,
        )}
      {wait && <ProgressDialog wait={wait} />}
    </>
  );
}
function ProgressDialog({ wait }: { wait: Progress }) {
  const titleID = useId();
  const bodyID = useId();
  return (
    <Dialog
      titleID={titleID}
      descriptionID={bodyID}
      className="ds-restart"
      panelClassName="ds-restart__panel"
      onClose={() => {
        if (wait.failed) store.set(progress, null);
      }}
      busy={!wait.failed}
    >
      {!wait.failed && <div className="ds-restart__spinner" aria-hidden="true" />}
      <h2 id={titleID} tabIndex={-1} className="ds-restart__title">
        <Text value={wait.title} />
      </h2>
      <p id={bodyID} className="ds-restart__body" role="status">
        <Text value={wait.body} />
      </p>
      {wait.failed && (
        <Button
          variant="secondary"
          block
          action="close-restart"
          onClick={() => store.set(progress, null)}
        >
          <Text value={msg('action.close', '关闭')} />
        </Button>
      )}
    </Dialog>
  );
}
