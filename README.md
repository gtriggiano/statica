<p align="left">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/public/banner-dark.svg">
    <source media="(prefers-color-scheme: light)" srcset="docs/public/banner-light.svg">
    <img alt="statica — Static websites served from S3" src="docs/public/banner-light.svg" width="680">
  </picture>
</p>

[![Latest release](https://img.shields.io/github/v/release/gtriggiano/statica?sort=semver)](https://github.com/gtriggiano/statica/releases/latest) [![CI](https://github.com/gtriggiano/statica/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/gtriggiano/statica/actions/workflows/ci.yml) [![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/gtriggiano/statica/badge)](https://scorecard.dev/viewer/?uri=github.com/gtriggiano/statica)

**statica** is a web server for static websites stored in S3. One instance serves one bucket, or a prefix inside it, as one website, reading objects through a bounded in-memory cache. It is a single Go binary configured by one YAML file, with a separate admin listener for health checks and Prometheus metrics.

## Features

- Index objects, trailing-slash redirects, a custom 404 page, redirect rules and an optional single-page-app fallback, without listing the bucket or using S3 website hosting.
- A memory cache bounded in bytes and entries, with shared fills for concurrent misses, revalidation against the origin, short-lived entries for missing objects and optional stale-if-error.
- Conditional requests, single byte ranges, precompressed Brotli and gzip variants, and `Cache-Control` from the origin's metadata or from configured rules.
- AWS S3 or S3-compatible services at a custom endpoint, with credentials from the standard AWS credential chain.
- HTTPS with HTTP/2, a host allowlist, client IP attribution through trusted proxies, and limits on connections, concurrent requests and request phases.
- logfmt access and lifecycle events, Prometheus metrics, liveness and readiness endpoints with an optional origin health check, and a graceful shutdown on SIGINT or SIGTERM.

## Quickstart

You need a bucket with an `index.html` object at its root, and credentials that can read it through the standard AWS credential chain, for example `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`. Write `config.yaml` with your bucket and its region. In a container, statica must listen on the container's interfaces:

```yaml
s3:
  bucket: example-websites
  region: eu-west-1
server:
  listen: ":8080"
admin:
  listen: ":9090"
```

Check that statica can read `index.html`, then start the server with the container image:

<!-- x-release-please-start-version -->
```sh
IMAGE=ghcr.io/gtriggiano/statica:0.0.0
```
<!-- x-release-please-end -->

```sh
docker run --rm -v "$PWD/config.yaml:/etc/statica/config.yaml:ro" \
  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY \
  "$IMAGE" config check-origin
docker run --rm --read-only -p 8080:8080 -p 127.0.0.1:9090:9090 \
  -v "$PWD/config.yaml:/etc/statica/config.yaml:ro" \
  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY \
  "$IMAGE"
```

The site is served on `http://127.0.0.1:8080/`, and `/livez`, `/readyz` and `/metrics` on `http://127.0.0.1:9090/`. Stop the server with Ctrl-C. The [installation guide](docs/guide/installation.md) covers the release binaries for Linux and macOS, their verification and the build from source, and the [quickstart guide](docs/guide/quickstart.md) explains each step.

## Documentation

The documentation is published at <https://gtriggiano.github.io/statica/>, with the documentation of each released version. Its sources are in [`docs/`](docs/), as a site built with [VitePress](https://vitepress.dev/):

- [Guide](docs/guide/what-is-statica.md): what statica is, the installation, the quickstart, and how to configure, deploy and monitor it.
- [Architecture](docs/architecture/index.md): how requests, the memory cache and the process lifecycle work.
- [Reference](docs/reference/configuration.md): configuration fields, CLI commands, HTTP responses, logs, metrics, admin endpoints, the container image, versioning and the changelog.
- [Examples](docs/examples/): complete configuration files, validated by the test suite.

To preview the site locally, run `make docs-install` once, then `make docs-dev`, or `make docs-build` followed by `make docs-preview` for the production build.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md) for the requirements, the checks, the documentation rules and the format of pull request titles, and [RELEASING.md](RELEASING.md) for the release process.

## License

statica is released under the [MIT License](LICENSE).
