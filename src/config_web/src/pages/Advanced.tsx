import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import { useAtomValue } from 'jotai';
import type { ApplicationData, PageProps } from '../types';
import { apiError } from '../api/client';
import { useSettings, section, usePageHeader } from '../hooks/settings';
import { useQuery } from '../hooks/query';
import { isEmbedded } from '../lib/environment';
import { postToHost } from '../lib/host';
import { msg, resolve } from '../lib/i18n';
import { Button, Group, Row, Text } from '../components/ui';
import { ConfigField, QueryError } from '../components/fields';
import { toast } from '../components/overlays';
import { TomlEditor } from '../components/TomlEditor';
import { downloadBlob } from '../lib/download';
export const LOG_LEVELS = ['debug', 'info', 'warn', 'error'];
export function terminalUrl(location: Pick<Location, 'href'> = window.location) {
  const url = new URL(location.href);
  if (url.hostname === '192.168.42.1') {
    url.protocol = 'http:';
    url.port = '3000';
  } else url.port = url.protocol === 'https:' ? '8443' : '8080';
  url.pathname = '/webtty/';
  url.search = '';
  url.hash = '';
  return url.href;
}
export function LogsPage(props: PageProps) {
  const saver = useSettings(
    props,
    msg('advanced.logs', '日志'),
    props.model.local ? '/' : '/advanced',
  );
  const log = section(saver.config, 'log');
  const [busy, setBusy] = useState(false);
  async function exportLogs() {
    if (isEmbedded()) {
      postToHost({
        type: 'aico_open_url',
        url: new URL('/api/logs/support', window.location.href).href,
      });
      return;
    }
    setBusy(true);
    try {
      const response = await fetch('/api/logs/support', { cache: 'no-store' });
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      downloadBlob(
        await response.blob(),
        response.headers.get('Content-Disposition')?.match(/filename="([^"]+)"/i)?.[1] ||
          'aico-logs.tar.gz',
      );
    } catch (error) {
      toast(apiError(error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="ds-screen">
      <Group>
        <Row
          label={msg('advanced.agent_log', '实时 Agent 日志')}
          chevron
          action="open-agent-log"
          onPress={() => props.navigate('/advanced/logs/agent')}
        />
        <Row
          label={msg('advanced.llm_logs', '模型请求日志')}
          chevron
          onPress={() => window.location.assign('/llm-logs')}
        />
      </Group>
      <Group
        note={msg(
          'advanced.raw_http_help',
          '将模型原始 HTTP 请求和响应写入 Agent 日志目录，仅建议排查问题时启用。',
        )}
      >
        <ConfigField
          section="model"
          field={{ key: 'log_raw_http', widget: 'boolean' }}
          value={saver.config.model?.log_raw_http}
          save={(value) => saver.change('model.log_raw_http', { model: { log_raw_http: value } })}
        />
      </Group>
      <Group>
        <Row
          label={msg('advanced.log_level', '日志等级')}
          value={String(log.level || 'info')}
          chevron
          onPress={() => props.navigate('/advanced/logs/level')}
        />
        <ConfigField
          section="log"
          field={{
            key: 'llm_http_retention_days',
            widget: 'select',
            enum: [...new Set([Number(log.llm_http_retention_days || 7), 1, 3, 7, 14, 30])].map(
              (value) => ({ value: String(value), label: String(value) }),
            ),
          }}
          value={log.llm_http_retention_days}
          save={(value) =>
            saver.change('log.llm_http_retention_days', {
              log: { llm_http_retention_days: Number(value) },
            })
          }
        />
      </Group>
      {(!props.model.local || props.model.container) && (
        <Button disabled={busy} action="export-logs" onClick={() => void exportLogs()}>
          <Text value={msg('advanced.export_logs', '导出日志')} />
        </Button>
      )}
    </div>
  );
}
export function LogLevelPage(props: PageProps) {
  const saver = useSettings(props, msg('advanced.log_level', '日志等级'), '/advanced/logs');
  return (
    <Group>
      {LOG_LEVELS.map((level) => (
        <Row
          key={level}
          label={msg(`advanced.level_${level}`, level)}
          value={section(saver.config, 'log').level === level ? '✓' : ''}
          onPress={async () => {
            if ((await saver.change('log.level', { log: { level } })) === 'saved')
              props.navigate('/advanced/logs');
          }}
        />
      ))}
    </Group>
  );
}
export function agentLogLines(log: string) {
  let severity = 'info';
  return log.split(/\r?\n/).map((line) => {
    const record = line.match(/^(\S+Z) \[(DEBUG|INFO|WARN|ERROR)\]((?:\[[^\]]*\])*)(.*)$/);
    if (record) severity = record[2].toLowerCase();
    else if (/\bpanic\b|^goroutine \d+/.test(line)) severity = 'error';
    return { line, severity };
  });
}
export function AgentLogsPage(props: PageProps) {
  usePageHeader(props, msg('advanced.agent_log', '实时 Agent 日志'));
  const status = useAtomValue(props.model.resources.device.atom);
  const pre = useRef<HTMLPreElement>(null);
  const [shown, setShown] = useState('');
  const stick = useRef(true);
  const query = useQuery(
    (signal) =>
      props.model.send<{ agent_log?: { log?: string; error?: string } }>('/api/logs/agent', {
        signal,
      }),
    [props.model],
    2000,
  );
  const log = query.data?.agent_log?.log?.trim() || query.data?.agent_log?.error || '';
  useEffect(() => {
    const selection = window.getSelection();
    if (selection && !selection.isCollapsed && pre.current?.contains(selection.anchorNode)) return;
    const node = pre.current;
    stick.current =
      !shown || (!!node && node.scrollTop + node.clientHeight >= node.scrollHeight - 16);
    setShown(log);
  }, [log, shown]);
  useLayoutEffect(() => {
    if (stick.current && pre.current) pre.current.scrollTop = pre.current.scrollHeight;
  }, [shown]);
  return (
    <div className="ds-screen ds-screen--sticky">
      <Group>
        <Row
          label={msg('advanced.agent_status', 'Agent 状态')}
          value={
            status.data?.agent_status?.process_running
              ? msg('advanced.agent_running', '运行中')
              : msg('advanced.agent_stopped', '未运行')
          }
        />
      </Group>
      <QueryError {...query} />
      <div className="ds-group">
        <pre ref={pre} className="ds-log" data-action="agent-log">
          {shown ? (
            agentLogLines(shown).map(({ line, severity }, index) => (
              <span key={index} className={`ds-log__line ds-log__line--${severity}`}>
                {line}
              </span>
            ))
          ) : (
            <Text value={msg('advanced.agent_log_empty', '暂无日志')} />
          )}
        </pre>
      </div>
    </div>
  );
}
export function RawConfigPage(props: PageProps) {
  const query = useQuery(async (signal) => {
    const response = await fetch('/api/config/backup', { signal, cache: 'no-store' });
    if (!response.ok) throw new Error(`HTTP ${response.status}`);
    return response.text();
  }, []);
  const [draft, setDraft] = useState<string | null>(null);
  const [saved, setSaved] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const baseline = saved ?? query.data;
  const text = draft ?? baseline ?? '';
  const dirty = baseline !== null && baseline !== undefined && text !== baseline;
  async function save() {
    if (!dirty || busy) return;
    setBusy(true);
    const submitted = text;
    try {
      const result = await props.model.send<ApplicationData>('/api/config/backup', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/toml; charset=utf-8' },
        body: submitted,
      });
      props.model.publishSave(result);
      setSaved(submitted);
      setDraft((value) => (value === submitted ? null : value));
      toast(msg('basic.saved', '已保存'));
    } catch (caught) {
      const error = apiError(caught);
      if (error.persisted) {
        setSaved(submitted);
        setDraft((value) => (value === submitted ? null : value));
        props.model.publishSave(error);
        toast(msg('storage.backup_imported_not_applied', '配置备份已保存，但 Agent 未能应用。'));
      } else toast(error.message);
    } finally {
      setBusy(false);
    }
  }
  usePageHeader(props, msg('advanced.manual_config', '手动编辑配置'), dirty && !busy, save);
  return (
    <div className="ds-screen">
      <QueryError {...query} />
      <div className="ds-field">
        <Text value={msg('advanced.manual_config', '手动编辑配置')} />
        <TomlEditor
          value={text}
          label={resolve(msg('advanced.manual_config', '手动编辑配置'))}
          disabled={baseline === null || baseline === undefined || busy}
          onChange={setDraft}
        />
      </div>
      <Button disabled={!dirty || busy} onClick={() => void save()}>
        <Text value={msg('action.save', '保存')} />
      </Button>
    </div>
  );
}
