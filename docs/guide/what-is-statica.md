---
description: "What statica does, where it fits in a deployment, its configuration at a glance and what it leaves to other tools."
---

# What is statica?

**statica** is a web server for static websites stored in S3. This page describes what it does, where it fits in a deployment, and what it leaves to other tools.

## What it is

- **One bucket, one site.** An instance serves one S3 bucket, or one publication prefix inside it, as one website. That bucket and prefix are its origin. Every hostname it accepts gets the same content.
- **One binary.** statica is a single Go binary. The same binary runs the server (`statica serve`) and the commands that validate a configuration and check the origin.
- **Two listeners.** The public listener serves the site. A separate admin listener serves liveness, readiness and Prometheus metrics, and belongs on a private network.
- **A bounded memory cache.** Objects read from the bucket are kept in memory, within byte and entry limits that you configure, and checked against the bucket before an expired copy is used again.

## The problem it solves

A bucket holds objects, not a website. To serve a site from it, something has to map URLs to object keys, answer index paths and missing pages, speak HTTP the way browsers and CDNs expect, limit how much traffic reaches the bucket, and report what it is doing.

statica does this with one YAML file and read access to the bucket. It signs its requests with credentials from the standard AWS credential chain, so the bucket can stay private, and it does not use S3 static website hosting. It reads objects and their metadata, and never writes, lists or deletes them ([the origin is the source of truth](../architecture/index.md#design-principles)).

## How it fits in a deployment

- **Directly.** Bind [`server.listen`](../reference/configuration.md#server-listen) to a public address and configure [`server.tls`](../reference/configuration.md#server-tls) for HTTPS with HTTP/2 ([Serve HTTPS directly](./tls.md)).
- **Behind a CDN, reverse proxy, load balancer or ingress.** List the proxy's addresses in [`server.trusted_proxies`](../reference/configuration.md#server-trusted-proxies): statica then takes the client IP and the request ID from the proxy's headers ([Run behind a reverse proxy, CDN or ingress](./behind-a-proxy.md)).
- **One instance per site.** To serve several sites, run one instance for each. Instances that serve the same site do not share their memory caches: each reads the bucket on its own.
- **Operated like any service.** It logs to standard output, exposes `/livez`, `/readyz` and `/metrics` on the admin listener, stops gracefully on SIGINT or SIGTERM, and applies a configuration change when it restarts ([Deploy on Kubernetes](./kubernetes.md)).

## Features

- **Website paths.** The [`index`](../reference/configuration.md#index) object for paths that end in `/`, a redirect that adds a missing trailing slash, and an optional custom 404 page ([Publish a site to S3](./publishing.md)).
- **Redirect rules.** Path placeholders and query handling, answered without reading the bucket ([Configure redirects](./redirects.md)).
- **Single-page apps.** An app shell for client-side routes ([Serve a single-page application](./single-page-apps.md)).
- **Precompressed files.** The Brotli and gzip variants that you publish next to an object ([Serve precompressed files](./precompressed-files.md)).
- **A bounded memory cache.** Byte and entry limits, shared origin reads for concurrent misses, revalidation of expired objects, negative entries and optional stale-if-error ([Tune the memory cache](./cache-tuning.md)).
- **HTTP semantics.** Conditional requests, single byte ranges, `Cache-Control` and `Age` ([Control browser and CDN caching](./browser-caching.md)).
- **Response headers and CORS.** Your own headers on every response, and CORS grants for the origins you list ([Response headers and CORS](./headers-and-cors.md)).
- **HTTPS.** TLS with HTTP/2 on the public listener, from a certificate you provide ([Serve HTTPS directly](./tls.md)).
- **S3 and compatible origins.** AWS S3 or a custom endpoint, credentials from the standard AWS credential chain, and switches for optional origin capabilities ([Connect to S3 and S3-compatible providers](./s3-providers.md)).
- **Logs, metrics and health checks.** Redacted logfmt events on standard output, Prometheus metrics, liveness and readiness with an optional origin health check ([Work with the logs](./logs.md), [Monitor with Prometheus](./metrics.md), [Health checks and readiness](./health-checks.md)).
- **Confinement.** Every object key stays inside the publication prefix, and the `Host` header never selects content ([Security model](../architecture/security.md)).
- **Explicit limits.** Caps on connections, concurrent requests, header size, cache memory and the time of each request phase, with no queue: statica refuses work beyond them ([explicit resource limits](../architecture/index.md#design-principles)).
- **Checks before start.** An invalid configuration is rejected before any socket opens, and commands validate a file and check access to the origin offline or on demand ([CLI reference](../reference/cli.md)).

## The configuration at a glance

One YAML file configures an instance. Only [`s3.bucket`](../reference/configuration.md#s3-bucket) is required; every other field has a default.

| Block | What it controls | Read more |
| --- | --- | --- |
| [`s3`](../reference/configuration.md#s3) | The bucket, region, publication prefix, endpoint and origin timeouts. | [Connect to S3 and S3-compatible providers](./s3-providers.md) |
| [`server`](../reference/configuration.md#server) | The public listener: address, TLS, trusted proxies, limits and timeouts. | [Serve HTTPS directly](./tls.md), [Run behind a reverse proxy, CDN or ingress](./behind-a-proxy.md) |
| [`admin`](../reference/configuration.md#admin) | The admin listener and the origin health check. | [Health checks and readiness](./health-checks.md) |
| [`hosts`](../reference/configuration.md#hosts) | The hostnames that the instance accepts. | [Run behind a reverse proxy, CDN or ingress](./behind-a-proxy.md) |
| [`cache`](../reference/configuration.md#cache) | The memory cache: limits, lifetimes and per-path rules. | [Tune the memory cache](./cache-tuning.md) |
| [`cache_control`](../reference/configuration.md#cache-control) | The `Cache-Control` sent to browsers and CDNs. | [Control browser and CDN caching](./browser-caching.md) |
| [`index`](../reference/configuration.md#index), [`not_found`](../reference/configuration.md#not-found), [`spa`](../reference/configuration.md#spa) | Index objects, the custom 404 page and the single-page-app shell. | [Publish a site to S3](./publishing.md), [Serve a single-page application](./single-page-apps.md) |
| [`redirects`](../reference/configuration.md#redirects) | Redirect rules. | [Configure redirects](./redirects.md) |
| [`headers`](../reference/configuration.md#headers), [`cors`](../reference/configuration.md#cors) | Response headers and CORS grants. | [Response headers and CORS](./headers-and-cors.md) |
| [`compression`](../reference/configuration.md#compression) | Precompressed variants. | [Serve precompressed files](./precompressed-files.md) |
| [`errors`](../reference/configuration.md#errors) | The public answer to an origin denial. | [Connect to S3 and S3-compatible providers](./s3-providers.md#choose-how-denials-look-to-visitors) |
| [`observability`](../reference/configuration.md#observability) | Log level, access log and what the logs contain. | [Work with the logs](./logs.md) |

[Configuration examples](../reference/examples.md#advanced-configuration) shows a complete production-style file.

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

- [Quickstart](./quickstart.md): serve a bucket on your machine.
- [Deploy on Kubernetes](./kubernetes.md): run statica in production.
- [Architecture overview](../architecture/index.md): the design principles and components.
