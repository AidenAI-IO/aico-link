import { afterEach, expect, it, vi } from 'vitest';
import { waitForReboot } from '../lib/apply';

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

it('detects a new boot ID from a non-2xx application status response', async () => {
  vi.useFakeTimers();
  vi.stubGlobal(
    'fetch',
    async () => new Response(JSON.stringify({ boot_id: 'boot-new' }), { status: 503 }),
  );
  const waiting = waitForReboot('boot-old');
  await vi.advanceTimersByTimeAsync(2000);
  await expect(waiting).resolves.toBeUndefined();
});
