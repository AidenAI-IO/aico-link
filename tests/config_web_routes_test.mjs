import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import {fileURLToPath} from 'node:url';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

for (const retiredPath of [
  'src/config_web.cpp',
  'src/config_web_static_assets.cpp',
  'src/config_web_static_assets.h',
  'src/wifi_config.cpp',
  'src/wifi_config.h',
  'tests/agent_stub_main.cpp',
  'tests/config_web_e2e_test.cpp',
  'tests/config_web_source_test.cpp',
  'tests/config_web_test_assets.h',
  'tests/wifi_config_test.cpp',
]) {
  await assert.rejects(
    fs.access(path.join(repositoryRoot, retiredPath)),
    (error) => error?.code === 'ENOENT',
    `retired C++ Config Web file still exists: ${retiredPath}`,
  );
}

// Debian's aico-environment generator still shares the strict parser with
// its host tests; these files no longer implement any Config Web routes.
for (const environmentPath of [
  'src/system_env_parser.cpp',
  'src/system_env_parser.h',
  'tests/system_env_parser_test.cpp',
]) {
  await fs.access(path.join(repositoryRoot, environmentPath));
}

const rootCMake = await fs.readFile(path.join(repositoryRoot, 'CMakeLists.txt'), 'utf8');
assert.equal(rootCMake.includes('add_executable(config_web'), false, 'legacy C++ config_web target returned');

// Every script the settings pages and the LLM log viewer ship.
const webAssets = path.join(repositoryRoot, 'src/config_web/web/assets/js');
async function scripts(dir) {
  const entries = await fs.readdir(dir, {withFileTypes: true});
  const nested = await Promise.all(entries.map((entry) => {
    const full = path.join(dir, entry.name);
    return entry.isDirectory() ? scripts(full) : [full];
  }));
  return nested.flat().filter((file) => file.endsWith('.js'));
}
const sources = await Promise.all((await scripts(webAssets)).map((file) => fs.readFile(file, 'utf8')));
const bundle = sources.join('\n');

for (const route of [
  '/api/config',
  '/api/config/schema',
  '/api/config/application',
  '/api/config/locale',
  '/api/config/test',
  '/api/models?',
  '/api/storage/status',
  '/api/storage/format',
  '/api/storage/eject',
  '/api/device/snapshot',
  '/api/device/reboot',
  '/api/network/wifi/scan',
  '/api/network/wifi/connection',
  '/api/ota/status',
  '/api/ota/updates',
  '/api/logs/llm',
  '/api/logs/support',
  '/api/logs/agent',
  '/api/device/status',
  '/llm-logs',
]) {
  assert.ok(bundle.includes(route), `missing canonical frontend route: ${route}`);
}

assert.equal(bundle.includes('agentRequest'), false, 'Config Web still contains Agent cross-port requests');
assert.equal(bundle.includes("port='8080'"), false, 'Config Web still hard-codes the Agent port');

for (const retiredRoute of [
  "'/api/config/meta'",
  "'/api/agent/logs'",
  "'/api/logs/export'",
  '/api/llm-logs/',
  "'/api/ota/update'",
  "'/api/ota/logs'",
  "'/api/reboot'",
  "'/api/system/env'",
  '/api/wifi/',
]) {
  assert.equal(bundle.includes(retiredRoute), false, `retired frontend route remains: ${retiredRoute}`);
}

process.stdout.write('config web route tests passed\n');
