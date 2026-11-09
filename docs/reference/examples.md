# Configuration examples

The repository ships four complete configuration files in `docs/examples/`. A Go test loads and validates each of them, so they stay valid as statica changes. This page shows each file and what you need to change before you use it. The [configuration reference](./configuration.md) describes every field, and [Configuration basics](../guide/configuration.md) explains the format, defaults and environment variables.

Check a file before you run it:

```sh
statica config validate --config config.yaml
statica config check-origin --config config.yaml
```

`config validate` is offline. `config check-origin` checks one object of the origin with a `HEAD` request and your credentials (see the [CLI reference](./cli.md#statica-config-check-origin)).

## Minimal configuration

<<< @/examples/minimal.yaml{yaml}

The smallest useful configuration: [`s3.bucket`](./configuration.md#s3-bucket) is the only required field, and [`s3.region`](./configuration.md#s3-region) is set so that statica does not need to discover it. Everything else has its default:

- the public listener on `127.0.0.1:8080` and the admin listener on `127.0.0.1:9090`, both reachable from the same machine only;
- the whole bucket is the site, with `index.html` served for paths ending in `/`;
- a memory cache of up to 128 MiB, and the built-in 404 page;
- every valid hostname accepted, and logs at `info` on standard output.

Adapt it:

- Set [`s3.bucket`](./configuration.md#s3-bucket) and [`s3.region`](./configuration.md#s3-region) to your bucket and its region, or omit the region to take it from the AWS configuration (`AWS_REGION` or the shared configuration files).
- Provide read-only credentials through the standard AWS credential chain: for example `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, a profile in the shared files, or the role of the machine or container. The configuration file never holds credentials.
- To accept connections from other machines, set [`server.listen`](./configuration.md#server-listen), for example to `:8080`.

## Container

<<< @/examples/container.yaml{yaml}

The minimal configuration, for the [container image](./container-image.md):

- [`server.listen`](./configuration.md#server-listen) and [`admin.listen`](./configuration.md#admin-listen) are set to `:8080` and `:9090`, every interface of the container: the default loopback addresses cannot be reached from outside it.
- The image reads the file from `/etc/statica/config.yaml`: mount it there.

Adapt it as the minimal configuration, and publish or expose port `9090` only to your health checks and Prometheus: the admin endpoints have no authentication ([the admin listener](../architecture/security.md#the-admin-listener)).

## Local S3-compatible server

<<< @/examples/local-s3.yaml{yaml}

A site served from an S3-compatible server running on the same machine, such as a development server:

- [`s3.endpoint`](./configuration.md#s3-endpoint) points at the server instead of AWS, and [`s3.allow_http`](./configuration.md#s3-allow-http) permits its `http` URL.
- [`s3.path_style`](./configuration.md#s3-path-style) sends requests as `http://127.0.0.1:9000/example-websites/<key>`, which many S3-compatible servers require.
- [`s3.prefix`](./configuration.md#s3-prefix) serves the objects under `demo/`: `/` reads `demo/index.html` and `/guide/` reads `demo/guide/index.html`.
- [`s3.region`](./configuration.md#s3-region) is the signing region, `us-east-1`.

Adapt it:

- Set the endpoint, bucket, prefix and region to those of your server.
- Give statica the server's access key through the AWS credential chain, for example with `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`.
- Use an `https` endpoint, without `s3.allow_http`, for any server that is not on a trusted local network.

The sample [`startup`](./logs.md#startup) and [`http_request`](./logs.md#format) events in the log reference were captured with this file.

## Advanced configuration

<<< @/examples/advanced.yaml{yaml}

A production-style instance that terminates TLS behind a load balancer. Most values are the defaults, written out so that you can see and change them. The settings that differ from the defaults are these:

- **Hosts.** [`hosts`](./configuration.md#hosts) accepts only `docs.example.com` and `www.docs.example.com`; other hostnames get 421. It also bounds the `host` label of the metrics.
- **TLS.** [`server.listen`](./configuration.md#server-listen) binds port 8443 on every interface, and [`server.tls`](./configuration.md#server-tls) serves HTTPS with the certificate chain and key in `/run/tls/`, with HTTP/2.
- **Proxies.** [`server.trusted_proxies`](./configuration.md#server-trusted-proxies) trusts the load balancer's network, `10.20.0.0/16`: statica takes the client IP from its `X-Forwarded-For` and the request ID from its `X-Request-ID`.
- **Origin health.** [`admin.origin_health`](./configuration.md#admin-origin-health) checks `health.txt` about every 30 seconds, and `/readyz` reports not ready until the first check succeeds.
- **Origin.** [`s3.bucket`](./configuration.md#s3-bucket) comes from the `SITE_BUCKET` environment variable ([environment variables](./configuration.md#environment-variables)), and [`s3.prefix`](./configuration.md#s3-prefix) selects one release of the site, `docs/releases/2026-11-06/`.
- **Cache rules.** [`cache.rules`](./configuration.md#cache-rules) keep objects under `/assets/` fresh in memory for up to one hour, and never cache `/health.txt` for visitors. The origin health check never uses the memory cache anyway.
- **Not found.** [`not_found`](./configuration.md#not-found) serves `404.html` from the release as the 404 page.
- **Compression.** [`compression.mode`](./configuration.md#compression-mode) serves the `.br` and `.gz` variants published next to an object, preferring Brotli when a client accepts both equally.
- **Redirects.** The [`redirects`](./configuration.md#redirects) rule sends `/page/x?postId=42` to `/blog/post/42` with 308, only when `postId` is a number, and drops the rest of the query.
- **Headers.** [`headers.set`](./configuration.md#headers-set) adds `Referrer-Policy` to every response.
- **Browser caching.** The [`cache_control`](./configuration.md#cache-control) rule tells browsers to keep `/assets/` for a year; other paths follow the origin's metadata (`auto`).

`errors.access_denied`, `cache_control.default` and the `observability` block, including the list of redacted query parameters, repeat the defaults.

Adapt it:

- Set `SITE_BUCKET` in the environment of every `statica config` and `statica serve` command, since an unset variable is an error, or write the bucket name instead. Set the region and the release prefix.
- Replace the hostnames, the certificate and key paths, and the proxy network with yours. `config validate` and `serve` read the TLS files and fail without them; `config print` shows the configuration without reading them.
- Set `server.client_ip_header` and `server.request_id_header` to the headers your load balancer actually sends.
- Publish `health.txt` and `404.html` under the release prefix, and the `.br` and `.gz` variants you want served. Without `health.txt`, readiness never becomes ready.
- Keep the admin listener on loopback, or bind it to a private address that your orchestrator and Prometheus can reach: probes from another machine or container cannot reach `127.0.0.1`.
- Replace the redirect rule, the header and the cache rules with your own.
