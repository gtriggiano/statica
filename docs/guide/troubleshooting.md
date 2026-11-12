# Troubleshooting

Find the cause of a problem from its symptom: each section lists the likely causes, how to confirm which one applies, and how to fix it.

| Symptom | Section |
| --- | --- |
| statica does not start | [Exit status 2](#statica-exits-with-status-2-at-startup) · [`startup_failed`, exit status 1](#startup-failed-exit-status-1) |
| A status that visitors get | [400](#_400-bad-request) · [404 for every URL](#every-url-answers-404) · [406](#_406-not-acceptable) · [421](#_421-misdirected-request) · [502](#_502-bad-gateway) · [503](#_503-service-unavailable) · [504](#_504-gateway-timeout) |
| An instance or pod is not ready, or keeps restarting | [Readiness stays not ready](#readiness-stays-not-ready) · [On Kubernetes](#on-kubernetes) |
| Slow or wrong content | [Downloads are slow or interrupted](#downloads-are-slow-or-interrupted) · [Origin traffic or latency grows with the load](#origin-traffic-or-latency-grows-with-the-load) · [Old content after publishing](#old-content-after-publishing) |
| A feature does not apply | [Single-page-app routes answer 404](#single-page-app-routes-answer-404) · [Precompressed variants are not served](#precompressed-variants-are-not-served) |
| Missing logs | [Log lines are missing](#log-lines-are-missing) |

## Where to look

- **Standard error** carries the one-line reason when a command fails: `statica: <message>`.
- **The access log** has one `http_request` event per public request. Its `outcome`, `error_class` and `termination_reason` fields explain failures that the status code hides ([Work with the logs](./logs.md)).
- **The metrics** on the admin listener count the same outcomes, plus the origin's answers by `result` ([Monitor with Prometheus](./metrics.md)).
- **`statica config validate`** checks the configuration offline, and **`statica config check-origin`** reads one object with the same credentials, region and endpoint that `serve` uses ([CLI reference](../reference/cli.md#statica-config-check-origin)).

## statica exits with status 2 at startup

**Likely cause.** The configuration file is missing or invalid, or the TLS files cannot be loaded. Nothing started.

**Confirm.** Run `statica config validate --config <file>`. The message names the field and its position, never its value ([error messages](../reference/configuration.md#error-messages)):

```text
statica: server.tls.min_version (line 8, column 18): expected a string
```

| Message | Usual cause |
| --- | --- |
| `configuration file: cannot open` | Wrong path, or no `config.yaml` in the working directory when neither `--config` nor `STATICA_CONFIG` is set. |
| `<field>: unknown field` | A misspelled key. |
| `<field>: expected a string` | An unquoted value that YAML reads as a number, such as `min_version: 1.3`. |
| `<field>: expected a string with explicit units` | A duration without units, such as `ttl: 300` instead of `ttl: 5m`. |
| `<field>: expected an integral size with KiB, MiB or GiB units` | A byte size such as `512MB` instead of `512MiB`. |
| `<field>: null is not allowed; omit the field to use its default` | A key without a value. |
| `<field>: missing environment variable <NAME>` | A `${NAME}` substitution whose variable is not set. |
| `server.tls: cannot load a matching certificate and private key` | A missing TLS file, or a key that does not match the first certificate of the chain ([Serve HTTPS directly](./tls.md#validate-the-files)). |

**Fix.** Correct the field, run `config validate` again, and do not let your supervisor restart **statica** in a loop on status 2.

## `startup_failed`, exit status 1

**Likely cause.** The configuration is valid, but a component could not be built or a listener could not be bound. The event names the `stage`, and standard error holds a fixed message:

```text
ts=2026-11-08T00:18:28.002Z level=error event=startup_failed stage=bind_public
statica: cannot bind the public listener
```

| `stage` | Usual cause | Confirm and fix |
| --- | --- | --- |
| `bind_public`, `bind_admin` | The port is in use, the address does not belong to the host, or the port needs privileges (below 1024 on most systems). | Check which process holds the port. Bind to an address of this host, or give the process the right to bind low ports ([binding port 443](./tls.md#binding-port-443)). |
| `origin_client`, `health_client` | The AWS configuration could not be loaded: no region found, a discovered region with characters other than letters, digits, `-` and `_`, a named profile that does not exist, or a timeout of `server.response_header_timeout`. | Run `statica config check-origin`: it reports `origin check failed: origin configuration`, or `origin check timed out`. Set `s3.region` or `AWS_REGION`, and check the profile. |
| Any other stage | An internal failure while building a component. | Report it with the `stage`. |

[`startup_failed`](../reference/logs.md#startup-failed) describes every stage.

`statica config validate` cannot catch these failures: it is offline, so it neither resolves the region nor binds anything. It does not check credentials either, and neither does `serve`: an instance with wrong credentials starts, then answers as in [Every URL answers 404](#every-url-answers-404) or [502 Bad Gateway](#_502-bad-gateway). Run `statica config check-origin` with the same file and environment as `serve` before a rollout.

## 400 Bad Request

**Likely causes.** statica rejected the request before reading the origin. The access event, at `warn`, has no `outcome` and names the reason in `error_class`, which [`statica_http_rejected_requests_total`](../reference/metrics.md#statica-http-rejected-requests-total) counts by `reason`:

| `error_class` | Cause |
| --- | --- |
| `malformed_host` | The `Host` is not a valid hostname or IP address. Hostnames may contain only letters, digits, `-` and `.`: an internal name with an underscore, such as one that a proxy sets, is rejected. The access event shows the raw value in `authority`. |
| `request_body` | A `GET` or `HEAD` request carries a body. |
| `invalid_target` | The path or query is malformed: a bad percent-escape, an encoded `/` or `\`, a `.` or `..` segment, an empty segment, or a double encoding ([target validation](../architecture/request-flow.md#target-validation)). A target longer than 8 KiB gets 414 with the same reason. The event has no `path`. |
| `invalid_redirect_input` | A redirect rule matched, but the request's values cannot be placed in its destination; 414 when the `Location` would exceed 8 KiB. |
| `invalid_object_key` | The object key would be invalid; 414 when it would exceed 1024 bytes. |

Go's HTTP server also answers some malformed requests with 400 before statica sees them, such as a request without `Host`. Those have no access event and no `X-Request-ID` ([statuses from Go's HTTP server](../reference/http-responses.md#statuses-from-go-s-http-server)).

**Fix.** Correct the client or the proxy: pass a valid public `Host`, send no body with `GET`, and do not rewrite or double-encode paths.

## Every URL answers 404

**Likely causes.**

1. **Access is denied, and masked.** With the default `errors.access_denied: not_found`, an origin 403 Forbidden becomes a 404. Wrong credentials and a bucket policy that denies reads both answer 403, so every URL answers 404 ([what counts as a denial](../architecture/access-denied.md#what-counts-as-a-denial)).
2. **The publication prefix is wrong.** The objects are not where statica looks for them, and every lookup finds nothing.
3. **On AWS S3, `s3:ListBucket` is missing.** S3 then answers 403 for every missing object, so missing URLs become masked denials, and a path without a trailing slash no longer redirects to its index object ([AWS without `s3:ListBucket`](../architecture/access-denied.md#aws-without-s3-listbucket)).

**Confirm.** Read the access event of a request that should succeed:

| Fields | Cause |
| --- | --- |
| `level=error outcome=origin_failure error_class=origin_denied` | 1 or 3: the origin denied the read. |
| `level=info outcome=success resolution=not_found` | 2: the object is missing under the prefix. |

To tell cause 3 from cause 1, request a URL whose object certainly exists, such as `/` with its index object. If it is served, the credentials work and only missing objects are denied: `s3:ListBucket` is missing. If it also answers 404 with `error_class=origin_denied`, the credentials or the bucket policy deny every read.

`statica config check-origin` reports `origin check failed: access denied` for a denial. `statica config print` shows the effective prefix: compare it with the keys in your bucket. The metric `statica_origin_operations_total{result="denied"}` counts denials.

**Fix.** Correct the credentials or the bucket policy, or the prefix. Grant `s3:ListBucket` if you want missing objects to be reported as missing ([Connect to S3 and S3-compatible providers](./s3-providers.md)). To make denials visible as failures to visitors and load balancers, set [`errors.access_denied: bad_gateway`](../reference/configuration.md#errors-access-denied).

## 502 Bad Gateway

**Likely causes.** The origin failed before the response headers, in a way that is neither a denial masked as 404, a timeout nor throttling ([only a confirmed absence continues](../architecture/request-flow.md#only-a-confirmed-absence-continues)):

| `error_class` | Usual cause |
| --- | --- |
| `origin_configuration` | The origin named the problem in its error code: the bucket does not exist (`NoSuchBucket`), the region or the endpoint is wrong (codes such as `PermanentRedirect` or `AuthorizationHeaderMalformed`), or a credential error was answered with a status other than 403. On AWS S3, wrong credentials answer 403 and appear as `origin_denied`. |
| `origin_unavailable` | The origin is unreachable or answers with server errors; no credentials were found, which fails before any request is sent; or the origin answered with an error that carries none of the recognized codes, such as a redirect or a 400 Bad Request without a body. |
| `origin_ambiguous_not_found` | The origin answered a `GET` with 404 without the `NoSuchKey` error code. statica cannot tell such an answer from a missing bucket, so it answers 502 instead of 404, and runs no SPA fallback or custom 404 page. An S3-compatible service that answers missing objects this way produces a 502 for every missing URL. |
| `origin_invalid_response` | The origin's response was malformed: for example metadata with control characters, an invalid `Last-Modified`, or a `.br` or `.gz` variant whose `Content-Encoding` metadata does not match its suffix. |
| `origin_denied` | An origin 403, with `errors.access_denied: bad_gateway`. |

**Confirm.** Run `statica config check-origin`: its message distinguishes `origin configuration`, `origin unavailable`, `ambiguous object absence` and `access denied`. A wrong region shows as `origin_configuration` in the access log but as `origin unavailable` from `check-origin`, whose `HEAD` request gets an answer without an error code. `statica_origin_operations_total` by `result` shows which answers the origin gives.

**Fix.** Correct the bucket, region, endpoint or credentials; check that outbound traffic to the endpoint is allowed, through the proxy named by `HTTPS_PROXY` if you use one. With a service that omits `NoSuchKey`, missing objects keep answering 502.

## 503 Service Unavailable

**Likely causes.**

| Sign in the access event | Cause |
| --- | --- |
| `error_class=request_capacity`, with `authority` instead of `host` | Every one of `server.max_concurrent_requests` request slots was in use. The check runs before any other, so the request has no host yet. |
| `error_class=origin_throttled` | The origin kept throttling the read after `s3.max_attempts` attempts. |
| `termination_reason=shutdown`, at `warn` | A shutdown canceled the request before its headers. |
| `error_class=origin_inconsistent` | The object kept changing while statica served a range request. |

**Confirm.** `statica_http_rejected_requests_total{reason="request_capacity"}` counts capacity rejections, and `statica_http_requests_in_flight` shows how many slots are in use. Long downloads hold their slot for their whole duration ([request capacity](../architecture/request-flow.md#request-capacity)).

**Fix.** For capacity: raise `server.max_concurrent_requests` within what the process can handle, add instances, or keep misdirected traffic away (unknown hosts take a slot too). For throttling: reduce the request rate at the origin, for example with longer TTLs ([Tune the memory cache](./cache-tuning.md)).

## 504 Gateway Timeout

**Likely cause.** The preparation budget ran out. [`server.response_header_timeout`](../reference/configuration.md#server-response-header-timeout) (`30s`) expired before the response headers: the origin is slow or unreachable, attempts and backoff used up the budget, a shared fill did not finish in time, or statica has no credentials and the AWS SDK spent the budget looking for a machine role. With [`server.request_timeout`](../reference/configuration.md#server-request-timeout) set, that limit can end the request first.

**Confirm.** The access event has `level=error`, `termination_reason=preparation_timeout` (or `total_deadline` for `server.request_timeout`) and `error_class=origin_timeout` ([before the headers](../architecture/timeouts.md#before-the-headers)). These requests count in `statica_http_requests_total{status="504", outcome="origin_failure"}`. Look also at `statica_origin_header_duration_seconds` and at `statica_origin_operations_total{result="timeout"}`. Each attempt to connect to the origin, TLS handshake included, is bounded by `s3.response_header_timeout` (`10s`), so a blocked network path shows up as origin timeouts.

A client that goes away before the headers, often on its own timeout or a proxy's, is not a 504: its access event has `status=0`, `outcome=incomplete` and `termination_reason=disconnect`, at `warn`. Compare its `duration_ms` with your clients' and proxies' timeouts ([align the time limits](./behind-a-proxy.md#align-the-time-limits)).

**Fix.** Check the network path to the endpoint and any outbound proxy. If the origin is slow but working, adjust `s3.response_header_timeout`, `s3.max_attempts` and the preparation budget together. To serve expired copies when the origin stalls, see [stale-if-error](./cache-tuning.md#serve-expired-copies-when-the-origin-fails). For clients that leave, find what makes the response slow to start, and give the proxy in front a wait for response headers longer than the preparation budget.

## 406 Not Acceptable

**Likely cause.** The object exists, but the request's `Accept-Encoding` accepts none of its available forms: for example `identity;q=0` or `*;q=0` without an acceptable `.br` or `.gz` variant, or an object stored with `Content-Encoding: gzip` requested by a client that refuses gzip ([404 or 406](../architecture/precompressed-variants.md#_404-or-406)).

**Confirm.** Repeat the request with the client's `Accept-Encoding` header. The access event has `status=406` and `outcome=success`.

**Fix.** Publish an unencoded canonical object, plus the variants you want served.

## 421 Misdirected Request

**Likely cause.** [`hosts`](../reference/configuration.md#hosts) is set and the request's hostname is not in it: a missing name such as the apex or `www` form, or a proxy that replaces `Host` with an internal name.

**Confirm.** The access event has `status=421`, `error_class=unknown_host` and the raw `Host` in `authority`. `statica_http_rejected_requests_total{reason="unknown_host"}` counts them.

**Fix.** Add the name to `hosts`, or configure the proxy to pass the original `Host` ([preserve the Host header](./behind-a-proxy.md#preserve-the-host-header)).

## Readiness stays not ready

`/readyz` answers 503 with a reason ([`/readyz`](../reference/admin-endpoints.md#readyz)):

| Reason | Likely cause | Confirm |
| --- | --- | --- |
| `origin_unknown` | The origin health check has never succeeded: a missing key, a denial, wrong credentials, a wrong bucket or region. | `origin_health_check_failed` events give the `result`; `statica config check-origin` reads the same key with the same credentials. |
| `origin_unhealthy` | `failure_threshold` consecutive checks failed after a success. | `origin_health_changed state=unhealthy` with the `result` of the last check. |
| `origin_stale` | No check has succeeded for `max_staleness`, for example because checks time out. | `statica_origin_health_checks_total` by `result`, and the age of `statica_origin_health_last_success_timestamp_seconds`. |
| `draining` | A shutdown has started. | `shutdown_started` in the log. |

A missing health key is reported as `ambiguous_not_found` on providers that answer 404 and on AWS with `s3:ListBucket`, and as `denied` on AWS without it; never as `not_found`. Fix the cause the result points to; [Health checks and readiness](./health-checks.md#verify-it) lists what each result means.

## On Kubernetes

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| The pod runs but never becomes ready, and its probes are refused. | The listeners are on their default loopback addresses, which the kubelet and the Service cannot reach. | Set `server.listen` and `admin.listen` to the pod's interfaces, such as `:8080` and `:9090` ([the configuration](./kubernetes.md#the-configuration)). |
| The pod never becomes ready, and `/readyz` answers 503. | The origin health check has not succeeded. | See [Readiness stays not ready](#readiness-stays-not-ready). |
| `CrashLoopBackOff`. | statica exits at startup. `kubectl logs --previous` shows the reason: a `statica:` message for exit status 2, a `startup_failed` event for exit status 1. | See the two sections on startup above. For exit status 2, the ConfigMap usually has no `config.yaml` key or has invalid content; a ConfigMap that does not exist keeps the pod in `ContainerCreating`. |
| `OOMKilled`. | The container's memory limit is too close to the cache limits. | Size the limit and `GOMEMLIMIT` from `cache.max_bytes` and `cache.max_fill_bytes` ([Size the memory](./kubernetes.md#size-the-memory)). |
| Refused or reset connections during a rollout. | The pod received SIGTERM while the Service still sent it new connections, or the grace period ended before the drain. | Add the `preStop` delay, and keep `terminationGracePeriodSeconds` above the delay plus `server.shutdown_timeout` ([the manifests](./kubernetes.md#the-manifests)). |
| Every URL answers 404 or 502 after a deployment. | The pod has no credentials, or its ServiceAccount is bound to a role that cannot read the bucket. | Check the role annotation or the Pod Identity association ([Give the pods credentials](./kubernetes.md#give-the-pods-credentials)), then see [Every URL answers 404](#every-url-answers-404). |
| A changed ConfigMap has no effect. | statica reads its configuration once, at startup. | Restart the pods ([Change the configuration](./kubernetes.md#change-the-configuration)). |

## Downloads are slow or interrupted

**Likely causes.** A response cut after its headers keeps its status, often 200, and ends early. The access event has `outcome=incomplete` and a `termination_reason` ([after the headers](../architecture/timeouts.md#after-the-headers)):

| `termination_reason` | Cause | Fix |
| --- | --- | --- |
| `client_write_timeout` | The client, or a proxy in front, accepted no data for `server.write_idle_timeout`. | Usually the client. Raise the limit if legitimate clients pause longer. |
| `disconnect`, `client_write_failure` | The client or a proxy closed the connection, perhaps on its own timeout. | Compare with the proxy's logs and timeouts ([align the time limits](./behind-a-proxy.md#align-the-time-limits)). |
| `origin_read_timeout` | The origin sent no data for `s3.read_idle_timeout`. | Check the origin and the network path. |
| `origin_read_failure`, `checksum_mismatch`, `length_mismatch` | Reading the body from the origin failed, its checksum did not match, or the origin sent a body of another length than it announced. | Check the origin; with `s3.features.checksums`, the stored object may be corrupt. |
| `total_deadline` | `server.request_timeout` ended the transfer. | Raise or disable it for large files. |
| `shutdown` | A shutdown reached its drain deadline. | Raise `server.shutdown_timeout` and the grace period together ([Deploy, roll out and shut down](./deployment.md#shut-down-without-cutting-requests)). |

**Confirm.** `statica_stream_failures_total` by `reason` shows which cause dominates. The origin's reasons are logged at `error`, the others at `warn`. statica neither retries nor resumes a cut transfer; clients that resume with `Range` and `If-Range` get the missing bytes ([long downloads and resumption](../architecture/timeouts.md#long-downloads-and-resumption)).

## Origin traffic or latency grows with the load

**Likely cause.** Requests bypass the memory cache because a fill limit is reached. Each such request reads the origin on its own: the read is not shared, nothing is stored, and an expired copy cannot answer it when the origin fails.

| `cache_reason` | Limit reached |
| --- | --- |
| `fills_full` | `cache.max_concurrent_fills` fills were already running. |
| `fill_budget` | Running fills held `cache.max_fill_bytes`, so the object streamed instead of being stored. |
| `waiters_full` | One object's fill already had `cache.max_waiters_per_key` waiting requests. |
| `too_large` | Not a fill limit: objects above `cache.max_object_bytes` stream on every request. |

**Confirm.** The access events have `cache=bypass` with these reasons, and `statica_cache_admissions_total{result="bypass"}` counts them by `reason`. `statica_origin_operations_total` with `purpose="request"` grows against `purpose="cache_fill"`, and `statica_cache_fills_in_flight` stays at `statica_cache_fill_limit_concurrency`.

**Fix.** Raise the limit that the reason names, within the memory the process has ([bypasses](./cache-tuning.md#bypasses)).

## Old content after publishing

**Likely causes.**

1. **A fresh memory cache entry.** statica serves a fresh entry without asking the origin, for up to [`cache.ttl`](../reference/configuration.md#cache-ttl) (`5m`) or the TTL of a matching rule, or longer when `cache.ttl_mode` lets the origin's lifetime extend it ([lifetime](../architecture/memory-cache.md#lifetime)).
2. **A negative entry.** A URL requested before its object was published stays 404 for up to [`cache.negative_ttl`](../reference/configuration.md#cache-negative-ttl) (`5s`) ([negative entries](../architecture/memory-cache.md#negative-entries)).
3. **Browser or CDN caching.** With `cache_control.default: auto`, an object without its own cache metadata is sent with `Cache-Control: max-age=<ttl>`, so downstream caches keep it too ([Cache-Control and Age](../architecture/cache-control.md)).
4. **A new publication prefix.** A changed `s3.prefix` takes effect only when the instance restarts.

**Confirm.** The access event shows `cache=hit` or `cache=negative_hit`, and the response's `Age` header shows how old the copy is. A request with `Cache-Control: no-cache` makes statica revalidate the entry with the origin, which shows what the origin holds now and also refreshes the entry on that instance:

```sh
curl -sS -H 'Cache-Control: no-cache' https://www.example.com/
```

This does not work for objects whose [`cache.request_directives`](../reference/configuration.md#cache-request-directives) is `ignore`: their entries change only when they expire.

**Fix.** Give HTML a short TTL with `cache.rules` and `cache_control` rules, and long TTLs only to assets whose names change with their content ([Control browser and CDN caching](./browser-caching.md)). To drop every entry at once, restart the instance.

## Single-page-app routes answer 404

**Likely causes**, in the order statica checks them ([eligibility rules](../architecture/spa-fallback.md#eligibility-rules)):

- there is no `spa` block;
- a lookup before the fallback was denied: a denial, including the 403 for missing objects without `s3:ListBucket`, stops resolution before the fallback;
- the path is under one of `spa.exclude_prefixes`;
- with `spa.include_prefixes` set, the path is under none of them; without it, the last segment of the path contains a dot, as in `/users/jane.doe`;
- the request does not accept `text/html` explicitly: `curl` sends `Accept: */*` by default, which does not qualify;
- the `spa.fallback` object does not exist under the prefix.

**Confirm.** Repeat the request as a browser would, and read the `resolution` field:

```sh
curl -sS -o /dev/null -w '%{http_code}\n' -H 'Accept: text/html' https://www.example.com/orders/42
```

`resolution=spa` means the shell was served; `resolution=not_found` means the route was not eligible or the shell is missing; `error_class=origin_denied` means a denial.

**Fix.** Adjust `spa.include_prefixes` and `spa.exclude_prefixes`, publish the shell, or fix the permissions ([Serve a single-page application](./single-page-apps.md)).

## Precompressed variants are not served

**Likely causes.**

- [`compression.mode`](../reference/configuration.md#compression-mode) is `off`, the default;
- the request has no `Accept-Encoding` header: `curl` sends none unless asked;
- the variant is not named after the exact object key: `/` reads `index.html`, so its variants are `index.html.br` and `index.html.gz` ([sibling naming](../architecture/precompressed-variants.md#sibling-naming));
- the coding is missing from `compression.encodings`, or the client prefers the unencoded object, for example with `br;q=0.5`, which leaves `identity` at quality 1.

**Confirm.**

```sh
curl -sS -o /dev/null -D - -H 'Accept-Encoding: br, gzip' https://www.example.com/assets/app.js
```

A served variant carries `Content-Encoding: br` or `gzip`, and the access event's `origin_key` ends in `.br` or `.gz`.

**Fix.** Set `compression.mode: precompressed`, and publish each variant next to its canonical object, with matching metadata ([Serve precompressed files](./precompressed-files.md)).

## Log lines are missing

- **Level and switches.** `observability.log_level` above `info` drops successful requests' access events and `info` lifecycle events, and `observability.access_log: false` drops all access events.
- **Drops.** A `log_events_dropped` event and the metric `statica_log_events_dropped_total` count lines that statica could not write ([dropped events](./logs.md#dropped-events)).
- **Abrupt ends.** A second stop signal gives the logs 250 ms. SIGKILL and SIGHUP end the process without flushing them, and a closed standard output drops every line written while no reader is attached.
- **Never logged.** Requests that Go's HTTP server refuses before statica sees them, such as headers larger than `server.max_header_bytes`, and requests to the admin listener.

## Related pages

- [HTTP responses reference](../reference/http-responses.md): every status statica sends.
- [Log reference](../reference/logs.md): every event and field value.
- [Startup, readiness and shutdown](../architecture/lifecycle.md).
