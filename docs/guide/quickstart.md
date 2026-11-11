# Quickstart

Install **statica**, point it at a bucket, and serve the bucket's `index.html` on your machine.

## Prerequisites

- Linux or macOS, on amd64 or arm64, to install a release binary; or Go 1.27.4 or later with Git and Make, to build statica from source.
- A bucket on AWS S3 or on an S3-compatible server, with an `index.html` object at its root ([Publish a site to S3](./publishing.md)).
- Credentials that can read the bucket's objects, available through the standard AWS credential chain: environment variables, a profile in the shared AWS configuration files, or the role of the machine or container. On AWS, `s3:GetObject` is enough; without `s3:ListBucket`, a missing object looks like a denied one (see [Pitfalls](#pitfalls)).
- `curl`, to try the result.

## Install statica

Install the release binary for your system, as in [Release archive](./installation.md#release-archive), and check it:

```sh
statica version
```

The output starts with `statica` and the version you installed. To run statica in a container instead, follow [Container image](./installation.md#container-image) and adapt the commands of this guide.

To build statica from source instead, clone the repository, build the binary and add it to your `PATH` for this shell:

```sh
git clone https://github.com/gtriggiano/statica.git
cd statica
make build
export PATH="$PWD/bin:$PATH"
cd ..
statica version
```

```text
statica dev (revision unknown; go1.27.4)
```

A build from source reports the version `dev` and the revision `unknown`. The last part is the Go version that built the binary.

## Write a configuration

Create a working directory. statica reads `config.yaml` from the working directory unless you name another file:

```sh
mkdir statica-quickstart
cd statica-quickstart
```

Create `config.yaml` with your bucket's name and region:

```yaml [config.yaml]
s3:
  bucket: example-websites
  region: eu-west-1
```

[`s3.bucket`](../reference/configuration.md#s3-bucket) is required. [`s3.region`](../reference/configuration.md#s3-region) is optional: without it, statica takes the region from the AWS configuration when it starts, and fails when it finds none. Every other setting keeps its default.

For a bucket on an S3-compatible server, write the configuration in [Use an S3-compatible server](#use-an-s3-compatible-server) instead.

## Validate the configuration

```sh
statica config validate
```

```text
configuration valid
```

`config validate` works offline: it checks the file, not the bucket or the credentials. To see the configuration with every default applied, print it:

```sh
statica config print
```

```text
server:
    listen: 127.0.0.1:8080
    ...
admin:
    listen: 127.0.0.1:9090
s3:
    bucket: example-websites
    region: eu-west-1
    prefix: ""
    ...
cache:
    enabled: true
    max_bytes: 128MiB
    ...
index: index.html
...
```

The public listener uses `127.0.0.1:8080` and the admin listener `127.0.0.1:9090`, both reachable from this machine only. The whole bucket is the site, `index.html` answers paths that end in `/`, and the memory cache holds up to 128 MiB.

## Check access to the bucket

Make your credentials available in this shell if AWS tools on this machine do not already find them, for example:

```sh
export AWS_ACCESS_KEY_ID='your-access-key-id'
export AWS_SECRET_ACCESS_KEY='your-secret-access-key'
```

Then check that statica can read the bucket:

```sh
statica config check-origin
```

```text
origin object accessible
```

`config check-origin` reads the metadata of the `index` object, `index.html`, with your credentials. It changes nothing in the bucket. If it fails, see [Pitfalls](#pitfalls).

## Start the server

```sh
statica serve
```

statica logs one event per line on standard output. When both listeners are bound, it logs a `startup` event with its settings (shortened here; your values differ):

```text
ts=2026-11-08T00:13:23.192Z level=info event=startup version=dev revision=unknown go_version=go1.27.4 public_listen=127.0.0.1:8080 admin_listen=127.0.0.1:9090 tls=off hosts=any ...
```

Leave it running, and open a second terminal for the next steps.

## Verify it

Request the site. The headers and the page in the output below are examples: yours come from your own object.

```sh
curl -i http://127.0.0.1:8080/
```

```text
HTTP/1.1 200 OK
Accept-Ranges: bytes
Cache-Control: max-age=300
Content-Length: 63
Content-Type: text/html; charset=utf-8
Date: Sun, 08 Nov 2026 00:13:24 GMT
Etag: "8f70e53699b0f5ac39f98974e5b7572e"
Last-Modified: Sun, 08 Nov 2026 00:12:47 GMT
Vary: Accept-Encoding
X-Content-Type-Options: nosniff
X-Request-Id: 2ac785d84e8a41107536f0ad136dcecf

<!doctype html>
<title>Hello</title>
<p>Hello from statica</p>
```

The path `/` ends in `/`, so statica served the `index.html` object. In the first terminal, statica logged an `http_request` event for the request. Request the page a second time:

```sh
curl -s -o /dev/null http://127.0.0.1:8080/
```

Compare the two events:

```text
ts=2026-11-08T00:13:24.879Z level=info event=http_request request_id=2ac785d84e8a41107536f0ad136dcecf request_id_source=generated host=127.0.0.1 method=GET path=/ status=200 outcome=success resolution=index cache=miss origin_key=index.html duration_ms=166.621 ttfb_ms=166.464 bytes_written=63 client_ip=127.0.0.1 peer_ip=127.0.0.1 client_ip_source=peer
ts=2026-11-08T00:13:24.951Z level=info event=http_request request_id=99e03172132e7ad9a8494777ee0d3580 request_id_source=generated host=127.0.0.1 method=GET path=/ status=200 outcome=success resolution=index cache=hit origin_key=index.html duration_ms=1.005 ttfb_ms=0.208 bytes_written=63 client_ip=127.0.0.1 peer_ip=127.0.0.1 client_ip_source=peer
```

The first request read the object from the bucket (`cache=miss`); the second was answered from the memory cache (`cache=hit`). The [log reference](../reference/logs.md#http-request) describes every field.

Then query the admin listener. `/livez` reports that the process is running and `/readyz` that it is ready for traffic:

```sh
curl http://127.0.0.1:9090/livez
curl http://127.0.0.1:9090/readyz
```

```text
ok
ready
```

::: danger Keep the admin listener private
The admin endpoints have no authentication. If you change [`admin.listen`](../reference/configuration.md#admin-listen), bind it to loopback or to a private network, never to a public address ([the admin listener](../architecture/security.md#the-admin-listener)).
:::

`/metrics` serves Prometheus metrics. The request counters reflect the two requests:

```sh
curl -s http://127.0.0.1:9090/metrics | grep -E '^statica_(http|cache)_requests_total'
```

```text
statica_cache_requests_total{host="127.0.0.1",outcome="hit"} 1
statica_cache_requests_total{host="127.0.0.1",outcome="miss"} 1
statica_http_requests_total{host="127.0.0.1",method="GET",outcome="success",status="200"} 2
```

## Stop the server

Press Ctrl-C in the first terminal. statica stops accepting connections, lets active requests finish within [`server.shutdown_timeout`](../reference/configuration.md#server-shutdown-timeout), `30s` by default, logs the end of the shutdown and exits with status 0:

```text
ts=2026-11-08T00:13:25.771Z level=info event=shutdown_started reason=signal shutdown_timeout=30s
ts=2026-11-08T00:13:25.774Z level=info event=shutdown_complete reason=signal outcome=drained duration_ms=2.998
```

## Use an S3-compatible server

For a bucket on an S3-compatible server, write this configuration instead of the one in [Write a configuration](#write-a-configuration):

```yaml [config.yaml]
s3:
  bucket: example-websites
  region: us-east-1
  endpoint: http://127.0.0.1:9000
  path_style: true
  allow_http: true
```

- [`s3.endpoint`](../reference/configuration.md#s3-endpoint) is the server's URL, with a scheme, a host and an optional port.
- [`s3.path_style`](../reference/configuration.md#s3-path-style) puts the bucket name in the path (`http://127.0.0.1:9000/example-websites/index.html`), which many S3-compatible servers require.
- [`s3.allow_http`](../reference/configuration.md#s3-allow-http) permits an `http` endpoint. Use it only for a server on your machine or on a trusted network, and an `https` endpoint otherwise.
- [`s3.region`](../reference/configuration.md#s3-region) is the region that the server expects in request signatures: use the region your server is configured with.

Give statica the access key and secret that the server issued, through the same AWS credential chain, and continue with [Validate the configuration](#validate-the-configuration). The `startup` event then reports `s3_endpoint=custom`:

```text
... s3_endpoint=custom s3_region=us-east-1 s3_path_style=true s3_allow_http=true ...
```

## Pitfalls

- **`origin check failed: origin unavailable`, or `origin check timed out`.** No credentials were found, so no request was sent. While it looks for credentials, the AWS SDK may spend several seconds trying to reach a machine role, and the check then reports `origin check timed out` instead. Otherwise the endpoint could not be reached or, on AWS, the region is wrong. Check the credential chain, `s3.endpoint` and `s3.region`.
- **`origin check failed: origin configuration`.** The bucket does not exist: check [`s3.bucket`](../reference/configuration.md#s3-bucket). Or no valid region was found: set `s3.region`, or `AWS_REGION` in the environment. Without a region, `statica serve` fails at startup too, with `statica: cannot initialize the origin client`, logs `startup_failed stage=origin_client` and exits with status 1.
- **`origin check failed: access denied`.** The origin answered 403 Forbidden: the credentials were rejected, or they may not read `index.html`. On AWS without `s3:ListBucket`, S3 also answers 403 for an object that does not exist, so a missing or misnamed `index.html`, or a wrong [`s3.prefix`](../reference/configuration.md#s3-prefix), shows up here too.
- **`origin check failed: object not found`.** On S3-compatible servers, or on AWS with `s3:ListBucket`: the bucket has no `index.html` at its root, or at the root of `s3.prefix` when you set one.
- **`statica serve` starts without valid credentials.** It does not check the credentials or the bucket at startup, and `/readyz` answers `ready` unless the origin health check is enabled ([Health checks and readiness](./health-checks.md)). Without credentials, every request that reads the bucket gets 502 Bad Gateway, logged with `error_class=origin_unavailable`, or 504 Gateway Timeout, logged with `error_class=origin_timeout`, while the AWS SDK looks for a machine role. With credentials that the origin rejects with 403 Forbidden, it gets 404 Not Found by default, logged with `error_class=origin_denied`. Run `config check-origin` before `serve`.
- **`statica: cannot bind the public listener`.** Another process uses port 8080. Set [`server.listen`](../reference/configuration.md#server-listen) to a free address. For `cannot bind the admin listener`, set `admin.listen`.
- **The site is not reachable from another machine.** The default listeners bind `127.0.0.1`. Set `server.listen` to `:8080` to accept connections on every interface, or put statica behind a proxy ([Run behind a reverse proxy, CDN or ingress](./behind-a-proxy.md)). Keep the admin listener private.
- **On AWS, missing pages are logged as origin errors.** Without the `s3:ListBucket` permission, AWS S3 answers 403 Forbidden for an object that does not exist. statica answers 404 Not Found by default, but logs the request at `error` level, skips the custom 404 page and the single-page-app fallback, and no longer redirects `/guide` to `/guide/`: `/guide` answers 404 ([the `s3:ListBucket` trade-off](./s3-providers.md#the-s3-listbucket-trade-off)).

[Common failures](./s3-providers.md#common-failures) lists every `check-origin` message with its likely cause, and [Troubleshooting](./troubleshooting.md) goes from symptoms to causes.

## Related pages

- [What is statica?](./what-is-statica.md)
- [Configuration basics](./configuration.md)
- [Publish a site to S3](./publishing.md)
- [Connect to S3 and S3-compatible providers](./s3-providers.md)
- [Deploy, roll out and shut down](./deployment.md)
- [CLI reference](../reference/cli.md)
- [Configuration examples](../reference/examples.md)
- [Startup, readiness and shutdown](../architecture/lifecycle.md)
