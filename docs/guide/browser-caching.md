# Control browser and CDN caching

Choose the `Cache-Control` header that browsers and CDNs receive for each part of your site, independently of how long statica keeps objects in its own memory.

## Prerequisites

- A [published](./publishing.md) site, and a list of which files have names that change with their content, such as `app.3f9c2b.js`.

## Two separate policies

| | [`cache`](../reference/configuration.md#cache) | [`cache_control`](../reference/configuration.md#cache-control) |
| --- | --- | --- |
| Decides | What statica keeps in its memory cache, and for how long | The `Cache-Control` header that clients and CDNs receive |
| Rules match | The object path: `/guide/index.html`, `/app.js.br` | The request path: `/guide/`, `/app.js` |
| Default | `cache.ttl: 5m` | `cache_control.default: auto` |

A `cache_control` value never changes what statica stores, and a `cache` setting reaches clients only through `auto` ([Cache-Control and Age](../architecture/cache-control.md)).

## Know what `auto` sends

With the default `auto`, the header for each object comes from its origin metadata, in this order ([precedence](../architecture/cache-control.md#precedence)):

1. Malformed cache metadata gives `no-store`, even next to a valid value: an object with `Cache-Control: max-age=60` and `Expires: 0` gets `no-store`.
2. The object's own `Cache-Control` is sent unchanged, with its `Expires` if it has one.
3. An object with only `Expires` keeps it, without `Cache-Control`.
4. An object with neither gets `max-age` set to its memory cache TTL in seconds, `max-age=300` with the defaults, or `no-cache` when the memory cache is disabled for it or its TTL is below one second.

So, out of the box, every object without metadata, HTML pages included, may be kept by browsers for five minutes.

## Set a policy per path

[`cache_control.rules`](../reference/configuration.md#cache-control-rules) match the request path with `exact` or `prefix`; the first match wins, and [`cache_control.default`](../reference/configuration.md#cache-control-default) covers the rest. An explicit value replaces the object's `Cache-Control` and `Expires`. This configuration covers the usual cases:

```yaml [config.yaml]
cache_control:
  default: no-cache
  rules:
    - match:
        prefix: /assets/
      value: public, max-age=31536000, immutable
    - match:
        prefix: /downloads/
      value: public, max-age=86400
```

- **Content-addressed assets** under `/assets/` can be kept for a year without revalidation, because a new version gets a new name. Clients that honor `immutable` do not revalidate the file even when the user reloads the page.
- **HTML pages and everything else** get `no-cache`: clients may store them but must revalidate before each use. A revalidation that carries the page's `ETag` gets 304 Not Modified, without a body. Use a short `max-age=60` instead if a minute of delay after a deploy is acceptable.
- **Downloads** under `/downloads/` are kept for a day. Set `Content-Disposition` on the objects themselves if they should be saved rather than displayed ([object metadata](./publishing.md#set-object-metadata)).

::: warning Every file under the prefix needs a content hash
A rule for a whole prefix applies to every file under it. A file without a content hash in its name, such as `/assets/logo.png`, cannot be updated for a year in browsers and CDNs that already hold it. Put only content-addressed files under such a prefix.
:::

Any valid `Cache-Control` value works, for example `public, max-age=60, s-maxage=600` to let a CDN keep a page longer than browsers do. Use `value: auto` in a rule to keep the origin's policy for some paths under an explicit default.

## Set Cache-Control on the objects instead

With `auto`, you can set the policy when you upload each object instead, with the object's `Cache-Control` metadata, for example `aws s3 cp --cache-control 'public, max-age=31536000, immutable'`. The object's `Cache-Control` and `Expires` also limit statica's memory cache:

- `max-age`, `s-maxage` or `Expires` can shorten the time statica keeps the object, never extend it beyond its configured TTL;
- `no-cache` makes statica revalidate the object with the origin on every use;
- `no-store` or `private` keeps the object out of statica's memory.

Every variant of a [precompressed file](./precompressed-files.md) carries its own metadata, so give them all the same `Cache-Control`.

## Tune statica's memory separately

[`cache.ttl`](../reference/configuration.md#cache-ttl) and [`cache.rules`](../reference/configuration.md#cache-rules) decide how long statica serves an object from memory before it asks the origin again. They decide how quickly a change made in place becomes visible through statica, not how long clients keep it. For example, to see a new `index.html` within 30 seconds:

```yaml [config.yaml]
cache:
  rules:
    - match:
        exact: /index.html
      ttl: 30s
```

With `auto` and no origin metadata, this rule also changes what clients receive for `/` and `/index.html`: `max-age=30`. [Tune the memory cache](./cache-tuning.md) covers the memory side: size, TTLs and stale-if-error.

## How `Age` affects clients

A response that serves an object (other than the SPA shell or a custom 404 page) carries `Age` when its copy is at least one second old: for a response from statica's memory, the age of that copy, including the age it already had when the origin sent it; for a response read from the origin, the larger of the age the origin reports and the time since its `Date`. `max-age` is never lowered; clients subtract `Age` themselves. With `max-age=300` and `Age: 120`, a browser or CDN can keep the response for about 180 more seconds ([Age](../architecture/cache-control.md#age)).

## Verify it

The output is abridged: `...` stands for the headers left out.

```sh
curl -sI http://127.0.0.1:8080/assets/app.3f9c2b.js
curl -sI http://127.0.0.1:8080/guide/
curl -sI http://127.0.0.1:8080/guide/
```

```text
HTTP/1.1 200 OK
Cache-Control: public, max-age=31536000, immutable
Content-Type: text/javascript; charset=utf-8
...

HTTP/1.1 200 OK
Cache-Control: no-cache
Content-Type: text/html; charset=utf-8
...

HTTP/1.1 200 OK
Age: 2
Cache-Control: no-cache
Content-Type: text/html; charset=utf-8
...
```

The access log tells where each response came from. The third one came from memory: its event records `cache=metadata_hit` for this `HEAD` request, as it would record `cache=hit` for a `GET`, while the second records `cache=miss`. A malformed origin policy shows up as `cache_diagnostic=malformed_origin_cache_metadata`.

A browser revalidates a `no-cache` page with the `ETag` it received, and gets an empty 304 Not Modified while the page is unchanged:

```sh
curl -s -o /dev/null -w '%{http_code}\n' \
  -H 'If-None-Match: "8c0488487b359933c0ef03df4208ab27"' http://127.0.0.1:8080/guide/
```

```text
304
```

## Pitfalls

- **An explicit value overrides the origin's `no-store` for clients.** A rule that sends `public, max-age=600` for an object stored with `Cache-Control: no-store` lets browsers and CDNs keep it, while statica itself still refuses to store it. Keep private content out of the publication prefix.
- **Some responses are always `no-store`.** Redirects, 404 responses (the custom 404 page included), the single-page-app shell served for a route, 412 Precondition Failed and every error response carry `Cache-Control: no-store`, whatever `cache_control` says.
- **The two kinds of rules see different paths.** `cache_control.rules` match `/guide/` and `/app.js`, `cache.rules` match `/guide/index.html` and `/app.js.br`. An `exact` rule written for one kind can miss in the other ([which path a rule matches](../architecture/cache-control.md#which-path-a-rule-matches)).
- **There is no purge.** Copies in browsers or CDNs cannot be removed: a long `max-age` on a URL whose content changes keeps the old content in their caches until it expires.
- **Changes need a restart.** `cache_control` is read at startup, and copies that clients already hold keep the policy they were sent with.

## Related pages

- [Cache-Control and Age](../architecture/cache-control.md): precedence, rule matching and `Age` in detail.
- [Memory cache](../architecture/memory-cache.md#freshness): how long statica keeps objects.
- [`cache_control` configuration reference](../reference/configuration.md#cache-control)
- [Tune the memory cache](./cache-tuning.md): how long statica itself keeps objects.
- [Publish a site to S3](./publishing.md)
