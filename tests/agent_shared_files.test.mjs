import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {test} from 'node:test';
import vm from 'node:vm';

function harness(fetch) {
  const context = vm.createContext({
    URL, FormData, AbortController, setTimeout, clearTimeout, console,
    window: {location: {origin: 'http://192.168.42.1:8080', hostname: '192.168.42.1'}},
    loadingDiv: {classList: {contains: () => false}},
    fileUploadPending: false, draftAttachments: [], nextAttachmentId: 1, maxDraftImageAttachments: 4,
    composerHintTimer: null, composerHintEl: {children: [], appendChild(child) { this.children.push(child); }},
    document: {createElement: tag => ({tag, children: [], appendChild(child) { this.children.push(child); }})},
    fetch,
  });
  vm.runInContext(readFileSync('src/agent/internal/agent/web_ui/scripts/attachments.js', 'utf8'), context);
  vm.runInContext(`setComposerState = function() {}; setComposerHint = function(text) { lastHint = text; }; resetComposerHint = function() {}; renderDraftAttachments = function() {};`, context);
  return context;
}

test('download cards accept encoded board links and reject external or executable URLs', () => {
  const context = harness();
  const links = context.sharedLinksFromText('[报告](/share/%E6%8A%A5%E5%91%8A%20%281%29.pdf) http://192.168.42.1:8000/share/report.csv [bad](https://evil.test/share/file) [bad](javascript:alert)');
  assert.equal(links.length, 2);
  assert.equal(links[0].name, '报告');
  assert.equal(links[1].download_url, '/share/report.csv');
  assert.equal(context.safeSharedURL('//evil.test/share/file'), '');
  assert.equal(context.safeSharedURL('file:///share/file'), '');
  assert.equal(context.sharedLinksFromText('下载 /share/report.pdf。')[0].download_url, '/share/report.pdf');
});

test('upload completion adds only a file reference and failure preserves existing draft files', async () => {
  let requests = 0;
  const context = harness(async (url, options) => {
    requests++;
    assert.equal(url, '/api/files');
    assert.ok(options.body instanceof FormData);
    assert.equal(context.fileUploadPending, true);
    if (requests === 2) return {ok: false, text: async () => 'Storage full'};
    return {ok: true, json: async () => ({kind: 'file', path: '/share/report.csv', name: 'report.csv', download_url: '/share/report.csv', size: 3})};
  });
  const event = {target: {files: [new File(['abc'], 'report.csv'), new File(['bad'], 'failed.csv')], value: 'selected'}};
  await context.handleFileSelection(event);
  assert.equal(event.target.value, '');
  assert.equal(context.fileUploadPending, false);
  assert.equal(context.draftAttachments.length, 1);
  assert.equal(context.lastHint, 'Storage full');
  const payload = context.cloneAttachmentsForTransport(context.draftAttachments)[0];
  assert.equal(payload.path, '/share/report.csv');
  assert.equal(payload.data, undefined);
});

test('image selection respects the combined attachment limit', async () => {
  const context = harness();
  context.draftAttachments = Array.from({length: 16}, (_, index) => ({kind: 'file', path: '/share/' + index}));
  await context.addImageFiles([new File(['image'], 'photo.png', {type: 'image/png'})], 'selected');
  assert.equal(context.draftAttachments.length, 16);
  assert.match(context.lastHint, /16/);
});

test('expired upload sessions offer local login without navigating away or losing the draft', async () => {
  const context = harness(async () => ({status: 401, ok: false, text: async () => '{"error":"login_required"}'}));
  context.draftAttachments.push({kind: 'file', path: '/share/existing.csv'});
  await context.handleFileSelection({target: {files: [new File(['abc'], 'report.csv')], value: 'selected'}});
  assert.equal(context.draftAttachments.length, 1);
  assert.equal(context.fileUploadPending, false);
  assert.equal(context.composerHintEl.children.length, 1);
  const link = context.composerHintEl.children[0];
  assert.equal(link.href, '/login?next=%2Fshare%2F');
  assert.equal(link.target, '_blank');
  assert.equal(link.rel, 'noopener');
});

test('download cards let a login redirect open as a page without replacing the composer', () => {
  const context = harness();
  const wrapper = context.renderMessageAttachments([{kind: 'file', name: 'report.csv', download_url: '/share/report.csv'}]);
  const link = wrapper.children[0].children.find(child => child.tag === 'a');
  assert.equal(link.href, '/share/report.csv');
  assert.equal(link.download, undefined);
  assert.equal(link.target, '_blank');
  assert.equal(link.rel, 'noopener');
});

test('other authorization errors do not prompt for a device password', async () => {
  for (const [status, error] of [[401, 'maintenance_session_required'], [403, 'lan_access_disabled']]) {
    const context = harness(async () => ({status, ok: false, text: async () => JSON.stringify({error})}));
    await context.handleFileSelection({target: {files: [new File(['abc'], 'report.csv')], value: 'selected'}});
    assert.equal(context.composerHintEl.children.length, 0);
    assert.equal(context.draftAttachments.length, 0);
  }
});
