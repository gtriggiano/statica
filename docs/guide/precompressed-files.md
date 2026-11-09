# Serve precompressed files

Publish Brotli and gzip copies of your text files next to the originals, and let statica send each client the copy it prefers among those that exist. Responses are never compressed on the fly.

## Prerequisites

- A site [published](./publishing.md) under the publication prefix.
- Tools that produce Brotli (`.br`) and gzip (`.gz`) files in your build, such as the `brotli` and `gzip` command-line tools.

## Produce the variants when you build

Compress each text file into a copy with the same name plus `.br` or `.gz`, and keep the original. For example, with the `brotli` and `gzip` tools; `--force` replaces the copies of an earlier run, which both tools otherwise refuse to overwrite:

```sh
find dist -type f \( -name '*.html' -o -name '*.css' -o -name '*.js' \
  -o -name '*.json' -o -name '*.svg' \) \
  -exec brotli --keep --best --force {} \; \
  -exec gzip --keep --best --no-name --force {} \;
```

The build directory then contains:

```sh
find dist -type f | sort
```

```text
dist/assets/app.3f9c2b.js
dist/assets/app.3f9c2b.js.br
dist/assets/app.3f9c2b.js.gz
dist/assets/logo.png
dist/index.html
dist/index.html.br
dist/index.html.gz
```

Formats that are already compressed, such as PNG, JPEG, WOFF2 or video, gain nothing from another pass: publish them without variants.

## Upload the variants next to each object

Upload every variant under the same prefix as the original object, which statica calls the canonical object: `assets/app.3f9c2b.js.br` next to `assets/app.3f9c2b.js`. Set their metadata like this:

| Metadata | On the `.br` and `.gz` variants |
| --- | --- |
| `Content-Encoding` | `br` on `.br` files and `gzip` on `.gz` files, or none: the coding of the suffix is then assumed. Any other value, `identity` included, makes the request fail with 502 Bad Gateway. |
| `Content-Type` | Ignored. The response has the canonical object's type or, when it has none, the type of the canonical name's extension, so `app.js.br` is served as JavaScript. |
| `Cache-Control`, `Expires`, `Content-Disposition`, `Content-Language` | Taken from the variant that is served. Give every variant the same values as its canonical object. |

For example, with the AWS CLI, upload the canonical objects first, then the variants of the file types that `find` compressed, each with its coding:

```sh
DEST=s3://example-site/releases/2026-11-08/
aws s3 cp dist/ "$DEST" --recursive --exclude '*.br' --exclude '*.gz'
aws s3 cp dist/ "$DEST" --recursive --exclude '*' \
  --include '*.html.br' --include '*.css.br' --include '*.js.br' \
  --include '*.json.br' --include '*.svg.br' --content-encoding br
aws s3 cp dist/ "$DEST" --recursive --exclude '*' \
  --include '*.html.gz' --include '*.css.gz' --include '*.js.gz' \
  --include '*.json.gz' --include '*.svg.gz' --content-encoding gzip
```

The first command skips every `.br` and `.gz` file. Upload other files with these suffixes, such as `.tar.gz` archives, separately and without `--content-encoding`. If you set `--cache-control` on the first command, set the same value on the other two. Upload the variants and their canonical objects together, ideally under a new [release prefix](./publishing.md#deploy-with-release-prefixes), so that statica never serves a variant from one build with a canonical object from another.

## Turn on precompressed mode

```yaml [config.yaml]
compression:
  mode: precompressed
  encodings: [br, gzip]
```

With [`compression.mode: precompressed`](../reference/configuration.md#compression-mode), statica looks for the variants listed in [`compression.encodings`](../reference/configuration.md#compression-encodings) that a request's `Accept-Encoding` header accepts, and serves the one the client prefers among those that exist. When the client accepts several with the same quality, as browsers do, the list order decides: with `[br, gzip]`, a browser that sends `Accept-Encoding: gzip, deflate, br, zstd` gets the `.br` file.

List only the codings you publish. If you publish only `.br` files, set `encodings: [br]`, so that statica does not look for `.gz` files that never exist.

A request without `Accept-Encoding` gets the canonical object, without any variant lookup. [Precompressed variants](../architecture/precompressed-variants.md#negotiation) describes the full negotiation.

## Verify it

Ask for the Brotli variant, then for the uncompressed object. The output is abridged: `...` stands for the headers left out.

```sh
curl -sI -H 'Accept-Encoding: br' http://127.0.0.1:8080/assets/app.3f9c2b.js
curl -sI http://127.0.0.1:8080/assets/app.3f9c2b.js
```

```text
HTTP/1.1 200 OK
Cache-Control: public, max-age=31536000, immutable
Content-Encoding: br
Content-Length: 44
Content-Type: text/javascript; charset=utf-8
Etag: "54abce3f1e259c1a1a1fcbcc387c2ba0"
Vary: Accept-Encoding
...

HTTP/1.1 200 OK
Cache-Control: public, max-age=31536000, immutable
Content-Length: 3801
Content-Type: text/javascript; charset=utf-8
Etag: "4981db3b99f9216c2619c8b3e05527ee"
Vary: Accept-Encoding
...
```

The access log shows the object that was served in `origin_key`, with its suffix, while `path` keeps the requested URL:

```text
level=info event=http_request host=127.0.0.1 method=HEAD path=/assets/app.3f9c2b.js status=200 outcome=success resolution=ordinary cache=miss origin_key=releases/2026-11-08/assets/app.3f9c2b.js.br ...
```

On the admin listener, [`statica_origin_operations_total`](../reference/metrics.md#statica-origin-operations-total) counts every lookup of a missing variant. For a `GET`, it is one `get` operation with `result="not_found"`. For a `HEAD`, such as the `curl -sI` above, it is a `head` operation with `result="ambiguous_not_found"` followed by a `get` with `result="not_found"`, because S3 cannot confirm an absence in a `HEAD` response.

## Pitfalls

- **Stale variants are served.** Nothing checks that a variant holds the same content as its canonical object. A `.br` file left over from an earlier build is sent to every client that accepts Brotli, while other clients get the new file.
- **A variant without its canonical object.** If `app.js.br` exists but `app.js` does not, clients that accept Brotli get the variant, and every other client gets 404 Not Found. Always publish the canonical object.
- **Objects without variants cost extra lookups.** For each request with `Accept-Encoding`, statica looks for every acceptable variant before it serves an image or any other object without one. Each missing variant becomes a negative entry for [`cache.negative_ttl`](../reference/configuration.md#cache-negative-ttl), `5s` by default, and is looked up again once the entry expires. A [`cache.rules`](../reference/configuration.md#cache-rules) entry with a longer `negative_ttl` for a directory without variants, such as `/images/`, keeps these answers longer, but a new image published in place then also stays missing longer.
- **Every `.gz` and `.br` object is a variant.** With `gzip` in `compression.encodings`, a download such as `downloads/site.tar.gz` is also the gzip variant of `/downloads/site.tar`. That URL then answers 200 OK with `Content-Encoding: gzip` to clients that accept gzip, and 404 Not Found to the others.
- **More memory per object.** Each variant is a separate memory cache entry. When statica serves a variant to a `GET` request for a canonical object that fits in [`cache.max_object_bytes`](../reference/configuration.md#cache-max-object-bytes), it also stores the canonical object, so both count against [`cache.max_bytes`](../reference/configuration.md#cache-max-bytes).
- **406 Not Acceptable.** When an object exists but the client accepts none of its available forms, for example `Accept-Encoding: identity;q=0` for an object without variants, the response is 406. It is logged with `outcome=success`.
- **Exact memory cache rules miss the variants.** `cache.rules` match the path of the object that is served, so an `exact: /app.js` rule does not apply to `/app.js.br`. Add exact rules for `/app.js.br` and `/app.js.gz` as well, or use a directory prefix rule such as `/assets/`. `cache_control.rules` match the request path and apply to every variant ([Control browser and CDN caching](./browser-caching.md)).
- **Each variant has its own `ETag`.** Downstream caches must keep a copy per `Accept-Encoding` value, as `Vary: Accept-Encoding` tells them, and an `If-None-Match` with the ETag of one variant does not match another.

## Related pages

- [Precompressed variants](../architecture/precompressed-variants.md): negotiation, sibling metadata and lookup costs in detail.
- [Publish a site to S3](./publishing.md)
- [Tune the memory cache](./cache-tuning.md): budget and rules that account for the variants.
- [Troubleshooting](./troubleshooting.md#precompressed-variants-are-not-served): variants that are not served.
- [`compression` configuration reference](../reference/configuration.md#compression)
