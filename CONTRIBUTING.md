# Contributing

## Layout

```
cmd/ridiculytics/     entry point, two listeners
internal/ingest/      HTTP handler, validation, rate limit, visitor hashing
internal/enrich/      path/referrer/UTM normalization, UA parsing
internal/aggregate/   metric families, cardinality guard, admission
internal/sketch/      rolling HyperLogLog, TTL session map
internal/geo/         provider interface + mmdb/null
internal/config/      env and YAML loading, hot reload
web/counter.js        the tracking script, MIT
test/                 integration tests
deploy/               nginx, Prometheus rules, Grafana dashboard
```

## Tests

```sh
make test              # everything, race detector on
make test-unit
make test-integration
make test-js
make test-cardinality
make lint
make cover
```

Unit tests live beside the code they cover. `test/` holds integration tests
that wire the real components together over real listeners — the same wiring
`cmd/ridiculytics` uses — and additionally assert on the deployment files: that
the production stack runs no Prometheus or Grafana, that certbot stays behind
its opt-in profile, that the metrics port is bound to loopback, and that nginx
never routes `/metrics`.

`counter.js` has its own suite that runs the real script inside a `node:vm`
sandbox with a hand-rolled DOM — no test dependencies, matching the script's
own zero-dependency policy. It covers the failure paths specifically: beacon
refusal, fetch rejection, a 4xx response, no transport at all, a frozen
`history` object, and an internal error during a patched `pushState`.

### Load-bearing tests

Understand these before changing anything near them:

- `TestMarginalsRemainExactUnderCardinalityPressure` — if `sum by (country)`
  stops being a true total, every dashboard silently starts lying.
- `TestGuardResistsSquatting` — the admission gate is the only thing between a
  public endpoint and your Prometheus bill.
- `TestNoHardcodedEndpoint` — proves `counter.js` has no fallback URL, so a
  fork that forgets `data-host` is a no-op rather than a silent redirect of
  somebody else's traffic.
- `a throwing pageview cannot break host navigation` (JS) — we patch
  `history.pushState`; an exception escaping it would break the host site's
  router, which is far worse than losing a pageview.

## Design rules

Two that are not negotiable without redesigning the storage model:

- **At most two labels per metric family**: its own dimension plus `path`. A
  third multiplies every series again.
- **Never widen an existing family.** If you need a new correlation, it is a
  new, explicitly-capped family, and you should be able to say why it earns its
  cost.

## Releases

Tagging `vX.Y.Z` publishes, only after the full suite passes:

- binaries for linux/darwin/windows/freebsd on amd64/arm64, plus checksums
- a multi-arch container image to `ghcr.io/fjaeckel/ridiculytics`
- the npm package, which is what makes the jsDelivr URL resolve

The tag must match `package.json`'s version or the release fails early —
otherwise npm would publish a version that cannot be traced back to a commit.

`counter.js` reaches sites two ways:

```html
<!-- via npm -->
<script src="https://cdn.jsdelivr.net/npm/ridiculytics@1/web/counter.min.js"></script>

<!-- straight from the git tag, no npm involved -->
<script src="https://cdn.jsdelivr.net/gh/fjaeckel/ridiculytics@v1/web/counter.min.js"></script>
```

Publishing to npm uses a trusted publisher: npm is configured to trust
`.github/workflows/release.yml` in this repo and issues a short-lived credential
against the workflow's OIDC token, so there is no publish token to store or
rotate. Renaming or moving that workflow file breaks publishing until the
trusted publisher entry on npmjs.com is updated to match.

The npm job is skipped on forks, since a fork cannot be a trusted publisher for
a package it does not own. The jsDelivr `gh` path keeps working regardless, so a
fork still gets a usable CDN URL without configuring anything.

## counter.js budget

CI fails the build if the minified script exceeds **2048 bytes gzipped**. The
whole premise is a script small enough that nobody thinks twice about embedding
it; that is enforced rather than trusted.
