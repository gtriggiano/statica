---
outline: [2, 2]
---

# Metrics reference

statica exports Prometheus metrics on the admin listener, at [`/metrics`](./admin-endpoints.md#metrics). This page lists every metric family whose name starts with `statica_`, with its type, its labels and every value they take, and when its series exist. It also names the standard Go and process families exported next to them.

Each family has its own heading, named after its full name. Durations are in seconds and sizes in bytes. Scrapes read in-memory state only: they never contact the origin. [Monitor with Prometheus](../guide/metrics.md) shows how to scrape them and which queries answer common questions.

| Family | Type | Labels |
| --- | --- | --- |
| [`statica_http_requests_total`](#statica-http-requests-total) | counter | `host`, `method`, `status`, `outcome` |
| [`statica_http_request_duration_seconds`](#statica-http-request-duration-seconds) | histogram | `host`, `method` |
| [`statica_http_time_to_first_byte_seconds`](#statica-http-time-to-first-byte-seconds) | histogram | `host` |
| [`statica_http_response_bytes_total`](#statica-http-response-bytes-total) | counter | `host` |
| [`statica_http_requests_in_flight`](#statica-http-requests-in-flight) | gauge | none |
| [`statica_http_rejected_requests_total`](#statica-http-rejected-requests-total) | counter | `reason` |
| [`statica_stream_failures_total`](#statica-stream-failures-total) | counter | `host`, `reason` |
| [`statica_cache_requests_total`](#statica-cache-requests-total) | counter | `host`, `outcome` |
| [`statica_cache_response_bytes_total`](#statica-cache-response-bytes-total) | counter | `host` |
| [`statica_cache_enabled`](#statica-cache-enabled) | gauge | none |
| [`statica_cache_bytes`](#statica-cache-bytes) | gauge | none |
| [`statica_cache_retired_bytes`](#statica-cache-retired-bytes) | gauge | none |
| [`statica_cache_entries`](#statica-cache-entries) | gauge | none |
| [`statica_cache_fill_reserved_bytes`](#statica-cache-fill-reserved-bytes) | gauge | none |
| [`statica_cache_fills_in_flight`](#statica-cache-fills-in-flight) | gauge | none |
| [`statica_cache_waiters`](#statica-cache-waiters) | gauge | none |
| [`statica_cache_admissions_total`](#statica-cache-admissions-total) | counter | `result`, `reason` |
| [`statica_cache_evictions_total`](#statica-cache-evictions-total) | counter | `kind` |
| [`statica_cache_expirations_total`](#statica-cache-expirations-total) | counter | `kind` |
| [`statica_cache_revalidations_total`](#statica-cache-revalidations-total) | counter | `result` |
| [`statica_cache_fill_duration_seconds`](#statica-cache-fill-duration-seconds) | histogram | `result` |
| [`statica_cache_limit_bytes`](#statica-cache-limit-bytes) | gauge | none |
| [`statica_cache_limit_entries`](#statica-cache-limit-entries) | gauge | none |
| [`statica_cache_object_limit_bytes`](#statica-cache-object-limit-bytes) | gauge | none |
| [`statica_cache_fill_limit_bytes`](#statica-cache-fill-limit-bytes) | gauge | none |
| [`statica_cache_fill_limit_concurrency`](#statica-cache-fill-limit-concurrency) | gauge | none |
| [`statica_cache_waiter_limit_per_key`](#statica-cache-waiter-limit-per-key) | gauge | none |
| [`statica_origin_operations_total`](#statica-origin-operations-total) | counter | `operation`, `result`, `purpose` |
| [`statica_origin_attempts_total`](#statica-origin-attempts-total) | counter | `operation`, `result`, `purpose` |
| [`statica_origin_header_duration_seconds`](#statica-origin-header-duration-seconds) | histogram | `operation`, `purpose` |
| [`statica_origin_bytes_total`](#statica-origin-bytes-total) | counter | `purpose` |
| [`statica_ready`](#statica-ready) | gauge | none |
| [`statica_origin_health_enabled`](#statica-origin-health-enabled) | gauge | none |
| [`statica_origin_health_checks_total`](#statica-origin-health-checks-total) | counter | `result` |
| [`statica_origin_health_last_success_timestamp_seconds`](#statica-origin-health-last-success-timestamp-seconds) | gauge | none |
| [`statica_log_events_dropped_total`](#statica-log-events-dropped-total) | counter | none |
| [`statica_build_info`](#statica-build-info) | gauge | `version`, `revision`, `go_version` |

## Labels and series

Every label value is one of the fixed values listed on this page, except `host`, the HTTP `status` and the labels of `statica_build_info`. Paths, object keys, queries, IP addresses and request IDs never become label values. An unexpected empty value of a fixed label is exported as `unknown`; statica does not produce one in normal operation.

### `host`

The normalized hostname of an admitted request: lowercase, without port or trailing dot, and an IP address without brackets (`www.example.com`, `192.0.2.10`, `2001:db8::1`). Only admitted requests have a host: a rejected request counts in [`statica_http_rejected_requests_total`](#statica-http-rejected-requests-total) only, even when its host was valid.

With [`hosts`](./configuration.md#hosts), the values are bounded by the allowlist. Without it, every distinct hostname that clients send creates its own series, and statica never removes a series.

::: danger
Without `hosts`, a client can create new series by sending arbitrary `Host` headers, and the memory they use is never released until the process restarts. Set `hosts` when the instance is reachable by untrusted clients.
:::

The value `_unknown` labels an admitted request that has no host. It marks a defect: statica does not produce it in normal operation, and no hostname can take that value.

### When series exist

| Families | Series |
| --- | --- |
| `statica_build_info`, `statica_cache_enabled`, `statica_http_requests_in_flight`, `statica_ready`, `statica_origin_health_enabled`, `statica_origin_health_last_success_timestamp_seconds`, `statica_log_events_dropped_total` | One series each, from startup. |
| `statica_origin_health_checks_total` | Every `result` from startup, at zero while the origin health check is disabled. |
| The `statica_cache_*` families without a `host` label | Every series from startup, counters with every label value at zero, when the memory cache is enabled. |
| Families with a `host` label | Created by the first admitted request with that combination of labels. |
| `statica_http_rejected_requests_total` and the `statica_origin_*` families except the health ones | Created by the first observation with that combination of labels. |

With [`cache.enabled: false`](./configuration.md#cache-enabled), the only `statica_cache_*` series is `statica_cache_enabled 0`: `statica_cache_requests_total` and `statica_cache_response_bytes_total` have no series, since no request reaches a cache, and no other cache family is exported.

## HTTP

These families count requests on the public listener. A request rejected by Go's HTTP server before statica's handler, such as one whose headers exceed `server.max_header_bytes` plus Go's 4 KiB allowance, does not appear in any of them (see [HTTP responses](./http-responses.md#statuses-from-go-s-http-server)).

### `statica_http_requests_total`

**Type:** counter\
**Labels:** `host`, `method`, `status`, `outcome`

Admitted requests, counted once when their response ends. Admitted means not rejected: allowed CORS preflights and redirect rules count here.

| Label | Values |
| --- | --- |
| `host` | See [`host`](#host). |
| `method` | `GET`, `HEAD`, `OPTIONS` (CORS preflights), or `OTHER`. Other methods are rejected with 405, so `OTHER` does not occur in practice. |
| `status` | The status code sent, such as `200`, `304` or `404`. `0` when the request ended before a status was sent. |
| `outcome` | `success`: the response completed, whatever its status. `origin_failure`: the request failed before the response headers, because the origin failed, a deadline expired or a shutdown began (503); this includes a denial masked as 404. `incomplete`: the response did not complete, including a client that went away before the response headers (`status="0"`); see [`statica_stream_failures_total`](#statica-stream-failures-total). |

### `statica_http_request_duration_seconds`

**Type:** histogram\
**Labels:** `host`, `method`\
**Buckets:** 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600

Duration of admitted requests, from the moment statica's handler receives the request until the response ends or is aborted. Long downloads count with their full duration.

### `statica_http_time_to_first_byte_seconds`

**Type:** histogram\
**Labels:** `host`\
**Buckets:** 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30

Time from the moment statica's handler receives an admitted request until its status and headers are sent. Requests that ended before sending a status are not observed.

### `statica_http_response_bytes_total`

**Type:** counter\
**Labels:** `host`

Body bytes written to admitted requests, partial transfers included. Headers are not counted; `HEAD` requests and 304 responses write no body bytes.

### `statica_http_requests_in_flight`

**Type:** gauge\
**Labels:** none

Requests holding one of the `server.max_concurrent_requests` request slots, including requests that are about to be rejected by a later check.

### `statica_http_rejected_requests_total`

**Type:** counter\
**Labels:** `reason`

Requests that statica rejected before serving them, by reason. They have no `host` label and do not count in any other `statica_http_*` family except `statica_http_requests_in_flight`. The reason is also the `error_class` of the request's [`http_request`](./logs.md#http-request) event.

| `reason` | Status | Cause |
| --- | --- | --- |
| `request_capacity` | 503 | Every request slot is in use. |
| `malformed_host` | 400 | The `Host` is not a valid hostname or IP address. |
| `unknown_host` | 421 | The hostname is not in `hosts`. |
| `unsupported_method` | 405 | The method is not `GET`, `HEAD`, or `OPTIONS` with `cors` configured. |
| `request_body` | 400 | A `GET` or `HEAD` request carries a body. |
| `invalid_target` | 400, 404, 414 | The request target is invalid (400), names a hidden path (404) or is longer than 8 KiB (414). |
| `cors_preflight_denied` | 403 | With `cors` configured, an `OPTIONS` request whose `Origin` is not exactly one allowed value, that does not carry exactly one `Access-Control-Request-Method`, whose requested method is not `GET` or `HEAD`, or that asks for a header other than `Range`, `If-Match`, `If-None-Match`, `If-Modified-Since`, `If-Unmodified-Since` and `If-Range`. |
| `invalid_redirect_input` | 400, 414 | A redirect rule matched, but the request's values cannot be placed in its destination (400) or the `Location` would exceed 8 KiB (414). |
| `invalid_object_key` | 400, 414 | The object key would be invalid or longer than 1024 bytes (414). |

### `statica_stream_failures_total`

**Type:** counter\
**Labels:** `host`, `reason`

Admitted requests with `outcome="incomplete"`, by the reason the response did not complete. The values are the `termination_reason` values of the [access log](./logs.md#termination-reason) that can end a response: `total_deadline`, `client_write_timeout`, `client_write_failure`, `disconnect`, `shutdown`, `origin_read_timeout`, `origin_read_failure`, `checksum_mismatch` and `length_mismatch`. A request that failed inside statica without a termination reason counts as `handler_panic`, or as `response_aborted` for an abort without any other classification.

## Memory cache, per request

### `statica_cache_requests_total`

**Type:** counter\
**Labels:** `host`, `outcome`

Admitted requests whose response used a memory cache lookup, counted once per request by the lookup that decided the response. The outcomes partition these requests.

| `outcome` | Meaning |
| --- | --- |
| `hit` | A fresh body served from memory to a `GET`, ranges included. |
| `metadata_hit` | A `HEAD` answered from a fresh entry. A `HEAD` is never a `hit`. |
| `negative_hit` | A fresh negative entry: a confirmed missing object, without an origin read. |
| `miss` | An origin read for a missing or unusable entry, or its failure. |
| `revalidated` | A stored body that the origin confirmed unchanged. |
| `stale` | An expired entry served after an origin failure, within `cache.stale_if_error`. |
| `bypass` | An origin read that the cache neither reused nor stored. The access log's `cache_reason` says why. |
| `coalesced` | The request joined a shared fill that another request started, whatever the fill produced for it. |

`coalesced` is a metric value only: the access log shows the fill's outcome for the request in `cache` and adds `cache_coalesced=true`.

### `statica_cache_response_bytes_total`

**Type:** counter\
**Labels:** `host`

Body bytes written from a body already in memory: requests whose cache outcome is `hit`, `revalidated` or `stale`, whole or sliced for a range, including such requests that joined another request's fill. Bytes of a `miss` do not count, even for a request that joined another request's fill that stored the object, and neither do `HEAD` requests, 304 and 412 responses, generated bodies and streamed objects. Compare it with [`statica_http_response_bytes_total`](#statica-http-response-bytes-total) for a byte hit ratio.

## Memory cache, global

These families have no `host` label: one shared fill can serve requests for several hostnames. They exist only when the memory cache is enabled, except `statica_cache_enabled`. Gauges are read from the cache when Prometheus scrapes. [Memory cache](../architecture/memory-cache.md) explains the mechanisms behind them.

### `statica_cache_enabled`

**Type:** gauge\
**Labels:** none

`1` when the memory cache is enabled, `0` when `cache.enabled` is `false`.

### `statica_cache_bytes`

**Type:** gauge\
**Labels:** none

Bytes charged to cache entries: bodies, metadata and a fixed overhead per entry, for resident entries and for removed entries that responses still use. At most `cache.max_bytes`.

### `statica_cache_retired_bytes`

**Type:** gauge\
**Labels:** none

Bytes charged to entries that were replaced or removed but that responses still use. Part of `statica_cache_bytes`.

### `statica_cache_entries`

**Type:** gauge\
**Labels:** none

Entries charged to the cache, resident and retired, including metadata and negative entries. At most `cache.max_entries`.

### `statica_cache_fill_reserved_bytes`

**Type:** gauge\
**Labels:** none

Fill buffer capacity reserved by running shared fills and by bodies served without being stored. At most `cache.max_fill_bytes`.

### `statica_cache_fills_in_flight`

**Type:** gauge\
**Labels:** none

Running shared fills, including abandoned fills that are still unwinding. At most `cache.max_concurrent_fills`.

### `statica_cache_waiters`

**Type:** gauge\
**Labels:** none

Requests waiting on a shared fill.

### `statica_cache_admissions_total`

**Type:** counter\
**Labels:** `result`, `reason`

Outcomes of storing entries, and lookups that bypassed the cache. All 17 series exist from startup.

Each attempt to store an entry (body, metadata or negative) or to refresh one in place counts once under its result, with the reason `none`:

| `result` | Meaning |
| --- | --- |
| `admitted` | Stored under a key that had no entry. |
| `replaced` | Stored, replacing the key's entry. |
| `refreshed` | The entry's metadata was refreshed in place. |
| `too_large` | Not stored: larger than `cache.max_object_bytes`, or its charge exceeds `cache.max_bytes`. |
| `no_space` | Not stored: entries in use leave no room. |
| `invalid` | Not stored: the entry was malformed. |
| `conflict` | Not stored: the key's entry changed meanwhile. |

Each lookup that bypassed the cache counts once under the result `bypass`, with one of these reasons. A request whose resolution makes several lookups can count several times. The access log's [`cache_reason`](./logs.md#cache-reason) uses the same values, except `range`, which only this metric shows.

| `reason` | Meaning |
| --- | --- |
| `disabled` | A `cache.rules` entry disables the cache for the object. |
| `request_no_store` | The request carries `Cache-Control: no-store`. |
| `shutdown` | statica is shutting down and starts no new fills. |
| `fills_full` | `cache.max_concurrent_fills` fills are running. |
| `waiters_full` | The key's fill already has `cache.max_waiters_per_key` waiting requests. |
| `unknown_size` | A fill streamed the object: its length is unknown. |
| `too_large` | A fill streamed the object: it is larger than `cache.max_object_bytes`. |
| `uncacheable` | The origin's metadata does not allow storing the object (`no-store`, `private` or malformed cache metadata). |
| `fill_budget` | A fill streamed the object: other fills hold `cache.max_fill_bytes`. |
| `range` | A range or `If-Match` read found no fresh body in memory to cut from. |

### `statica_cache_evictions_total`

**Type:** counter\
**Labels:** `kind`

Entries evicted to make room, by entry kind: `body`, `metadata` or `negative`. Replacements and removals are not evictions. All three series exist from startup.

### `statica_cache_expirations_total`

**Type:** counter\
**Labels:** `kind`

Entries that a lookup found past their freshness lifetime, counted once per stored version, by entry kind: `body`, `metadata` or `negative`. Entries with a zero lifetime, such as origin `no-cache` or a zero TTL, are revalidated on every use and never count. All three series exist from startup.

### `statica_cache_revalidations_total`

**Type:** counter\
**Labels:** `result`

Conditional origin reads (`If-None-Match`) made by shared fills to revalidate a stale entry, by the origin's answer. Without [`s3.features.conditional_reads`](./configuration.md#s3-features-conditional-reads), or for an entry without a valid entity tag, statica reads the object again instead, and nothing is counted here. A `HEAD` that refreshes a stale entry in place is not counted either. All five series exist from startup.

| `result` | Meaning |
| --- | --- |
| `not_modified` | 304 with the stored entity tag: the stored bytes are confirmed. |
| `etag_changed` | 304 with another entity tag: statica reads the object again. |
| `modified` | 200: the object changed; it replaces the entry when it can be stored. |
| `missing` | The object no longer exists. |
| `error` | Any other failure. |

### `statica_cache_fill_duration_seconds`

**Type:** histogram\
**Labels:** `result`\
**Buckets:** 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30

Duration of each shared fill, from its start until its result is available to the waiting requests, by result. All ten series exist from startup.

| `result` | Meaning |
| --- | --- |
| `admitted` | A complete body was stored. |
| `detached` | A complete body was served without being stored, because of a conflict or a lack of room. |
| `refreshed` | A 304 refreshed the stored metadata in place. |
| `unstored` | A 304 confirmed the stored bytes, but the merged metadata was not stored. |
| `streamed` | The origin stream was handed to one waiting request instead of being stored. |
| `not_found` | The object does not exist. |
| `denied` | The origin denied access. |
| `timeout` | `cache.fill_timeout` or an origin timeout ended the fill. |
| `canceled` | Every waiting request left, or statica is shutting down. |
| `failed` | Any other origin or body failure. |

### `statica_cache_limit_bytes`

**Type:** gauge\
**Labels:** none

The configured `cache.max_bytes`.

### `statica_cache_limit_entries`

**Type:** gauge\
**Labels:** none

The configured `cache.max_entries`.

### `statica_cache_object_limit_bytes`

**Type:** gauge\
**Labels:** none

The configured `cache.max_object_bytes`.

### `statica_cache_fill_limit_bytes`

**Type:** gauge\
**Labels:** none

The configured `cache.max_fill_bytes`.

### `statica_cache_fill_limit_concurrency`

**Type:** gauge\
**Labels:** none

The configured `cache.max_concurrent_fills`.

### `statica_cache_waiter_limit_per_key`

**Type:** gauge\
**Labels:** none

The configured `cache.max_waiters_per_key`.

## Origin

These families describe the requests statica sends to the origin. They have no `host` label.

| Label | Values |
| --- | --- |
| `operation` | `get` or `head`. |
| `purpose` | `request`: on behalf of one public request. `cache_fill`: a shared fill or revalidation. `health`: an origin health check. |
| `result` | `success` or one of the origin error kinds below, except `checksum_mismatch`. |

| Error kind | Origin answer |
| --- | --- |
| `not_found` | 404 with the code `NoSuchKey` to a `GET`: the object does not exist. |
| `ambiguous_not_found` | Any other 404, including every 404 to a `HEAD`, which cannot say whether the object is missing. |
| `denied` | 403, including credential errors that S3 sends with 403, such as `InvalidAccessKeyId` and `SignatureDoesNotMatch`. |
| `configuration` | Another status with an error code that points at the bucket, the endpoint or the credentials: `NoSuchBucket`, `InvalidBucketName`, `AuthorizationHeaderMalformed`, `PermanentRedirect`, `IncorrectEndpoint`, `InvalidAccessKeyId` or `SignatureDoesNotMatch`. |
| `throttled` | 429, or the codes `SlowDown`, `Throttling` or `ThrottlingException`. |
| `timeout` | A deadline, a network timeout or the code `RequestTimeout`. |
| `canceled` | The read was canceled: the request that needed it went away, or statica is shutting down. |
| `not_modified` | 304 to a revalidation: the stored entry is current. |
| `precondition` | 412 to a range read guarded by the entity tag: the object changed. |
| `invalid_range` | 416. |
| `invalid_response` | A malformed or unexpected response. |
| `checksum_mismatch` | A checksum did not match, with `s3.features.checksums`. Checksums are verified once the body has been read, after the operation is counted, so this kind is never a `result` here: it appears as `termination_reason=checksum_mismatch` in the access log, or as `error_class=origin_checksum_mismatch` when a shared fill read the body before the response headers. |
| `unavailable` | Any other failure, such as a connection error, a 5xx response, an expired session token, or missing credentials, which fail before any request is sent. |

### `statica_origin_operations_total`

**Type:** counter\
**Labels:** `operation`, `result`, `purpose`

Logical origin reads, counted once when the call returns, by its final result, retries included. A read that statica refuses before any origin access, such as an invalid key, is not counted. Operations are attributed to whoever started them: a `GET` that a shared fill starts is a `cache_fill` operation, even when its stream is then handed to one request.

### `statica_origin_attempts_total`

**Type:** counter\
**Labels:** `operation`, `result`, `purpose`

HTTP round trips to the origin, each retry included, each by its own result. An operation that fails before reaching the network, for example on a credential or signing error, has no attempt, so operations can exceed attempts; retries make attempts exceed operations. Retries follow `s3.max_attempts`.

### `statica_origin_header_duration_seconds`

**Type:** histogram\
**Labels:** `operation`, `purpose`\
**Buckets:** 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30

Time from the start of a logical origin operation until its response headers or its failure, retries and backoff included.

### `statica_origin_bytes_total`

**Type:** counter\
**Labels:** `purpose`

Body bytes read from the origin, by the purpose of the reader. Bytes of a shared fill's stream that was handed to one request count as `request`, although the operation counts as `cache_fill`.

## Readiness and origin health

These series are read when Prometheus scrapes, so staleness of the origin health check is judged at scrape time.

### `statica_ready`

**Type:** gauge\
**Labels:** none

`1` when the instance is ready, the same decision as [`/readyz`](./admin-endpoints.md#readyz) at scrape time; otherwise `0`. The reason is never a label.

### `statica_origin_health_enabled`

**Type:** gauge\
**Labels:** none

`1` when [`admin.origin_health.enabled`](./configuration.md#admin-origin-health-enabled) is `true`, otherwise `0`.

### `statica_origin_health_checks_total`

**Type:** counter\
**Labels:** `result`

Completed origin health checks, by raw result. The result is the origin's own answer: [`errors.access_denied`](./configuration.md#errors-access-denied) does not turn `denied` into `not_found`. A check interrupted by the shutdown is not counted. All nine series exist from startup.

| `result` | Meaning |
| --- | --- |
| `success` | The object's metadata was read. |
| `not_found` | Never counted: the check uses `HEAD`, and a 404 to a `HEAD` is always `ambiguous_not_found`. The series exists and stays at 0. |
| `ambiguous_not_found` | The origin answered 404 to the `HEAD`. |
| `denied` | The origin answered 403. On AWS without `s3:ListBucket`, also a missing key. |
| `throttled` | The origin throttled the check. |
| `unavailable` | The origin could not be reached or answered with a server error. |
| `configuration` | The origin answered with an error code that points at the bucket, endpoint or credentials (see [Origin](#origin)). |
| `timeout` | `admin.origin_health.timeout` or an origin timeout. |
| `failed` | Any other error. |

### `statica_origin_health_last_success_timestamp_seconds`

**Type:** gauge\
**Labels:** none

Unix time, in seconds with a fractional part, at which the last successful check completed. `0` before the first success and while the check is disabled.

## Logs

### `statica_log_events_dropped_total`

**Type:** counter\
**Labels:** none

Log lines that statica dropped instead of writing them: the bounded output queue was full, writing to standard output failed, or the line came after the output closed at the end of a shutdown. See [Bounded output](./logs.md#bounded-output).

## Build

### `statica_build_info`

**Type:** gauge\
**Labels:** `version`, `revision`, `go_version`

Always `1`. The labels identify the binary, with the same values as [`statica version`](./cli.md#statica-version): `dev` and `unknown` when the version and revision were not set at build time.

```text
statica_build_info{go_version="go1.27.3",revision="unknown",version="dev"} 1
```

## Go and process metrics

`/metrics` also exports the standard collectors of the Prometheus Go client:

- `go_*`: the Go runtime, such as `go_goroutines`, `go_threads`, `go_info`, `go_gc_duration_seconds` and the `go_memstats_*` memory statistics;
- `process_*`: the process, such as `process_cpu_seconds_total`, `process_resident_memory_bytes`, `process_open_fds` and `process_start_time_seconds`. Which `process_*` families exist depends on the operating system.

There are no metrics about the scrapes themselves.
