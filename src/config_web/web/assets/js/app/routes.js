/**
 * Route table.
 *
 * `name` drives the panel's route class, which the desktop layout uses to keep
 * the right-hand panel empty on the list itself.
 */

import {homePage} from './pages/home.js';
import {wifiDetailPage, wifiPage} from './pages/wifi.js';
import {basicPage, languagePage} from './pages/basic.js';
import {
  conversationPage,
  promptPage,
  searchProviderPage,
  websearchPage,
} from './pages/conversation.js';
import {decisionPage, mainModelPage, modelPage, modelsPage, providerEditPage, providersPage} from './pages/model.js';
import {voiceExtraPage, voiceModelPage, voicePage, voiceProviderEditPage, voiceProvidersPage} from './pages/voice.js';
import {memoryPage} from './pages/memory.js';
import {storagePage} from './pages/storage.js';
import {advancedPage, agentLogPage, logLevelPage, logsPage, manualConfigPage} from './pages/advanced.js';
import {otaLogPage, otaPage} from './pages/ota.js';

/** Section ids that have a real page on the new layout. */
export const routes = [
  {path: '/', name: 'home', page: homePage},
  {path: '/wifi', name: 'wifi', page: wifiPage},
  {path: '/wifi/:ssid', name: 'wifi-detail', page: wifiDetailPage},
  {path: '/basic', name: 'basic', page: basicPage},
  {path: '/basic/language', name: 'basic-language', page: languagePage},
  {path: '/conversation', name: 'conversation', page: conversationPage},
  {path: '/conversation/prompt', name: 'conversation-prompt', page: promptPage},
  {path: '/conversation/tools', name: 'conversation-websearch', page: websearchPage},
  {path: '/conversation/tools/provider', name: 'conversation-search-provider', page: searchProviderPage},
  {path: '/model', name: 'model', page: modelPage},
  {path: '/model/main', name: 'model-main', page: mainModelPage},
  {path: '/model/decision', name: 'model-decision', page: decisionPage},
  {path: '/model/providers', name: 'model-providers', page: providersPage},
  {path: '/model/providers/edit', name: 'model-provider-edit', page: providerEditPage},
  {path: '/model/models', name: 'model-models', page: modelsPage},
  {path: '/voice', name: 'voice', page: voicePage},
  {path: '/voice/providers', name: 'voice-providers', page: voiceProvidersPage},
  {path: '/voice/providers/edit', name: 'voice-provider-edit', page: voiceProviderEditPage},
  {path: '/voice/model', name: 'voice-model', page: voiceModelPage},
  {path: '/voice/extra', name: 'voice-extra', page: voiceExtraPage},
  {path: '/memory', name: 'memory', page: memoryPage},
  {path: '/storage', name: 'storage', page: storagePage},
  {path: '/advanced', name: 'advanced', page: advancedPage},
  {path: '/advanced/logs', name: 'advanced-logs', page: logsPage},
  {path: '/advanced/logs/agent', name: 'advanced-agent-log', page: agentLogPage},
  {path: '/advanced/logs/level', name: 'advanced-log-level', page: logLevelPage},
  {path: '/advanced/config', name: 'advanced-config', page: manualConfigPage},
  {path: '/firmware', name: 'firmware', page: otaPage},
  {path: '/firmware/log', name: 'firmware-log', page: otaLogPage},
];
