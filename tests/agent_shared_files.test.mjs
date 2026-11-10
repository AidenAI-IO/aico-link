import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {test} from 'node:test';
import vm from 'node:vm';

function harness(fetch) {
  class FileReader {
    async readAsDataURL(file) {
      try {
        this.result = 'data:' + (file.type || 'application/octet-stream') + ';base64,' + Buffer.from(await file.arrayBuffer()).toString('base64');
        this.onload();
      } catch (error) {
        this.error = error;
        this.onerror();
      }
    }
  }
  const context = vm.createContext({
    URL, FormData, FileReader, AbortController, setTimeout, clearTimeout, console,
    window: {location: {origin: 'http://192.168.42.1:8080', hostname: '192.168.42.1'}},
    loadingDiv: {classList: {contains: () => false}},
    fileUploadPending: false, draftAttachments: [], nextAttachmentId: 1, maxDraftImageAttachments: 4, maxDraftAttachments: 16,
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

test('individual attachment failures preserve the draft and process later mixed files', async t => {
  for (const [failure, reason] of [
    ['http', 'Storage full'],
    ['network', 'Network unavailable'],
    ['timeout', 'Upload timed out. Please try again.'],
    ['oversize', 'File exceeds the 100 MiB limit: failed.csv'],
    ['image', 'Image could not be read'],
  ]) {
    await t.test(failure, async () => {
      const requests = [];
      const context = harness(async (url, options) => {
        assert.equal(url, '/api/files');
        assert.ok(options.body instanceof FormData);
        assert.equal(context.fileUploadPending, true);
        const name = options.body.get('file').name;
        requests.push(name);
        if (name === 'failed.csv') {
          if (failure === 'network') throw new TypeError(reason);
          if (failure === 'timeout') throw Object.assign(new Error('aborted'), {name: 'AbortError'});
          return {ok: false, text: async () => reason};
        }
        if (name === 'last-failure.csv') return {ok: false, text: async () => 'Later upload error'};
        return {ok: true, json: async () => ({kind: 'file', path: '/share/' + name, name, size: 3})};
      });
      context.draftAttachments.push({kind: 'file', path: '/share/existing.csv', name: 'existing.csv'});
      context.maxDraftImageAttachments = 1;
      const failedFile = new File(['bad'], failure === 'image' ? 'failed.png' : 'failed.csv');
      if (failure === 'oversize') Object.defineProperty(failedFile, 'size', {value: 100 * 1024 * 1024 + 1});
      if (failure === 'image') failedFile.arrayBuffer = async () => { throw new Error(reason); };
      const event = {target: {files: [
        failedFile,
        new File(['png'], 'photo.png'),
        new File(['abc'], 'report.csv'),
        new File(['bad'], 'last-failure.csv'),
        new File(['jpg'], 'skipped.jpg'),
      ], value: 'selected'}};
      await context.handleFileSelection(event);
      assert.deepEqual(requests, [
        ...(['oversize', 'image'].includes(failure) ? [] : ['failed.csv']),
        'report.csv', 'last-failure.csv',
      ]);
      assert.equal(event.target.value, '');
      assert.equal(context.fileUploadPending, false);
      const payload = JSON.parse(JSON.stringify(context.cloneAttachmentsForTransport(context.draftAttachments)));
      assert.deepEqual(payload.map(attachment => attachment.name), ['existing.csv', 'photo.png', 'report.csv']);
      assert.equal(payload[1].kind, 'image');
      assert.equal(payload[1].data, Buffer.from('png').toString('base64'));
      assert.equal(payload[2].path, '/share/report.csv');
      assert.equal(payload[2].data, undefined);
      assert.equal(context.lastHint, '2 files could not be attached. ' + reason + ' 1 file skipped. Attach up to 1 images and 16 files/images in total.');
    });
  }
});

test('one picker routes mixed selections to visual input or shared files in selection order', async () => {
  const uploadedNames = [];
  const context = harness(async (url, options) => {
    assert.equal(url, '/api/files');
    const name = options.body.get('file').name;
    uploadedNames.push(name);
    return {ok: true, json: async () => ({kind: 'file', name, path: '/share/' + name})};
  });
  const files = [
    new File(['png'], 'camera-upload', {type: 'image/png'}),
    new File(['csv'], 'report.csv', {type: 'text/csv'}),
    new File(['jpg'], 'PHOTO.JPG'),
    new File(['audio'], 'audio.wav', {type: 'audio/wav'}),
  ];
  await context.handleFileSelection({target: {files, value: 'selected'}});
  assert.deepEqual(uploadedNames, ['report.csv', 'audio.wav']);
  const payload = JSON.parse(JSON.stringify(context.cloneAttachmentsForTransport(context.draftAttachments)));
  assert.deepEqual(payload.map(attachment => attachment.kind), ['image', 'file', 'image', 'file']);
  for (const [index, mime] of [[0, 'image/png'], [2, 'image/jpeg']]) {
    assert.equal(payload[index].mime_type, mime);
    assert.equal(payload[index].data, Buffer.from(await files[index].arrayBuffer()).toString('base64'));
    assert.equal(payload[index].path, undefined);
  }
  for (const index of [1, 3]) {
    assert.equal(payload[index].path, '/share/' + files[index].name);
    assert.equal(payload[index].data, undefined);
  }
});

test('image and combined limits still allow eligible files later in a mixed selection', async () => {
  for (const [images, files, expectedImages, expectedFiles, expectedUploads] of [
    [4, 0, 4, 1, 1],
    [0, 15, 1, 15, 0],
    [0, 16, 0, 16, 0],
  ]) {
    let uploads = 0;
    const context = harness(async () => {
      uploads++;
      return {ok: true, json: async () => ({kind: 'file', path: '/share/report.csv'})};
    });
    context.draftAttachments = [
      ...Array.from({length: images}, () => ({kind: 'image'})),
      ...Array.from({length: files}, (_, index) => ({kind: 'file', path: '/share/' + index})),
    ];
    await context.handleFileSelection({target: {files: [new File(['image'], 'photo.png'), new File(['csv'], 'report.csv')]}});
    assert.equal(context.getDraftImageCount(), expectedImages);
    assert.equal(context.draftAttachments.length, expectedImages + expectedFiles);
    assert.equal(uploads, expectedUploads);
    assert.match(context.lastHint, /4 images and 16/);
  }
});

test('image reads block concurrent selections and pasted images retain visual input', async () => {
  const context = harness(() => assert.fail('Images must not upload as shared files'));
  let finishRead;
  const photo = new File(['image'], '', {type: 'image/png'});
  photo.arrayBuffer = () => new Promise(resolve => { finishRead = resolve; });
  let pasteHandled = false;
  const pending = context.handleComposerPaste({
    clipboardData: {files: [photo]},
    preventDefault() { pasteHandled = true; },
  });
  assert.equal(pasteHandled, true);
  assert.equal(context.fileUploadPending, true);
  await context.handleFileSelection({target: {files: [new File(['csv'], 'report.csv')]}});
  assert.equal(context.draftAttachments.length, 0);
  finishRead(new Uint8Array([1, 2, 3]).buffer);
  await pending;
  assert.equal(context.fileUploadPending, false);
  assert.equal(context.draftAttachments.length, 1);
  assert.equal(context.draftAttachments[0].kind, 'image');
  assert.equal(context.draftAttachments[0].name, 'pasted-image-1.png');
  assert.equal(context.draftAttachments[0].data, 'AQID');
});

test('expired upload sessions offer local login without navigating away or losing the draft', async () => {
  let requests = 0;
  const context = harness(async () => {
    requests++;
    return {status: 401, ok: false, text: async () => '{"error":"login_required"}'};
  });
  context.draftAttachments.push({kind: 'file', path: '/share/existing.csv'});
  await context.handleFileSelection({target: {files: [new File(['abc'], 'report.csv'), new File(['png'], 'photo.png'), new File(['later'], 'later.csv')], value: 'selected'}});
  assert.equal(requests, 1);
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
