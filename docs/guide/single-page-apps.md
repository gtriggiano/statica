# Serve a single-page application

Configure statica so that deep links into a single-page application, such as `/orders/42`, load the application shell, while missing assets and other files still answer 404 Not Found.

## Prerequisites

- The built application [published](./publishing.md) under the publication prefix: its shell (usually `index.html`) and its assets.
- Asset URLs in the shell that are absolute, such as `/assets/main.4c1d.js`. statica serves the shell at the URL that was requested, at any depth, so a relative `assets/main.4c1d.js` would resolve to `/orders/assets/main.4c1d.js` on `/orders/42`.
- On AWS, credentials with `s3:ListBucket` on the bucket. Without it, AWS answers 403 Forbidden for every missing route, and statica never serves the shell for a denied read ([AWS without `s3:ListBucket`](../architecture/access-denied.md#aws-without-s3-listbucket)).

## How the fallback decides

When a GET or HEAD request has no object of its own and no index redirect, statica serves the [`spa.fallback`](../reference/configuration.md#spa-fallback) object with 200 OK at the original URL, if:

- the request's `Accept` header lists `text/html` explicitly, as browsers do when they open a page; `*/*` does not count;
- the path is not under one of `spa.exclude_prefixes`;
- the path is under one of `spa.include_prefixes` or, when that list is omitted, its last segment contains no dot.

Otherwise the request continues to the `not_found` page or the built-in 404. [SPA fallback](../architecture/spa-fallback.md#eligibility-rules) has the exact rules.

## Choose where the application lives

### The application owns the whole site

```yaml [config.yaml]
s3:
  bucket: example-app
  prefix: site
spa:
  fallback: index.html
  exclude_prefixes: [/assets/, /api/, /.well-known/]
```

Every path without a dot in its last segment is a route: `/orders/42` and `/settings/` get the shell, while a missing `/favicon.png` gets 404. Objects that exist, such as `/robots.txt` or `/docs/index.html`, are still served as themselves.

### The application lives under a path

```yaml [config.yaml]
s3:
  bucket: example-app
  prefix: site
spa:
  fallback: app/index.html
  include_prefixes: [/app/]
  exclude_prefixes: [/app/assets/]
```

Only paths under `/app/` get the shell, `app/index.html`; the rest of the site keeps ordinary 404 responses. A prefix matches at a segment boundary: `/app/` covers `/app` and `/app/orders/42`, not `/application`. A request for `/app` gets 308 Permanent Redirect to `/app/` when `app/index.html` exists, like any directory with an index object.

## Exclude paths that must stay 404

List in `spa.exclude_prefixes` every namespace where a missing URL should answer 404, not the shell. Exclusions win over `spa.include_prefixes`.

- **Assets.** Browsers do not list `text/html` in the `Accept` header of a script or stylesheet request, so such a request never gets the shell. The same URL opened in a browser tab does list it, and gets the shell under an included prefix. Excluding the asset directory keeps those answers 404.
- **APIs.** If another service answers under a path such as `/api/`, excluding it keeps a request that reaches statica by mistake from receiving HTML with 200.
- **`/.well-known/`.** Without the exclusion, a missing well-known URI such as `/.well-known/change-password` has no dot in its last segment and gets the shell when a browser opens it.

## Handle routes that contain dots

Without `spa.include_prefixes`, a route whose last segment contains any dot, such as `/users/jane.doe` or `/releases/1.2`, gets 404 Not Found. To serve such routes, list their namespace in `spa.include_prefixes`.

`include_prefixes` replaces the dot rule rather than adding to it: only the listed prefixes get the shell. For an application that owns the whole site and has dotted routes, include `/` and exclude everything that is not a route:

```yaml [config.yaml]
spa:
  fallback: index.html
  include_prefixes: [/]
  exclude_prefixes: [/assets/, /api/, /.well-known/]
```

With `include_prefixes: [/]`, a missing `/bundle.js` opened in a browser tab gets the shell too, unless it is under an excluded prefix.

## Cache the shell briefly

The shell reaches clients in two ways, with different caching:

- **As the fallback for a route**, it always carries `Cache-Control: no-store` and `Vary: Accept`, whatever its metadata and your `cache_control` rules say, so browsers and CDNs never store it under route URLs.
- **At its own URL**, such as `/` or `/app/`, it is an ordinary object and follows [`cache_control`](../reference/configuration.md#cache-control). Give it `no-cache`, so that browsers revalidate it with its `ETag` before each use and get a new shell as soon as statica serves it.

statica's memory cache holds the shell once, for every route, under its object path (`/index.html` here). If you overwrite the shell in place, shorten its lifetime in memory with a [`cache.rules`](../reference/configuration.md#cache-rules) entry:

```yaml [config.yaml]
spa:
  fallback: index.html
  exclude_prefixes: [/assets/, /.well-known/]
cache:
  rules:
    - match:
        exact: /index.html
      ttl: 30s
cache_control:
  rules:
    - match:
        prefix: /assets/
      value: public, max-age=31536000, immutable
    - match:
        exact: /
      value: no-cache
    - match:
        exact: /index.html
      value: no-cache
```

`cache_control.rules` match the request path (`/`), while `cache.rules` match the object path (`/index.html`). [Control browser and CDN caching](./browser-caching.md) explains the difference.

## Verify it

Request a route as a browser does, with `Accept: text/html`. The output is abridged: `...` stands for the headers left out.

```sh
curl -sI -H 'Accept: text/html' http://127.0.0.1:8080/orders/42
```

```text
HTTP/1.1 200 OK
Cache-Control: no-store
Content-Length: 34
Content-Type: text/html; charset=utf-8
Etag: "e3dc31c2b97c3392ba18435b8cd0ed3c"
Last-Modified: Mon, 02 Nov 2026 10:00:00 GMT
Vary: Accept
Vary: Accept-Encoding
X-Content-Type-Options: nosniff
...
```

The access log records `resolution=spa`, the requested `path` and the shell's key in `origin_key`:

```text
level=info event=http_request host=127.0.0.1 method=HEAD path=/orders/42 status=200 outcome=success resolution=spa cache=miss origin_key=site/index.html ...
```

Then check that non-HTML requests and excluded paths still get 404:

```sh
curl -s -o /dev/null -w '%{http_code}\n' -H 'Accept: */*' http://127.0.0.1:8080/orders/42
curl -s -o /dev/null -w '%{http_code}\n' -H 'Accept: text/html' http://127.0.0.1:8080/assets/missing.js
```

```text
404
404
```

## Pitfalls

::: warning Unknown routes answer 200
Every eligible URL without an object gets the shell with 200 OK, mistyped and obsolete links included. Crawlers, link checkers and status-based monitoring see 200, not 404. Your application must render its own "not found" view for routes it does not know.
:::

- **Origin errors never become the shell.** A 403 Forbidden, a timeout or a provider error on any lookup, the shell's own included, ends the request with that error. On AWS without `s3:ListBucket`, every route then answers 404 Not Found with the default `errors.access_denied`.
- **Every route costs origin lookups.** Before serving the shell, statica confirms that the route has no object of its own and no index object: it reads the exact key and the index key (only the index key for a path ending in `/`), plus their variants with [precompressed files](./precompressed-files.md). Each confirmed absence becomes a negative entry for [`cache.negative_ttl`](../reference/configuration.md#cache-negative-ttl), so a route repeated within that time is answered from memory, while a new route, such as a new order ID, reads the origin again.
- **Negative entries per route fill the memory cache.** They count against [`cache.max_entries`](../reference/configuration.md#cache-max-entries) like any other entry, so many distinct routes can push objects out of memory. A `cache.rules` entry with `negative_ttl: 0s` for the route namespace stores none, at the price of reading the origin for every route request.
- **Redirect rules run first.** A [`redirects`](./redirects.md) rule that matches a route answers before any lookup.
- **Clients that do not ask for HTML get 404.** Tools that send `Accept: */*`, as `curl` does by default, see 404 on routes that browsers can open.

## Related pages

- [SPA fallback](../architecture/spa-fallback.md): the exact eligibility rules and response headers.
- [Publish a site to S3](./publishing.md)
- [Control browser and CDN caching](./browser-caching.md)
- [Memory cache](../architecture/memory-cache.md#negative-entries): negative entries.
- [Tune the memory cache](./cache-tuning.md): `cache.rules` and memory lifetimes.
- [Troubleshooting](./troubleshooting.md#single-page-app-routes-answer-404): routes that answer 404.
- [`spa` configuration reference](../reference/configuration.md#spa)
