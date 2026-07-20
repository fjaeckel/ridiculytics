/**
 * ridiculytics counter.js — MIT licensed.
 *
 * No cookies, no localStorage, no fingerprinting, no persistent identifier.
 * There is deliberately no fallback endpoint in this file: a missing
 * data-host is a silent no-op, so a fork can never accidentally report to
 * somebody else's server.
 *
 * Failure policy: analytics must never break, block, or slow the host page.
 * Every entry point is wrapped, every network call is best-effort, and a
 * collector that is down or unreachable degrades to silence rather than to
 * console noise or a request storm.
 *
 *   <script defer src="https://cdn.jsdelivr.net/npm/ridiculytics@1/counter.min.js"
 *           data-host="https://stats.example.com"
 *           data-site="example.com"></script>
 */
(function (window, document) {
  'use strict';

  // safe wraps a function so it can never throw into the host page. This
  // matters most for the history patch below: an exception escaping from
  // pushState would break the host application's router, which is a far worse
  // outcome than losing a pageview.
  function safe(fn) {
    return function () {
      try {
        return fn.apply(this, arguments);
      } catch (e) {
        return undefined;
      }
    };
  }

  var script = document.currentScript;
  if (!script) return;

  var host = script.getAttribute('data-host');
  var site = script.getAttribute('data-site') || location.hostname;
  if (!host) return;

  var endpoint = host.replace(/\/+$/, '') + '/api/event';
  var spa = script.getAttribute('data-spa') !== 'false';
  var trackLocal = script.getAttribute('data-local') === 'true';

  // file:// and localhost are almost never wanted in production stats, and
  // forgetting to exclude them is the most common first-day complaint.
  var isLocal =
    /^(localhost|127\.|0\.0\.0\.0|\[::1\]|.*\.local)$/.test(location.hostname) ||
    location.protocol === 'file:';
  if (isLocal && !trackLocal) {
    window.ridiculytics = function (n) {
      if (window.console && console.info) console.info('[ridiculytics] ignored locally:', n);
    };
    return;
  }

  // Circuit breaker. If the collector is unreachable — DNS failure, blocked by
  // an ad blocker, down for maintenance — there is no value in attempting a
  // request on every single pageview for the rest of the visit. After a few
  // consecutive failures we stop trying entirely.
  var FAILURE_LIMIT = 3;
  var failures = 0;
  var disabled = false;

  function onFailure() {
    if (++failures >= FAILURE_LIMIT) disabled = true;
  }

  function onSuccess() {
    failures = 0;
  }

  var post = safe(function (body) {
    if (disabled) return;

    var json = JSON.stringify(body);

    // text/plain keeps this a CORS "simple request", so there is no preflight
    // round trip on every pageview.
    if (navigator.sendBeacon) {
      var queued = false;
      try {
        queued = navigator.sendBeacon(endpoint, new Blob([json], { type: 'text/plain' }));
      } catch (e) {
        queued = false;
      }
      // sendBeacon returns false when the user agent refuses to queue the
      // request — over quota, or blocked. That is a real signal, so fall
      // through to fetch rather than silently dropping the event.
      if (queued) {
        onSuccess();
        return;
      }
    }

    if (!window.fetch) {
      onFailure();
      return;
    }

    try {
      fetch(endpoint, {
        method: 'POST',
        body: json,
        keepalive: true,
        credentials: 'omit',
        mode: 'cors',
        headers: { 'Content-Type': 'text/plain' }
      }).then(
        function (res) {
          // Any 2xx means accepted. A 4xx is a permanent misconfiguration —
          // wrong domain, disallowed origin — and retrying it on every
          // pageview would only add load for both sides, so it counts as a
          // failure and trips the breaker too.
          if (res && res.ok) onSuccess();
          else onFailure();
        },
        function () {
          onFailure();
        }
      );
    } catch (e) {
      onFailure();
    }
  });

  var lastPath = null;
  var enteredAt = 0;

  var pageview = safe(function () {
    var path = location.pathname + location.search;
    if (path === lastPath) return; // replaceState churn is not a pageview
    flush();
    lastPath = path;
    enteredAt = Date.now();

    post({
      n: 'pageview',
      d: site,
      u: location.href,
      r: document.referrer || '',
      w: window.innerWidth || 0
    });
  });

  // flush reports time on the page we are leaving. One beacon at hide time
  // rather than a heartbeat, so a background tab costs nothing.
  var flush = safe(function () {
    if (!lastPath || !enteredAt) return;
    var seconds = Math.round((Date.now() - enteredAt) / 1000);
    enteredAt = 0;
    if (seconds < 1 || seconds > 3600) return;
    post({ n: 'engagement', d: site, u: location.href, e: seconds });
  });

  // event queues a custom event / goal.
  var event = safe(function (name, props) {
    if (!name || name === 'pageview' || name === 'engagement') return;
    post({
      n: String(name).slice(0, 64),
      d: site,
      u: location.href,
      p: props || null
    });
  });

  // Drain any calls made before this script finished loading.
  var queued = window.ridiculytics && window.ridiculytics.q;
  window.ridiculytics = event;
  if (queued && queued.length) {
    for (var i = 0; i < queued.length; i++) {
      event.apply(null, queued[i]);
    }
  }

  if (spa) {
    var wrap = function (name) {
      var original = history[name];
      if (typeof original !== 'function') return;
      history[name] = function () {
        // The host application's call runs first and its result is returned
        // untouched. pageview is already wrapped in safe(), so nothing we do
        // can turn a working navigation into a broken one.
        var result = original.apply(this, arguments);
        pageview();
        return result;
      };
    };
    try {
      wrap('pushState');
      wrap('replaceState');
      window.addEventListener('popstate', pageview);
    } catch (e) {
      // A locked-down history object just means no SPA tracking.
    }
  }

  // visibilitychange is the only unload signal that fires reliably on mobile
  // Safari; pagehide covers bfcache navigation.
  try {
    document.addEventListener('visibilitychange', function () {
      if (document.visibilityState === 'hidden') flush();
    });
    window.addEventListener('pagehide', flush);
  } catch (e) {
    // No engagement reporting, but pageviews still work.
  }

  if (document.visibilityState === 'prerender') {
    document.addEventListener('visibilitychange', function onShow() {
      if (document.visibilityState !== 'prerender') {
        document.removeEventListener('visibilitychange', onShow);
        pageview();
      }
    });
  } else {
    pageview();
  }
})(window, document);
