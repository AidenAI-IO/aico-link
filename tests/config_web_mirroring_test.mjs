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

test('status recovery clears errors and lost support invalidates verified frames', async () => {
  let supported = true, fail = false;
  const model = mirroringConnection(async (_url, options) => {
    if (options) return {ok: true, restored: true, screenshot_available: true};
    if (fail) throw Object.assign(new Error('unavailable'), {code: 'bridge_unavailable'});
    return {ok: true, supported};
  }, () => {});
  await model.load();
  await model.reconnect();
  await model.load();
  assert.equal(model.state.restored, true, 'unchanged support preserves verification');
  fail = true;
  await model.load();
  assert.equal(model.state.readError, 'bridge_unavailable');
  fail = false;
  await model.load();
  assert.equal(model.state.readError, null);
  assert.equal(model.state.restored, false);
  await model.reconnect();
  supported = false;
  await model.load();
  assert.equal(model.state.restored, false);
  supported = true;
  await model.load();
  assert.equal(model.state.restored, false, 'a new session needs verification');
});

for (const code of ['input_permission_required', 'session_busy', 'capture_failed']) {
  test(`status recovery preserves reconnect failure: ${code}`, async () => {
    let failRead = false, recovered = false;
    const model = mirroringConnection(async (_url, options) => {
      if (options) return recovered
        ? {ok: true, restored: true, screenshot_available: true}
        : {ok: false, code};
      if (failRead) throw Object.assign(new Error('unavailable'), {code: 'agent_unavailable'});
      return {ok: true, supported: true};
    }, () => {});
    await model.load();
    await model.reconnect();
    await model.load();
    assert.equal(model.state.error, code, 'support checks do not verify reconnection');
    failRead = true;
    await model.load();
    assert.equal(model.state.readError, 'agent_unavailable');
    failRead = false;
    await model.load();
    assert.equal(model.state.readError, null);
    assert.equal(model.state.error, code, 'read recovery preserves the reconnect failure');
    recovered = true;
    await model.reconnect();
    assert.equal(model.state.error, null);
    assert.equal(model.state.restored, true);
  });
}
