# What is statica?

statica is a web server for static websites stored in S3. This page describes what it does, where it fits in a deployment, and what it leaves to other tools.

## What it is

- **One bucket, one site.** An instance serves one S3 bucket, or one publication prefix inside it, as one website. That bucket and prefix are its origin. Every hostname it accepts gets the same content.
- **One binary.** statica is a single Go binary. The same binary runs the server (`statica serve`) and the commands that validate a configuration and check the origin.
- **Two listeners.** The public listener serves the site. A separate admin listener serves liveness, readiness and Prometheus metrics, and belongs on a private network.
- **A bounded memory cache.** Objects read from the bucket are kept in memory, within byte and entry limits that you configure, and checked against the bucket before an expired copy is used again.

## The problem it solves

A bucket holds objects, not a website. To serve a site from it, something has to map URLs to object keys, answer index paths and missing pages, speak HTTP the way browsers and CDNs expect, limit how much traffic reaches the bucket, and report what it is doing.

statica does this with one YAML file and read access to the bucket. It signs its requests with credentials from the standard AWS credential chain, so the bucket can stay private, and it does not use S3 static website hosting. It reads objects and their metadata, and never writes, lists or deletes them ([the origin is the source of truth](../architecture/index.md#the-origin-is-the-source-of-truth)).

## How it fits in a deployment

- **Directly.** Bind [`server.listen`](../reference/configuration.md#server-listen) to a public address and configure [`server.tls`](../reference/configuration.md#server-tls) for HTTPS with HTTP/2 ([Serve HTTPS directly](./tls.md)).
- **Behind a CDN, reverse proxy, load balancer or ingress.** List the proxy's addresses in [`server.trusted_proxies`](../reference/configuration.md#server-trusted-proxies): statica then takes the client IP and the request ID from the proxy's headers ([Run behind a reverse proxy, CDN or ingress](./behind-a-proxy.md)).
- **One instance per site.** To serve several sites, run one instance for each. Instances that serve the same site do not share their memory caches: each reads the bucket on its own.
- **Operated like any service.** It logs to standard output, exposes `/livez`, `/readyz` and `/metrics` on the admin listener, stops gracefully on SIGINT or SIGTERM, and applies a configuration change when it restarts ([Deploy, roll out and shut down](./deployment.md)).

## Features

### Serving

- Serves the bucket, or the publication prefix set by [`s3.prefix`](../reference/configuration.md#s3-prefix), as one site ([Publish a site to S3](./publishing.md)).
- Maps paths to object keys: the [`index`](../reference/configuration.md#index) object for paths that end in `/`, a 308 Permanent Redirect that adds the trailing slash when only the index object exists, and an optional custom 404 Not Found page, [`not_found`](../reference/configuration.md#not-found) ([from path to object](../architecture/request-flow.md#from-path-to-object)).
- Answers [`redirects`](../reference/configuration.md#redirects) rules, with path placeholders and query handling, without reading the bucket ([Configure redirects](./redirects.md)).
- Serves an app shell for the client-side routes of a single-page app when you configure [`spa`](../reference/configuration.md#spa) ([Serve a single-page application](./single-page-apps.md)).
- Serves the Brotli and gzip variants that you publish next to an object, with [`compression.mode: precompressed`](../reference/configuration.md#compression-mode) ([Serve precompressed files](./precompressed-files.md)).

### Memory cache

- Holds objects up to [`cache.max_object_bytes`](../reference/configuration.md#cache-max-object-bytes) within [`cache.max_bytes`](../reference/configuration.md#cache-max-bytes) and [`cache.max_entries`](../reference/configuration.md#cache-max-entries), and streams larger objects without storing them ([limits and accounting](../architecture/memory-cache.md#limits-and-accounting)).
- Lets concurrent requests that miss the same object share one origin read, within configured limits ([shared fills](../architecture/memory-cache.md#shared-fills)).
- Keeps an object fresh for [`cache.ttl`](../reference/configuration.md#cache-ttl), or less when the origin's metadata asks for less. After that, it checks the object against the origin before using it again: with a conditional request when the object has an ETag and [`s3.features.conditional_reads`](../reference/configuration.md#s3-features-conditional-reads) is on, otherwise by reading it again ([freshness](../architecture/memory-cache.md#freshness), [revalidation](../architecture/memory-cache.md#revalidation)).
- Remembers missing objects for a short time ([negative entries](../architecture/memory-cache.md#negative-entries)) and can serve an expired copy while the origin fails ([stale-if-error](../architecture/memory-cache.md#stale-if-error)).
- Takes per-path overrides in [`cache.rules`](../reference/configuration.md#cache-rules) ([Tune the memory cache](./cache-tuning.md)).

### HTTP behavior

- Evaluates `If-None-Match`, `If-Modified-Since`, `If-Match` and `If-Unmodified-Since`, and serves single byte ranges with `If-Range` ([ranges and conditional requests](../architecture/ranges-and-conditionals.md)).
- Forwards the origin's `Cache-Control` or `Expires`, or sends a `max-age` taken from `cache.ttl` when the origin sets neither, unless [`cache_control`](../reference/configuration.md#cache-control) sets a value for the path. A response that serves an object, other than the SPA shell or a custom 404 page, carries `Age` once it is at least one second old ([Cache-Control and Age](../architecture/cache-control.md), [Control browser and CDN caching](./browser-caching.md)).
- Adds the headers you set in [`headers.set`](../reference/configuration.md#headers-set) and CORS grants for [`cors.allowed_origins`](../reference/configuration.md#cors-allowed-origins) ([Response headers and CORS](./headers-and-cors.md)). Every response it writes carries `X-Request-ID` and `X-Content-Type-Options: nosniff` ([response headers](../reference/http-responses.md#response-headers)).
- Serves HTTP/1.1, or HTTPS with HTTP/2 and HTTP/1.1 when `server.tls` is set ([TLS on the public listener](../architecture/security.md#tls-on-the-public-listener)).

### S3 and compatible origins

- Reads AWS S3, or an S3-compatible service at [`s3.endpoint`](../reference/configuration.md#s3-endpoint), with path-style requests ([`s3.path_style`](../reference/configuration.md#s3-path-style)) and plain HTTP for a local server ([`s3.allow_http`](../reference/configuration.md#s3-allow-http)) ([Connect to S3 and S3-compatible providers](./s3-providers.md)).
- Takes credentials from the standard AWS credential chain, never from its configuration file ([origin credentials](../architecture/security.md#origin-credentials)), and the region from [`s3.region`](../reference/configuration.md#s3-region) or the AWS configuration.
- Switches optional origin capabilities on or off with [`s3.features`](../reference/configuration.md#s3-features), and bounds each origin request with attempts and timeouts ([timeouts](../architecture/timeouts.md)).
- Answers an origin's 403 Forbidden as 404 Not Found or 502 Bad Gateway, as [`errors.access_denied`](../reference/configuration.md#errors-access-denied) says ([access denied](../architecture/access-denied.md)).

### Operations

- Writes logfmt events to standard output: startup and shutdown events and, by default ([`observability.access_log`](../reference/configuration.md#observability-access-log)), one `http_request` event for each public request that statica handles. The values of sensitive query parameters such as `token` are redacted. Requests that Go's HTTP server rejects before statica sees them are not logged ([log reference](../reference/logs.md), [Work with the logs](./logs.md)).
- Exposes Prometheus metrics on `/metrics` ([metrics reference](../reference/metrics.md), [Monitor with Prometheus](./metrics.md)).
- Answers `/livez` and `/readyz` on the admin listener, with an optional origin health check, [`admin.origin_health`](../reference/configuration.md#admin-origin-health) ([admin endpoints](../reference/admin-endpoints.md), [Health checks and readiness](./health-checks.md)).
- Drains requests on SIGINT or SIGTERM within one budget, [`server.shutdown_timeout`](../reference/configuration.md#server-shutdown-timeout) ([shutdown](../architecture/lifecycle.md#shutdown)).
- Validates and prints a configuration offline, and checks access to the origin, before you start the server ([CLI reference](../reference/cli.md)).

### Security and limits

- Keeps every object key inside the publication prefix: it refuses `..` segments, encoded separators and hidden segments instead of cleaning the path ([confinement](../architecture/security.md#requests-cannot-leave-the-publication-prefix)).
- Never lets the `Host` header select the origin, and can restrict accepted hostnames with [`hosts`](../reference/configuration.md#hosts) ([Host never selects the origin](../architecture/security.md#host-never-selects-the-origin)).
- Trusts forwarded client IPs and request IDs only from the proxies you list ([client IP attribution](../architecture/security.md#trusted-proxies-and-client-ip-attribution)).
- Caps connections, concurrent requests, header size, cache memory and the time of each request phase, and keeps no queue of its own: it refuses requests beyond [`server.max_concurrent_requests`](../reference/configuration.md#server-max-concurrent-requests), and clients beyond [`server.max_connections`](../reference/configuration.md#server-max-connections) wait in the operating system's connection backlog ([explicit resource limits](../architecture/index.md#explicit-resource-limits)).
- Rejects an invalid configuration before it opens any socket ([checks before anything starts](../architecture/lifecycle.md#checks-before-anything-starts)).
- Keeps credentials, provider error messages and redacted query values out of logs and command output ([safe logging](../architecture/security.md#safe-logging)).

## What statica does not do

- **Several sites per instance.** It serves one origin, and the `Host` header never selects a bucket or a prefix. Run one instance per site.
- **Visitor authentication.** It does not authenticate or authorize visitors.
- **Rate limiting or request filtering.** It has no per-client limits and no web application firewall. The only request limit is the global `server.max_concurrent_requests`.
- **Dynamic compression.** It serves the bytes stored in the bucket and never compresses a response itself.
- **Directory listings.** It never lists the bucket. A path that ends in `/` maps to its index object, never to a listing.
- **Redirects from the bucket.** It ignores S3 website routing rules and per-object website redirects, and serves HTML pages as stored. Redirects come only from the `redirects` rules and the trailing-slash redirect.
- **Hot reload.** It reads its configuration and TLS certificate once, at startup. Restart it to apply a change.
- **Disk cache.** The memory cache is not written to disk or shared between instances, and a restart empties it.
- **Purge API.** It has no way to invalidate cached objects on request. A change in the bucket becomes visible once the cached copy expires.
- **HTTP to HTTPS.** With `server.tls`, the public listener serves only HTTPS: there is no cleartext listener next to it and no redirect from HTTP to HTTPS.
- **Certificates.** It does not obtain or renew TLS certificates (no ACME) and does not verify client certificates (no mutual TLS).
- **Admin authentication.** The admin listener has no authentication and no TLS. Keep it on loopback or a private network ([the admin listener](../architecture/security.md#the-admin-listener)).
- **Writing to the bucket.** It never writes, deletes or lists objects. Publish your site with your own tools.

## Related pages

- [Quickstart](./quickstart.md): build statica and serve a bucket.
- [Configuration basics](./configuration.md): how the configuration file works.
- [Architecture overview](../architecture/index.md): the design principles and components.
- [Security model](../architecture/security.md): trust boundaries and what statica leaves to other layers.
