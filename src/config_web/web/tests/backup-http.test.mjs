import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { createHash, randomBytes } from 'node:crypto';

const source = (name) => readFile(new URL(`../assets/js/config/${name}`, import.meta.url), 'utf8');
const moduleURL = (code) => `data:text/javascript;base64,${Buffer.from(code).toString('base64')}`;
const { createMaintenanceRequestID } = await import(moduleURL(await source('backup-session.js')));
const { sha256Hex } = await import(moduleURL(await source('backup-checksum.js')));
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

test('HTTP request IDs are UUID v4 without randomUUID', () => {
  const httpCrypto = { getRandomValues: (bytes) => bytes.set(randomBytes(bytes.length)) };
  for (let index = 0; index < 100; index += 1)
    assert.match(createMaintenanceRequestID(httpCrypto), uuid);
  assert.match(createMaintenanceRequestID({}), uuid);
});

test('HTTP SHA-256 fallback matches native hashes at padding and chunk boundaries', async () => {
  for (const size of [0, 1, 3, 55, 56, 63, 64, 65, 127, 128, 4096, 4 * 1024 * 1024]) {
    const bytes = randomBytes(size);
    assert.equal(await sha256Hex(bytes, {}), createHash('sha256').update(bytes).digest('hex'));
  }
  assert.equal(
    await sha256Hex(new TextEncoder().encode('abc'), {}),
    'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad',
  );
});
