import { afterEach, describe, expect, it, vi } from 'vitest';
import { createHash, randomBytes } from 'node:crypto';
import { Blob as NodeBlob } from 'node:buffer';
import { sha256Hex } from '../lib/checksum';
import { createMaintenanceRequestID } from '../lib/maintenance';
import { readArchiveHeader, uploadBrowserChunks } from '../lib/transfers';
import { createBackupEngine } from '../lib/backup';
import { request } from '../api/client';
import { createRouter } from '../app/router';
import {
  currentNetworks,
  otherNetworks,
  parseSecuredSsids,
  proxyChangeRequest,
  wifiSignalLevel,
} from '../api/wifi';
import {
  processLogChunk,
  buildMessageDiffOps,
  buildLineDiffRows,
  imageUrlFromPart,
  isValidLogFileName,
} from '../lib/logs';
import type { LogParser } from '../lib/logs';
afterEach(() => vi.unstubAllGlobals());
it('produces HTTP-safe UUIDs and SHA-256 hashes at padding and chunk boundaries', async () => {
  for (let index = 0; index < 20; index++)
    expect(createMaintenanceRequestID({})).toMatch(
      /^[\da-f]{8}-[\da-f]{4}-4[\da-f]{3}-[89ab][\da-f]{3}-[\da-f]{12}$/,
    );
  for (const size of [0, 1, 3, 55, 56, 63, 64, 65, 127, 128, 4096, 4 * 1024 * 1024]) {
    const bytes = randomBytes(size);
    expect(await sha256Hex(bytes, undefined)).toBe(
      createHash('sha256').update(bytes).digest('hex'),
    );
  }
});
it('bounds archive headers and uploads only one authenticated chunk at a time', async () => {
  const header = { protection: { algorithm: 'sha256-chunked' }, created_at: '2026-11-24' };
  const bytes = new TextEncoder().encode(JSON.stringify(header));
  const prefix = new Uint8Array(14);
  prefix.set(new TextEncoder().encode('AICOBKP1'));
  const view = new DataView(prefix.buffer);
  view.setUint16(8, 1);
  view.setUint32(10, bytes.length);
  const file = new NodeBlob([prefix, bytes]) as unknown as Blob;
  expect(await readArchiveHeader(file)).toEqual(header);
  view.setUint32(10, 65537);
  await expect(readArchiveHeader(new NodeBlob([prefix, bytes]) as unknown as Blob)).rejects.toThrow(
    'manifest_invalid',
  );
  const sequence: string[] = [];
  let uploaded = 0;
  await uploadBrowserChunks(
    new NodeBlob(['abcdefg']) as unknown as Blob,
    3,
    async (index, chunk, hash) => {
      sequence.push(`send${index}`);
      expect(hash).toBe(createHash('sha256').update(chunk).digest('hex'));
      return index;
    },
    async (index, offset) => {
      sequence.push(`ack${index}`);
      uploaded = offset;
    },
  );
  expect(sequence).toEqual(['send0', 'ack0', 'send1', 'ack1', 'send2', 'ack2']);
  expect(uploaded).toBe(7);
});
it('serializes config, locale and TOML writes while reads continue after a failure', async () => {
  let release!: () => void;
  const started: string[] = [];
  const fetcher = vi.fn(async (path: string, options?: RequestInit) => {
    started.push(path);
    if (options?.body === 'first')
      await new Promise<void>((resolve) => {
        release = resolve;
      });
    return new Response(JSON.stringify({ persisted: true, state: 'pending' }), {
      status: options?.body === 'first' ? 503 : 200,
    });
  });
  vi.stubGlobal('fetch', fetcher);
  const first = request('/api/config', { method: 'PATCH', body: 'first' });
  const rejected = expect(first).rejects.toMatchObject({ status: 503, persisted: true });
  const second = request('/api/config/locale', { method: 'PUT' });
  const third = request('/api/config/backup', { method: 'PUT' });
  await request('/api/config/application');
  expect(started).toEqual(['/api/config/application', '/api/config']);
  release();
  await rejected;
  await Promise.all([second, third]);
  expect(started.slice(-2)).toEqual(['/api/config/locale', '/api/config/backup']);
  vi.stubGlobal('fetch', async () => new Response('null', { status: 200 }));
  expect(await request('/api/test')).toEqual({});
});
it('decodes route metadata and leaves download, external, and standalone page links alone', async () => {
  window.history.replaceState({}, '', '/');
  const render = vi.fn();
  const router = createRouter({
    routes: [
      { path: '/', name: 'home' },
      { path: '/wifi/:ssid', name: 'network' },
    ],
    render,
  });
  await router.start();
  await router.navigate('/wifi/Aico%20IoT?focus=password');
  expect(router.current()).toMatchObject({
    name: 'network',
    params: { ssid: 'Aico IoT' },
    query: { focus: 'password' },
  });
  await router.navigate('/wifi/%E0');
  expect(router.current()?.name).toBe('home');
  for (const [href, download, target] of [
    ['/api/backup/jobs/a/archive', 'backup', ''],
    ['/webtty/', '', '_blank'],
    ['/llm-logs', '', ''],
  ]) {
    const anchor = document.createElement('a');
    anchor.href = href;
    if (download) anchor.download = download;
    if (target) anchor.target = target;
    document.body.append(anchor);
    const event = new MouseEvent('click', { bubbles: true, cancelable: true });
    document.body.addEventListener(
      'click',
      (e) => {
        expect(e.defaultPrevented).toBe(false);
        e.preventDefault();
      },
      { once: true },
    );
    anchor.dispatchEvent(event);
    anchor.remove();
  }
  router.stop();
});
describe('Wi-Fi protocol', () => {
  it('requires a proxy URL, parses ordered BSS security and keeps stable network ordering', () => {
    expect(proxyChangeRequest({ enabled: true, url: '  ' })).toMatchObject({ ok: false });
    expect(
      proxyChangeRequest({ enabled: true, url: ' ', savedUrl: 'http://stored' }),
    ).toMatchObject({ ok: true, proxyUrl: 'http://stored' });
    expect(proxyChangeRequest({ enabled: false })).toMatchObject({ ok: true, proxyMode: 'system' });
    expect([
      ...parseSecuredSsids('BSS a\ncapability: ESS Privacy\nSSID: Locked\nBSS b\nSSID: Open'),
    ]).toEqual(['Locked']);
    expect(wifiSignalLevel({ signalPercent: 50 })).toBe(2);
    expect(wifiSignalLevel({ signalDbm: -80 })).toBe(1);
    expect(wifiSignalLevel({})).toBe(null);
    expect(
      currentNetworks(
        [
          { ssid: 'A', priority: 3 },
          { ssid: 'B', priority: 1 },
        ],
        'B',
      ).map((value) => value.ssid),
    ).toEqual(['B', 'A']);
    expect(
      otherNetworks(
        [
          { ssid: 'A', secured: true },
          { ssid: 'C', secured: false },
          { ssid: 'C', secured: false },
        ],
        ['A'],
        'B',
      ),
    ).toEqual([{ ssid: 'C', secured: false }]);
  });
});
it('groups split NDJSON records and preserves truncated diagnostics without rendering unsafe images', () => {
  const parser: LogParser = { groups: [], pendingGroup: null, buffer: '', invalidLines: 0 };
  processLogChunk(parser, '{"kind":"request","body":"{}"}\n{"kind":"res', false);
  processLogChunk(parser, 'ponse","body":"answer"}\nbad\n', true);
  expect(parser.groups).toHaveLength(1);
  expect(parser.groups[0].response?.body).toBe('answer');
  expect(parser.invalidLines).toBe(1);
  for (const url of [
    'javascript:alert(1)',
    'file:///etc/passwd',
    'data:text/html;base64,SGk=',
    'data:image/svg+xml;base64,PHN2Zz4=',
  ])
    expect(imageUrlFromPart({ image_url: url })).toBe('');
  expect(isValidLogFileName('../llm-http-a.log')).toBe(false);
  const ops = buildMessageDiffOps(
    [{ role: 'user', content: 'a' }],
    [
      { role: 'user', content: 'b' },
      { role: 'assistant', content: 'answer' },
    ],
  );
  expect(ops.map((value) => value.kind)).toEqual(['modified', 'added']);
  expect(buildLineDiffRows('a\nb', 'a\nc').map((value) => value.kind)).toEqual([
    'same',
    'del',
    'add',
  ]);
});

it('renews expired maintenance sessions using the same idempotency key', async () => {
  let sessions = 0,
    attempts = 0;
  const headers: Headers[] = [];
  vi.stubGlobal('fetch', async (path: string, options?: RequestInit) => {
    if (path === '/api/maintenance/sessions') {
      sessions++;
      return new Response(
        JSON.stringify({ expires_at: '2099-01-01', csrf_token: `csrf-${sessions}` }),
      );
    }
    headers.push(new Headers(options?.headers));
    attempts++;
    return new Response(
      JSON.stringify(attempts === 1 ? { error: 'maintenance_session_required' } : { ok: true }),
      { status: attempts === 1 ? 401 : 200 },
    );
  });
  await createBackupEngine(() => {}).capabilities();
  expect(sessions).toBe(2);
  expect(headers[0].get('X-Aico-Request-ID')).toBe(headers[1].get('X-Aico-Request-ID'));
  expect(headers.map((value) => value.get('X-Aico-CSRF-Token'))).toEqual(['csrf-1', 'csrf-2']);
});
