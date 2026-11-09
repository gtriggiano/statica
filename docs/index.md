---
layout: home
hero:
  name: statica
  text: Static websites, served from S3
  tagline: >-
    A single Go binary that serves an S3 bucket, or a prefix inside it, as a
    website, with a bounded memory cache, conditional and range requests, and
    logs, metrics and health endpoints built in.
  image:
    light: /logo-light.svg
    dark: /logo-dark.svg
    alt: statica
  actions:
    - theme: brand
      text: Quickstart
      link: /guide/quickstart
    - theme: alt
      text: What is statica?
      link: /guide/what-is-statica
    - theme: alt
      text: How it works
      link: /architecture/
features:
  - title: One bucket, one site
    details: >-
      Serves a bucket or a publication prefix as a website: index objects,
      trailing-slash redirects, a custom 404 page, redirect rules and an
      optional single-page-app fallback. It never lists the bucket and does not
      use S3 website hosting.
    link: /guide/publishing
  - title: A bounded memory cache
    details: >-
      Bounded in bytes and entries. Concurrent GET misses for an object that fits
      the cache share one origin read, within the fill limits. Expired copies are checked
      against the origin, missing objects are remembered briefly, and an expired
      copy can optionally be served while the origin fails.
    link: /architecture/memory-cache
  - title: HTTP semantics
    details: >-
      Conditional requests, single byte ranges, optional Brotli and gzip variants
      published next to an object, <code>Cache-Control</code> from the origin's
      metadata or from your rules, and <code>Age</code>.
    link: /architecture/ranges-and-conditionals
  - title: S3 and compatible origins
    details: >-
      AWS S3 or an S3-compatible service at a custom endpoint, with optional
      path-style requests, credentials from the standard AWS credential chain, and
      switches for optional origin capabilities.
    link: /guide/s3-providers
  - title: Logs, metrics and health checks
    details: >-
      logfmt events on standard output with sensitive query values redacted,
      Prometheus metrics, and a separate admin listener for liveness, readiness
      and an optional origin health check.
    link: /guide/metrics
  - title: Explicit limits
    details: >-
      A strict configuration checked before any socket opens, object keys that
      cannot leave the publication prefix, caps on connections, concurrent
      requests and cache memory, and a graceful shutdown within one time budget.
    link: /architecture/#explicit-resource-limits
---

## A minimal configuration

This configuration serves the bucket `example-websites`, in the AWS region `eu-west-1`, as a website:

<<< @/examples/minimal.yaml{yaml}

Save it as `config.yaml` with your own bucket and region. With statica [installed](./guide/installation.md) and on your `PATH`, check the file and start the server:

```sh
statica config validate --config config.yaml
statica serve --config config.yaml
```

The defaults put the site on `127.0.0.1:8080` and the admin endpoints on `127.0.0.1:9090`, enable a memory cache of up to 128 MiB, and serve `index.html` for paths that end in `/`. The [Quickstart](./guide/quickstart.md) installs statica and walks through each step, and [Configuration basics](./guide/configuration.md) explains the file.

## How it fits

Visitors reach the public listener directly or through a CDN or ingress, and orchestrators and Prometheus use a separate admin listener.

```mermaid
flowchart LR
  accTitle: statica in a deployment
  accDescr: Visitors reach the public listener directly or through an optional CDN or ingress. Objects are read through the memory cache, which reads the one configured S3 bucket when it holds no usable copy. Orchestrators and Prometheus use the separate admin listener for liveness, readiness and metrics.
  V["Visitor"]
  P["CDN or ingress<br/>(optional)"]
  subgraph statica
    L["Public listener"]
    C["Memory cache"]
    A["Admin listener"]
  end
  B[("S3 bucket")]
  O["Orchestrator,<br/>Prometheus"]
  V --> P --> L
  V -.->|direct| L
  L --> C
  C -->|reads| B
  O -->|"/livez, /readyz,<br/>/metrics"| A
```

One instance serves one site. Instances that serve the same site do not share their memory caches.

## Where to go next

- [Guide](./guide/what-is-statica.md): install, configure, deploy and monitor statica, task by task.
- [Architecture](./architecture/index.md): how a request is mapped to objects, how the memory cache works, and how statica starts and shuts down.
- [Reference](./reference/configuration.md): every configuration field, CLI command, HTTP response, log event and metric.
