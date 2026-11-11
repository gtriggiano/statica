# Publish a site to S3

Upload a static site to a bucket so that **statica** serves it with the right paths, index pages, 404 page and headers, and switch to a new version of the site without mixing two versions.

## Prerequisites

- A built site: a directory of HTML, CSS, JavaScript and other files.
- A bucket, and a tool that uploads objects with their metadata, such as the AWS CLI.
- A statica configuration that reads the bucket ([Connect to S3 and S3-compatible providers](./s3-providers.md)).

## Choose a publication prefix

Each statica instance serves one part of one bucket: the [publication prefix](../reference/configuration.md#s3-prefix), or the whole bucket when `s3.prefix` is empty. The object key for a request is the prefix followed by the decoded request path without its leading slash. Leading and trailing slashes in `s3.prefix` are optional: `docs`, `/docs` and `docs/` all select `docs/`.

| Request path | Object key with `s3.prefix: releases/2026-11-08` |
| --- | --- |
| `/` | `releases/2026-11-08/index.html` |
| `/guide/` | `releases/2026-11-08/guide/index.html` |
| `/assets/app.3f9c2b.js` | `releases/2026-11-08/assets/app.3f9c2b.js` |
| `/a%20b.txt` | `releases/2026-11-08/a b.txt` |

Upload the content of your build directory under the prefix, keeping its directory structure as object keys. The bucket is never listed, so no "folder" objects are needed. A request can never read an object outside the prefix ([confinement](../architecture/security.md#requests-cannot-leave-the-publication-prefix)).

::: danger Everything under the prefix is public
Every object under the prefix is served to anyone who can reach statica, even when the bucket is private in S3. Keep drafts, backups and build caches out of the prefix.
:::

## Provide index files

A path that ends in `/` reads its [`index`](../reference/configuration.md#index) object, `index.html` by default: `/guide/` reads `guide/index.html`. Publish one in every directory that should answer a URL.

A path without the trailing slash, such as `/guide`, first reads the object `guide`. When that object does not exist but `guide/index.html` does, statica answers 308 Permanent Redirect to `/guide/`, query kept. Relative links in the page then resolve against the directory, as browsers expect.

`/guide/index.html` is an ordinary object too: statica serves it at that URL and does not redirect it to `/guide/`. If you want a single URL per page, add a [redirect rule](./redirects.md).

## Add a custom 404 page

Set [`not_found`](../reference/configuration.md#not-found) to the key of an HTML page, relative to the prefix:

```yaml [config.yaml]
s3:
  bucket: example-site
  prefix: releases/2026-11-08
not_found: 404.html
```

The page is served with status 404 Not Found when no object, index redirect or single-page-app fallback answers the request, always with `Cache-Control: no-store`. It is served at the URL that was requested, which can be at any depth, so reference its stylesheets, scripts and images with absolute paths (`/assets/site.css`), not relative ones. When the `not_found` object itself is missing, statica answers with its built-in plain-text 404.

Some 404 responses never use the page: hidden paths ([next section](#keep-hidden-files-out-of-reach)), and an origin 403 Forbidden masked as 404 by [`errors.access_denied`](../reference/configuration.md#errors-access-denied). These get statica's built-in plain-text body.

## Keep hidden files out of reach

A request path with a segment that starts with a dot, such as `/.git/config` or `/assets/.DS_Store`, gets 404 Not Found without any origin read. The only exception is a first segment that is exactly `.well-known`, so `/.well-known/security.txt` and other well-known URIs are served ([target validation](../architecture/request-flow.md#target-validation)).

Some object keys can never be requested, so do not publish content under them:

- keys with a segment that starts with a dot, apart from a first segment `.well-known` under the prefix;
- keys with an empty segment (`a//b`), a `.` or `..` segment, a backslash, a control character or invalid UTF-8;
- keys that end in `/`, such as the "folder" objects some tools create: a path that ends in `/` reads its index object instead;
- keys that contain a literal `%` followed by two hexadecimal digits;
- keys longer than 1024 bytes, prefix included.

The `index`, `not_found` and `spa.fallback` settings name object keys, not request paths, so these rules do not apply to them.

## Set object metadata

Only an allowlist of each object's metadata reaches visitors ([origin metadata](../reference/http-responses.md#origin-metadata)). Set these when you upload:

| Metadata | What statica does with it |
| --- | --- |
| `Content-Type` | Sent as stored when it is a valid media type. When it is missing or invalid, statica picks a type from a [fixed table](../reference/http-responses.md#content-type) of common web extensions, such as `.html`, `.css`, `.js`, `.json`, `.svg`, `.png` and `.woff2`, and unknown extensions get `application/octet-stream`. |
| `Cache-Control`, `Expires` | Forwarded to clients by default, and they can shorten how long statica keeps the object in memory. See [Control browser and CDN caching](./browser-caching.md). |
| `Content-Encoding` | Only `gzip` or `br`, for an object stored compressed. `identity` counts as none. Any other value makes the read fail with 502 Bad Gateway. |
| `Content-Disposition` | Forwarded when well formed, for example `attachment; filename="report.pdf"` for a download. |
| `Content-Language` | Forwarded when it is a list of language tags, such as `de` or `en-US`. |

Set `Content-Type` on every object. A generic type that a tool or provider stores by default, such as `binary/octet-stream`, is a valid media type, so statica sends it as is and the extension table does not apply.

Everything else stays at the origin: user-defined metadata (`x-amz-meta-*`), storage, encryption and version headers, and S3 website redirect metadata (`x-amz-website-redirect-location`), which statica neither forwards nor follows.

For example, with the AWS CLI:

```sh
aws s3 cp site/files/report.pdf \
  s3://example-site/releases/2026-11-08/files/report.pdf \
  --content-type application/pdf \
  --content-disposition 'attachment; filename="report.pdf"'
```

## Deploy with release prefixes

The configuration is read once, at startup, and objects stay in the memory cache for [`cache.ttl`](../reference/configuration.md#cache-ttl), `5m` by default. If you overwrite objects in place, each one changes when its memory cache entry expires, so visitors can get a new page with old assets, or the reverse, for a few minutes.

To switch versions at once, publish each version under its own prefix and point statica at it:

1. Upload the complete new version under a new prefix, such as `releases/2026-11-08/`, precompressed variants included.
2. Name the prefix through an environment variable, so that the configuration file does not change between releases:

   ```yaml [config.yaml]
   s3:
     bucket: example-site
     prefix: releases/${RELEASE_ID}
   ```

3. Check the new prefix before switching:

   ```sh
   RELEASE_ID=2026-11-08 statica config check-origin --config config.yaml
   ```

   It prints `origin object accessible` when the release's `index` object can be read. On AWS without `s3:ListBucket`, a release that was not uploaded is reported as `access denied`, not `object not found`. A `RELEASE_ID` that is not set fails the configuration:

   ```text
   statica: s3.prefix (line 5, column 11): missing environment variable RELEASE_ID
   ```

4. Restart statica with the new value, one instance at a time if you run several. A restarted process starts with an empty memory cache and reads only the new prefix ([configuration changes and restarts](../architecture/lifecycle.md#configuration-changes-and-restarts)).
5. Keep the previous prefix until you no longer need to roll back to it.

[Deploy, roll out and shut down](./deployment.md#change-the-configuration) shows how to restart instances one at a time under systemd or Kubernetes.

During a rolling restart, instances on the old and the new prefix serve at the same time, and browsers and CDNs keep pages they already have for as long as their `Cache-Control` allows. If your asset names contain a content hash, a page can ask an instance for an asset that its prefix does not have:

- a page from the old release, cached or served by an old instance, asks a new instance for an old asset. Copy the previous release's assets into the new prefix as well.
- a page from the new release asks an old instance for a new asset, which gets 404 Not Found and leaves a negative entry there. Copy the new release's assets into the old prefix before the rollout, or keep each visitor on one instance with session affinity in the load balancer.

Neither the `startup` event nor `config print` shows the prefix in use: the event leaves it out, and `config print` shows a value taken from an environment variable as `[REDACTED]`. To check which release an instance serves, read the `origin_key` of its access events, such as `origin_key=releases/2026-11-08/index.html`.

## How changes become visible

| Change | Visible through statica |
| --- | --- |
| A new prefix | After the restart. |
| An object changed in place | When its memory cache entry expires and statica revalidates it: after `cache.ttl`, the TTL of a matching [`cache.rules`](../reference/configuration.md#cache-rules) entry, or a shorter lifetime from the object's own `Cache-Control` or `Expires`. |
| A new object | At once if no request asked for it before. A request that found it missing leaves a negative entry, so the object appears when that entry expires, after [`cache.negative_ttl`](../reference/configuration.md#cache-negative-ttl) (`5s` by default). |
| A deleted object | When its entry expires and the origin confirms the deletion. |
| An object that statica does not keep in memory: larger than [`cache.max_object_bytes`](../reference/configuration.md#cache-max-object-bytes), or stored with `no-store`, `private` or malformed cache metadata | At the next `GET`, which reads it from the origin each time. |

There is no purge command. A restart empties the memory cache at once ([memory cache](../architecture/memory-cache.md#what-statica-does-not-do)). To shorten these delays for some paths, see [Choose how long objects stay fresh](./cache-tuning.md#choose-how-long-objects-stay-fresh).

## Verify it

Request a directory without its trailing slash, then with it. The output is abridged: `...` stands for the headers left out.

```sh
curl -sI http://127.0.0.1:8080/guide
curl -sI http://127.0.0.1:8080/guide/
```

```text
HTTP/1.1 308 Permanent Redirect
Cache-Control: no-store
Location: /guide/
Vary: Accept-Encoding
X-Content-Type-Options: nosniff
...

HTTP/1.1 200 OK
Cache-Control: max-age=300
Content-Type: text/html; charset=utf-8
Etag: "8c0488487b359933c0ef03df4208ab27"
Last-Modified: Mon, 02 Nov 2026 10:00:00 GMT
Vary: Accept-Encoding
X-Content-Type-Options: nosniff
...
```

Then request a missing page and a hidden path:

```sh
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/missing
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/.git/config
```

```text
404
404
```

The access log shows how statica answered each request, in the [`resolution`](../reference/logs.md#resolution) field, and which object it read, in `origin_key`. The missing page was answered with the custom 404 page, and the hidden path was rejected before any origin read: its event has no `path` and records `error_class=invalid_target`.

```text
level=info event=http_request ... method=HEAD path=/guide status=308 outcome=success resolution=index cache=miss ...
level=info event=http_request ... method=HEAD path=/guide/ status=200 outcome=success resolution=index cache=metadata_hit origin_key=releases/2026-11-08/guide/index.html ...
level=info event=http_request ... method=GET path=/missing status=404 outcome=success resolution=not_found cache=miss origin_key=releases/2026-11-08/404.html ...
level=warn event=http_request ... method=GET status=404 ... bytes_written=10 error_class=invalid_target ...
```

## Pitfalls

- **A wrong `Content-Type` is served as stored.** Content is not inspected, and every response carries `X-Content-Type-Options: nosniff`, so a stylesheet stored as `text/plain` or `binary/octet-stream` is not applied by browsers.
- **Objects stored compressed in place are refused to some clients.** An object stored with `Content-Encoding: gzip` is sent compressed to clients that accept gzip or send no `Accept-Encoding`, and gets 406 Not Acceptable for a client that refuses gzip. Publish compressed copies next to the object instead ([Serve precompressed files](./precompressed-files.md)).
- **`Expires: 0` and other malformed cache metadata disable caching.** The object is sent with `Cache-Control: no-store` and never stored in memory, and its access events record `cache_diagnostic=malformed_origin_cache_metadata`.
- **S3 website redirects are not followed.** An object that only carries `x-amz-website-redirect-location` is served as an ordinary object, usually empty. Use [`redirects`](./redirects.md) instead.
- **Missing pages on AWS without `s3:ListBucket`.** AWS answers 403 instead of 404 for a missing key. The trailing-slash redirect, the custom 404 page and the single-page-app fallback are then skipped, and every missing URL is logged as an origin error ([AWS without `s3:ListBucket`](../architecture/access-denied.md#aws-without-s3-listbucket)).
- **A page published right after a request for it stays missing briefly.** The negative entry from that request answers 404 until it expires.

## Related pages

- [Request flow and URL mapping](../architecture/request-flow.md): the exact order in which a path is mapped to object keys.
- [HTTP responses reference](../reference/http-responses.md): every header statica sends.
- [Control browser and CDN caching](./browser-caching.md)
- [Tune the memory cache](./cache-tuning.md): how long changes take to show.
- [Deploy, roll out and shut down](./deployment.md): restarts and rollouts.
- [Serve precompressed files](./precompressed-files.md)
- [Serve a single-page application](./single-page-apps.md)
- [Configure redirects](./redirects.md)
- [Troubleshooting](./troubleshooting.md#old-content-after-publishing): old content after publishing, and URLs that answer 404.
