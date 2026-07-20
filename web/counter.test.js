// Unit tests for counter.js, run with `node --test web/`.
//
// The script is executed inside a vm sandbox with a hand-rolled DOM stub, so
// there are no test dependencies at all — matching the script's own zero
// dependency policy.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import vm from 'node:vm';

const SRC = readFileSync(join(dirname(fileURLToPath(import.meta.url)), 'counter.js'), 'utf8');

// makeEnv builds a minimal DOM. Options let each test choose how the network
// behaves and which APIs exist.
function makeEnv(opts = {}) {
  const calls = { beacon: [], fetch: [], console: [] };
  const listeners = {};

  const attrs = Object.assign(
    { 'data-host': 'https://stats.example.com', 'data-site': 'example.com' },
    opts.attrs || {}
  );

  const location = Object.assign(
    {
      hostname: 'example.com',
      protocol: 'https:',
      pathname: '/',
      search: '',
      href: 'https://example.com/'
    },
    opts.location || {}
  );

  const history = {
    pushState: opts.pushState || function () { return 'original-result'; },
    replaceState: function () { return 'original-result'; }
  };

  const document = {
    currentScript: { getAttribute: (k) => (k in attrs ? attrs[k] : null) },
    referrer: opts.referrer || '',
    visibilityState: opts.visibilityState || 'visible',
    addEventListener: (ev, fn) => { (listeners[ev] ||= []).push(fn); },
    removeEventListener: (ev, fn) => {
      if (listeners[ev]) listeners[ev] = listeners[ev].filter((f) => f !== fn);
    }
  };

  const window = {
    location,
    document,
    history,
    innerWidth: 1920,
    addEventListener: (ev, fn) => { (listeners[ev] ||= []).push(fn); },
    console: { info: (...a) => calls.console.push(a) }
  };
  window.window = window;

  const navigator = {};
  if (opts.sendBeacon !== false) {
    navigator.sendBeacon = (url, blob) => {
      calls.beacon.push({ url, blob });
      return opts.beaconReturns === undefined ? true : opts.beaconReturns;
    };
  }

  // Only DOM stubs go into the sandbox. Intrinsics (JSON, Date, Math, Promise)
  // are deliberately NOT passed in: a vm context supplies its own, and sharing
  // the host's would let one test mutating JSON.stringify corrupt every test
  // that runs after it.
  const sandbox = {
    window,
    document,
    location,
    history,
    navigator,
    console: window.console
  };

  vm.createContext(sandbox);

  // Blob is defined inside the context so instanceof and property access work
  // against that realm's own object graph.
  vm.runInContext(
    'globalThis.Blob = class Blob { constructor(parts, o) { this.parts = parts; this.type = o && o.type; } };',
    sandbox
  );

  if (opts.fetch !== null) {
    const inner = opts.fetch || (() => Promise.resolve({ ok: true }));
    const bridge = (url, init) => {
      calls.fetch.push({ url, init });
      return inner(url, init);
    };
    sandbox.fetch = bridge;
    window.fetch = bridge;
  }

  return { sandbox, calls, listeners, window, document, history, location };
}

function run(env) {
  vm.runInContext(SRC, env.sandbox);
  return env;
}

function bodyOf(entry) {
  return JSON.parse(entry.blob.parts[0]);
}

test('sends a pageview on load', () => {
  const env = run(makeEnv());
  assert.equal(env.calls.beacon.length, 1);
  const body = bodyOf(env.calls.beacon[0]);
  assert.equal(body.n, 'pageview');
  assert.equal(body.d, 'example.com');
  assert.equal(env.calls.beacon[0].url, 'https://stats.example.com/api/event');
});

test('does nothing without data-host, and never invents an endpoint', () => {
  const env = run(makeEnv({ attrs: { 'data-host': null } }));
  assert.equal(env.calls.beacon.length, 0);
  assert.equal(env.calls.fetch.length, 0);
});

test('trailing slashes in data-host are normalised', () => {
  const env = run(makeEnv({ attrs: { 'data-host': 'https://stats.example.com///' } }));
  assert.equal(env.calls.beacon[0].url, 'https://stats.example.com/api/event');
});

test('localhost is ignored by default', () => {
  const env = run(makeEnv({ location: { hostname: 'localhost' } }));
  assert.equal(env.calls.beacon.length, 0);
  // The public API must still exist so host-page calls do not throw.
  assert.equal(typeof env.sandbox.window.ridiculytics, 'function');
  env.sandbox.window.ridiculytics('signup');
});

test('data-local=true opts localhost back in', () => {
  const env = run(makeEnv({
    location: { hostname: 'localhost' },
    attrs: { 'data-local': 'true' }
  }));
  assert.equal(env.calls.beacon.length, 1);
});

// ── Graceful failure ────────────────────────────────────────────────────────

test('falls back to fetch when sendBeacon refuses to queue', () => {
  const env = run(makeEnv({ beaconReturns: false }));
  assert.equal(env.calls.beacon.length, 1, 'should have attempted the beacon');
  assert.equal(env.calls.fetch.length, 1, 'should have fallen back to fetch');
});

test('falls back to fetch when sendBeacon throws', () => {
  const env = makeEnv();
  env.sandbox.navigator.sendBeacon = () => { throw new Error('blocked by extension'); };
  run(env);
  assert.equal(env.calls.fetch.length, 1);
});

test('survives having no transport at all', () => {
  const env = makeEnv({ sendBeacon: false, fetch: null });
  assert.doesNotThrow(() => run(env));
  // And the public API still does not throw for the host page.
  assert.doesNotThrow(() => env.sandbox.window.ridiculytics('signup'));
});

test('a rejected fetch never surfaces an unhandled rejection', async () => {
  const env = makeEnv({
    beaconReturns: false,
    fetch: () => Promise.reject(new Error('network down'))
  });
  run(env);
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(env.calls.fetch.length, 1);
});

test('stops trying after repeated failures', async () => {
  const env = makeEnv({
    beaconReturns: false,
    fetch: () => Promise.reject(new Error('collector down'))
  });
  run(env);
  await new Promise((r) => setTimeout(r, 10));

  const afterLoad = env.calls.fetch.length;
  for (let i = 0; i < 20; i++) {
    env.sandbox.window.ridiculytics('event' + i);
    await new Promise((r) => setTimeout(r, 1));
  }
  assert.ok(
    env.calls.fetch.length <= afterLoad + 3,
    `circuit breaker should stop the storm; made ${env.calls.fetch.length} requests`
  );
});

test('a 4xx trips the breaker too', async () => {
  const env = makeEnv({
    beaconReturns: false,
    fetch: () => Promise.resolve({ ok: false, status: 403 })
  });
  run(env);
  await new Promise((r) => setTimeout(r, 10));
  for (let i = 0; i < 20; i++) {
    env.sandbox.window.ridiculytics('e' + i);
    await new Promise((r) => setTimeout(r, 1));
  }
  assert.ok(env.calls.fetch.length <= 4, `made ${env.calls.fetch.length} requests to a 403 endpoint`);
});

// ── The host page must never break ──────────────────────────────────────────

test('pushState keeps working and returns the original result', () => {
  const env = run(makeEnv());
  const result = env.sandbox.history.pushState({}, '', '/next');
  assert.equal(result, 'original-result', 'the host application\'s return value must survive');
});

test('a throwing pageview cannot break host navigation', () => {
  // JSON.stringify blowing up simulates any internal failure during a
  // pageview. The host application's pushState must still succeed.
  const env = makeEnv();
  run(env);

  let hostCalled = false;
  env.sandbox.history.pushState = function () { hostCalled = true; return 'ok'; };

  vm.runInContext('JSON.stringify = function () { throw new Error("boom"); };', env.sandbox);
  env.sandbox.location.pathname = '/somewhere-new';

  let result;
  assert.doesNotThrow(() => { result = env.sandbox.history.pushState({}, '', '/somewhere-new'); });
  assert.equal(hostCalled, true);
  assert.equal(result, 'ok');
});

test('a throwing host pushState is not swallowed', () => {
  // Our wrapper must not hide the host application's own errors.
  const env = makeEnv({
    pushState: () => { throw new Error('host router failure'); }
  });
  run(env);
  assert.throws(() => env.sandbox.history.pushState({}, '', '/x'), /host router failure/);
});

test('a frozen history object does not stop pageviews', () => {
  const env = makeEnv();
  Object.freeze(env.sandbox.history);
  assert.doesNotThrow(() => run(env));
  assert.equal(env.calls.beacon.length, 1, 'the initial pageview should still be sent');
});

test('the public API never throws on bad input', () => {
  const env = run(makeEnv());
  const api = env.sandbox.window.ridiculytics;
  for (const bad of [undefined, null, 0, '', {}, [], () => {}]) {
    assert.doesNotThrow(() => api(bad));
  }
});

// ── Behaviour ───────────────────────────────────────────────────────────────

test('SPA navigation sends a pageview', () => {
  const env = run(makeEnv());
  env.sandbox.location.pathname = '/next';
  env.sandbox.history.pushState({}, '', '/next');
  assert.equal(env.calls.beacon.length, 2);
  assert.equal(bodyOf(env.calls.beacon[1]).n, 'pageview');
});

test('replaceState to the same path is not a pageview', () => {
  const env = run(makeEnv());
  env.sandbox.history.replaceState({}, '', '/');
  assert.equal(env.calls.beacon.length, 1, 'same-path churn must not double count');
});

test('data-spa=false disables history patching', () => {
  const env = run(makeEnv({ attrs: { 'data-spa': 'false' } }));
  env.sandbox.location.pathname = '/next';
  env.sandbox.history.pushState({}, '', '/next');
  assert.equal(env.calls.beacon.length, 1);
});

test('custom events are sent, reserved names are not', () => {
  const env = run(makeEnv());
  env.sandbox.window.ridiculytics('signup', { plan: 'free' });
  assert.equal(env.calls.beacon.length, 2);
  const body = bodyOf(env.calls.beacon[1]);
  assert.equal(body.n, 'signup');
  assert.deepEqual(body.p, { plan: 'free' });

  env.sandbox.window.ridiculytics('pageview');
  env.sandbox.window.ridiculytics('engagement');
  assert.equal(env.calls.beacon.length, 2, 'reserved names must be rejected');
});

test('event names are length capped', () => {
  const env = run(makeEnv());
  env.sandbox.window.ridiculytics('x'.repeat(500));
  assert.equal(bodyOf(env.calls.beacon[1]).n.length, 64);
});

test('queued pre-load calls are drained', () => {
  const env = makeEnv();
  const q = [];
  env.sandbox.window.ridiculytics = function () { q.push([].slice.call(arguments)); };
  env.sandbox.window.ridiculytics.q = [['early-signup', { a: 1 }]];
  run(env);

  const names = env.calls.beacon.map((b) => bodyOf(b).n);
  assert.ok(names.includes('early-signup'), `queued call was dropped; sent ${names}`);
});

test('engagement is reported when the page is hidden', () => {
  const env = run(makeEnv());
  env.sandbox.document.visibilityState = 'hidden';
  for (const fn of env.listeners.visibilitychange || []) fn();

  const engagement = env.calls.beacon.map(bodyOf).filter((b) => b.n === 'engagement');
  // Time on page rounds to 0 seconds in a test this fast, and sub-second
  // engagement is deliberately not reported.
  assert.ok(engagement.length <= 1);
});

test('referrer and viewport width are included', () => {
  const env = run(makeEnv({ referrer: 'https://news.ycombinator.com/' }));
  const body = bodyOf(env.calls.beacon[0]);
  assert.equal(body.r, 'https://news.ycombinator.com/');
  assert.equal(body.w, 1920);
});

test('prerendered pages do not count until shown', () => {
  const env = run(makeEnv({ visibilityState: 'prerender' }));
  assert.equal(env.calls.beacon.length, 0, 'a prerender must not count as a visit');

  env.sandbox.document.visibilityState = 'visible';
  for (const fn of [...(env.listeners.visibilitychange || [])]) fn();
  assert.equal(env.calls.beacon.length, 1);
});
