import { useEffect, useRef, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { request } from './api/client';
import { useQuery } from './hooks/query';
import { triggerBrowserDownload } from './lib/transfers';
import { formatBytes } from './lib/backup';
import {
  requestMessages,
  parseBody,
  messageText,
  extractResponseMessage,
  streamLogEntries,
  isValidLogFileName,
  buildMessageDiffOps,
  computeDiffStats,
  formatLogTimestamp,
} from './lib/logs';
import { LogMessageView } from './components/LogMessage';
import type { LogGroup } from './lib/logs';
interface LogFile {
  name: string;
  size_bytes?: number;
  modified_at?: string;
}
function Detail({ groups, index }: { groups: LogGroup[]; index: number }) {
  const [view, setView] = useState(index > 0 ? 'diff' : 'messages');
  const group = groups[index];
  if (!group) return <p className="empty">Select a request to view details.</p>;
  const current = requestMessages(parseBody(group.request.body));
  const previous = index > 0 ? requestMessages(parseBody(groups[index - 1].request.body)) : [];
  const stats = computeDiffStats(previous, current);
  const response = group.response ? extractResponseMessage(group.response.body) : null;
  return (
    <div className="group">
      <div className="group-head">
        <strong>Request #{index + 1}</strong>
        <span className="badge">{group.response?.status || 'No Response'}</span>
        <time title={group.request.ts}>{formatLogTimestamp(group.request.ts)}</time>
        {index > 0 && (
          <span className="diff-stats">
            +{stats.added} −{stats.removed} ~{stats.modified}
          </span>
        )}
        <div className="group-actions">
          {['messages', ...(index > 0 ? ['diff'] : []), 'raw'].map((value) => (
            <button
              key={value}
              className={`button ${view === value ? 'active' : ''}`}
              onClick={() => setView(value)}
            >
              {value[0].toUpperCase() + value.slice(1)}
            </button>
          ))}
        </div>
      </div>
      <div className="group-body">
        {view === 'raw' ? (
          <>
            <pre>{JSON.stringify(parseBody(group.request.body), null, 2)}</pre>
            <pre>{group.response?.body}</pre>
          </>
        ) : (
          <>
            {view === 'diff'
              ? buildMessageDiffOps(previous, current).map((op, index) => {
                  const message =
                    op.kind === 'removed' ? previous[op.oldIndex!] : current[op.newIndex!];
                  return (
                    <LogMessageView
                      key={index}
                      message={message}
                      index={op.newIndex ?? op.oldIndex ?? 0}
                      kind={op.kind}
                      previous={op.kind === 'modified' ? previous[op.oldIndex!] : undefined}
                    />
                  );
                })
              : current.map((message, index) => (
                  <LogMessageView key={index} message={message} index={index} />
                ))}
            {response ? (
              <LogMessageView message={response} index={current.length} />
            ) : (
              group.response && <pre>{group.response.body}</pre>
            )}
          </>
        )}
      </div>
    </div>
  );
}
function LogsApp() {
  const files = useQuery(
    (signal) => request<{ files: LogFile[] }>('/api/logs/llm', { signal }),
    [],
  );
  const [selected, setSelected] = useState(''),
    [groups, setGroups] = useState<LogGroup[]>([]),
    [index, setIndex] = useState(0),
    [status, setStatus] = useState(''),
    [busy, setBusy] = useState(false);
  const input = useRef<HTMLInputElement>(null);
  const name = selected || (files.data?.files[0]?.name ?? '');
  useEffect(() => {
    if (!name) return;
    const controller = new AbortController();
    setGroups([]);
    setIndex(0);
    setStatus('Loading…');
    streamLogEntries(
      name,
      (loaded, total, count) => {
        if (!controller.signal.aborted)
          setStatus(`${formatBytes(loaded)} / ${formatBytes(total)} · ${count} requests`);
      },
      controller.signal,
    ).then(
      (result) => {
        if (controller.signal.aborted) return;
        setGroups(result.groups);
        setStatus(`${result.groups.length} requests · ${result.invalidLines} invalid lines`);
      },
      (error) => {
        if (!controller.signal.aborted) setStatus(String(error));
      },
    );
    return () => controller.abort();
  }, [name, files.data]);
  async function importLog(file: File) {
    if (!isValidLogFileName(file.name)) {
      setStatus('File name must match llm-http-*.log');
      return;
    }
    if (
      files.data?.files.some((entry) => entry.name === file.name) &&
      !window.confirm(`Replace ${file.name}?`)
    )
      return;
    setBusy(true);
    try {
      await request('/api/logs/llm/' + encodeURIComponent(file.name), {
        method: 'PUT',
        headers: { 'Content-Type': 'text/plain; charset=utf-8' },
        body: file,
      });
      setSelected(file.name);
      files.retry();
    } catch (error) {
      setStatus(String(error));
    } finally {
      setBusy(false);
    }
  }
  return (
    <>
      <div className="topbar">
        <a href="/">← Back to Config</a>
        <h1>LLM HTTP Logs</h1>
      </div>
      <div className="layout">
        <aside className="sidebar">
          <div className="sidebar-head">
            <strong>Log Files</strong>
            <div className="sidebar-actions">
              <button className="button" onClick={files.retry}>
                Refresh
              </button>
              <button className="button" disabled={busy} onClick={() => input.current?.click()}>
                Import Raw
              </button>
            </div>
          </div>
          <input
            ref={input}
            type="file"
            hidden
            accept=".log,text/plain"
            onChange={(event) => {
              const file = event.target.files?.[0];
              event.target.value = '';
              if (file) void importLog(file);
            }}
          />
          <div className="action-status" role="status">
            {files.error || status}
          </div>
          <div id="fileList">
            {files.data?.files.map((file) => (
              <button
                key={file.name}
                className={`file-row ${name === file.name ? 'active' : ''}`}
                onClick={() => setSelected(file.name)}
              >
                <span className="file-name">{file.name}</span>
                <small className="file-meta">{formatBytes(file.size_bytes)}</small>
              </button>
            ))}
          </div>
        </aside>
        <div className="detail">
          <div className="request-list">
            <div className="request-list-head">Requests</div>
            <div id="requestList">
              {groups.map((group, position) => {
                const messages = requestMessages(parseBody(group.request.body));
                return (
                  <button
                    key={position}
                    className={`request-item ${index === position ? 'active' : ''}`}
                    onClick={() => setIndex(position)}
                  >
                    <div className="request-item-head">
                      #{position + 1}{' '}
                      <span className="badge">{group.response?.status || 'No Response'}</span>
                      <time title={group.request.ts}>{formatLogTimestamp(group.request.ts)}</time>
                    </div>
                    <div className="request-item-preview">
                      {messages.length ? messageText(messages.at(-1)!) : 'No message content'}
                    </div>
                  </button>
                );
              })}
            </div>
          </div>
          <div className="request-detail">
            <div className="request-detail-head">
              <span>Request Detail – {name}</span>
              <button
                className="button"
                disabled={!name || busy}
                onClick={() =>
                  triggerBrowserDownload('/api/logs/llm/' + encodeURIComponent(name), name)
                }
              >
                Export Raw
              </button>
            </div>
            <div id="requestDetail">
              <Detail key={`${name}:${index}`} groups={groups} index={index} />
            </div>
          </div>
        </div>
      </div>
    </>
  );
}
createRoot(document.getElementById('app')!).render(<LogsApp />);
