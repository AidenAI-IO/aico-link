import { readFile, readdir } from 'node:fs/promises';
import path from 'node:path';
import { expect, it } from 'vitest';
import { applyLocale, t } from '../lib/i18n';
import { messages } from '../lib/messages';
it('translates every literal settings message in both languages', async () => {
  async function sources(directory: string): Promise<string[]> {
    const entries = await readdir(directory, { withFileTypes: true });
    const nested = await Promise.all(
      entries
        .filter((entry) => entry.name !== 'tests' && entry.name !== 'messages.ts')
        .map(async (entry) => {
          const file = path.join(directory, entry.name);
          return entry.isDirectory()
            ? sources(file)
            : /\.tsx?$/.test(file)
              ? [await readFile(file, 'utf8')]
              : [];
        }),
    );
    return nested.flat();
  }
  const keys = new Set<string>();
  for (const source of await sources(path.resolve('src')))
    for (const match of source.matchAll(/\b(?:msg|t)\(\s*'([a-z0-9_]+(?:\.[a-z0-9_]+)+)'/g))
      keys.add(match[1]);
  for (const locale of ['en-US', 'zh-CN'])
    expect(
      [...keys].filter((key) => !messages[locale][key]),
      locale,
    ).toEqual([]);
  applyLocale('zh-CN', true);
  expect(t('memory.days', { n: 7 })).toContain('7');
  expect(document.documentElement.lang).toBe('zh-CN');
  applyLocale('en-US', true);
  expect(t('ui.retry')).toBe('Retry');
  expect(localStorage.getItem('aico.config.locale')).toBe('en-US');
});
