# Tune the memory cache

Size **statica**'s memory cache for your site, choose how long objects stay fresh, and read the metrics that show whether the limits fit your traffic.

Start from the defaults, watch the metrics below and change one limit at a time. [Memory cache](../architecture/memory-cache.md) explains the mechanisms behind each setting.

## Prerequisites

- Prometheus scraping statica's `/metrics`, as in [Monitor with Prometheus](./metrics.md).
- An idea of your site's objects: how many there are, how large the largest ones are, and which ones visitors request most.

## What the cache holds

The memory cache keeps one entry per object key, shared by every hostname and client:

- **body entries**: the complete bytes and metadata of an object, which answer `GET`, `HEAD` and range requests;
- **metadata entries**: the result of a metadata read from the origin, for a `HEAD` or a range request, which never supply a body;
- **negative entries**: a confirmed missing object, which answer requests for it as missing without an origin read.

Every entry is charged 640 bytes, plus the length of its key and its metadata, plus its body ([limits and accounting](../architecture/memory-cache.md#limits-and-accounting)). Objects larger than the per-object limit, or of unknown size, are streamed from the origin on every request and never stored.

## Size the memory budget

```yaml [config.yaml]
cache:
  max_bytes: 512MiB
  max_entries: 50000
  max_object_bytes: 8MiB
  max_fill_bytes: 64MiB
  max_concurrent_fills: 16
  max_waiters_per_key: 64
```

| Setting (default) | What it bounds | When it is too small |
| --- | --- | --- |
| [`cache.max_bytes`](../reference/configuration.md#cache-max-bytes) (`128MiB`) | Memory charged to entries, including replaced entries that responses still send. | Steady evictions while the cache is full, and a falling hit ratio. |
| [`cache.max_entries`](../reference/configuration.md#cache-max-entries) (`10000`) | Number of entries of every kind, negative entries included. | Evictions while `statica_cache_bytes` is well below its limit: many small entries reach this limit first. |
| [`cache.max_object_bytes`](../reference/configuration.md#cache-max-object-bytes) (`2MiB`) | Largest body stored. Must not exceed `max_bytes` or `max_fill_bytes`. | Frequently requested objects stream from the origin every time, with `cache_reason=too_large`. |
| [`cache.max_fill_bytes`](../reference/configuration.md#cache-max-fill-bytes) (`32MiB`) | Memory for bodies being read into the cache at the same time. | Fills stream instead of storing, with `cache_reason=fill_budget`. |
| [`cache.max_concurrent_fills`](../reference/configuration.md#cache-max-concurrent-fills) (`16`) | Shared fills running at once. | Misses read the origin alone, unshared and unstored, with `cache_reason=fills_full`. |
| [`cache.max_waiters_per_key`](../reference/configuration.md#cache-max-waiters-per-key) (`64`) | Requests sharing one fill. | Requests for a popular object that is being filled read the origin alone, with `cache_reason=waiters_full`. |

Raising `max_object_bytes` stores larger objects, but each one takes more of `max_bytes` and, while it is read, of `max_fill_bytes`, and its first visitors receive their first byte only after the whole object has arrived from the origin ([buffered or streamed](../architecture/memory-cache.md#buffered-or-streamed)).

The fill limits do not cap the requests that statica sends to the origin: a request over a fill limit reads the origin on its own. That read is not shared, nothing it reads is stored, and an expired copy cannot answer it when the origin fails.

## Leave room for the rest of the process

The cache limits bound the cache, not the process ([what the budget does not cover](../architecture/memory-cache.md#what-the-budget-does-not-cover)):

- `cache.max_fill_bytes` is separate from `cache.max_bytes`;
- each streamed response uses a 32 KiB buffer;
- the Go runtime, connections, TLS, metrics and the log queue (up to 4 MiB) need memory of their own.

Stored bodies are ordinary Go heap memory. With the Go runtime's default garbage collector setting, `GOGC=100`, the heap can grow to about twice its live size between two collections, so a full cache can need more memory than its budget before the garbage collector runs. statica sets no Go memory limit of its own; the Go runtime's standard `GOGC` and `GOMEMLIMIT` environment variables apply as to any Go program.

Give the process, or its container, a memory limit well above `max_bytes` plus `max_fill_bytes`, then compare its resident memory with those budgets under real traffic:

```text
process_resident_memory_bytes
statica_cache_limit_bytes + statica_cache_fill_limit_bytes
```

## Choose how long objects stay fresh

A fresh entry is served without asking the origin. An expired entry is revalidated before its next use: with its `ETag` when [`s3.features.conditional_reads`](../reference/configuration.md#s3-features-conditional-reads) allows it, so an unchanged object costs a 304 instead of a new download.

- [`cache.ttl`](../reference/configuration.md#cache-ttl) (`5m`) is how long an object stays fresh. With the default `cache.ttl_mode`, it is the longest: an origin `Cache-Control` (`s-maxage`, `max-age`, `no-cache`) or `Expires` can shorten it, never extend it ([lifetime](../architecture/memory-cache.md#lifetime)).
- [`cache.ttl_mode`](../reference/configuration.md#cache-ttl-mode) (`ceiling`) sets the role of the TTL against the origin's lifetime. `fixed` makes the TTL apply whatever the origin's metadata says: use it for objects uploaded with `Cache-Control: no-cache` that you still want statica to keep, since otherwise every request for them revalidates with the origin. `floor` and `fallback` let an object's `max-age` keep it longer than the TTL: use them only when the origin's lifetimes match how you publish.
- [`cache.request_directives`](../reference/configuration.md#cache-request-directives) (`honor`) decides whether a client's `Cache-Control: no-store`, `no-cache` or `max-age` can make statica read the origin. Browsers send `max-age=0` or `no-cache` on a reload; with `ignore`, reloads are served from memory too.
- [`cache.rules`](../reference/configuration.md#cache-rules) set other values by object path: the first matching rule applies.
- [`cache.negative_ttl`](../reference/configuration.md#cache-negative-ttl) (`5s`) is how long a missing object is remembered. A longer value spares the origin repeated requests for missing URLs; a shorter one makes a newly published object visible sooner.

```yaml [config.yaml]
cache:
  ttl: 5m
  rules:
    - match:
        prefix: /assets/
      ttl: 1h
    - match:
        exact: /index.html
      ttl: 30s
```

Rules match the object path, not the request path: a request for `/` uses `/index.html`, and a precompressed variant keeps its suffix, so an `exact` rule for `/app.js` does not cover `/app.js.br` while a `prefix` rule does.

::: warning The TTL also reaches browsers
With [`cache_control.default`](../reference/configuration.md#cache-control-default) at `auto`, an object without its own `Cache-Control` or `Expires` metadata is sent with `Cache-Control: max-age=<ttl in seconds>`. A longer `cache.ttl` therefore also lengthens how long browsers and CDNs keep such objects. Set [`cache_control`](../reference/configuration.md#cache-control) rules when the two should differ ([Control browser and CDN caching](./browser-caching.md)).
:::

There is no purge: a change in the bucket becomes visible on an instance when the entry for the changed object expires, or when the instance restarts.

## Serve expired copies when the origin fails

[`cache.stale_if_error`](../reference/configuration.md#cache-stale-if-error) lets an expired entry answer a request when refreshing it fails because the origin timed out, kept throttling or was unavailable. A 403, a missing object and some origin `Cache-Control` directives never qualify ([stale-if-error](../architecture/memory-cache.md#stale-if-error)).

For an origin that stalls, stale service also needs [`cache.fill_timeout`](../reference/configuration.md#cache-fill-timeout) below [`server.response_header_timeout`](../reference/configuration.md#server-response-header-timeout):

```yaml [config.yaml]
server:
  response_header_timeout: 30s
cache:
  fill_timeout: 20s
  stale_if_error: 1h
```

::: warning A request's deadline can end before the fill
Each request waiting for a fill stops at its own deadline, `server.response_header_timeout` (`30s` by default), with 504 Gateway Timeout and no stale copy; when it was the only waiter, the fill is canceled with it ([deadlines and cancellation](../architecture/memory-cache.md#deadlines-and-cancellation)).

An origin that never sends response headers usually fails the fill in time: each attempt ends after [`s3.response_header_timeout`](../reference/configuration.md#s3-response-header-timeout) (`10s`), so the default two attempts give up after about 20 seconds and the expired copy is served. Opening a connection to the origin, TLS handshake included, is bounded by the same `s3.response_header_timeout` per attempt, so an origin that does not answer connection attempts fails the fill in time too. The fill outlasts the request instead when:

- the body read stalls for [`s3.read_idle_timeout`](../reference/configuration.md#s3-read-idle-timeout) (`30s`);
- the attempts together outlast the request's deadline.

With the defaults, `cache.fill_timeout` and `server.response_header_timeout` are both `30s`, and the request that started the fill, admitted shortly before it, reaches its deadline first. A `cache.fill_timeout` below `server.response_header_timeout` ends the fill while its requests still wait, and they receive the expired copy in these cases too.
:::

Stale service applies only to reads made through the memory cache. A request that bypasses it, for example over a fill limit, gets the origin's error.

## Disable caching where it does not help

- **Per path.** A rule with `enabled: false` bypasses the memory cache for the matching objects, logged as `cache=bypass cache_reason=disabled`:

  ```yaml [config.yaml]
  cache:
    rules:
      - match:
          prefix: /downloads/
        enabled: false
  ```

- **Globally.** [`cache.enabled: false`](../reference/configuration.md#cache-enabled) sends every request to the origin. The access log then has no `cache` field, and the only cache metric is `statica_cache_enabled 0`.

::: warning Disabling the cache changes `Cache-Control`
With `cache_control.default: auto`, an object without its own cache metadata gets `max-age=<ttl>` only when the memory cache applies to it. With the cache disabled, globally or by a rule, such an object is sent with `Cache-Control: no-cache`.
:::

## Verify it

Request the same object twice and compare the `cache` field of the two `http_request` events:

```sh
curl -sS -o /dev/null https://www.example.com/
curl -sS -o /dev/null https://www.example.com/
```

```text
... path=/ status=200 outcome=success resolution=index cache=miss origin_key=index.html ...
... path=/ status=200 outcome=success resolution=index cache=hit origin_key=index.html ...
```

The `startup` event reports the limits in force, sizes in bytes: `cache=on cache_max_bytes=134217728 cache_max_entries=10000 cache_max_object_bytes=2097152 ...`.

## Metrics to watch

The examples use a 5-minute rate window; choose yours. Every query is a starting point, not an alert rule.

### Usage against the limits

```text
statica_cache_bytes / statica_cache_limit_bytes
statica_cache_entries / statica_cache_limit_entries
statica_cache_fills_in_flight / statica_cache_fill_limit_concurrency
statica_cache_fill_reserved_bytes / statica_cache_fill_limit_bytes
```

A cache that stays near one of its limits while evictions continue is smaller than your working set for that limit. `statica_cache_retired_bytes` is the part of `statica_cache_bytes` held by replaced or removed entries that slow responses still send.

### Evictions and refused admissions

```text
sum by (kind) (rate(statica_cache_evictions_total[5m]))
sum by (result) (rate(statica_cache_admissions_total{reason="none"}[5m]))
```

| Signal | Meaning |
| --- | --- |
| Evictions of `body` entries | Objects leave the cache to make room. Steady evictions together with misses for the same objects mean `max_bytes` or `max_entries` is short for your traffic. |
| Evictions of `negative` entries | Requests for many different missing URLs, such as from scanners, create negative entries that cycle through the cache and take room from other entries. |
| `result="no_space"` | No room even after evicting every entry not in use: the memory is held by entries pinned by responses still being served, including replaced entries whose readers have not finished. Fills do not count here: a fill that finds `cache.max_fill_bytes` full streams the object instead and shows up as `result="bypass"` with `reason="fill_budget"`. |
| `result="too_large"` | An entry larger than `max_object_bytes`, or whose charge alone exceeds `max_bytes`. |

### Bypasses

```text
sum by (reason) (rate(statica_cache_admissions_total{result="bypass"}[5m]))
```

| `reason` | Meaning | Setting to review |
| --- | --- | --- |
| `too_large` | Objects above the per-object limit stream every time. | `max_object_bytes`, if they are requested often and memory allows. |
| `unknown_size` | The origin sent no length. | None in statica. |
| `uncacheable` | Origin metadata forbids storing (`no-store`, `private`) or is malformed. | The objects' metadata. |
| `fill_budget` | Other fills hold `max_fill_bytes`. | `max_fill_bytes`. |
| `fills_full` | `max_concurrent_fills` fills are running. | `max_concurrent_fills`. |
| `waiters_full` | One fill already has `max_waiters_per_key` requests. | `max_waiters_per_key`. |
| `disabled`, `request_no_store` | A rule disables the cache, or the request carries `Cache-Control: no-store`. | Intended behavior. |
| `shutdown` | statica is shutting down and starts no new fills. | Expected during a shutdown. |

### Hit ratios

The share of requests that memory answered without an origin read:

```text
sum(rate(statica_cache_requests_total{outcome=~"hit|metadata_hit|negative_hit"}[5m]))
/
sum(rate(statica_cache_requests_total[5m]))
```

A body hit ratio, for requests that needed an object's body:

```text
sum(rate(statica_cache_requests_total{outcome="hit"}[5m]))
/
sum(rate(statica_cache_requests_total{outcome!~"metadata_hit|negative_hit"}[5m]))
```

`hit` counts only `GET` requests served from a fresh body; a `HEAD` answered from memory is a `metadata_hit`, as is a range request whose metadata came from memory and whose bytes came from the origin. The denominator still contains `HEAD` and range requests whose metadata read reached the origin, which count as `miss`, so read this ratio as an approximation. `coalesced` counts requests that joined another request's fill: they waited for the origin but did not read it themselves.

A byte hit ratio, the share of body bytes sent from memory:

```text
sum(rate(statica_cache_response_bytes_total[5m]))
/
sum(rate(statica_http_response_bytes_total[5m]))
```

Large objects that always stream keep this ratio low even when the request hit ratio is high.

### Freshness and fills

```text
sum by (kind) (rate(statica_cache_expirations_total[5m]))
sum by (result) (rate(statica_cache_revalidations_total[5m]))
sum by (result) (rate(statica_cache_fill_duration_seconds_count[5m]))
sum by (purpose) (rate(statica_origin_operations_total[5m]))
```

- Many revalidations answered `not_modified` mean that entries expire while the objects do not change. If your publishing allows it, a longer TTL for those paths saves origin requests.
- Fills ending with `result="timeout"` or `failed` point at the origin; with `stale_if_error`, look for `cache=stale` in the access log.
- `statica_origin_operations_total` by `purpose` shows the origin traffic that requests (`request`), fills (`cache_fill`) and the health check (`health`) cause.

## Pitfalls

- **Small entries fill `max_entries` first.** Metadata and negative entries pay the same 640-byte overhead as bodies. Scanners requesting many missing URLs create one negative entry per key tried.
- **Every instance has its own cache.** Instances share neither entries nor fills, and a restart empties the cache.
- **Origin `no-cache`.** With the default `cache.ttl_mode`, an object uploaded with `Cache-Control: no-cache` is checked with the origin on every request: the access log shows `cache=revalidated`, or `cache=miss` without conditional reads.

## Related pages

- [Control browser and CDN caching](./browser-caching.md): what browsers and CDNs are told to keep.
- [Monitor with Prometheus](./metrics.md): scrape and query the cache series.
- [Troubleshooting](./troubleshooting.md#origin-traffic-or-latency-grows-with-the-load): origin traffic that grows with the load, and [old content after publishing](./troubleshooting.md#old-content-after-publishing).
