import { msg, resolve } from '../lib/i18n';

export function Loading({ block = false }: { block?: boolean }) {
  return (
    <span
      className={`ds-loading ${block ? 'ds-loading--block' : ''}`}
      role="status"
      aria-label={resolve(msg('status.loading', '加载中'))}
      aria-busy="true"
    >
      <span
        className={`ds-loading__bar ${block ? 'ds-loading__bar--long' : ''}`}
        aria-hidden="true"
      />
      {block && (
        <>
          <span className="ds-loading__bar ds-loading__bar--medium" aria-hidden="true" />
          <span className="ds-loading__bar ds-loading__bar--short" aria-hidden="true" />
        </>
      )}
    </span>
  );
}
