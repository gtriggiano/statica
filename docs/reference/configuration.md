---
outline: [2, 2]
---

# Configuration reference

Each **statica** command that needs a configuration reads one YAML file when it starts. This page lists every field with its type, its default and the rules it must satisfy. The [CLI reference](./cli.md) describes the commands that load, check and print the file, [Configuration examples](./examples.md) shows complete files, and [Configuration basics](../guide/configuration.md) introduces the format.

Each field has its own heading, named after its full path. `[]` stands for an item of a list and `<name>` for an entry of a map: `cache.rules[].match.prefix`, `headers.set.<name>`.

## Loading

### Selecting the file

Every command that needs a configuration selects exactly one file, the first of:

1. the `--config` flag;
2. the `STATICA_CONFIG` environment variable, when it is set;
3. `config.yaml` in the working directory.

No other directory is searched and files are never merged. An empty `--config` value, an empty `STATICA_CONFIG` and a file that cannot be opened are errors. The file is read once: a change takes effect when the process restarts.

### Format

- The file is at most 1 MiB and holds exactly one YAML document whose root is a mapping.
- Keys are plain strings. Unknown keys and duplicate keys are errors, in every mapping, including `headers.set` and `redirects[].captures`.
- YAML aliases (`*name`), merge keys (`<<`) and custom tags (`!tag`) are rejected, so anchors (`&name`) have no use. Standard tags such as `!!str` are accepted when they match the field's type.
- An explicit null (`key: null`, `key: ~`, or `key:` without a value) is rejected. Omit the field to use its default.
- Values are not converted between types:

| Type | Accepted | Rejected |
| --- | --- | --- |
| string | A YAML string. Quote a value that YAML reads as another type: `"1.3"`. | `1.3` (a number), `8080` (an integer) |
| integer | A decimal YAML integer. | `0x10`, `1_000`, `+5`, `1.0`, `"16"` |
| boolean | `true` or `false`. | `True`, `yes`, `on`, `"true"` |
| duration | A string of decimal numbers, each followed by a unit: `ns`, `us` (or `µs`), `ms`, `s`, `m` or `h`. Fractions (`1.5s`) and a leading `+` are accepted, and units combine: `1h30m`. Write zero as `0s`. | `300`, `"0"`, `1d` |
| byte size | A string: a non-negative integer followed directly by `KiB`, `MiB` or `GiB` (multiples of 1024). | `1.5GiB`, `1MB`, `1024`, `10 MiB` |

No duration field accepts a negative value.

### Environment variables

`${NAME}` inside a string, duration or byte-size value is replaced with the value of the environment variable `NAME`. `NAME` starts with a letter or an underscore, followed by letters, digits or underscores.

- Substitution applies to values only: not to keys (such as the names in `headers.set`), integers or booleans.
- It happens after YAML parsing. The variable's value is inserted once, is never parsed as YAML, and is validated as the field's value.
- `$$` writes a literal `$`. A `$` that is not followed by `{` is kept as is: `$HOME` stays `$HOME`.
- A variable that is not set is an error that names the variable. A variable set to the empty string inserts an empty string. An unclosed `${` and an invalid name are errors.
- `config print` shows every substituted value as `[REDACTED]`, and also the defaults derived from one: `admin.origin_health.key` from `index`, `compression.encodings` from `compression.mode`.

```yaml [config.yaml]
s3:
  bucket: ${SITE_BUCKET}
  prefix: releases/${RELEASE_ID}
```

### Defaults

statica applies a default only to an omitted field. An explicit value is never replaced, even when it is `false`, zero or empty, and an empty value without a meaning is an error (`hosts: []`, `index: ""`). Some defaults apply only when their block is present: `server.tls.min_version`, the fields of `admin.origin_health`, `compression.encodings` in `precompressed` mode and `redirects[].query`. `config print` shows the configuration with every default applied.

### Normalization

statica normalizes some values while it validates them. `config print` shows the normalized values.

| Field | Normalization |
| --- | --- |
| `hosts` | Lowercase ASCII (internationalized names become punycode), one trailing dot removed. IP addresses in canonical form. |
| `s3.prefix` | Leading and trailing `/` removed, then one `/` appended when the result is nonempty (see [`s3.prefix`](#s3-prefix)). |
| `server.trusted_proxies` | Host bits cleared: `10.1.2.3/8` becomes `10.0.0.0/8`. |
| `server.client_ip_header`, `server.request_id_header`, names in `headers.set` | Canonical header case: `x-real-ip` becomes `X-Real-Ip`. |
| `server.tls.cert_file`, `server.tls.key_file` | A relative path is resolved against the directory of the configuration file. |

### Ports

A port in `server.listen`, `admin.listen`, `s3.endpoint`, `cors.allowed_origins` or a redirect destination is a decimal integer from 1 to 65535, written in ASCII digits without a sign or a leading zero: `8080` is accepted, `08080` and `+8080` are rejected.

### Error messages

An invalid configuration stops the command before anything starts, with exit status 2. The message names the field and, when known, its position in the file:

```text
statica: cache.ttl (line 5, column 8): expected a string with explicit units
```

- Paths use dots and list indexes: `redirects[0].status`. An unknown key that is not a plain identifier is reported as `<unknown>`.
- A YAML syntax error reports the line; the column is always 1: `configuration (line 2, column 1): invalid YAML syntax`.
- Messages never repeat configured values. They repeat keys as part of the reported path (a header name under `headers.set`, a capture name, an unknown key), and the name of a missing environment variable.

## `hosts`

**Type:** list of strings\
**Default:** none — every valid hostname is accepted.

Allowlist of request hostnames. When present, the list is nonempty and each entry is an exact DNS name or IP address, without scheme, port, path, brackets or wildcard (`www.example.com`, `192.0.2.10`, `2001:db8::10`). A name made only of digits and dots must be a valid IPv4 address: `192.0.2.010` is rejected. Entries must be unique after [normalization](#normalization). A request whose `Host`, without its port, is not listed gets 421 Misdirected Request ([host admission](../architecture/request-flow.md#host-admission)). Every listed name serves the same site.

::: tip
Accepted hostnames become the [`host` label](./metrics.md#host) of the request metrics. With `hosts`, the label values are bounded by the list; without it, every hostname clients send adds new series.
:::

## `server`

**Type:** mapping\
**Default:** `{}`

The public listener. Each field has its own default. [Timeouts and long downloads](../architecture/timeouts.md) explains how the timeouts below combine.

### `server.listen`

**Type:** string\
**Default:** `127.0.0.1:8080`

Address of the public listener, as `host:port`. The host is a hostname, an IP address (IPv6 in brackets: `[::1]:8080`) or empty for every interface (`:8080`). The port follows the [port syntax](#ports). Must not share an address and port with [`admin.listen`](#admin-listen). The listener serves HTTP/1.1, or HTTPS with HTTP/2 and HTTP/1.1 when `server.tls` is present.

### `server.tls`

**Type:** mapping\
**Default:** none — the public listener serves plain HTTP.

Serves HTTPS with one certificate chain, offering HTTP/2 and HTTP/1.1 through ALPN. `config validate` and `serve` load the certificate and key and fail if they do not match; `config print` does not read them. The files are read once, at startup. See [TLS on the public listener](../architecture/security.md#tls-on-the-public-listener), and [Serve HTTPS directly](../guide/tls.md) for a walkthrough.

```yaml [config.yaml]
server:
  listen: :8443
  tls:
    cert_file: tls/fullchain.pem
    key_file: tls/privkey.pem
    min_version: "1.3"
```

### `server.tls.cert_file`

**Type:** string\
**Default:** none — required when `server.tls` is present.

Path of the PEM certificate chain, leaf certificate first. A relative path is resolved against the directory of the configuration file.

### `server.tls.key_file`

**Type:** string\
**Default:** none — required when `server.tls` is present.

Path of the PEM private key that matches the leaf certificate. A relative path is resolved like `cert_file`.

### `server.tls.min_version`

**Type:** string\
**Default:** `"1.2"` when `server.tls` is present.

Minimum TLS version: `"1.2"` or `"1.3"`. Quote the value: an unquoted `1.3` is a YAML number and is rejected.

### `server.trusted_proxies`

**Type:** list of strings\
**Default:** `[]`

CIDR ranges of the proxies allowed to supply the client IP (`server.client_ip_header`) and the request ID (`server.request_id_header`). These headers are used only when the immediate peer's address is in this list, and the `X-Forwarded-For` walk also skips the addresses in it. With an empty list, both headers are ignored and the peer address is used ([client IP attribution](../architecture/security.md#trusted-proxies-and-client-ip-attribution)). To choose the ranges, see [Trust only your proxies](../guide/behind-a-proxy.md#trust-only-your-proxies).

- A single address needs a full-length prefix: `192.0.2.10/32`, `2001:db8::1/128`.
- A prefix length of 0 (trust every address) is rejected.
- IPv4-mapped IPv6 ranges (`::ffff:10.0.0.0/104`) are rejected; write the IPv4 range (`10.0.0.0/8`).

### `server.client_ip_header`

**Type:** string\
**Default:** `X-Forwarded-For`

Header from which statica takes the client IP when the peer is a trusted proxy: `X-Forwarded-For` or `X-Real-IP`, in any letter case.

- `X-Forwarded-For`: statica reads every line of the header as one list and walks it from the right. The client is the first address that is not in `server.trusted_proxies`, or the leftmost address when all of them are.
- `X-Real-IP`: the header must hold exactly one IP address.

A missing, malformed or too long header is not an error: statica uses the peer address. To choose the header, see [Choose the client IP header](../guide/behind-a-proxy.md#choose-the-client-ip-header).

### `server.request_id_header`

**Type:** string\
**Default:** `X-Request-ID`

Request header from which statica reuses a request ID. It reuses the ID only when the peer is a trusted proxy and the header has exactly one value of 1–128 characters from `A–Z`, `a–z`, `0–9`, `.`, `_`, `:` and `-`. Otherwise it generates a 32-character hexadecimal ID. Every response statica writes carries the ID in `X-Request-ID`, whatever this setting ([request IDs](../architecture/security.md#request-ids); [Pass request IDs](../guide/behind-a-proxy.md#pass-request-ids)). Responses that the Go HTTP server writes before statica handles the request (431 Request Header Fields Too Large for oversized headers, 400 Bad Request for some malformed requests) have no `X-Request-ID`.

The name must be a valid header name and not one of these:

| Group | Names |
| --- | --- |
| Hop-by-hop | `Connection`, `Keep-Alive`, `Proxy-Authenticate`, `Proxy-Authorization`, `Proxy-Connection`, `TE`, `Trailer`, `Transfer-Encoding`, `Upgrade` |
| Message | `Host`, `Content-Length` |
| Credentials | `Authorization`, `WWW-Authenticate`, `Cookie`, `Set-Cookie` |
| Forwarding | `X-Forwarded-For`, `X-Real-IP`, `Forwarded` |

### `server.max_forwarded_hops`

**Type:** integer\
**Default:** `16`

Maximum number of addresses statica reads from `X-Forwarded-For`, 1–256. A longer list, or one longer than 64 bytes per allowed address, is ignored and statica uses the peer address ([Choose the client IP header](../guide/behind-a-proxy.md#choose-the-client-ip-header)).

### `server.read_header_timeout`

**Type:** duration\
**Default:** `5s`

Positive. Time allowed to read the headers of a request.

### `server.response_header_timeout`

**Type:** duration\
**Default:** `30s`

Positive. Budget from the admission of a request to its response headers. It covers origin attempts and retries and waiting for a shared fill. When it expires first, the response is 504 Gateway Timeout. It also bounds the setup of the origin client when `serve` starts and the whole `config check-origin` probe.

### `server.write_idle_timeout`

**Type:** duration\
**Default:** `60s`

Positive. Each write of a response to the client, of up to 32 KiB, must finish within this time. A slow download that keeps progressing is not cut. When a write takes longer, statica aborts the response: it closes an HTTP/1.1 connection or resets the HTTP/2 stream.

### `server.idle_timeout`

**Type:** duration\
**Default:** `60s`

Positive. How long a keep-alive connection may stay idle between requests. Idle connections count toward `server.max_connections`.

### `server.request_timeout`

**Type:** duration\
**Default:** `0s`

Zero or positive. Total time limit of a request, from its admission to the end of the response body. `0s` sets no total limit; the other timeouts still apply. When it expires before the response headers, the response is 504 Gateway Timeout; after them, statica aborts the response (it closes an HTTP/1.1 connection or resets the HTTP/2 stream).

### `server.shutdown_timeout`

**Type:** duration\
**Default:** `30s`

Positive. Total budget of a graceful shutdown, from the first signal to the exit. Requests still active when the drain phase ends, at the budget minus `min(budget/4, 1s)`, are canceled, however long they are allowed to run ([one budget](../architecture/lifecycle.md#one-budget)). To size it with your supervisor's grace period, see [Shut down without cutting requests](../guide/deployment.md#shut-down-without-cutting-requests).

### `server.max_header_bytes`

**Type:** integer\
**Default:** `32768`

Maximum size of the request line and headers, in bytes, 1–1073741824. HTTP/1.1 requests may exceed it by up to 4 KiB. Larger requests get 431 Request Header Fields Too Large from the Go HTTP server before statica handles them, without `X-Request-ID`, `headers.set` or `X-Content-Type-Options`.

### `server.max_connections`

**Type:** integer\
**Default:** `1024`

Positive. Maximum open connections on the public listener, including TLS handshakes and idle keep-alive connections. At the limit, statica stops accepting connections: new ones wait in the operating system's backlog; statica does not refuse them.

### `server.max_concurrent_requests`

**Type:** integer\
**Default:** `256`

Positive. Maximum requests in progress at once, across every connection (an HTTP/2 connection carries several). A request over the limit gets 503 Service Unavailable at once, before any other check. A request holds its place until its response ends, downloads included ([request capacity](../architecture/request-flow.md#request-capacity)).

## `admin`

**Type:** mapping\
**Default:** `{}`

The admin listener, which serves `/livez`, `/readyz` and `/metrics` ([Admin endpoints](./admin-endpoints.md)).

### `admin.listen`

**Type:** string\
**Default:** `127.0.0.1:9090`

Address of the admin listener, with the same syntax as `server.listen`. With the same port as `server.listen`, it is rejected when the two hosts are equal after [normalization](#normalization), when either host is empty, `0.0.0.0` or `::` (every interface), or when both are loopback addresses (`localhost`, `127.0.0.0/8`, `::1`). Different ports are always accepted. Other addresses that the system cannot bind together, such as two names that resolve to the same address, fail when `serve` starts. The admin listener serves plain HTTP/1.1.

::: danger
The admin endpoints have no authentication. Bind `admin.listen` to a loopback or private address ([the admin listener](../architecture/security.md#the-admin-listener), [Admin endpoints](./admin-endpoints.md)).
:::

### `admin.origin_health`

**Type:** mapping\
**Default:** none — no origin health check; readiness does not depend on the origin.

A background check of one object's metadata: one `HEAD` request, one attempt, never from the memory cache. With the check enabled, `/readyz` reports not ready until the first success, then follows the thresholds below ([origin health](../architecture/lifecycle.md#origin-health)). To decide whether to enable it and choose its key and thresholds, see [Health checks and readiness](../guide/health-checks.md#decide-whether-readiness-follows-the-origin).

::: warning
The check runs only with `enabled: true`. Without it, the block is validated but has no effect, apart from the key that `config check-origin` uses.
:::

```yaml [config.yaml]
admin:
  listen: 10.0.0.5:9090
  origin_health:
    enabled: true
    key: health/ok.txt
    interval: 15s
```

### `admin.origin_health.enabled`

**Type:** boolean\
**Default:** `false`

Turns the check on.

### `admin.origin_health.key`

**Type:** string\
**Default:** none — the value of `index`.

Object key to check, relative to `s3.prefix`, with the same rules as `not_found`. `config check-origin` also uses this key when the block is present.

### `admin.origin_health.interval`

**Type:** duration\
**Default:** `30s`

Positive. Time from the end of one check to the start of the next, shortened by a random jitter of up to 10%. The first check starts once both listeners are bound.

### `admin.origin_health.timeout`

**Type:** duration\
**Default:** `3s`

Positive and less than `interval`. Deadline of one check. `s3.response_header_timeout` applies to it as well.

### `admin.origin_health.failure_threshold`

**Type:** integer\
**Default:** `3`

Positive. Consecutive failed checks that turn a healthy origin unhealthy. Every error is a failure, a missing object included.

### `admin.origin_health.success_threshold`

**Type:** integer\
**Default:** `1`

Positive. Consecutive successful checks that turn an unhealthy origin healthy again. The first success after startup makes the origin healthy whatever this value.

### `admin.origin_health.max_staleness`

**Type:** duration\
**Default:** `90s`

Must be greater than `interval` plus `timeout`. When the last success is older, `/readyz` reports `origin_stale`.

## `s3`

**Type:** mapping\
**Default:** none — required: `s3.bucket` has no default.

The origin: one bucket and, optionally, a publication prefix in it. Credentials come from the standard AWS credential chain; the configuration never holds them. Origin and credential requests honor the `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` environment variables ([origin connections](../architecture/security.md#origin-connections)). [Connect to S3 and S3-compatible providers](../guide/s3-providers.md) covers AWS S3, compatible services and credentials.

### `s3.bucket`

**Type:** string\
**Default:** none — required.

Bucket name, up to 255 characters: a letter or digit, then letters, digits, `.`, `_` or `-`. ARNs, paths and templates are rejected. Names ending in `--x-s3`, `.mrap`, `--ol-s3`, `-s3alias` or `--table-s3` (directory buckets, Multi-Region and Object Lambda access points, access point aliases and table buckets) are rejected.

### `s3.region`

**Type:** string\
**Default:** none — discovered from the AWS configuration when `serve` or `config check-origin` starts.

Signing region. When present, it is nonempty and contains only ASCII letters, digits, `-` and `_`. Without it, statica takes the region from the AWS configuration (environment variables such as `AWS_REGION`, then the shared configuration files), and `config validate` prints `S3 region deferred to AWS discovery at startup`. `serve` and `config check-origin` fail if no region is found or if the discovered region contains other characters.

### `s3.prefix`

**Type:** string\
**Default:** `""`

Publication prefix: the part of the bucket that holds the site. Empty selects the bucket root. Leading and trailing slashes are optional:

| Configured value | Effective prefix | Object key for `/index.html` |
| --- | --- | --- |
| omitted, `""` or `/` | none (bucket root) | `index.html` |
| `docs`, `/docs`, `docs/` or `/docs/` | `docs/` | `docs/index.html` |
| `/docs/current` | `docs/current/` | `docs/current/index.html` |

Only leading and trailing slashes are removed. Empty interior segments (`docs//current`), `.` and `..` segments, backslashes and control characters are rejected. Other characters, spaces, Unicode and `%` included, are kept as written, not URL-decoded. The effective prefix is at most 1024 bytes, and so is the prefix plus each configured key (`index`, `not_found`, `spa.fallback`, `admin.origin_health.key`). Every object key statica reads starts with the prefix ([confinement](../architecture/security.md#requests-cannot-leave-the-publication-prefix)). See [Choose a publication prefix](../guide/publishing.md#choose-a-publication-prefix) and [Deploy with release prefixes](../guide/publishing.md#deploy-with-release-prefixes).

### `s3.endpoint`

**Type:** string\
**Default:** none — the AWS S3 endpoint of the region.

URL of an S3-compatible service: `http` or `https`, a host and an optional [port](#ports), and nothing else (no credentials, query, fragment, or path other than `/`). An `http` endpoint requires `s3.allow_http: true`. Endpoint overrides from the AWS environment and shared configuration are ignored: only this field selects the endpoint.

### `s3.path_style`

**Type:** boolean\
**Default:** `false`

Uses path-style requests (`https://endpoint/bucket/key`) instead of virtual-hosted-style requests (`https://bucket.endpoint/key`). Some S3-compatible services require it.

### `s3.allow_http`

**Type:** boolean\
**Default:** `false`

Must be `true` for an `http` endpoint. It has no other effect: it does not change certificate verification for an `https` endpoint.

### `s3.insecure_skip_verify`

**Type:** boolean\
**Default:** `false`

Skips verification of the origin's TLS certificate chain and hostname: of `s3.endpoint` or, without one, of the AWS S3 endpoint. It applies to requests to the origin only, not to AWS credential providers, and cannot be combined with an `http` endpoint. `serve` logs `origin_tls_verification_disabled` at warn level after `startup`, and `config check-origin` prints a warning.

::: danger
Without verification, anyone who can intercept the traffic between statica and the origin can change the content statica serves.
:::

### `s3.response_header_timeout`

**Type:** duration\
**Default:** `10s`

Positive. Time to wait for the response headers of one origin request; each retry waits again. It also applies to requests to AWS credential providers. The request's `server.response_header_timeout` still bounds the total. It also bounds the setup of each connection, for the origin and for the credential providers alike: opening the TCP connection (otherwise up to 30 s) and the TLS handshake (otherwise up to 10 s) each take at most this value, so an origin that does not answer connection attempts fails each attempt as a timeout within the same budget.

### `s3.read_idle_timeout`

**Type:** duration\
**Default:** `30s`

Positive. Longest wait for data while statica reads an object body from the origin. Time during which statica is not reading, for example while the client is slow to accept data, does not count. When it expires, the read fails as a timeout.

### `s3.max_attempts`

**Type:** integer\
**Default:** `2`

Attempts per origin request, the first included, 1–3. Retries follow the AWS SDK's standard rules (server errors, throttling, connection errors), with a backoff of at most 500 ms. Reading a body is never retried once it has started. The origin health check always makes one attempt.

### `s3.features`

**Type:** mapping\
**Default:** `{}`

Switches for optional origin capabilities. Turn off a capability the provider does not support ([effect of `s3.features`](../architecture/ranges-and-conditionals.md#effect-of-s3-features)).

### `s3.features.conditional_reads`

**Type:** boolean\
**Default:** `true`

Allows `If-None-Match` and `If-Match` on origin reads. When on, the memory cache revalidates a stale entry with its ETag ([revalidation](../architecture/memory-cache.md#revalidation)); when off, it reads the object again in full. Range reads from the origin also require it.

### `s3.features.range_reads`

**Type:** boolean\
**Default:** `true`

Allows `Range` on origin reads. With this and `conditional_reads` on, statica advertises `Accept-Ranges: bytes` for objects with a known size and a strong ETag, and answers a range request for an object not held in memory by reading only that range. Otherwise such a request gets the whole object with 200 OK. An object held in the memory cache, with a known size and a strong ETag, answers a single range either way.

### `s3.features.checksums`

**Type:** boolean\
**Default:** `false`

Asks the origin for object checksums (`x-amz-checksum-mode: ENABLED`) and validates a checksum when the response carries one. Objects without a checksum are served normally. A mismatch fails the read.

## `cache`

**Type:** mapping\
**Default:** `{}`

The [memory cache](../architecture/memory-cache.md). Its limits are validated even when the cache is disabled. Some responses are never stored or reused, whatever this block says: an object whose origin metadata is malformed or whose origin `Cache-Control` contains `no-store` or `private` is never stored; a request with `Cache-Control: no-store` bypasses the cache; a request `no-cache` or `max-age` can force revalidation; and an origin `no-cache` gives a stored object a zero lifetime ([freshness](../architecture/memory-cache.md#freshness)). To size and tune it, see [Tune the memory cache](../guide/cache-tuning.md).

```yaml [config.yaml]
cache:
  max_bytes: 512MiB
  max_object_bytes: 8MiB
  ttl: 10m
  stale_if_error: 1h
```

### `cache.enabled`

**Type:** boolean\
**Default:** `true`

`false` disables the memory cache: every request reads the origin, and no rule can enable it again.

### `cache.max_bytes`

**Type:** byte size\
**Default:** `128MiB`

Positive. Memory charged to cache entries: bodies, metadata and a fixed overhead per entry, including entries that were replaced or removed but are still being sent. When it is full, statica evicts the least recently used entries that no response is using; when it cannot make room, it serves the object without storing it.

### `cache.max_entries`

**Type:** integer\
**Default:** `10000`

Positive. Maximum number of entries, counted like `max_bytes`.

### `cache.max_object_bytes`

**Type:** byte size\
**Default:** `2MiB`

Positive, at most `max_bytes` and at most `max_fill_bytes`. Largest object body stored in memory. Larger objects, and objects whose size is unknown, are streamed from the origin and not stored. Because of this rule, setting `max_bytes` or `max_fill_bytes` below `2MiB` requires a smaller `max_object_bytes` too.

### `cache.max_fill_bytes`

**Type:** byte size\
**Default:** `32MiB`

Positive. Memory for the bodies being read from the origin into the cache at the same time. A fill that does not fit is streamed instead, and not stored.

### `cache.max_concurrent_fills`

**Type:** integer\
**Default:** `16`

Positive. Maximum shared fills (origin reads into the cache) in progress at once. A request over the limit reads the origin directly, without sharing and without storing the object. It does not cap the number of origin requests.

### `cache.max_waiters_per_key`

**Type:** integer\
**Default:** `64`

Positive. Maximum requests sharing one fill, the request that started it included: `1` disables sharing. A request over the limit reads the origin directly.

### `cache.fill_timeout`

**Type:** duration\
**Default:** `30s`

Positive. Lifetime of one shared fill or revalidation, from its start; requests that join later do not extend it. It applies even when `server.request_timeout` is `0s`. When it expires, the requests still waiting get 504 Gateway Timeout, unless `cache.stale_if_error` serves an expired copy.

Each request also stops waiting at its own deadline (`server.response_header_timeout`, `server.request_timeout`) and then gets 504 Gateway Timeout without a stale copy; when the last waiting request leaves, the fill is canceled. With the defaults, both 30 seconds, the request that started a fill gives up before the fill times out. To let `cache.stale_if_error` cover a slow origin, set `cache.fill_timeout` below `server.response_header_timeout` ([deadlines and cancellation](../architecture/memory-cache.md#deadlines-and-cancellation)).

### `cache.ttl`

**Type:** duration\
**Default:** `5m`

Zero or positive. How long a stored object stays fresh. An origin `Cache-Control` (`s-maxage`, otherwise `max-age`) or `Expires` can shorten it, never extend it. A stale entry is revalidated with the origin before it is used again. `0s` stores objects but revalidates them on every use.

### `cache.negative_ttl`

**Type:** duration\
**Default:** `5s`

Zero or positive. How long statica remembers that an object does not exist (a negative entry), answering requests for it without reading the origin. `0s` stores no negative entries. An origin 403 Forbidden never creates one ([negative entries](../architecture/memory-cache.md#negative-entries)).

### `cache.stale_if_error`

**Type:** duration\
**Default:** `0s`

Zero or positive. How long after it expires an entry may still be served when refreshing it fails because the origin timed out (`cache.fill_timeout` included), kept throttling, or was unavailable. Unavailability includes server errors, network errors, missing credentials and provider errors without a more specific class, such as `ExpiredToken` (400). A 403, credential errors answered with 403 included, a missing object, an invalid response and an error that carries a bucket, endpoint or credential code (`NoSuchBucket`, `AuthorizationHeaderMalformed`, `InvalidAccessKeyId`, …) never qualify, and neither does an object whose origin `Cache-Control` contains `must-revalidate`, `proxy-revalidate`, `no-cache` or `s-maxage`. A request whose own deadline passes while it waits for the origin gets 504 Gateway Timeout without a stale copy (see `cache.fill_timeout`). `0s` disables it ([stale-if-error](../architecture/memory-cache.md#stale-if-error)).

### `cache.rules`

**Type:** list of mappings\
**Default:** none — every object uses the global values.

Overrides of `enabled`, `ttl`, `negative_ttl` and `stale_if_error` for some objects. The first rule whose `match` matches the object path applies; the fields it omits keep the global value, not the value of a later rule.

The object path is `/` followed by the object key without `s3.prefix`. A request for `/docs/` uses `/docs/index.html`, and a precompressed variant keeps its suffix: `/app.js.br`. An `exact` rule for `/app.js` therefore does not cover `/app.js.br`; a `prefix` rule covers both.

```yaml [config.yaml]
cache:
  rules:
    - match:
        prefix: /assets/
      ttl: 1h
    - match:
        exact: /index.html
      ttl: 30s
      stale_if_error: 10m
    - match:
        prefix: /drafts/
      enabled: false
```

### `cache.rules[].match`

**Type:** mapping\
**Default:** none — required.

Exactly one of `exact` and `prefix`.

### `cache.rules[].match.exact`

**Type:** string\
**Default:** none — set either `exact` or `prefix`.

An absolute path, compared for equality. Write the decoded path (`/a b.html`, not `/a%20b.html`): percent-encoded sequences are rejected. Segments cannot be empty, `.` or `..`, and cannot contain `\` or control characters. A path ending in `/`, `/` included, is rejected because it never matches an object path; name the index object instead (`/docs/index.html`).

### `cache.rules[].match.prefix`

**Type:** string\
**Default:** none — set either `exact` or `prefix`.

An absolute path ending in `/`, with the same rules as `exact`. It matches the paths under it and the path without the trailing slash: `/assets/` matches `/assets/app.js` and `/assets`. `/` matches every path.

### `cache.rules[].enabled`

**Type:** boolean\
**Default:** none — inherits `cache.enabled`.

`false` bypasses the memory cache for the matching objects. `true` does not enable a cache that `cache.enabled: false` disables.

### `cache.rules[].ttl`

**Type:** duration\
**Default:** none — inherits `cache.ttl`.

Zero or positive. Replaces `cache.ttl` for the matching objects.

### `cache.rules[].negative_ttl`

**Type:** duration\
**Default:** none — inherits `cache.negative_ttl`.

Zero or positive. Replaces `cache.negative_ttl` for the matching objects.

### `cache.rules[].stale_if_error`

**Type:** duration\
**Default:** none — inherits `cache.stale_if_error`.

Zero or positive. Replaces `cache.stale_if_error` for the matching objects.

## `index`

**Type:** string\
**Default:** `index.html`

Name of the directory index object. A request path ending in `/` reads `<path><index>`. A path without the trailing slash, whose own object does not exist while `<path>/<index>` does, gets 308 Permanent Redirect to the path with a slash, query kept ([from path to object](../architecture/request-flow.md#from-path-to-object)). A single filename: no `/`, not `.` or `..`, no `\` or control characters ([Provide index files](../guide/publishing.md#provide-index-files)).

## `not_found`

**Type:** string\
**Default:** none — statica answers with a built-in plain-text 404 page.

Object key, relative to `s3.prefix`, of a custom 404 page, served with status 404 Not Found when no object, index redirect or single-page-app fallback matches the request. Origin errors and denied access never use it. The key has no leading or trailing `/`, no empty, `.` or `..` segments, and no `\` or control characters ([Add a custom 404 page](../guide/publishing.md#add-a-custom-404-page)).

## `errors`

**Type:** mapping\
**Default:** `{}`

How statica answers some origin errors.

### `errors.access_denied`

**Type:** string\
**Default:** `not_found`

Response to a 403 Forbidden from the origin: `not_found` answers 404 Not Found, `bad_gateway` answers 502 Bad Gateway. Either way, statica stops resolving the request (no index redirect, single-page-app fallback or `not_found` page) and stores no negative entry. See [Access denied](../architecture/access-denied.md), and [Choose how denials look to visitors](../guide/s3-providers.md#choose-how-denials-look-to-visitors).

::: warning
Without the `s3:ListBucket` permission, S3 answers 403 instead of 404 for an object that does not exist. Missing objects then never reach the `spa` fallback or the `not_found` page.
:::

## `redirects`

**Type:** list of mappings\
**Default:** none — no redirect rules.

Ordered redirect rules. statica evaluates them after the host, method and request-target checks and before reading the origin, so a rule also redirects paths that exist as objects ([redirect rules](../architecture/request-flow.md#redirect-rules)). The first matching rule answers with its status and a `Location` header. [Configure redirects](../guide/redirects.md) shows common rules.

| Item | Rules |
| --- | --- |
| Path | `from` and `to` are absolute paths. Each segment is literal text (percent-encoding allowed) or one placeholder that fills the whole segment. `from` matches a request path with the same number of segments; a trailing `/` counts as an empty last segment, so `/docs` and `/docs/` differ. |
| Placeholder | `{name}`: a letter or `_`, then letters, digits or `_`. Bound once in `from`; every placeholder in `to` must be bound in `from`. It matches one nonempty decoded value of at most 256 bytes, optionally constrained by `captures`. |
| Query in `from` | `?name=value&…` with literal, unique names; each value is literal or a placeholder. Every listed field must appear exactly once in the request, and other fields are allowed. When the path matches, a listed field that appears more than once answers 400 Bad Request. |
| Destination | A local absolute path, or `https://` and a literal host with an optional [port](#ports), followed by an optional path. Then an optional query, with placeholders allowed in values, and an optional literal fragment. `http://` destinations are rejected. |
| Query forwarding | [`redirects[].query`](#redirects-query): `drop`, `preserve` or `unmatched`. |
| Status | 301, 302, 307 or 308. |
| Size limits | Each template is at most 8 KiB. A `Location` longer than 8 KiB answers 414 URI Too Long. |
| Rejected sets | Two rules with equivalent `from` templates, and literal rules that redirect into each other in a cycle. |
| Warnings | `config validate` warns on standard error when an earlier rule may match every request of a later rule: `redirects[1] may be shadowed by redirects[0]`. |

```yaml [config.yaml]
redirects:
  - from: /blog/{year}/{slug}
    to: /posts/{slug}?year={year}
    status: 301
    captures:
      year:
        pattern: "[0-9]{4}"
  - from: /docs/latest
    to: /docs/v2/
    status: 302
    query: preserve
```

### `redirects[].from`

**Type:** string\
**Default:** none — required.

Source template: an absolute path, then an optional `?query`. It cannot start with `//` or contain a fragment. A literal segment cannot decode to `.` or `..`, or to text containing `/`, `\`, a percent-encoded sequence or control characters. Empty segments are allowed only at the end. A literal segment that starts with `.` (after decoding) is rejected, except a first segment `.well-known`: requests for such hidden paths get 404 Not Found before the rules run ([target validation](../architecture/request-flow.md#target-validation)).

### `redirects[].to`

**Type:** string\
**Default:** none — required.

Destination template. A placeholder in a path segment must hold a valid segment; a query value that contains `/` cannot fill a path segment, and such a request gets 400 Bad Request.

### `redirects[].status`

**Type:** integer\
**Default:** none — required.

301, 302, 307 or 308.

### `redirects[].query`

**Type:** string\
**Default:** `drop`

What happens to the query of the request:

| Value | `Location` query |
| --- | --- |
| `drop` | Only the query written in `to`, if any. |
| `preserve` | The whole request query, unchanged except that bytes that could alter the header are percent-encoded. `to` must have no query. |
| `unmatched` | The request's query fields, in their order, without those named in the query of `from`. `to` must have no query. |

### `redirects[].captures`

**Type:** mapping of mappings\
**Default:** none — placeholders accept any nonempty value of up to 256 bytes.

Constraints on placeholders, by placeholder name. Each name must be bound in `from`.

### `redirects[].captures.<name>`

**Type:** mapping\
**Default:** none — the placeholder is unconstrained.

The constraint for placeholder `<name>`.

### `redirects[].captures.<name>.pattern`

**Type:** string\
**Default:** none — required.

Nonempty RE2 regular expression that the whole decoded value must match: statica anchors it at both ends.

## `headers`

**Type:** mapping\
**Default:** `{}`

Static response headers.

### `headers.set`

**Type:** mapping of strings\
**Default:** `{}`

Headers added to every response statica writes on the public listener, errors and redirects included, but not to the admin listener. Responses that the Go HTTP server writes before statica handles the request (431 for oversized headers, 400 for some malformed requests) carry none of them. `X-Content-Type-Options: nosniff` is always sent, and this setting cannot change it ([response safety](../architecture/security.md#response-safety)). Names are HTTP header names, case-insensitive, unique after canonicalization, and not reserved:

| Group | Reserved names |
| --- | --- |
| Hop-by-hop | `Connection`, `Keep-Alive`, `Proxy-Authenticate`, `Proxy-Authorization`, `Proxy-Connection`, `TE`, `Trailer`, `Transfer-Encoding`, `Upgrade` |
| Set by statica | `X-Request-ID`, `X-Content-Type-Options`, `Location`, `Vary`, `Date`, `Age`, `Accept-Ranges` |
| Representation and validators | `Content-Length`, `Content-Type`, `Content-Encoding`, `Content-Range`, `Content-Disposition`, `Content-Language`, `ETag`, `Last-Modified` |
| Caching (use [`cache_control`](#cache-control)) | `Cache-Control`, `Expires` |
| CORS (use [`cors`](#cors)) | every name starting with `Access-Control-` |
| Credentials and routing | `Host`, `Authorization`, `WWW-Authenticate`, `Cookie`, `Set-Cookie` |

A name that is not a valid HTTP header name is reported for `headers.set` as a whole. A reserved or duplicate name, and an invalid value, are reported under `headers.set.<name>`, as are a value that is not a string or whose substitution fails. [Add static headers](../guide/headers-and-cors.md#add-static-headers) shows common security headers, and [Response headers](./http-responses.md#response-headers) lists the headers statica sets itself.

```yaml [config.yaml]
headers:
  set:
    Content-Security-Policy: "default-src 'self'"
    Referrer-Policy: strict-origin-when-cross-origin
    Strict-Transport-Security: max-age=31536000
```

### `headers.set.<name>`

**Type:** string\
**Default:** none — one entry per header.

Value of the header `<name>`, sent as written. It is nonempty and may contain tabs but no other control characters (C0, `DEL` and C1).

## `cache_control`

**Type:** mapping\
**Default:** `{}`

The `Cache-Control` header of responses that serve an object with status 200 OK, and of the 206 Partial Content and 304 Not Modified responses derived from them (a 304 also keeps `Expires` and `Age`). Every other response, the `not_found` page and the single-page-app fallback included, carries `Cache-Control: no-store`. This block does not affect the memory cache. See [Cache-Control and Age](../architecture/cache-control.md), and [Control browser and CDN caching](../guide/browser-caching.md) for common policies.

```yaml [config.yaml]
cache_control:
  rules:
    - match:
        prefix: /assets/
      value: public, max-age=31536000, immutable
    - match:
        exact: /
      value: no-cache
```

### `cache_control.default`

**Type:** string\
**Default:** `auto`

Value for the requests no rule matches: `auto` or a `Cache-Control` value.

`auto` follows the origin's metadata, in this order ([precedence](../architecture/cache-control.md#precedence)):

1. Malformed origin cache metadata: `no-store`.
2. An origin `Cache-Control`: sent unchanged, with the origin `Expires` if present.
3. An origin `Expires` only: sent unchanged, without `Cache-Control`.
4. Neither: `max-age` set to the object's memory-cache TTL in seconds, when the memory cache is enabled for the object and the TTL is at least 1 second; otherwise `no-cache`.

An explicit value replaces the origin's `Cache-Control` and `Expires`, an origin `no-store` included. A value must follow these rules:

| Rule | Detail |
| --- | --- |
| Members | Directives separated by commas; spaces and tabs are allowed around commas and `=`. |
| Directive | A token name, optionally `=` and a token or quoted-string argument. Each name at most once, ignoring case. |
| Numeric arguments | `max-age`, `s-maxage`, `min-fresh`, `stale-if-error` and `stale-while-revalidate` require an argument of digits (quoted digits are accepted). `max-stale` takes an optional one. |
| Other directives | Accepted, extensions included. |
| Rejected | An empty value, empty members (`public,,max-age=60`, a trailing comma), control characters other than tab, invalid UTF-8. |

### `cache_control.rules`

**Type:** list of mappings\
**Default:** none — `cache_control.default` applies to every request.

The first rule whose `match` matches the decoded request path sets the value. Unlike `cache.rules`, matching uses the request path itself: a request for `/docs/` is matched as `/docs/`, not as `/docs/index.html` ([which path a rule matches](../architecture/cache-control.md#which-path-a-rule-matches)).

### `cache_control.rules[].match`

**Type:** mapping\
**Default:** none — required.

Exactly one of `exact` and `prefix`.

### `cache_control.rules[].match.exact`

**Type:** string\
**Default:** none — set either `exact` or `prefix`.

An absolute path compared for equality with the request path, with the syntax of [`cache.rules[].match.exact`](#cache-rules-match-exact). A path ending in `/` matches that directory request.

### `cache_control.rules[].match.prefix`

**Type:** string\
**Default:** none — set either `exact` or `prefix`.

An absolute path ending in `/`, matched like [`cache.rules[].match.prefix`](#cache-rules-match-prefix).

### `cache_control.rules[].value`

**Type:** string\
**Default:** none — required.

`auto` or a `Cache-Control` value, with the rules of `cache_control.default`.

## `cors`

**Type:** mapping\
**Default:** none — statica sends no CORS headers, and `OPTIONS` gets 405 Method Not Allowed.

Cross-origin access to `GET` and `HEAD` ([methods and CORS preflight](../architecture/request-flow.md#methods-and-cors-preflight)). With this block:

- Responses to requests that pass the host, method and request-target checks carry `Vary: Origin`.
- A request whose single `Origin` header matches a listed origin gets `Access-Control-Allow-Origin` with that origin. Requests from other origins are served without it.
- `OPTIONS` is handled as a preflight. A listed origin asking for `GET` or `HEAD`, with request headers among `Range`, `If-Match`, `If-None-Match`, `If-Modified-Since`, `If-Unmodified-Since` and `If-Range`, gets 204 No Content with `Access-Control-Allow-Methods: GET, HEAD`. Every other `OPTIONS` request gets 403 Forbidden.
- `Access-Control-Allow-Credentials`, `Access-Control-Expose-Headers` and `Access-Control-Max-Age` are never sent.

See [Allow cross-origin reads](../guide/headers-and-cors.md#allow-cross-origin-reads) for a walkthrough, and [Vary](./http-responses.md#vary) for the `Vary` values of each response.

### `cors.allowed_origins`

**Type:** list of strings\
**Default:** none — required when `cors` is present.

Nonempty list of exact origins, `scheme://host` with an optional [port](#ports). The scheme is `http` or `https`; there is no path (not even `/`), query, fragment, credentials or wildcard. Hostnames are compared after normalization and default ports are ignored, so `https://example.com:443` and `https://example.com` are the same origin; equivalent entries are rejected.

## `compression`

**Type:** mapping\
**Default:** `{}`

Responses are never compressed on the fly; statica can serve precompressed variants published next to an object ([Precompressed variants](../architecture/precompressed-variants.md)). [Serve precompressed files](../guide/precompressed-files.md) shows how to produce and upload them.

```yaml [config.yaml]
compression:
  mode: precompressed
  encodings: [br, gzip]
```

### `compression.mode`

**Type:** string\
**Default:** `off`

- `off`: statica serves the object itself.
- `precompressed`: for a request with an `Accept-Encoding` header, statica also considers the variants `<key>.br` (Brotli) and `<key>.gz` (gzip) and serves the preferred one that exists, with `Content-Encoding` and `Vary: Accept-Encoding`.

In both modes, when the object exists but the request accepts none of its available codings, the response is 406 Not Acceptable.

### `compression.encodings`

**Type:** list of strings\
**Default:** `[br, gzip]` in `precompressed` mode.

The codings statica looks for: `br`, `gzip`, or both, without duplicates. When the client accepts several with the same quality, the list order decides. Not allowed in `off` mode.

## `spa`

**Type:** mapping\
**Default:** none — no single-page-app fallback.

Serves an app shell for client-side routes ([eligibility rules](../architecture/spa-fallback.md#eligibility-rules); [Serve a single-page application](../guide/single-page-apps.md)). When the object for a request does not exist and there is no index redirect, statica serves `spa.fallback` with status 200 OK, `Cache-Control: no-store` and `Vary: Accept`, provided that:

- the request's `Accept` header explicitly accepts `text/html` with a nonzero quality;
- the path is not under one of `spa.exclude_prefixes`;
- the path is under one of `spa.include_prefixes` or, when that list is omitted, its last segment contains no `.`.

Otherwise, or when the fallback object does not exist, the request continues to the `not_found` page.

```yaml [config.yaml]
spa:
  fallback: index.html
  exclude_prefixes: [/assets/, /api/]
```

### `spa.fallback`

**Type:** string\
**Default:** none — required when `spa` is present.

Object key of the app shell, relative to `s3.prefix`, with the rules of `not_found`.

### `spa.include_prefixes`

**Type:** list of strings\
**Default:** none — every path whose last segment has no `.` qualifies.

When present, a nonempty list: the fallback applies only under these prefixes, including paths whose last segment contains a `.`. Each entry is a unique absolute path ending in `/`, with the syntax of [`cache.rules[].match.prefix`](#cache-rules-match-prefix). `/app/` matches `/app` and every path under `/app/`.

### `spa.exclude_prefixes`

**Type:** list of strings\
**Default:** none — no path is excluded.

Paths under these prefixes never get the fallback, even when they are also under an included prefix. Same syntax as `spa.include_prefixes`; an empty list excludes nothing.

## `observability`

**Type:** mapping\
**Default:** `{}`

Logs, written as logfmt events on standard output. The [log reference](./logs.md) lists every event and field, and [Work with the logs](../guide/logs.md) shows how to read them.

### `observability.log_level`

**Type:** string\
**Default:** `info`

Minimum level of the events written: `debug`, `info`, `warn` or `error`. It applies to every event, request events included; their level depends on the outcome of the request ([levels](./logs.md#levels)).

### `observability.access_log`

**Type:** boolean\
**Default:** `true`

`false` stops the [`http_request`](./logs.md#http-request) events only. Metrics are not affected.

### `observability.log_client_ip`

**Type:** boolean\
**Default:** `true`

`false` leaves `client_ip`, `peer_ip`, `client_ip_source` and `client_ip_reason` out of the `http_request` events ([client addresses](./logs.md#client-addresses)).

### `observability.redact_query_params`

**Type:** list of strings\
**Default:** `[token, access_token, api_key, password, secret, signature, X-Amz-Credential, X-Amz-Signature, X-Amz-Security-Token]`

Query parameters whose values are replaced with `[REDACTED]` in the logged `path`. Names are literal, not patterns, and are compared with the decoded parameter name, ignoring ASCII case. Each entry is nonempty, has no control characters and is unique ignoring case. A list replaces the default list; `[]` logs queries without redaction. A query that cannot be decoded is left out of the logged path, also with `[]`. Responses are not affected ([query redaction](./logs.md#query-redaction)).
