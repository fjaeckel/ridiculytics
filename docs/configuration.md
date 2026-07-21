# Configuration

Everything is settable as a `RIDICULYTICS_*` environment variable. Run with no
`-config` flag and the process is configured entirely from the environment —
this is the intended container deployment.

A YAML file is still supported (`-config sites.yaml`, see
[`sites.yaml`](../sites.yaml)). **Environment always wins over the file**, so an
image can ship a sensible default and a deployment can override any part of it
without editing files.

Invalid values fail at startup and **every** bad variable is reported at once,
so a misconfigured container does not need one restart per typo.

## Sites

```sh
RIDICULYTICS_SITES=example.com,blog.example.co.uk
```

Origins default to `https://<domain>` and `https://www.<domain>`, so a
single-site deployment needs only this one variable. A site with an empty
origin allowlist is rejected at startup rather than silently accepting events
from anywhere.

Per-site variables use the domain as an infix, uppercased with every
non-alphanumeric character replaced by `_`:

| domain | infix |
|---|---|
| `example.com` | `SITE_EXAMPLE_COM` |
| `blog.example.co.uk` | `SITE_BLOG_EXAMPLE_CO_UK` |

```sh
RIDICULYTICS_SITE_EXAMPLE_COM_ORIGINS=https://example.com,https://staging.example.com
RIDICULYTICS_SITE_EXAMPLE_COM_CROSS_PATH_CAP=25
RIDICULYTICS_SITE_EXAMPLE_COM_PATH_LABELS=country,referrer
RIDICULYTICS_SITE_EXAMPLE_COM_HMAC_KEY=secret
RIDICULYTICS_SITE_EXAMPLE_COM_UNIQUE_VISITORS_BY_PATH=true
```

Explicit origins **replace** the inferred ones rather than extending them.

## Reference

### Server

| variable | default | notes |
|---|---|---|
| `INGEST_ADDR` | `:8080` | public listener |
| `METRICS_ADDR` | `127.0.0.1:9090` | keep off the internet |
| `TRUST_PROXY` | `false` | mandatory behind a reverse proxy |
| `WORKERS` | `4` | aggregation workers |
| `QUEUE_SIZE` | `8192` | events buffered before shedding |
| `RATE_PER_MIN` | `600` | per IPv4 /24 or IPv6 /64 |
| `MAX_BODY_BYTES` | `4096` | |
| `SALT_ROTATE` | `24h` | visitor-hash salt lifetime |
| `SERVE_COUNTER_JS` | `true` | serve the script at `/counter.js` |

### Cardinality

| variable | default | notes |
|---|---|---|
| `PATH_CAP` | `200` | paths in `pageviews_total` |
| `CROSS_PATH_CAP` | `50` | path as a secondary label — see [cardinality](cardinality.md) |
| `REFERRER_CAP` | `200` | |
| `ASN_CAP` | `200` | |
| `CAMPAIGN_CAP` | `100` | also source and medium |
| `EVENT_CAP` | `50` | custom event names |
| `ADMIT_MIN_COUNT` | `3` | sightings before a value earns a series |
| `PATH_LABELS` | see below | families carrying a secondary path label |
| `UNIQUE_VISITORS_BY_PATH` | `false` | coarse 24h per-path sketch |
| `SESSION_TTL` | `30m` | idle timeout |
| `DECAY_AFTER` | `168h` | drop label values unseen this long |

`PATH_LABELS` defaults to
`country,referrer,source,medium,campaign,browser,os,device,screen,event` —
notably **not** `asn`, which has the worst multiplier and the least-asked
cross-slice.

Lists are comma-separated. Use `-` or `none` to set one explicitly empty, since
an empty variable is indistinguishable from an unset one:

```sh
RIDICULYTICS_PATH_LABELS=none    # drop path labels entirely
```

### Geolocation

| variable | default | notes |
|---|---|---|
| `GEO_PROVIDER` | `dbip` | `dbip`, `maxmind` or `none` |
| `GEO_CITY_DB` | | mmdb path; unset means discover |
| `GEO_COUNTRY_DB` | | mmdb path; unset means discover |
| `GEO_ASN_DB` | | mmdb path; unset means discover |
| `REGION_ENABLED` | `false` | opt-in, no path label |
| `CITY_ENABLED` | `false` | opt-in, no path label |

The container ships DB-IP Lite country and ASN databases, and with no path set
they are discovered automatically — geo works with none of these variables. See
[deployment](deployment.md#geoip) for the search order and why the provider is
pluggable.

## Path rewrites

Available in YAML only, since they are regexes:

```yaml
sites:
  - domain: example.com
    origins: ["https://example.com"]
    path_rewrites:
      - match: "^/products/[^/]+$"
        replace: "/products/:slug"
```

Normalization already strips query strings, lowercases, drops trailing slashes
and collapses UUIDs, long hex and long digit runs to `:id`. Four-digit years
survive, because `/2024/review` is a route rather than an object id. Rewrites
are for what is left.

## Reloading

```sh
kill -HUP $(pidof ridiculytics)
```

Re-reads configuration and GeoIP databases. A failed reload keeps the previous
config and logs the error, so a bad edit is a log line rather than an outage.
Existing counters and sketches survive a reload — it never punches a hole in a
live dashboard.
