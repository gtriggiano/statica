# Health checks and readiness

Wire your orchestrator's or load balancer's probes to statica, and decide whether an instance's readiness should follow the health of its origin.

## Prerequisites

- The admin listener bound to an address that your prober reaches, and kept private ([the admin listener](../architecture/security.md#the-admin-listener)).
- For the origin health check: an object under the publication prefix that always exists and that statica's credentials can read.

## Liveness and readiness

The admin listener answers two probes from in-memory state. Neither contacts the origin, so probing as often as you like causes no S3 traffic.

| Probe | Answer | Meaning |
| --- | --- | --- |
| [`/livez`](../reference/admin-endpoints.md#livez) | 200 `ok` | The process runs. It stays 200 during origin outages, while not ready and throughout a drain. |
| [`/readyz`](../reference/admin-endpoints.md#readyz) | 200 `ready`, or 503 `not ready: <reason>` | Whether the instance should receive traffic. |

`/readyz` reports one reason at a time, the first that applies:

| Reason | When |
| --- | --- |
| `draining` | A shutdown has started. |
| `origin_unknown` | The origin health check is enabled and has not succeeded yet. |
| `origin_unhealthy` | The origin health check failed `failure_threshold` times in a row and has not recovered. |
| `origin_stale` | The last successful origin health check is older than `max_staleness`. |

One more reason, `initializing`, exists but probes do not normally see it: the admin listener starts answering only once both listeners are bound. Without the origin health check, an instance is ready from the moment it binds its listeners until a shutdown starts.

Readiness does not gate the public listener. A not-ready instance keeps serving every request it receives, from its memory cache when it can; readiness only tells your orchestrator or load balancer where to send traffic ([readiness does not gate public requests](../architecture/lifecycle.md#readiness-does-not-gate-public-requests)).

## Wire the probes

- Point liveness probes at `/livez` and readiness probes at `/readyz`, on the admin listener. Paths match exactly: `/livez/` is a 404.
- Both answer `GET` and `HEAD`, with `Cache-Control: no-store`.
- Never use `/readyz` for liveness: an origin outage, or a long drain, would make your supervisor restart instances that are working.
- The admin listener accepts at most 16 connections and has its own short time limits, independent of the public listener, so probes keep answering while the public listener is at capacity ([fixed limits](../reference/admin-endpoints.md#fixed-limits)).

[Deploy, roll out and shut down](./deployment.md#kubernetes) shows a Kubernetes fragment with both probes. A load balancer that can reach only the public listener can check an ordinary page instead, but that check is logged and counted like any visitor request.

## Decide whether readiness follows the origin

Without [`admin.origin_health`](../reference/configuration.md#admin-origin-health), readiness follows the process alone. statica does not check its credentials or its bucket at startup, so an instance with wrong credentials becomes ready and then fails the requests that need the origin.

With the origin health check enabled, a background worker reads the metadata of one object at a regular interval, and readiness follows its results.

| Enable it when | Leave it off when |
| --- | --- |
| A new instance should not receive traffic until it has proved that it can read the bucket, for example to stop a rollout with wrong credentials or a wrong prefix. | The instances have nowhere else to send traffic, and serving what the memory caches hold during an origin outage is better than serving nothing. |
| Your load balancer or DNS can send traffic to another site or region when an origin fails. | Your load balancer removes every target when all of them are unready. |

::: warning An origin outage makes every instance unready
Instances that share an origin see its outage at about the same time. With the origin health check enabled, they all become unready together, and your orchestrator removes them from traffic, even though their memory caches could still answer requests for fresh objects. What happens next depends on your load balancer: some keep sending traffic to unready targets when none is ready, others send none.
:::

## Configure the check

```yaml [config.yaml]
admin:
  listen: 10.0.12.20:9090
  origin_health:
    enabled: true
    key: health/ok.txt
```

- [`enabled: true`](../reference/configuration.md#admin-origin-health-enabled) is required. A block without it is validated but has no effect, apart from the key that `statica config check-origin` reads.
- [`key`](../reference/configuration.md#admin-origin-health-key) is relative to `s3.prefix`. It defaults to the value of `index`, the site's root page.

### Choose the key

Choose a small object that always exists under the publication prefix and that no publication removes, such as `health/ok.txt`. If you publish each release under its own prefix, include it in every release ([Publish a site to S3](./publishing.md)).

- The check sends one `HEAD` request: it reads the object's metadata, never its body, and never uses the memory cache. So no `cache.rules` entry is needed for the key.
- A missing key never counts as healthy. On providers that answer 404, and on AWS with `s3:ListBucket`, S3 answers a `HEAD` for a missing object with a 404 that cannot say whether the object is missing, so the result is `ambiguous_not_found`, never `not_found`. On AWS without `s3:ListBucket`, the answer is 403 and the result is `denied` ([the `s3:ListBucket` trade-off](./s3-providers.md#the-s3-listbucket-trade-off)).
- The result is the origin's raw answer. A 403 Forbidden is `denied`, even though visitors receive 404 Not Found with `errors.access_denied: not_found`.
- The check uses the same credentials as the site. A key that a bucket policy denies, but that visitors never request, makes readiness fail while the site works.

### Choose the thresholds

| Setting (default) | Role |
| --- | --- |
| [`interval`](../reference/configuration.md#admin-origin-health-interval) (`30s`) | Time from the end of one check to the start of the next, less a random jitter of up to 10%. |
| [`timeout`](../reference/configuration.md#admin-origin-health-timeout) (`3s`) | Deadline of one check, less than `interval`. One attempt, whatever `s3.max_attempts` says. |
| [`failure_threshold`](../reference/configuration.md#admin-origin-health-failure-threshold) (`3`) | Consecutive failures that make a healthy origin unhealthy. |
| [`success_threshold`](../reference/configuration.md#admin-origin-health-success-threshold) (`1`) | Consecutive successes that make an unhealthy origin healthy again. The first success after startup is enough, whatever this value. |
| [`max_staleness`](../reference/configuration.md#admin-origin-health-max-staleness) (`90s`) | Age of the last success beyond which readiness reports `origin_stale`. Must exceed `interval` plus `timeout`. |

The timing that follows from them ([timing with the defaults](../architecture/lifecycle.md#timing-with-the-defaults)):

- **At startup**, the first check runs as soon as both listeners are bound. A healthy origin makes the instance ready within that check's duration, at most `timeout`. If it fails, the next check starts about one `interval` later.
- **When the origin breaks**, readiness turns false at the earlier of two moments: the `failure_threshold`-th consecutive failure, about `failure_threshold` × `interval` after the last success, and `max_staleness` after the last success. So readiness is false at most `max_staleness` after the last success: 90 s with the defaults.
- **When the origin recovers**, the next check succeeds within one `interval` plus one `timeout`, and `success_threshold` consecutive successes make the instance ready again.

Your prober's own period and failure count add to these times. A shorter `interval` detects failures sooner but sends more `HEAD` requests to your bucket: one per instance per interval, counted as `purpose="health"` in the origin metrics.

## Verify it

The `startup` event reports the check:

```text
ts=2026-11-08T00:17:32.951Z level=info event=startup ... origin_health=on origin_health_interval=30s origin_health_timeout=3s ...
```

Probe readiness:

```sh
curl -sS -w '%{http_code}\n' http://10.0.12.20:9090/readyz
```

The command prints the body, then the status:

```text
ready
200
```

When the check fails before its first success, readiness stays `origin_unknown` and the worker logs the result, once per change of result:

```text
not ready: origin_unknown
503
```

```text
ts=2026-11-08T00:17:17.749Z level=warn event=origin_health_check_failed state=unknown result=ambiguous_not_found
```

State changes are logged as `origin_health_changed`, at `error` level when the origin becomes unhealthy:

```text
ts=2026-11-08T00:17:32.960Z level=info event=origin_health_changed state=healthy previous=unknown result=success
ts=2026-11-08T00:17:36.901Z level=error event=origin_health_changed state=unhealthy previous=healthy result=denied
ts=2026-11-08T00:17:40.699Z level=info event=origin_health_changed state=healthy previous=unhealthy result=success
```

The metrics report the same state at scrape time:

```text
statica_ready 1
statica_origin_health_enabled 1
statica_origin_health_checks_total{result="success"} 42
statica_origin_health_last_success_timestamp_seconds 1.794097939081934e+09
```

`statica config check-origin` reads the same key with the same credentials, and explains a failure in one line, such as `origin check failed: access denied` ([`config check-origin`](../reference/cli.md#statica-config-check-origin)).

| Result | Likely cause |
| --- | --- |
| `ambiguous_not_found` | On providers that answer 404, and on AWS with `s3:ListBucket`: the key does not exist under the publication prefix, or the bucket does not exist. A 404 to a `HEAD` says neither; `statica config check-origin` tells them apart. |
| `denied` | The credentials, or a bucket policy, deny the read; or, on AWS without `s3:ListBucket`, the key does not exist. |
| `configuration` | The origin named the bucket, endpoint or credentials in an error code. Rare: an answer to a `HEAD` has no body to carry an error code. |
| `unavailable` | The origin is unreachable or answers with server errors, credentials are missing, or the region or endpoint is wrong. |
| `throttled` | The origin throttles requests. |
| `timeout` | No answer within `timeout`. |
| `failed` | Any other error, such as an invalid response: metadata with control characters or an invalid `Last-Modified`. |

## Pitfalls

- **A block without `enabled: true`.** The `startup` event says `origin_health=off` and `statica_origin_health_enabled` is 0.
- **A missing key.** Readiness never becomes ready: `origin_unknown` forever, with `result=ambiguous_not_found` on providers that answer 404 and on AWS with `s3:ListBucket`, or `result=denied` on AWS without it.
- **Probes on loopback.** A prober on another host or in another container cannot reach the default `127.0.0.1:9090`.
- **Draining looks like a failure.** `/readyz` answers 503 `not ready: draining` as soon as a shutdown starts, while the instance still finishes its requests.

## Related pages

- [Startup, readiness and shutdown](../architecture/lifecycle.md#readiness-and-liveness): readiness phases, origin health states and their timing.
- [Admin endpoints reference](../reference/admin-endpoints.md).
- [Origin health results](../reference/logs.md#origin-health-results).
- [Deploy, roll out and shut down](./deployment.md).
- [Monitor with Prometheus](./metrics.md#readiness-and-origin-health).
- [Troubleshooting](./troubleshooting.md#readiness-stays-not-ready): readiness that stays not ready.
