import assert from 'node:assert/strict';
import {test} from 'node:test';
import {mirroringConnection} from '../src/config_web/web/assets/js/app/pages/iphone-mirroring.js';

test('mirroring reconnect is conditional and requires a verified screenshot', async () => {
  let supported = false;
  let posts = 0;
  let outcome = {ok: true, restored: true, screenshot_available: false};
  const model = mirroringConnection(async (_url, options) => {
    if (!options) return {ok: true, supported};
    posts++;
    assert.equal(options.method, 'POST');
    assert.equal(options.body, '{}');
    return outcome;
  }, () => {});
  await model.load();
  await model.reconnect();
  assert.equal(posts, 0, 'other Agent modes must not reconnect');
  supported = true;
  await model.load();
  await model.reconnect();
  assert.equal(model.state.restored, false);
  assert.equal(model.state.error, 'capture_failed');
  outcome = {...outcome, screenshot_available: true};
  await model.reconnect();
  assert.equal(model.state.restored, true);
  assert.equal(model.state.busy, false);
});

test('duplicate clicks and stale status reads cannot interfere with reconnection', async () => {
  let finishRead, finishPost;
  let reads = 0, posts = 0;
  const model = mirroringConnection(async (_url, options) => {
    if (!options) {
      if (++reads === 2) return new Promise(resolve => { finishRead = resolve; });
      return {ok: true, supported: true};
    }
    posts++;
    return new Promise(resolve => { finishPost = resolve; });
  }, () => {});
  await model.load();
  const pendingRead = model.load();
  const reconnect = model.reconnect();
  assert.equal(model.state.busy, true);
  await model.reconnect();
  await model.load();
  assert.equal(posts, 1);
  finishPost({ok: true, restored: true, screenshot_available: true});
  await reconnect;
  finishRead({ok: true, supported: false});
  await pendingRead;
  assert.equal(model.state.supported, true);
  assert.equal(model.state.restored, true);
});

test('failed reconnect preserves the action and reports the failure for retry', async () => {
  const model = mirroringConnection(async (_url, options) => {
    if (!options) return {ok: true, supported: true};
    throw Object.assign(new Error('busy'), {code: 'session_busy'});
  }, () => {});
  await model.load();
  await model.reconnect();
  assert.equal(model.state.supported, true);
  assert.equal(model.state.busy, false);
  assert.equal(model.state.restored, false);
  assert.equal(model.state.error, 'session_busy');
});
