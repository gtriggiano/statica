# Quickstart

Point **statica** at a bucket and serve the bucket's `index.html` on your machine, with the release binary or with Docker.

## Prerequisites

- The `statica` binary on your `PATH` ([Installation](./installation.md#release-archive)), or Docker.
- A bucket on AWS S3 or on an S3-compatible service, with an `index.html` object at its root ([Publish a site to S3](./publishing.md)).
- Credentials that can read the bucket's objects ([Provide credentials](./s3-providers.md#provide-credentials)), here in the `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` environment variables of your shell.

## Write a configuration

In an empty directory, create `config.yaml` with your bucket's name and region:

```yaml [config.yaml]
s3:
  bucket: example-websites
  region: eu-west-1
```

Every other setting keeps its default ([Configuration basics](./configuration.md#a-minimal-configuration)). With Docker, add both listen addresses, because the defaults accept connections from inside the container only:

```yaml [config.yaml]
server:
  listen: ":8080"
admin:
  listen: ":9090"
```

For a bucket on an S3-compatible service, add the settings in [Configure an S3-compatible service](./s3-providers.md#configure-an-s3-compatible-service).

## Check the configuration and the bucket

`config validate` checks the file offline, and `config check-origin` reads the metadata of `index.html` with your credentials, without changing anything in the bucket:

::: code-group

```sh [Binary]
statica config validate
statica config check-origin
```

```sh [Docker]
docker run --rm -v "$PWD/config.yaml:/etc/statica/config.yaml:ro" \
  ghcr.io/gtriggiano/statica:{{VERSION}} config validate
docker run --rm -v "$PWD/config.yaml:/etc/statica/config.yaml:ro" \
  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY \
  ghcr.io/gtriggiano/statica:{{VERSION}} config check-origin
```

:::

```text
configuration valid
origin object accessible
```

If `check-origin` fails, [Common failures](./s3-providers.md#common-failures) lists each message with its likely cause.

## Start the server

::: code-group

```sh [Binary]
statica serve
```

```sh [Docker]
docker run --rm --read-only \
  -p 8080:8080 -p 127.0.0.1:9090:9090 \
  -v "$PWD/config.yaml:/etc/statica/config.yaml:ro" \
  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY \
  ghcr.io/gtriggiano/statica:{{VERSION}}
```

:::

statica logs one event per line on standard output, starting with a `startup` event when both listeners are bound. Leave it running, and open a second terminal.

## Verify it

Request the site. The headers and the page below are examples: yours come from your own object.

```sh
curl -i http://127.0.0.1:8080/
```

```text
HTTP/1.1 200 OK
Accept-Ranges: bytes
Cache-Control: max-age=300
Content-Length: 63
Content-Type: text/html; charset=utf-8
Etag: "8f70e53699b0f5ac39f98974e5b7572e"
...

<!doctype html>
<title>Hello</title>
<p>Hello from statica</p>
```

The path `/` ends in `/`, so statica served the `index.html` object. It logged an `http_request` event with `cache=miss`: the object came from the bucket. Request the page again, and the event says `cache=hit`: the answer came from the memory cache ([Work with the logs](./logs.md)).

Then ask the admin listener whether the process runs and whether it is ready for traffic:

```sh
curl http://127.0.0.1:9090/livez
curl http://127.0.0.1:9090/readyz
```

```text
ok
ready
```

The admin listener also serves Prometheus metrics on `/metrics`. Its endpoints have no authentication: [keep it private](../architecture/security.md#the-admin-listener).

## Stop the server

Press Ctrl-C in the first terminal. statica stops accepting connections, lets active requests finish and exits with status 0 ([shutdown](../architecture/lifecycle.md#shutdown)).

## Next steps

- [Configuration basics](./configuration.md): how the file works, and how to check a change.
- [Publish a site to S3](./publishing.md): lay out the bucket and release without downtime.
- [Deploy on Kubernetes](./kubernetes.md): run statica in production.
