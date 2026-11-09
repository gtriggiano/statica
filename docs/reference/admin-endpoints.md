# Admin endpoints reference

`statica serve` runs a second listener, the admin listener, for liveness, readiness and metrics. It binds [`admin.listen`](./configuration.md#admin-listen), `127.0.0.1:9090` by default, and serves cleartext HTTP/1.1: no TLS and no HTTP/2. [Health checks and readiness](../guide/health-checks.md) shows how to wire the probes, and [Monitor with Prometheus](../guide/metrics.md) how to scrape `/metrics`.

::: danger Keep the admin listener private
The admin endpoints have no authentication. Anyone who reaches them can read your metrics, including the hostnames of admitted requests and traffic volumes, and watch readiness. Bind `admin.listen` to loopback or to a private network, restrict access with your network controls, and never expose it to the internet.
:::

## Routes

| Path | Answer |
| --- | --- |
| [`/livez`](#livez) | Liveness: 200 while the process runs. |
| [`/readyz`](#readyz) | Readiness: 200 when ready, 503 with a reason otherwise. |
| [`/metrics`](#metrics) | Prometheus metrics. |

- **Methods.** Each route answers `GET` and `HEAD`. A `HEAD` gets the headers of the `GET` without the body.
- **Exact paths.** The path must match exactly as sent: `/livez/`, `/LIVEZ` and percent-encoded spellings such as `/%6Civez` are other paths. The query string is ignored.
- **404 Not Found** for any other path, whatever the method, with the body `not found`.
- **405 Method Not Allowed** for another method on one of the three paths, with `Allow: GET, HEAD` and the body `method not allowed`.
- **No caching.** Every response carries `Cache-Control: no-store`.

There is no other route: no index page, no debug or profiling endpoint.

## `/livez`

```text
HTTP/1.1 200 OK
Cache-Control: no-store
Content-Length: 3
Content-Type: text/plain; charset=utf-8
X-Content-Type-Options: nosniff
Date: Sat, 07 Nov 2026 23:47:41 GMT

ok
```

`/livez` answers 200 with the body `ok` for as long as the admin listener runs: during origin outages, while readiness is false and throughout a drain. It does not consult readiness, the origin health check or the origin.

## `/readyz`

`/readyz` answers 200 with the body `ready`, or 503 Service Unavailable with the body `not ready: <reason>`, as `text/plain; charset=utf-8` with `X-Content-Type-Options: nosniff`.

```text
not ready: origin_unknown
```

When several reasons apply, `/readyz` reports the first one of this table:

| Reason | When |
| --- | --- |
| `initializing` | Components are being built or the listeners bound. |
| `draining` | A shutdown has started. |
| `origin_unknown` | The origin health check is enabled and no check has succeeded yet. |
| `origin_unhealthy` | `admin.origin_health.failure_threshold` consecutive checks failed, and `success_threshold` consecutive successes have not followed yet. |
| `origin_stale` | The last successful check is older than `admin.origin_health.max_staleness`. |

Without [`admin.origin_health`](./configuration.md#admin-origin-health) enabled, only `initializing` and `draining` apply. The decision is made when `/readyz` is requested, so a check that stopped running cannot keep the instance ready. [`statica_ready`](./metrics.md#statica-ready) reports the same decision at scrape time. [Startup, readiness and shutdown](../architecture/lifecycle.md#readiness-and-liveness) explains the states and their timing.

## `/metrics`

`/metrics` serves every metric in the Prometheus exposition format, negotiated from the request's `Accept` header: the text format by default, or the protobuf format when the scraper asks for it. OpenMetrics is not offered. The response is compressed with gzip when `Accept-Encoding` allows it, and carries `Vary: Accept, Accept-Encoding`.

```text
HTTP/1.1 200 OK
Cache-Control: no-store
Content-Type: text/plain; version=0.0.4; charset=utf-8; escaping=underscores
Vary: Accept
Vary: Accept-Encoding
```

If the metrics cannot be gathered, the endpoint answers 500 Internal Server Error. The [metrics reference](./metrics.md) lists every family.

## No origin traffic

Probes and scrapes read in-memory state only. They never contact the origin, so a client of the admin listener cannot cause S3 requests or costs. The optional origin health check runs in the background on its own schedule, whatever the probes do.

Requests to the admin listener produce no `http_request` event and count in no `statica_http_*` metric. A failure inside the admin handler logs [`admin_http_error`](./logs.md#admin-http-error).

## Fixed limits

The admin listener has its own limits, independent of the public listener's and not configurable, so that probes keep answering while the public listener is at capacity:

| Limit | Value |
| --- | --- |
| Reading the request headers | 5 s |
| Reading the whole request | 10 s |
| Writing the response | 10 s |
| Idle keep-alive connection | 10 s |
| Request header size | 8 KiB plus Go's 4 KiB allowance, request line included; larger headers get 431 from Go's HTTP server |
| Open connections | 16; further clients wait in the operating system's backlog |
| Shutdown | At most 5 s, within `server.shutdown_timeout` |

## During startup and shutdown

- **Startup.** statica binds the public listener, then the admin listener, and starts answering admin requests only once readiness has become serving. Probes therefore do not normally see `initializing`. With the origin health check enabled, `/readyz` reports `origin_unknown` until the first check succeeds.
- **Shutdown.** When a shutdown starts, `/readyz` answers 503 with `not ready: draining` while `/livez` still answers 200 and `/metrics` keeps working. The admin listener stays up while public requests drain and stops at the end of the shutdown, after them, so orchestrators can watch the drain. A second signal closes it at once. See [Shutdown](../architecture/lifecycle.md#shutdown).
