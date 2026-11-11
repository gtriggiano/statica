# Monitor with Prometheus

Scrape **statica**'s metrics with Prometheus and query the signals that show traffic, failures, cache behavior, origin health and resource use.

statica ships no dashboards and no alert rules. This page shows queries and names the signals worth watching; it sets no thresholds.

## Prerequisites

- A Prometheus server, or another scraper of the Prometheus format, that can reach statica's admin listener.
- The admin listener bound to an address that the scraper reaches and that is otherwise private.

## Expose the admin listener privately

statica serves its metrics on the admin listener, at `/metrics`, next to `/livez` and `/readyz`. Bind [`admin.listen`](../reference/configuration.md#admin-listen) to a private address:

```yaml [config.yaml]
admin:
  listen: 10.0.12.20:9090
```

::: danger The admin listener has no authentication
Anyone who reaches it can read your metrics, including the hostnames of admitted requests, traffic volumes and the exact build version. Allow only your scrapers and probes to reach it ([the admin listener](../architecture/security.md#the-admin-listener)).
:::

A scrape reads in-memory state only: it never contacts the origin, so scrapes cause no S3 requests or costs.

## Scrape it

An example scrape configuration, to adapt:

```yaml [prometheus.yml]
scrape_configs:
  - job_name: statica
    static_configs:
      - targets:
          - 10.0.12.20:9090
          - 10.0.12.21:9090
```

The default metrics path, `/metrics`, is the one statica serves. In Kubernetes or another dynamic environment, use your usual service discovery to find the admin port of each instance.

## Check the scrape

```sh
curl -sS http://10.0.12.20:9090/metrics | grep -E '^statica_(build_info|ready|cache_enabled)[ {]'
```

```text
statica_build_info{go_version="go1.27.3",revision="unknown",version="dev"} 1
statica_cache_enabled 1
statica_ready 1
```

In Prometheus, `up{job="statica"}` is 1 for every instance that it scrapes successfully.

## Useful queries

The queries use a 5-minute rate window; choose yours. The [metrics reference](../reference/metrics.md) lists every family, label and value. Some series appear only after their first observation: a `host` label value after the first admitted request for that host, a rejection reason after the first rejection, an origin result after the first origin answer with it. A missing series means that nothing was counted since the process started.

### Traffic

```text
sum by (host, status) (rate(statica_http_requests_total[5m]))
sum by (outcome) (rate(statica_http_requests_total[5m]))
sum by (reason) (rate(statica_http_rejected_requests_total[5m]))
sum by (host) (rate(statica_http_response_bytes_total[5m]))
```

`statica_http_requests_total` counts admitted requests when their response ends. Rejected requests, such as an unknown host (421) or a full set of request slots (503), count only in `statica_http_rejected_requests_total`, by `reason`, without a host.

### Failures that the status hides

```text
sum by (status) (rate(statica_http_requests_total{outcome="origin_failure"}[5m]))
sum by (purpose, result) (rate(statica_origin_operations_total{result=~"denied|configuration|throttled|timeout|unavailable|invalid_response|checksum_mismatch"}[5m]))
sum by (reason) (rate(statica_stream_failures_total[5m]))
```

- `outcome="origin_failure"` counts the error responses decided before the headers ([`statica_http_requests_total`](../reference/metrics.md#statica-http-requests-total)): origin failures, including an origin 403 masked as 404 by `errors.access_denied: not_found`, and expired deadlines. It also counts a request that a shutdown canceled, recorded as 503. A 5xx rate alone misses the masked denials ([why status-only monitoring misses it](../architecture/access-denied.md#why-status-only-monitoring-misses-it)), and a 503 needs its `termination_reason` in the access log to tell the origin from a shutdown.
- `statica_origin_operations_total` by `result` tells what the origin answered, and `purpose` whether the read served a request (`request`), a shared fill (`cache_fill`) or the origin health check (`health`). `not_found` and `ambiguous_not_found` are left out above, since missing objects are normal.
- `statica_stream_failures_total` counts responses cut after their headers, and clients that left before them (`disconnect`, with `status="0"` in `statica_http_requests_total`), by reason: the client (`disconnect`, `client_write_timeout`, `client_write_failure`), the origin (`origin_read_timeout`, `origin_read_failure`, `checksum_mismatch`, `length_mismatch`) or statica's limits (`total_deadline`, `shutdown`).

### Latency

```text
histogram_quantile(0.99, sum by (le) (rate(statica_http_time_to_first_byte_seconds_bucket[5m])))
histogram_quantile(0.99, sum by (le, purpose) (rate(statica_origin_header_duration_seconds_bucket[5m])))
histogram_quantile(0.5, sum by (le, method) (rate(statica_http_request_duration_seconds_bucket[5m])))
```

Prefer time to first byte for responsiveness: request duration includes the whole transfer, so long downloads dominate it. Origin header duration includes retries and backoff. A quantile is only as precise as the [histogram buckets](../reference/metrics.md#statica-http-request-duration-seconds).

### Memory cache

```text
sum by (outcome) (rate(statica_cache_requests_total[5m]))
```

This splits the requests that used the memory cache by outcome: `hit`, `miss`, `revalidated`, `stale`, `bypass` and the others. [Tune the memory cache](./cache-tuning.md#metrics-to-watch) has the queries for hit ratios, fills, revalidations, bypasses, evictions and the limits, and what each signal means. With `cache.enabled: false`, only `statica_cache_enabled 0` exists.

### Capacity

```text
statica_http_requests_in_flight
rate(statica_http_rejected_requests_total{reason="request_capacity"}[5m])
```

`statica_http_requests_in_flight` counts the requests holding one of the [`server.max_concurrent_requests`](../reference/configuration.md#server-max-concurrent-requests) slots. No metric exports that configured limit: compare with your configuration. Long downloads hold their slot for their whole duration. The memory cache's own limits, such as concurrent fills, are covered in [Tune the memory cache](./cache-tuning.md#usage-against-the-limits).

### Readiness and origin health

```text
statica_ready
time() - statica_origin_health_last_success_timestamp_seconds and statica_origin_health_last_success_timestamp_seconds > 0
sum by (result) (increase(statica_origin_health_checks_total[15m]))
```

`statica_ready` makes the same decision as `/readyz` at scrape time. The second query is the age of the last successful origin health check; the timestamp is 0 before the first success and while the check is disabled (`statica_origin_health_enabled 0`). [Health checks and readiness](./health-checks.md) explains the results.

### Logs, process and build

```text
increase(statica_log_events_dropped_total[15m])
process_resident_memory_bytes
changes(process_start_time_seconds[1h])
count by (version, revision) (statica_build_info)
```

- Dropped log lines mean that the access log is incomplete ([dropped events](./logs.md#dropped-events)).
- Compare resident memory with your memory limit and with the cache's budgets: the process needs memory beyond them ([leave room for the rest of the process](./cache-tuning.md#leave-room-for-the-rest-of-the-process)).
- `changes(process_start_time_seconds[1h])` counts the restarts of a process that keeps its `instance` label, for example a systemd service; a replaced pod or machine appears as a new instance instead. Each restart empties the memory cache. `process_*` families depend on the operating system.
- `statica_build_info` shows which build every instance runs, during a rollout for example.

## Host-label cardinality

The `host` label of the request metrics takes every hostname that statica admits. Without [`hosts`](../reference/configuration.md#hosts), a client can create new series by sending arbitrary `Host` headers, and those series stay until the process restarts ([`host`](../reference/metrics.md#host)). Count the values in use:

```text
count(count by (host) (statica_http_requests_total))
```

Set `hosts` whenever untrusted clients can reach the public listener. Rejected requests never add a host series.

## Signals worth watching

statica does not ship alert rules, and the right thresholds depend on your traffic. These signals are worth watching:

- Increases of `statica_http_requests_total{outcome="origin_failure"}` with `status="404"`, which are denied reads masked as missing objects, or with `status="502"`. For 503, check the `termination_reason` of the access events first: a shutdown also produces them.
- Origin operations with `result="denied"`, `"configuration"` or `"unavailable"`: credentials, bucket, endpoint or region problems.
- `statica_ready` at 0 outside rollouts, and a growing age of the last successful origin health check.
- Rejections with `reason="request_capacity"`: every request slot is in use.
- `statica_stream_failures_total` by reason, against its usual level.
- `statica_log_events_dropped_total` increasing: the logs are incomplete.
- New values of the `host` label, when `hosts` is not set.
- Resident memory approaching your memory limit.

## Pitfalls

- **Admin listener on loopback.** The default `127.0.0.1:9090` is unreachable from another host or container.
- **Rates across restarts.** Counters restart from zero with the process; `rate` and `increase` handle the reset, but the memory cache restarts empty too.
- **No metrics for some requests.** Requests that Go's HTTP server refuses before statica's handler and requests to the admin listener are not counted.

## Related pages

- [Metrics reference](../reference/metrics.md): every family, label and value.
- [Admin endpoints reference](../reference/admin-endpoints.md#metrics).
- [Tune the memory cache](./cache-tuning.md).
- [Health checks and readiness](./health-checks.md).
- [Work with the logs](./logs.md).
- [Troubleshooting](./troubleshooting.md): from a symptom to its cause.
