# Log reference

`statica serve` writes its logs to standard output as logfmt events, one event per line. This page describes the line format, the levels, every event with its fields, and the fixed values that fields take. The messages that commands print on standard error are described in the [CLI reference](./cli.md#output-streams). [Work with the logs](../guide/logs.md) shows how to choose what to log, read an access event and query the logs.

## Format

A request, captured from `statica serve` with [`local-s3.yaml`](./examples.md#local-s3-compatible-server):

```text
ts=2026-11-07T23:56:06.073Z level=info event=http_request request_id=005f9d3e5c6ea8a6b00fd6c4fd2e78d0 request_id_source=generated host=www.example.com method=GET path="/guide/?utm_source=news&token=[REDACTED]" status=200 outcome=success resolution=index cache=miss origin_key=demo/guide/index.html duration_ms=33.694 ttfb_ms=33.487 bytes_written=36 client_ip=127.0.0.1 peer_ip=127.0.0.1 client_ip_source=peer
```

- **One event per line.** Each line is one logfmt record of `key=value` pairs, ended by a newline.
- **Fixed start.** Every line starts with `ts`, `level` and `event`, followed by the event's fields in the order this page lists them. Optional fields are left out when they do not apply.
- **Time.** `ts` is the event time in UTC, RFC 3339 with milliseconds: `2026-11-07T23:56:06.073Z`.
- **Values.** A value that contains a space, `=`, `"`, a control character or invalid UTF-8 is quoted and escaped, so a value can never start a new line or a new event. C1 control characters, the Unicode line and paragraph separators and invalid UTF-8 become U+FFFD. Each value is cut to 16 KiB at a character boundary. Keys contain only printable ASCII other than `=` and `"`, and each key appears once per line.
- **Numbers and flags.** Counts and byte sizes are integers (`cache_max_bytes=134217728`), booleans are `true` or `false`, and fields named `*_ms` are milliseconds with up to three decimals (`duration_ms=33.694`).
- **Durations.** Configuration durations use the Go form: `30s`, `1m0s`, `5m0s`, `0s`.

## Levels

Each event has a level: `debug`, `info`, `warn` or `error`. [`observability.log_level`](./configuration.md#observability-log-level) sets the minimum level written, `info` by default. It applies to every event, request events included. statica writes no `debug` events.

Most events always have the same level. `http_request`, `startup_aborted`, `shutdown_complete` and `origin_health_changed` take theirs from what happened, as described with each event.

## Events

| Event | Level | When |
| --- | --- | --- |
| [`startup`](#startup) | `info` | Both listeners are bound and statica is serving. |
| [`origin_tls_verification_disabled`](#origin-tls-verification-disabled) | `warn` | After `startup`, when `s3.insecure_skip_verify` is `true`. |
| [`startup_failed`](#startup-failed) | `error` | A component could not be built or a listener could not be bound. |
| [`startup_aborted`](#startup-aborted) | `info` | A signal arrived before both listeners were bound. |
| [`listener_failed`](#listener-failed) | `error` | A listener stopped unexpectedly. |
| [`shutdown_started`](#shutdown-started) | `info` | A shutdown begins. |
| [`shutdown_complete`](#shutdown-complete) | `info` or `warn` | The shutdown ends, before the remaining logs are flushed. |
| [`http_request`](#http-request) | `info`, `warn` or `error` | A public request ends. |
| [`http_server_error`](#http-server-error) | `warn` | A public request failed inside statica's handler. |
| [`admin_http_error`](#admin-http-error) | `warn` | An admin request failed inside statica's handler. |
| [`origin_health_changed`](#origin-health-changed) | `info` or `error` | The origin health state changed. |
| [`origin_health_check_failed`](#origin-health-check-failed) | `warn` | An origin health check failed before the first success. |
| [`log_events_dropped`](#log-events-dropped) | `warn` | Log lines were dropped since the previous report. |

### `startup`

Logged once, when both listeners are bound and readiness has become serving. It reports the operating mode and the budgets, never the bucket, the prefix, the endpoint URL, host names, header values or credentials. Captured from `statica serve` with [`local-s3.yaml`](./examples.md#local-s3-compatible-server):

```text
ts=2026-11-07T23:56:04.545Z level=info event=startup version=dev revision=unknown go_version=go1.27.3 public_listen=127.0.0.1:8080 admin_listen=127.0.0.1:9090 tls=off hosts=any trusted_proxies=0 read_header_timeout=5s response_header_timeout=30s write_idle_timeout=1m0s idle_timeout=1m0s request_timeout=0s shutdown_timeout=30s max_connections=1024 max_concurrent_requests=256 max_header_bytes=32768 s3_endpoint=custom s3_region=us-east-1 s3_path_style=true s3_allow_http=true s3_insecure_skip_verify=false s3_conditional_reads=true s3_range_reads=true s3_checksums=false s3_max_attempts=2 s3_response_header_timeout=10s s3_read_idle_timeout=30s cache=on cache_max_bytes=134217728 cache_max_entries=10000 cache_max_object_bytes=2097152 cache_max_fill_bytes=33554432 cache_max_concurrent_fills=16 cache_max_waiters_per_key=64 cache_fill_timeout=30s cache_ttl=5m0s cache_negative_ttl=5s cache_stale_if_error=0s cache_rules=0 compression=off spa=off origin_health=off access_log=true log_client_ip=true log_level=info
```

| Field | Value |
| --- | --- |
| `version`, `revision`, `go_version` | The build identity, as [`statica version`](./cli.md#statica-version) prints it. |
| `public_listen`, `admin_listen` | The addresses actually bound. |
| `tls` | `on` or `off`. |
| `tls_min_version` | `1.2` or `1.3`. Only with `tls=on`. |
| `hosts` | `allowlist` when `hosts` is set, otherwise `any`. |
| `hosts_count` | The number of `hosts` entries. Only with `hosts=allowlist`. |
| `trusted_proxies` | The number of `server.trusted_proxies` entries. |
| `read_header_timeout`, `response_header_timeout`, `write_idle_timeout`, `idle_timeout`, `request_timeout`, `shutdown_timeout` | The `server` durations. |
| `max_connections`, `max_concurrent_requests`, `max_header_bytes` | The `server` limits. |
| `s3_endpoint` | `aws`, or `custom` when `s3.endpoint` is set. |
| `s3_region` | The region in use, discovered when `s3.region` is omitted. |
| `s3_path_style`, `s3_allow_http`, `s3_insecure_skip_verify` | `true` or `false`. |
| `s3_conditional_reads`, `s3_range_reads`, `s3_checksums` | The `s3.features` values. |
| `s3_max_attempts`, `s3_response_header_timeout`, `s3_read_idle_timeout` | The `s3` limits. |
| `cache` | `on` or `off`. |
| `cache_max_bytes`, `cache_max_entries`, `cache_max_object_bytes`, `cache_max_fill_bytes`, `cache_max_concurrent_fills`, `cache_max_waiters_per_key`, `cache_fill_timeout`, `cache_ttl`, `cache_negative_ttl`, `cache_stale_if_error` | The `cache` settings, sizes in bytes. Only with `cache=on`. |
| `cache_rules` | The number of `cache.rules`. Only with `cache=on`. |
| `compression` | `off` or `precompressed`. |
| `compression_encodings` | The encodings in order, comma-separated: `br,gzip`. Only with `compression=precompressed`. |
| `spa` | `on` when `spa` is configured, otherwise `off`. |
| `origin_health` | `on` when `admin.origin_health.enabled` is `true`, otherwise `off`. |
| `origin_health_interval`, `origin_health_timeout` | Only with `origin_health=on`. |
| `access_log`, `log_client_ip`, `log_level` | The `observability` settings. |

### `origin_tls_verification_disabled`

Level `warn`, no fields. Logged right after `startup` when [`s3.insecure_skip_verify`](./configuration.md#s3-insecure-skip-verify) is `true`.

### `startup_failed`

Level `error`. A component could not be built, or a listener could not be bound. Nothing stays bound, and `statica serve` exits with status 1 after printing a fixed message on standard error (see [CLI reference](./cli.md#startup)).

| Field | Value |
| --- | --- |
| `stage` | What failed: `metrics`, `origin_client`, `health_client`, `cache`, `content`, `public_handler`, `public_server`, `health_worker`, `admin_server`, `bind_public` or `bind_admin`. |
| `signal_pending` | `true` when a shutdown signal also arrived. Absent otherwise. |

```text
ts=2026-11-07T23:48:31.335Z level=error event=startup_failed stage=bind_admin
```

### `startup_aborted`

A shutdown signal arrived before both listeners were bound, and no failure occurred. Nothing stays bound, and `statica serve` exits with status 0.

| Field | Value |
| --- | --- |
| `reason` | `signal`, at level `info`. The value `context`, at level `warn`, exists for embedding and is not produced by `statica serve`. |

### `listener_failed`

Level `error`. A listener stopped accepting connections unexpectedly. While serving, this starts a shutdown with `reason=listener_failure`, and `statica serve` exits with status 1. During a shutdown, the event is logged and the exit status still follows the cause of the shutdown.

| Field | Value |
| --- | --- |
| `listener` | `public` or `admin`. |

### `shutdown_started`

Level `info`. A shutdown begins: from now on `/readyz` answers 503.

| Field | Value |
| --- | --- |
| `reason` | `signal` or `listener_failure`. The value `context` exists for embedding and is not produced by `statica serve`. |
| `shutdown_timeout` | The shutdown budget, `server.shutdown_timeout`. |

### `shutdown_complete`

The last event of a shutdown, at level `info` when every request drained and `warn` when the drain was cut short or did not take place. [Startup, readiness and shutdown](../architecture/lifecycle.md#shutdown) describes the sequence.

| Field | Value |
| --- | --- |
| `reason` | The `reason` of `shutdown_started`. |
| `outcome` | `drained`, or `forced` when the drain was cut short or did not take place. |
| `forced_by` | `budget`: the drain reached the end of its share of `server.shutdown_timeout`. `signal`: another signal arrived during the shutdown. `listener_failure`: the public listener failed, which closes every public connection at once, so nothing drained. `error`: the drain ended with an error other than its deadline. Only with `outcome=forced`. |
| `cancelled_requests` | The number of requests still active when the drain was cut. Only with `outcome=forced`; a best-effort count after a listener failure. |
| `duration_ms` | The duration of the shutdown. |

```text
ts=2026-11-07T23:56:12.455Z level=info event=shutdown_started reason=signal shutdown_timeout=30s
ts=2026-11-07T23:56:12.458Z level=info event=shutdown_complete reason=signal outcome=drained duration_ms=2.665
```

### `http_request`

The access event: one per public request handled by statica, rejected requests included, logged when the response ends. Requests that Go's HTTP server refuses before statica sees them, such as headers over `server.max_header_bytes` plus Go's 4 KiB allowance, are not logged (see [HTTP responses](./http-responses.md#statuses-from-go-s-http-server)). Requests to the admin listener are not logged. [`observability.access_log: false`](./configuration.md#observability-access-log) turns this event off without affecting the others or the metrics.

The fields, in order:

| Field | Present | Value |
| --- | --- | --- |
| `request_id` | Always | The request ID, also sent in the `X-Request-ID` response header. |
| `request_id_source` | Always | See [`request_id_source`](#request-id-source). |
| `request_id_reason` | When the peer is a trusted proxy whose request ID was not used | See [`request_id_reason`](#request-id-reason). |
| `host` | Once the `Host` passed host admission | The normalized hostname. |
| `authority` | Instead of `host`, when the request never passed host admission and its `Host` is not empty | The raw `Host` value, cut to 256 bytes. |
| `method` | Always | The request method, cut to 32 bytes. |
| `path` | When the request target is valid | The original path and query, before index or SPA resolution, with redacted query values. See [Query redaction](#query-redaction). |
| `query_redaction_failed` | Only `true` | The query could not be decoded and was left out of `path`. |
| `path_truncated` | Only `true` | `path` was cut to 8 KiB. |
| `status` | Always | The status code sent, `0` when none was sent. |
| `outcome` | Admitted requests | See [`outcome`](#outcome). Rejected requests have none. |
| `resolution` | When an object, a trailing-slash redirect or a 404 for a missing object was selected, including for a 416 and a 406; absent for failures | See [`resolution`](#resolution). |
| `cache` | When a memory cache lookup decided the response | See [`cache`](#cache). |
| `cache_reason` | With `cache=bypass` | See [`cache_reason`](#cache-reason). |
| `cache_coalesced` | Only `true` | The request joined a shared fill that another request started. |
| `origin_key` | When object headers were sent | The full object key, publication prefix included, including for `HEAD`, 304, 412, 206 and interrupted transfers. |
| `duration_ms` | Always | From the moment statica's handler received the request until the response ended. |
| `ttfb_ms` | When a status was sent | Until the status and headers were sent. |
| `bytes_written` | Always | Body bytes written, partial transfers included. |
| `error_class` | Rejections, origin failures and handler failures | See [`error_class`](#error-class). |
| `termination_reason` | When a deadline, a failure or the client ended the response | See [`termination_reason`](#termination-reason). |
| `cache_diagnostic` | When the origin's cache metadata was malformed | See [`cache_diagnostic`](#cache-diagnostic). |
| `client_ip` | With `log_client_ip`, when the peer address is known | The attributed client IP address. |
| `peer_ip` | With `log_client_ip`, when the peer address is known | The address of the connection's peer. |
| `client_ip_source` | With `log_client_ip` | See [`client_ip_source`](#client-ip-source). |
| `client_ip_reason` | With `log_client_ip`, when a forwarded address was not used | See [`client_ip_reason`](#client-ip-reason). |

The target is validated before it is logged: a request whose target is invalid, including a hidden path, has no `path` field. A request rejected before target validation, for example for its `Host`, has a `path` only when its target is valid.

The level depends on the request. The first rule that matches decides:

1. `warn` when `termination_reason` is `disconnect`, `client_write_timeout`, `client_write_failure` or `shutdown`, even after an origin failure;
2. `error` for `outcome=origin_failure`, including a denial masked as 404, and for `error_class=handler_panic`;
3. for `outcome=incomplete`: `error` when the origin ended the transfer (`origin_read_timeout`, `origin_read_failure`, `checksum_mismatch`, `length_mismatch`), `warn` otherwise;
4. `warn` for rejected requests and for any other request with a `termination_reason`;
5. `info` for everything else, including redirects and 404 responses for missing objects.

### `http_server_error`

Level `warn`. A public request failed inside statica's handler, and Go's HTTP server reported it. The request's `http_request` event, with `error_class=handler_panic`, comes first. Go's other server messages, such as TLS handshake errors, are discarded.

| Field | Value |
| --- | --- |
| `error_class` | `handler_panic`. |

### `admin_http_error`

Level `warn`. An admin request, for example a scrape of `/metrics`, failed inside statica's handler. Go's other server messages are discarded.

| Field | Value |
| --- | --- |
| `error_class` | `handler_panic`. |

### `origin_health_changed`

The [origin health check](./configuration.md#admin-origin-health) changed state: `info` when the origin becomes healthy, `error` when it becomes unhealthy.

| Field | Value |
| --- | --- |
| `state` | The new state: `healthy` or `unhealthy`. |
| `previous` | The previous state: `unknown` (no success yet), `healthy` or `unhealthy`. |
| `result` | The result of the check that caused the change. See [Origin health results](#origin-health-results). |

```text
ts=2026-11-07T23:48:12.826Z level=error event=origin_health_changed state=unhealthy previous=healthy result=unavailable
ts=2026-11-07T23:48:16.796Z level=info event=origin_health_changed state=healthy previous=unhealthy result=success
```

### `origin_health_check_failed`

Level `warn`. Before the first successful check, the origin stays `unknown` and readiness reports `origin_unknown`. Each failed check whose result differs from the last one logged produces this event, so an origin that never becomes healthy is explained once per change of result rather than at every interval.

| Field | Value |
| --- | --- |
| `state` | `unknown`. |
| `result` | The failed result. See [Origin health results](#origin-health-results). |

### `log_events_dropped`

Level `warn`. Log lines were dropped since the previous report. See [Bounded output](#bounded-output).

| Field | Value |
| --- | --- |
| `dropped` | Lines dropped since the previous report. |
| `dropped_total` | Lines dropped since statica started. |

## Field values

The fields of `http_request` that have a fixed set of values.

### `request_id_source`

| Value | Meaning |
| --- | --- |
| `proxy` | The request ID came from a trusted proxy, in `server.request_id_header`. |
| `generated` | statica generated the request ID: 32 hexadecimal characters. |

### `request_id_reason`

Only for requests whose peer is in `server.trusted_proxies`.

| Value | Meaning |
| --- | --- |
| `missing_request_id` | The request had no `server.request_id_header`. |
| `invalid_request_id` | The header was repeated, or its value was not 1 to 128 characters of `A-Z`, `a-z`, `0-9`, `.`, `_`, `:` and `-`. |

### `client_ip_source`

| Value | Meaning |
| --- | --- |
| `peer` | The client IP is the address of the connection's peer. |
| `x_forwarded_for` | Taken from `X-Forwarded-For`, sent by a trusted proxy. |
| `x_real_ip` | Taken from `X-Real-IP`, sent by a trusted proxy. |

### `client_ip_reason`

Why statica fell back to the peer address. [Security model](../architecture/security.md#trusted-proxies-and-client-ip-attribution) describes the attribution rules.

| Value | Meaning |
| --- | --- |
| `invalid_peer` | The peer address could not be parsed; `client_ip` and `peer_ip` are absent. |
| `missing_client_ip_header` | A trusted proxy sent no `server.client_ip_header`. |
| `invalid_real_ip` | `X-Real-IP` was repeated or is not one IP address. |
| `forwarded_chain_too_long` | `X-Forwarded-For` holds more than `server.max_forwarded_hops` addresses, or more than 64 bytes per allowed hop. |
| `invalid_forwarded_chain` | `X-Forwarded-For` contains something other than IP addresses. |

### `outcome`

Admitted requests only.

| Value | Meaning |
| --- | --- |
| `success` | The response completed, whatever its status, including redirects and 404 for a missing object. |
| `origin_failure` | The request failed before the response headers: the origin failed, a deadline expired, or a shutdown began (503). It includes a denial masked as 404. `termination_reason` tells which. |
| `incomplete` | The response did not complete: it was cut after the headers, or the client went away. A client that goes away before the headers receives nothing and is recorded with `status=0`. |

### `resolution`

How statica selected the object, independently of the status. [Request flow](../architecture/request-flow.md#from-path-to-object) describes the order.

| Value | Meaning |
| --- | --- |
| `ordinary` | The object at the exact key. |
| `index` | The `index` object of a path ending in `/`, or a 308 redirect that adds the slash. |
| `spa` | The SPA shell, `spa.fallback`. |
| `not_found` | A 404 for a missing object: the `not_found` object or the built-in page. |

### `cache`

The outcome of the memory cache lookup that decided the response. Absent with `cache.enabled: false`.

| Value | Meaning |
| --- | --- |
| `hit` | A fresh body served from memory. |
| `metadata_hit` | A `HEAD` answered from memory. |
| `negative_hit` | A fresh negative entry: the object is missing, without an origin read. |
| `miss` | An origin read for a missing or unusable entry, or its failure. |
| `revalidated` | Stored bytes that the origin confirmed unchanged. |
| `stale` | An expired entry served after an origin failure, within `cache.stale_if_error`. |
| `bypass` | An origin read that the cache neither reused nor stored; see `cache_reason`. |

### `cache_reason`

Only with `cache=bypass`: `disabled`, `request_no_store`, `shutdown`, `fills_full`, `waiters_full`, `unknown_size`, `too_large`, `uncacheable` or `fill_budget`. The [metrics reference](./metrics.md#statica-cache-admissions-total) describes each one. The `cache` fields describe the lookup that selected the object, so a range read from the origin that follows it is not reflected: the bypass reason `range` appears only in the metric.

### `error_class`

| Value | Meaning |
| --- | --- |
| A rejection reason | For a rejected request: `request_capacity`, `malformed_host`, `unknown_host`, `unsupported_method`, `request_body`, `invalid_target`, `cors_preflight_denied`, `invalid_redirect_input` or `invalid_object_key`. See [`statica_http_rejected_requests_total`](./metrics.md#statica-http-rejected-requests-total). |
| `origin_` and an origin error kind | For an origin failure: `origin_` followed by an [origin `result`](./metrics.md#origin) other than `success`, such as `origin_denied`, `origin_unavailable`, `origin_throttled`, `origin_timeout` or `origin_invalid_response`. |
| `origin_inconsistent` | The object kept changing during a range read guarded by its entity tag (503). |
| `origin_timeout` | Also whenever a deadline, not the origin, ended the request before the headers (504). |
| `origin_canceled` | A shutdown canceled the request before the headers. |
| `origin_error` | Any other failure before the headers. |
| `handler_panic` | The request failed inside statica's handler. |

A 504 with `termination_reason=preparation_timeout` or `total_deadline` always has `error_class=origin_timeout`, wherever the deadline expired.

A response that is cut after its headers has no `error_class`: its `termination_reason` tells why. Neither has a client that went away before the headers: it is not an origin failure.

### `termination_reason`

| Value | Meaning |
| --- | --- |
| `preparation_timeout` | `server.response_header_timeout` expired, or an origin read timed out, before the response headers (504). |
| `total_deadline` | `server.request_timeout` expired, before the headers (504) or during the transfer. |
| `origin_failure` | The origin failed before the response headers (404 for a masked denial, 502 or 503). |
| `origin_read_timeout` | The origin sent no body bytes for `s3.read_idle_timeout` during the transfer. |
| `origin_read_failure` | Reading the body from the origin failed during the transfer. |
| `checksum_mismatch` | The body's checksum did not match, with `s3.features.checksums`. |
| `length_mismatch` | The origin sent more or fewer bytes than its declared length. |
| `client_write_timeout` | The client accepted no data for `server.write_idle_timeout`. |
| `client_write_failure` | Writing to the client failed. |
| `disconnect` | The client went away, before the response headers (`status=0`) or during the transfer. |
| `shutdown` | A shutdown canceled the request. |

[Timeouts and long downloads](../architecture/timeouts.md#before-and-after-the-response-headers) explains which reasons occur before and after the headers.

### `cache_diagnostic`

| Value | Meaning |
| --- | --- |
| `malformed_origin_cache_metadata` | The object's `Cache-Control`, `Expires`, `Date` or `Age` metadata is malformed, so the response carries `Cache-Control: no-store`. Reported only when `cache_control` leaves the origin's policy in effect. |

## Origin health results

The `result` of `origin_health_changed` and `origin_health_check_failed`, also the `result` label of [`statica_origin_health_checks_total`](./metrics.md#statica-origin-health-checks-total): `success`, `ambiguous_not_found`, `denied`, `throttled`, `unavailable`, `configuration`, `timeout` or `failed`. The result is the origin's raw answer: `errors.access_denied` does not turn `denied` into `not_found`. The check reads metadata with `HEAD`, and a 404 to a `HEAD` is always `ambiguous_not_found`, so the result `not_found` does not occur.

## Query redaction

The `path` field keeps the query string, with the values of sensitive parameters replaced:

- The parameters are those listed in [`observability.redact_query_params`](./configuration.md#observability-redact-query-params).
- Only `&` separates parameters; `;` is ordinary data.
- A parameter's name is decoded once, with percent-escapes and `+` as a space, and compared with the listed names ignoring ASCII case.
- The value of a matching parameter that has `=` becomes `[REDACTED]`; a name without `=` is kept. Everything else stays byte for byte as sent.
- When a name or value cannot be decoded into text without control characters, the query is left out of `path` and `query_redaction_failed=true` is added. This also applies when the list is empty.
- After redaction, `path` is cut to 8 KiB, with `path_truncated=true`.

Redaction affects the log only. Responses, redirects and origin requests are not changed.

## Client addresses

With [`observability.log_client_ip`](./configuration.md#observability-log-client-ip) set to `true`, the default, `http_request` carries `client_ip`, `peer_ip`, `client_ip_source` and `client_ip_reason`. With `false`, these four fields are left out; the request ID and the other fields are unchanged.

## Bounded output

Events go through a bounded queue to standard output, so a slow or blocked output never delays requests:

- The queue holds up to 4096 lines and 4 MiB. When it is full, new lines are dropped.
- A line that cannot be written to standard output, and a line produced after the output closed at the end of a shutdown, is dropped too.
- Every dropped line counts in [`statica_log_events_dropped_total`](./metrics.md#statica-log-events-dropped-total).
- After lines were dropped, statica logs `log_events_dropped` with the drops since the previous report, at most once every 10 seconds: after a successful write or on a 10-second tick, and once more when the output closes. With `observability.log_level: error` the report is not written, but the metric still counts.

During a shutdown, the remaining lines are written before statica exits, within `server.shutdown_timeout`. After a second signal, statica waits at most 250 ms for them, and the `http_request` events of the requests that the signal cut short can be lost.

## What is never logged

- Request headers, such as `Cookie`, `Authorization`, `Referer` and `User-Agent`, and response headers.
- Redirect destinations (`Location`).
- Origin error messages and response bodies, credentials, the bucket name and the endpoint.
- The text of Go's HTTP server errors, such as TLS handshake errors. Only a handler failure becomes an event, `http_server_error` or `admin_http_error`, without any of its text.
- Requests refused by Go's HTTP server before statica's handler, and requests to the admin listener.
