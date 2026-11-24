import { useState } from 'react';
import {
  array,
  messageText,
  imageUrlFromPart,
  imageLabelFromUrl,
  contentPartText,
  buildLineDiffRows,
  messageDiffText,
} from '../lib/logs';
import type { LogMessage } from '../lib/logs';
export function LogMessageView({
  message,
  index,
  kind = '',
  previous,
}: {
  message: LogMessage;
  index: number;
  kind?: string;
  previous?: LogMessage;
}) {
  const [collapsed, setCollapsed] = useState(false);
  const text = messageText(message);
  const long = text.split('\n').length > 8 || text.length > 3000;
  return (
    <div className={`msg diff-${kind}`}>
      <div className="msg-head">
        <span>
          {message.role} #{index + 1} {kind}
        </span>
        {long && (
          <button className="msg-collapse" onClick={() => setCollapsed((value) => !value)}>
            {collapsed ? 'Expand' : 'Collapse'}
          </button>
        )}
      </div>
      <div className={`msg-content ${collapsed ? 'collapsed' : ''}`}>
        {previous ? (
          <div className="line-diff">
            {buildLineDiffRows(messageDiffText(previous), messageDiffText(message)).map(
              (row, index) => (
                <div key={index} className={`line-diff-row ${row.kind}`}>
                  <span className="line-diff-mark">
                    {row.kind === 'add' ? '+' : row.kind === 'del' ? '-' : ''}
                  </span>
                  <span>{row.text}</span>
                </div>
              ),
            )}
          </div>
        ) : (
          <>
            {message.reasoning_content && (
              <details open className="msg-reasoning">
                <summary>Reasoning</summary>
                <pre>{message.reasoning_content}</pre>
              </details>
            )}
            {typeof message.content === 'string' ? (
              <pre>{message.content}</pre>
            ) : (
              array(message.content).map((part, index) => {
                const url = imageUrlFromPart(part);
                return url ? (
                  <figure key={index}>
                    <img
                      src={url}
                      alt="Message attachment"
                      loading="lazy"
                      referrerPolicy="no-referrer"
                    />
                    <figcaption>{imageLabelFromUrl(url)}</figcaption>
                  </figure>
                ) : (
                  <pre key={index}>{contentPartText(part)}</pre>
                );
              })
            )}
            {message.tool_calls?.map((call, index) => (
              <div key={call.id || index} className="tool-call">
                <strong>
                  {call.function.name} {call.id}
                </strong>
                <pre>{call.function.arguments}</pre>
              </div>
            ))}
          </>
        )}
      </div>
    </div>
  );
}
