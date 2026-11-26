import { Fragment, useLayoutEffect, useRef } from 'react';
interface Token {
  kind: string;
  text: string;
}
const patterns: [string, RegExp][] = [
  ['comment', /#.*/y],
  ['string', /"""[^]*?(?:"""|$)|'''[^]*?(?:'''|$)|"(?:[^"\\]|\\.)*"?|'[^']*'?/y],
  ['date', /\d{4}-\d{2}-\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:\d{2})?)?/y],
  [
    'number',
    /[+-]?(?:inf|nan|0x[\da-fA-F_]+|0o[0-7_]+|0b[01_]+|\d[\d_]*(?:\.[\d_]+)?(?:[eE][+-]?\d+)?)(?![\w.-])/y,
  ],
  ['boolean', /(?:true|false)(?![\w-])/y],
];
function values(text: string): Token[] {
  const tokens: Token[] = [];
  let index = 0;
  while (index < text.length) {
    let matched = false;
    for (const [kind, pattern] of patterns) {
      pattern.lastIndex = index;
      const found = pattern.exec(text);
      if (found?.[0]) {
        tokens.push({ kind, text: found[0] });
        index += found[0].length;
        matched = true;
        break;
      }
    }
    if (!matched) {
      const end = index + 1;
      const previous = tokens.at(-1);
      if (previous?.kind === '') previous.text += text.slice(index, end);
      else tokens.push({ kind: '', text: text.slice(index, end) });
      index = end;
    }
  }
  return tokens;
}
export function tomlTokens(line: string): Token[] {
  const table = /^(\s*)(\[\[?[^\]#]*\]\]?)(.*)$/.exec(line);
  if (table)
    return [{ kind: '', text: table[1] }, { kind: 'table', text: table[2] }, ...values(table[3])];
  const pair =
    /^(\s*)((?:"(?:[^"\\]|\\.)*"|'[^']*'|[A-Za-z0-9_.-]+)(?:\s*\.\s*(?:"(?:[^"\\]|\\.)*"|'[^']*'|[A-Za-z0-9_-]+))*)(\s*=)(.*)$/.exec(
      line,
    );
  return pair
    ? [
        { kind: '', text: pair[1] },
        { kind: 'key', text: pair[2] },
        { kind: 'punct', text: pair[3] },
        ...values(pair[4]),
      ]
    : values(line);
}
export function TomlEditor({
  value,
  label,
  disabled,
  onChange,
}: {
  value: string;
  label: string;
  disabled: boolean;
  onChange: (value: string) => void;
}) {
  const highlight = useRef<HTMLPreElement>(null),
    input = useRef<HTMLTextAreaElement>(null),
    caret = useRef<number | null>(null);
  useLayoutEffect(() => {
    if (caret.current !== null && input.current) {
      input.current.setSelectionRange(caret.current, caret.current);
      caret.current = null;
    }
  }, [value]);
  return (
    <div className="ds-toml-frame">
      <div className="ds-toml">
        <pre ref={highlight} className="ds-toml__highlight" aria-hidden="true">
          <code className="ds-toml__code">
            {value.split('\n').map((line, index) => (
              <Fragment key={index}>
                {index > 0 ? '\n' : ''}
                {tomlTokens(line).map((token, index) =>
                  token.kind ? (
                    <span key={index} className={`ds-toml__${token.kind}`}>
                      {token.text}
                    </span>
                  ) : (
                    token.text
                  ),
                )}
              </Fragment>
            ))}
            {'\n '}
          </code>
        </pre>
        <textarea
          ref={input}
          className="ds-toml__input"
          aria-label={label}
          data-action="edit-toml"
          spellCheck={false}
          autoCapitalize="none"
          autoComplete="off"
          wrap="off"
          value={value}
          disabled={disabled}
          onChange={(event) => onChange(event.target.value)}
          onScroll={(event) => {
            if (highlight.current) {
              highlight.current.scrollTop = event.currentTarget.scrollTop;
              highlight.current.scrollLeft = event.currentTarget.scrollLeft;
            }
          }}
          onKeyDown={(event) => {
            if (event.key !== 'Tab' || event.shiftKey || disabled) return;
            event.preventDefault();
            const node = event.currentTarget;
            node.setRangeText('  ', node.selectionStart, node.selectionEnd, 'end');
            caret.current = node.selectionStart;
            onChange(node.value);
          }}
        />
      </div>
    </div>
  );
}
