# Work with the logs

Collect statica's logs, choose what they contain, and read the access and lifecycle events to find out what happened to a request or to the process.

## Prerequisites

- A collector for the standard output of `statica serve`: your supervisor's journal, a container log driver or a log shipper.
- A parser for logfmt, the `key=value` format of the events. Most log pipelines and query tools have one.

## How statica writes logs

- **Standard output, one event per line.** Every line is one logfmt event that starts with `ts`, `level` and `event`, followed by the event's fields in a fixed order. Optional fields are left out when they do not apply ([format](../reference/logs.md#format)).
- **Never multi-line.** Values that contain spaces, `=`, quotes or control characters are quoted and escaped, so a hostile URL cannot break a line or forge an event. Each value is cut to 16 KiB. Use a logfmt parser rather than splitting on spaces: `path` and `authority` can contain quoted text.
- **Standard error.** When a command fails, it prints one fixed line on standard error, such as `statica: cannot bind the public listener`. Collect it too: configuration errors appear only there.
- **A bounded queue.** Events go through a queue of 4096 lines and 4 MiB. When standard output cannot keep up, statica drops new lines instead of slowing requests, and counts them ([dropped events](#dropped-events)).

The [log reference](../reference/logs.md) lists every event and field.

## Choose what to log

```yaml [config.yaml]
observability:
  log_level: info
  access_log: true
  log_client_ip: true
```

- [`observability.log_level`](../reference/configuration.md#observability-log-level) (`info`) sets the minimum level written, for every event, access events included. An `http_request` event's level follows the request ([levels](../reference/logs.md#http-request)):
  - `info` for completed responses, 404s for missing objects and redirects included;
  - `warn` for rejections; for requests that the client ended or a shutdown canceled, before or after the headers, such as a 504 recorded for a client that left during preparation or a 503 for a shutdown; and for transfers that `server.request_timeout` cut after the headers;
  - `error` for origin failures and expired deadlines before the headers, a masked denial included, and for transfers that the origin ended.

  At `warn`, you keep failures but lose the access events of successful requests and the `info` lifecycle events, such as `startup`, `shutdown_started` and the recovery of the origin health check. At `error`, `log_events_dropped` reports are not written either.
- [`observability.access_log`](../reference/configuration.md#observability-access-log) (`true`): `false` stops the `http_request` events only. The other events and every metric continue.
- [`observability.log_client_ip`](../reference/configuration.md#observability-log-client-ip) (`true`): `false` leaves `client_ip`, `peer_ip`, `client_ip_source` and `client_ip_reason` out of the access events.

## Keep secrets and personal data out

The `path` field logs the request path with its query string. The values of the query parameters named in [`observability.redact_query_params`](../reference/configuration.md#observability-redact-query-params) become `[REDACTED]`. Names are compared without ASCII case, after decoding.

Your list replaces the default list. To add your own names, repeat the defaults:

```yaml [config.yaml]
observability:
  redact_query_params:
    - token
    - access_token
    - api_key
    - password
    - secret
    - signature
    - X-Amz-Credential
    - X-Amz-Signature
    - X-Amz-Security-Token
    - session_id
    - email
```

::: warning Paths are never redacted
Redaction applies to query values only. A URL that carries personal data or a secret in its path, such as `/invite/<token>/` or `/users/jane.doe@example.com`, is logged as sent. Keep such data out of URLs, or treat the logs as personal data: restrict access and set a retention period.
:::

Request headers, cookies, redirect destinations, credentials and origin error messages are never logged ([what is never logged](../reference/logs.md#what-is-never-logged)).

## Read an access event

One `http_request` event is written when each public request ends:

```text
ts=2026-11-08T09:12:44.512Z level=info event=http_request request_id=lb-7c1e2f request_id_source=proxy host=www.example.com method=GET path="/guide/?utm_source=news&token=[REDACTED]" status=200 outcome=success resolution=index cache=hit origin_key=releases/2026-11-01/guide/index.html duration_ms=0.214 ttfb_ms=0.102 bytes_written=5120 client_ip=203.0.113.7 peer_ip=10.0.12.5 client_ip_source=x_forwarded_for
```

| Field | What it tells you |
| --- | --- |
| `request_id`, `request_id_source` | The ID also sent in the `X-Request-ID` response header, from your proxy (`proxy`) or `generated`. Search for it in your proxy's logs. |
| `host` or `authority` | `host` is the normalized hostname of an admitted request. `authority` is the raw `Host` of a request rejected before host admission, such as an unknown host or a request over capacity. |
| `path` | The original path and query, before index or SPA resolution. Absent when the request target was invalid or hidden. |
| `status`, `outcome` | `status` is what the client received; `outcome` is how the request ended: `success`, `origin_failure` or `incomplete`. They can disagree: a 404 can be a `success` (the object is missing) or an `origin_failure` (a masked denial). |
| `resolution` | Which candidate answered: `ordinary`, `index`, `spa` or `not_found`. |
| `cache`, `cache_reason`, `cache_coalesced` | The memory cache lookup that decided the response: `hit`, `miss`, `revalidated`, `stale`, `bypass` with its reason, and so on. `cache_coalesced=true` means the request joined another request's fill. |
| `origin_key` | The full object key, publication prefix included, when the response carried an object's headers. Absent for generated responses. |
| `duration_ms`, `ttfb_ms`, `bytes_written` | Total time, time to the response headers, and body bytes written, partial transfers included. |
| `error_class` | Why a request was rejected or failed before its headers, such as `unknown_host`, `origin_denied` or `origin_timeout`. |
| `termination_reason` | Why a response was cut or failed: a deadline, the origin, the client or a shutdown. |
| `client_ip`, `peer_ip`, `client_ip_source`, `client_ip_reason` | The attributed client and the connection's peer. See [Run behind a reverse proxy](./behind-a-proxy.md#verify-it). |

The [field values](../reference/logs.md#field-values) section of the reference lists every value of these fields.

### Incomplete transfers

A response that was cut after its headers keeps its status and has `outcome=incomplete`, the `bytes_written` it sent and a `termination_reason`:

```text
ts=2026-11-08T00:17:01.874Z level=warn event=http_request request_id=ef7c683798fe57dc2d80ac50706722f6 request_id_source=generated host=www.example.com method=GET path=/downloads/archive.zip status=200 outcome=incomplete resolution=ordinary cache=bypass cache_reason=too_large origin_key=downloads/archive.zip duration_ms=4525.686 ttfb_ms=188.623 bytes_written=7106878 termination_reason=shutdown client_ip=203.0.113.7 peer_ip=203.0.113.7 client_ip_source=peer
```

`client_write_timeout`, `client_write_failure` and `disconnect` point at the client or the network, at `warn`. `origin_read_timeout`, `origin_read_failure`, `checksum_mismatch` and `length_mismatch` (the origin sent a body of another length than it announced) point at the origin, at `error`. `total_deadline` and `shutdown` are statica's own limits, at `warn` ([after the headers](../architecture/timeouts.md#after-the-headers)).

### A masked denial or a real 404

With the default [`errors.access_denied: not_found`](../reference/configuration.md#errors-access-denied), an origin 403 Forbidden reaches visitors as 404 Not Found. The access events tell the two apart:

```text
ts=2026-11-08T00:17:50.101Z level=info event=http_request request_id=1d9e0c4b7a2f48e6b5c3d1a0f9e8d7c6 request_id_source=generated host=www.example.com method=GET path=/missing.html status=404 outcome=success resolution=not_found cache=miss duration_ms=12.683 ttfb_ms=12.627 bytes_written=10 client_ip=203.0.113.7 peer_ip=203.0.113.7 client_ip_source=peer
ts=2026-11-08T00:17:50.240Z level=error event=http_request request_id=6a5b4c3d2e1f40a9b8c7d6e5f4a3b2c1 request_id_source=generated host=www.example.com method=GET path=/private.html status=404 outcome=origin_failure cache=miss duration_ms=1.033 ttfb_ms=0.988 bytes_written=10 error_class=origin_denied termination_reason=origin_failure client_ip=203.0.113.7 peer_ip=203.0.113.7 client_ip_source=peer
```

| | Missing object | Masked denial |
| --- | --- | --- |
| Level | `info` | `error` |
| `outcome` | `success` | `origin_failure` |
| `resolution` | `not_found` | absent |
| `error_class` | absent | `origin_denied` |
| `cache` | can be `negative_hit` | never `negative_hit` |
| `Vary: Accept-Encoding` in the response | present | absent |

A burst of masked denials usually means wrong credentials, a bucket policy that denies reads, or, on AWS S3, a missing `s3:ListBucket` permission, which turns every missing object into a 403 ([access denied](../architecture/access-denied.md#aws-without-s3-listbucket)).

## Watch the lifecycle events

| Event | Level | What it means |
| --- | --- | --- |
| `startup` | `info` | Both listeners are bound. It reports the settings in force, never the bucket, the prefix or credentials. |
| `origin_tls_verification_disabled` | `warn` | `s3.insecure_skip_verify` is `true`. |
| `startup_failed` | `error` | A component or a bind failed; `stage` says which. The process exits with status 1. |
| `startup_aborted` | `info` | A signal stopped the startup. |
| `shutdown_started`, `shutdown_complete` | `info`, or `warn` for `outcome=forced` | A shutdown and how it ended. |
| `listener_failed` | `error` | A listener stopped unexpectedly; a shutdown follows and the exit status is 1. |
| `origin_health_changed` | `error` when unhealthy, `info` when healthy | The origin health check changed state. |
| `origin_health_check_failed` | `warn` | The origin health check is failing before its first success, once per change of result. |
| `log_events_dropped` | `warn` | Log lines were dropped. |
| `http_server_error`, `admin_http_error` | `warn` | A request failed inside statica's handler (`error_class=handler_panic`). |

A log that ends without `shutdown_complete` means that the process ended without a graceful shutdown, for example by SIGKILL, SIGHUP or a closed standard output ([signals](./deployment.md#signals)).

## Dropped events

statica drops a line instead of writing it when the queue is full, when writing to standard output fails, or when the line comes after the output closed at the end of a shutdown ([bounded output](../reference/logs.md#bounded-output)):

```text
ts=2026-11-08T10:02:11.004Z level=warn event=log_events_dropped dropped=1532 dropped_total=1532
```

- `log_events_dropped` is written once lines can be written again, at most every 10 seconds, with the drops since the previous report.
- [`statica_log_events_dropped_total`](../reference/metrics.md#statica-log-events-dropped-total) counts every dropped line, even when the report is filtered out by `log_level`.
- After a second stop signal, statica gives the logs at most 250 ms, and the access events of the requests that the signal cut can be lost.

Drops that happen after statica's write, in a collector or a journal with its own rate limit, are not counted: compare the volume your pipeline receives with `statica_http_requests_total` when in doubt.

## Example queries

Expressed in terms of fields, for whatever query language your log system uses:

| Question | Filter | Group by |
| --- | --- | --- |
| Which origin failures happen? | `event=http_request`, `outcome=origin_failure` | `error_class`, `status` |
| Are denials hidden as 404? | `error_class=origin_denied` | `path` |
| Why are transfers cut? | `outcome=incomplete` | `termination_reason` |
| What did the proxy send? | `client_ip_source=peer` and a `client_ip_reason` | `client_ip_reason`, `peer_ip` |
| Which requests are rejected? | `event=http_request` without `outcome` | `error_class` |
| How is one path served? | `path` | `cache`, `status` |
| What happened to one request? | `request_id=<id>` | |
| What happened to the process? | `event` other than `http_request` | `event` |

For a quick look at a journal or a file, `grep` on whole fields works, although a logfmt parser is exact:

```sh
journalctl -u statica -o cat | grep 'event=http_request' | grep ' outcome=origin_failure '
```

## Pitfalls

- **`status` alone hides failures.** A masked denial is a 404, and a cut download is a 200. Watch `outcome`, `error_class` and `termination_reason`.
- **No access events for some requests.** Requests that Go's HTTP server refuses before statica sees them, such as headers larger than `server.max_header_bytes`, and requests to the admin listener, are not logged ([statuses from Go's HTTP server](../reference/http-responses.md#statuses-from-go-s-http-server)).
- **The log reader must stay up.** When standard output is a pipe whose reader goes away, the next write ends statica ([signals](./deployment.md#signals)).

## Related pages

- [Log reference](../reference/logs.md): every event, field and value.
- [Safe logging](../architecture/security.md#safe-logging).
- [Monitor with Prometheus](./metrics.md).
- [Troubleshooting](./troubleshooting.md).
