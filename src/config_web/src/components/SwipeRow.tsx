import { useEffect, useRef, useState } from 'react';
import type { PointerEvent, ReactNode } from 'react';
import type { TextSpec } from '../types';
import { msg } from '../lib/i18n';
import { Text } from './ui';

let closeOpenRow: (() => void) | null = null;
const width = 80;

/** Preserve the original swipe action without rebuilding a polled row. */
export function SwipeRow({
  children,
  onDelete,
  lockedLabel,
  onLocked,
  action,
}: {
  children: ReactNode;
  onDelete: () => void;
  lockedLabel?: TextSpec;
  onLocked?: () => void;
  action?: string;
}) {
  const [offset, setOffset] = useState(0);
  const [dragging, setDragging] = useState(false);
  const position = useRef(0);
  const suppress = useRef(false);
  const drag = useRef<{ x: number; y: number; start: number; id: number; active: boolean } | null>(
    null,
  );
  const close = useRef(() => {
    position.current = 0;
    setOffset(0);
  });
  useEffect(
    () => () => {
      if (closeOpenRow === close.current) closeOpenRow = null;
    },
    [],
  );
  function place(next: number) {
    position.current = next;
    setOffset(next);
  }
  function end(event: PointerEvent<HTMLDivElement>) {
    const current = drag.current;
    if (!current || current.id !== event.pointerId) return;
    drag.current = null;
    setDragging(false);
    if (!current.active) return;
    suppress.current = true;
    setTimeout(() => {
      suppress.current = false;
    }, 0);
    if (position.current < -width / 2) {
      if (closeOpenRow !== close.current) closeOpenRow?.();
      closeOpenRow = close.current;
      place(-width);
    } else close.current();
  }
  return (
    <div className="ds-swipe">
      <button
        type="button"
        onFocus={() => {
          if (closeOpenRow !== close.current) closeOpenRow?.();
          closeOpenRow = close.current;
          place(-width);
        }}
        className={`ds-swipe__action ${lockedLabel ? 'ds-swipe__action--locked' : ''} ${dragging ? '' : 'ds-swipe__action--settling'}`}
        style={{ width: -offset }}
        data-action={action}
        onClick={() => {
          close.current();
          if (lockedLabel) onLocked?.();
          else onDelete();
        }}
      >
        <Text value={lockedLabel ?? msg('action.delete', '删除')} />
      </button>
      <div
        className={`ds-swipe__content ${dragging ? 'ds-swipe__content--dragging' : 'ds-swipe__content--settling'}`}
        style={{ transform: offset ? `translateX(${offset}px)` : undefined }}
        onPointerDown={(event) => {
          if (event.button !== 0) return;
          drag.current = {
            x: event.clientX,
            y: event.clientY,
            start: position.current,
            id: event.pointerId,
            active: false,
          };
        }}
        onPointerMove={(event) => {
          const current = drag.current;
          if (!current || current.id !== event.pointerId) return;
          const dx = event.clientX - current.x,
            dy = event.clientY - current.y;
          if (!current.active) {
            if (Math.abs(dy) > 8 && Math.abs(dy) > Math.abs(dx)) {
              drag.current = null;
              return;
            }
            if (Math.abs(dx) < 8) return;
            current.active = true;
            setDragging(true);
            event.currentTarget.setPointerCapture(event.pointerId);
          }
          place(Math.max(-width, Math.min(0, current.start + dx)));
        }}
        onPointerUp={end}
        onPointerCancel={end}
        onClickCapture={(event) => {
          if (event.detail === 0) return;
          if (!suppress.current && position.current === 0) return;
          event.stopPropagation();
          event.preventDefault();
          if (suppress.current) suppress.current = false;
          else close.current();
        }}
      >
        {children}
      </div>
    </div>
  );
}
