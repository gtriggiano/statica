---
layout: home
hero:
  name: statica
  text: Static websites, served from S3.
  tagline: >-
    A single Go binary, one YAML file.
    
    With a bounded memory cache, health checks and metrics built-in.
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
  - icon: >-
      <svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="4" width="18" height="16" rx="2" stroke="var(--vp-c-text-1)"/><line x1="3" y1="9" x2="21" y2="9" stroke="var(--vp-c-text-1)"/><circle cx="6.5" cy="6.5" r="0.75" fill="var(--vp-c-text-1)"/><circle cx="9.5" cy="6.5" r="0.75" fill="var(--vp-c-text-1)"/><path d="M8 16l2.5-4.5" stroke="var(--vp-c-brand-1)"/><line x1="12" y1="16" x2="16" y2="16" stroke="var(--vp-c-brand-1)"/></svg>
    title: One bucket, one site
    details: >-
      Index objects, trailing-slash redirects, custom 404, redirect rules and
      SPA fallback, without S3 website hosting.
    link: /guide/publishing
    linkText: Publishing guide
  - icon: >-
      <svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 8V5a1 1 0 0 1 1-1h3M16 4h3a1 1 0 0 1 1 1v3M20 16v3a1 1 0 0 1-1 1h-3M8 20H5a1 1 0 0 1-1-1v-3" stroke="var(--vp-c-text-1)"/><polygon points="12 7 17 9.75 12 12.5 7 9.75 12 7" fill="var(--vp-c-brand-soft)" stroke="var(--vp-c-brand-1)"/><path d="M7 13.5L12 16.25l5-2.75" stroke="var(--vp-c-brand-1)"/></svg>
    title: A bounded memory cache
    details: >-
      Capped by bytes and entries, with shared fills, origin revalidation,
      negative caching and optional stale-if-error.
    link: /architecture/memory-cache
    linkText: Memory cache
  - icon: >-
      <svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 4H4v16h2M18 4h2v16h-2" stroke="var(--vp-c-text-1)"/><path d="M8 9.5h8L13.5 7M16 14.5H8l2.5 2.5" stroke="var(--vp-c-brand-1)"/></svg>
    title: HTTP semantics
    details: >-
      Conditional requests, single byte ranges, precompressed Brotli and gzip
      variants, Cache-Control, Age, custom headers and CORS.
    link: /architecture/ranges-and-conditionals
    linkText: HTTP behavior
  - icon: >-
      <svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><ellipse cx="12" cy="6" rx="7" ry="3" fill="var(--vp-c-brand-soft)" stroke="var(--vp-c-brand-1)"/><path d="M5 6v6c0 1.66 3.13 3 7 3s7-1.34 7-3V6" stroke="var(--vp-c-text-1)"/><path d="M5 12v6c0 1.66 3.13 3 7 3s7-1.34 7-3v-6" stroke="var(--vp-c-text-1)"/><circle cx="12" cy="6" r="1" fill="var(--vp-c-brand-1)"/></svg>
    title: S3 and compatible origins
    details: >-
      AWS S3 or custom S3-compatible endpoints, using the standard AWS
      credential chain, path-style requests and capability switches.
    link: /guide/s3-providers
    linkText: S3 providers
  - icon: >-
      <svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="4" width="18" height="16" rx="2" stroke="var(--vp-c-text-1)"/><polyline points="6 13 9 13 11 8.5 13 15.5 15 11.5 18 11.5" stroke="var(--vp-c-brand-1)"/></svg>
    title: Logs, metrics and health checks
    details: >-
      Redacted logfmt events on stdout, plus Prometheus metrics, liveness and
      readiness on a separate admin listener.
    link: /guide/metrics
    linkText: Monitoring guide
  - icon: >-
      <svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polygon points="12 3 20 7.5 20 16.5 12 21 4 16.5 4 7.5 12 3" stroke="var(--vp-c-text-1)"/><line x1="8" y1="9.5" x2="16" y2="9.5" stroke="var(--vp-c-brand-1)"/><path d="M12 16.5v-4.5M9.75 14L12 11.75 14.25 14" stroke="var(--vp-c-brand-1)"/></svg>
    title: Explicit limits
    details: >-
      Startup validation, path confinement to the prefix, connection and
      concurrency caps without queueing, and bounded shutdown.
    link: /architecture/#explicit-resource-limits
    linkText: Resource limits
---

## A minimal configuration

This configuration serves the bucket `example-websites`, in the AWS region `eu-west-1`, as a website:

<<< @/examples/minimal.yaml{yaml}

Save it as `config.yaml` with your own bucket and region. With statica [installed](./guide/installation.md) and on your `PATH`, validate the file, check access to the bucket and start the server:

```sh
statica config validate --config config.yaml
statica config check-origin --config config.yaml
statica serve --config config.yaml
```

The defaults put the site on `127.0.0.1:8080` and the admin endpoints on `127.0.0.1:9090`, enable a memory cache of up to `128MiB`, and serve `index.html` for paths that end in `/`. The [Quickstart](./guide/quickstart.md) installs statica and walks through each step, [Configuration basics](./guide/configuration.md) explains the file, and [Configuration examples](./reference/examples.md) shows complete configurations for single-page apps, TLS and reverse proxies.

## How it fits

Visitors reach the public listener directly or through a CDN or ingress, and orchestrators and Prometheus use a separate admin listener.

```mermaid
flowchart LR
  accTitle: statica in a deployment
  accDescr: Visitors reach the public listener directly or through an optional CDN or ingress. Objects are read through the memory cache, which reads the one configured S3 bucket with GetObject and HeadObject requests when it holds no usable copy. Orchestrators and Prometheus use the separate admin listener for liveness, readiness and metrics.
  V["Visitor"]
  P["CDN or ingress<br/>(optional)"]
  subgraph statica
    L["Public listener"]
    C["Memory cache"]
    A["Admin listener"]
  end
  B[("Private S3 bucket<br/>or prefix")]
  O["Orchestrator,<br/>Prometheus"]
  V --> P --> L
  V -.->|direct| L
  L --> C
  C -->|"GetObject,<br/>HeadObject"| B
  O -->|"/livez, /readyz,<br/>/metrics"| A
```

One instance serves one site, and instances that serve the same site keep separate memory caches. Because statica authenticates through the standard AWS credential chain and sends only `GetObject` and `HeadObject` requests, the bucket can stay private and read-only.

## Where to go next

- [What is statica?](./guide/what-is-statica.md): the problem it solves, its capabilities, and [what it leaves to other tools](./guide/what-is-statica.md#what-statica-does-not-do).
- [Quickstart](./guide/quickstart.md) and [Installation](./guide/installation.md): run the release binary, the container image or a build from source.
- [Architecture](./architecture/index.md): how a request is mapped to objects, how the memory cache works, and how statica starts and shuts down.
- [Reference](./reference/configuration.md) and [Examples](./reference/examples.md): every configuration field, CLI command, HTTP response, log event and metric, alongside complete example configurations.
