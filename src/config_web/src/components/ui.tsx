import type { ButtonHTMLAttributes, ReactNode, KeyboardEvent } from 'react';
import type { DeviceState } from '../state/device';
import type { TextSpec } from '../types';
import { Children, Component, Fragment, isValidElement } from 'react';
import { useAtomValue } from 'jotai';
import { glyphs } from '../lib/glyphs';
import { resolve } from '../lib/i18n';

export function Text({ value, className }: { value?: TextSpec | null; className?: string }) {
  return <span className={className}>{resolve(value)}</span>;
}

/** Only restore a removed list control; a portal dialog owns its own focus. */
export class FocusList extends Component<
  { children: ReactNode; className?: string },
  object,
  string | null
> {
  private host = { current: null as HTMLDivElement | null };
  getSnapshotBeforeUpdate() {
    const active = document.activeElement;
    return active instanceof HTMLElement && this.host.current?.contains(active)
      ? active.dataset.action || null
      : null;
  }
  componentDidUpdate(_props: unknown, _state: unknown, action: string | null) {
    if (!action || document.activeElement !== document.body) return;
    const equivalent = [
      ...(this.host.current?.querySelectorAll<HTMLElement>('[data-action]') || []),
    ].find((node) => node.dataset.action === action);
    equivalent?.focus();
  }
  render() {
    return (
      <div ref={this.host} className={this.props.className}>
        {this.props.children}
      </div>
    );
  }
}

/** React owns these nodes; polling updates props without replacing controls. */
export function Row({
  label,
  value,
  description,
  tone,
  onPress,
  action,
  children,
  chevron,
  leading,
  className = '',
}: {
  label?: TextSpec;
  value?: TextSpec | null;
  description?: TextSpec;
  tone?: string;
  onPress?: () => void;
  action?: string;
  children?: ReactNode;
  chevron?: boolean;
  leading?: ReactNode;
  className?: string;
}) {
  const press = onPress
    ? {
        role: 'button',
        tabIndex: 0,
        onClick: onPress,
        onKeyDown: (event: KeyboardEvent<HTMLDivElement>) => {
          if (event.target === event.currentTarget && ['Enter', ' '].includes(event.key)) {
            event.preventDefault();
            onPress();
          }
        },
      }
    : {};
  return (
    <div
      className={`ds-row ${onPress ? 'ds-row--tappable' : ''} ${className}`}
      data-action={action}
      {...press}
    >
      {leading}
      <div className="ds-row__body">
        <Text value={label} className="ds-row__label" />
        {description != null && <Text value={description} className="ds-row__description" />}
      </div>
      <div className="ds-row__accessory">
        {value != null && (
          <Text value={value} className={`ds-row__value ${tone ? `ds-row__value--${tone}` : ''}`} />
        )}
        {children}
        {chevron && <Icon name="chevron" className="ds-row__chevron" />}
      </div>
    </div>
  );
}

export function Group({
  caption,
  children,
  note,
  joined = false,
}: {
  caption?: TextSpec;
  children?: ReactNode;
  note?: TextSpec | null;
  joined?: boolean;
}) {
  return (
    <div>
      {caption && (
        <div className="ds-group__caption">
          <Text value={caption} />
        </div>
      )}
      <div className="ds-group">
        {Children.toArray(children).map((child, index) => (
          <Fragment key={isValidElement(child) ? (child.key ?? index) : index}>
            {index > 0 && !joined && <div className="ds-separator" />}
            {child}
          </Fragment>
        ))}
        {note && <Text value={note} className="ds-group__note" />}
      </div>
    </div>
  );
}

export function Button({
  children,
  variant = 'primary',
  block = false,
  action,
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: string;
  block?: boolean;
  action?: string;
}) {
  return (
    <button
      type="button"
      className={`ds-btn ds-btn--${variant} ${block ? 'ds-btn--block' : ''}`}
      data-action={action}
      {...props}
    >
      {children}
    </button>
  );
}

export function Switch({
  checked,
  label,
  onChange,
  disabled,
  action = 'toggle-switch',
}: {
  checked?: boolean;
  label: TextSpec;
  onChange: (value: boolean) => void;
  disabled?: boolean;
  action?: string;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={!!checked}
      aria-label={resolve(label)}
      disabled={disabled}
      data-action={action}
      className={`ds-switch ${checked ? 'ds-switch--on' : ''}`}
      onClick={() => onChange(!checked)}
    >
      <span className={`ds-switch__knob ${checked ? 'ds-switch__knob--on' : ''}`} />
    </button>
  );
}

export function Icon({
  name,
  size,
  className,
  signalLevel,
}: {
  name: keyof typeof glyphs;
  size?: number;
  className?: string;
  signalLevel?: number | null;
}) {
  const glyph = glyphs[name];
  if (!glyph) return null;
  const [, , width, height] = glyph.box.split(/\s+/).map(Number);
  return (
    <svg
      viewBox={glyph.box}
      width={size || width}
      height={size ? Math.round((size * height) / width) : height}
      className={className}
      fill="none"
      aria-hidden="true"
      focusable="false"
    >
      {(
        glyph.shapes as {
          tag: string;
          attrs: Record<string, string | number>;
          class?: string;
          children?: string[];
        }[]
      ).map((shape, index) => {
        const attrs: Record<string, string | number> = Object.fromEntries(
          Object.entries(shape.attrs).map(([key, value]) => [
            key.replace(/-([a-z])/g, (_, letter) => letter.toUpperCase()),
            value,
          ]),
        );
        if (
          name === 'wifi' &&
          Number.isInteger(signalLevel) &&
          ((signalLevel! < 3 && shape.class?.includes('outer')) ||
            (signalLevel! < 1 && shape.class?.includes('inner')))
        )
          attrs.stroke = 'var(--color-ink-tertiary)';
        const Shape = shape.tag as keyof import('react').JSX.IntrinsicElements;
        return (
          <Shape key={index} {...attrs} className={shape.class}>
            {shape.children?.map((d) => (
              <path key={d} d={d} />
            ))}
          </Shape>
        );
      })}
    </svg>
  );
}

export function ResourceError({
  model,
  name,
}: {
  model: DeviceState;
  name: keyof DeviceState['resources'];
}) {
  const state = useAtomValue(model.resources[name].atom);
  if (!state.stale || !state.error) return null;
  return (
    <p className="ds-field__error" role="status">
      {state.error}
    </p>
  );
}
