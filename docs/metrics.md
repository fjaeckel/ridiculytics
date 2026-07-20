# Metrics

Families marked ⊕ carry a secondary `path` label, subject to `cross_path_cap`
and the per-site `path_labels` list. Where path is disabled the label is still
present as `__all__`, so a metric never has two different label sets.

```
ridiculytics_pageviews_total{site,path}
ridiculytics_entries_total{site,path}
ridiculytics_exits_total{site,path}

ridiculytics_pageviews_by_country_total{site,country,path}     ⊕
ridiculytics_pageviews_by_referrer_total{site,referrer,path}   ⊕   host only
ridiculytics_pageviews_by_source_total{site,source,path}       ⊕
ridiculytics_pageviews_by_medium_total{site,medium,path}       ⊕
ridiculytics_pageviews_by_campaign_total{site,campaign,path}   ⊕
ridiculytics_pageviews_by_browser_total{site,browser,path}     ⊕
ridiculytics_pageviews_by_os_total{site,os,path}               ⊕
ridiculytics_pageviews_by_device_total{site,device,path}       ⊕
ridiculytics_pageviews_by_screen_total{site,class,path}        ⊕
ridiculytics_events_total{site,name,path}                      ⊕
ridiculytics_pageviews_by_asn_total{site,asn,as_org,path}          path off by default

ridiculytics_sessions_total{site}
ridiculytics_bounces_total{site}
ridiculytics_session_duration_seconds{site}
ridiculytics_time_on_page_seconds{site,path}

ridiculytics_unique_visitors{site,window}          gauge, window=1h|24h|7d|30d
ridiculytics_visitors_active{site}                 gauge, 5m
ridiculytics_unique_visitors_by_path{site,path}    gauge, opt-in, 24h, coarse
```

Self-observability:

```
ridiculytics_ingest_events_total{site,result}
ridiculytics_cardinality_capped_total{site,dimension}
ridiculytics_label_values{site,dimension}
ridiculytics_sessions_live{site}
ridiculytics_sessions_evicted_total{site}
ridiculytics_queue_depth
ridiculytics_ingest_duration_seconds
ridiculytics_geoip_lookups_total{result}
ridiculytics_geoip_db_age_seconds
```

## Label sentinels

| value | meaning |
|---|---|
| `__other__` | folded by a cardinality cap |
| `__none__` | genuinely absent (no referrer, no geo database) |
| `__all__` | this family does not carry a path label for this site |

`__none__` and `__other__` being distinct is what lets you tell "geo is broken"
apart from "geo is capped".

## Querying

Every ⊕ family has two labels, so **you must aggregate away the one you are not
asking about** — otherwise you get one series per pair. The shipped Grafana
dashboard does this correctly everywhere; copy from it.

```promql
# top pages, last 24h
topk(10, increase(ridiculytics_pageviews_total{site="example.com"}[24h]))

# top countries — note the sum by
topk(10, sum by (country) (
  increase(ridiculytics_pageviews_by_country_total{site="example.com"}[24h])))

# which countries read one specific post
topk(10, sum by (country) (
  increase(ridiculytics_pageviews_by_country_total{path="/blog/my-post"}[7d])))

# what drove traffic to the pricing page
topk(5, sum by (referrer) (
  increase(ridiculytics_pageviews_by_referrer_total{path="/pricing"}[24h])))

# mobile share per page
sum by (path) (increase(ridiculytics_pageviews_by_device_total{device="mobile"}[24h]))
  / sum by (path) (increase(ridiculytics_pageviews_by_device_total[24h]))

# bounce rate
increase(ridiculytics_bounces_total[24h]) / increase(ridiculytics_sessions_total[24h])

# average session duration
increase(ridiculytics_session_duration_seconds_sum[24h])
  / increase(ridiculytics_session_duration_seconds_count[24h])

# is the launch working
sum(rate(ridiculytics_pageviews_by_referrer_total{referrer="news.ycombinator.com"}[5m]))
```

## Unique visitors

Uniques are HyperLogLog estimates over sliding windows, keyed by
`hmac(rotating_salt, ip ‖ user-agent ‖ domain)`. The salt is random at boot,
held only in memory, and rotated every 24h.

Sketches cannot be merged by Prometheus, so uniques exist **only** for the
windows the server computes. For a specific day:

```promql
max_over_time(ridiculytics_unique_visitors{window="24h"}[1d])
```

Two caveats, both inherent to counting people without storing anything about
them:

- Arbitrary ranges are not derivable. There is no query that produces "uniques
  between these two dates" for a range that is not one of the fixed windows.
- Salt rotation means a visitor spanning midnight UTC counts twice in the 7d
  and 30d windows, so multi-day uniques skew slightly high. Fixing that would
  require persisting an identifier.

Counters restart at zero when the process does, which Prometheus handles
natively via `rate`/`increase`. Sketch-backed gauges lose their history on
restart. Durability lives in Prometheus, not here.
