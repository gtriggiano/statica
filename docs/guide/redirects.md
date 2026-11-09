# Configure redirects

Send visitors from old URLs to new ones, within the site or to another HTTPS origin, with rules that statica answers without reading the bucket.

## Prerequisites

- The list of URLs to redirect, with their destinations and whether each move is permanent or temporary.

## How rules are applied

The [`redirects`](../reference/configuration.md#redirects) rules are evaluated in order, after it has checked the host, the method and the request target, and before it reads the memory cache or the origin. The first rule that matches answers with its status, a `Location` header and `Cache-Control: no-store`. A rule therefore also redirects a path that exists as an object ([redirect rules](../architecture/request-flow.md#redirect-rules)).

## Redirect a single path

```yaml [config.yaml]
redirects:
  - from: /about-us
    to: /about/
    status: 301
  - from: /about-us/
    to: /about/
    status: 301
```

Matching is exact and case-sensitive, segment by segment, on the decoded path. `/About-Us` does not match `/about-us`, and a trailing slash counts as an empty last segment, so `/about-us` and `/about-us/` need a rule each. A literal segment may be written percent-encoded, such as `/caf%C3%A9`, and is compared with the decoded request path.

## Capture path segments

A placeholder such as `{slug}` takes the place of a whole segment and holds its decoded value, which `to` can reuse:

```yaml [config.yaml]
redirects:
  - from: /blog/{year}/{slug}
    to: /posts/{year}/{slug}/
    status: 301
    captures:
      year:
        pattern: "[0-9]{4}"
```

- `from` matches only paths with the same number of segments: this rule matches `/blog/2024/hello-world`, not `/blog/2024` or `/blog/2024/hello/world`. A rule cannot match any depth; write one rule per depth.
- A placeholder matches one nonempty value of at most 256 bytes. It must fill the whole segment: `/blog/{year}-{month}` is rejected.
- [`captures.<name>.pattern`](../reference/configuration.md#redirects-captures-name-pattern) constrains a placeholder with an RE2 regular expression that must match the whole value: `/blog/24/hello` does not match the rule above. Quote patterns in YAML: an unquoted `[0-9]{4}` is a syntax error.
- Every placeholder in `to` must be bound in `from`.

## Match query fields

A query in `from` lists fields that the request must have:

```yaml [config.yaml]
redirects:
  - from: /search?q={term}
    to: /find/?query={term}
    status: 302
  - from: /p?id={id}
    to: /products/{id}
    status: 301
```

- Each listed field must appear in the request, once: a repeated field is an error (see [Pitfalls](#pitfalls)). Other fields are allowed: `/search?q=red+shoes&page=2` matches the first rule.
- A value in `from` is literal, such as `?lang=en`, or a placeholder.
- A query value can fill a path segment of `to`, as `{id}` does above, only when it is a valid segment. A value that contains `/` or `\`, that is `.` or `..`, or that still contains a `%XX` sequence after decoding, as in `/p?id=a%2Fb` or `/p?id=%2541`, cannot, and the request gets 400 Bad Request.

## Choose what happens to the query

[`redirects[].query`](../reference/configuration.md#redirects-query) decides which query the `Location` carries:

| Value | `Location` query | Example |
| --- | --- | --- |
| `drop` (default) | Only the query written in `to`. | `/search?q=red+shoes&page=2` → `/find/?query=red+shoes` |
| `preserve` | The request's whole query, unchanged. | `/docs/latest/install?lang=en` → `/docs/v2/install?lang=en` |
| `unmatched` | The request's fields that `from` did not list, in their order. | `/p?id=42&utm_source=mail` → `/products/42?utm_source=mail` |

With `preserve` or `unmatched`, `to` cannot have a query of its own:

```yaml [config.yaml]
redirects:
  - from: /docs/latest/{page}
    to: /docs/v2/{page}
    status: 302
    query: preserve
  - from: /p?id={id}
    to: /products/{id}
    status: 301
    query: unmatched
```

## Redirect to another site

A destination can start with `https://` and a literal host, with an optional port, path, query and fragment. Placeholders are allowed in the path and the query, not in the host or the fragment. `http://` destinations are rejected.

```yaml [config.yaml]
redirects:
  - from: /chat
    to: https://chat.example.com/
    status: 307
  - from: /.well-known/change-password
    to: https://accounts.example.com/password
    status: 302
```

Paths under `/.well-known/` can be redirected: a first segment `.well-known` is the only segment starting with a dot that statica accepts in a request path.

## Choose the status

| Status | Use it for |
| --- | --- |
| 301 Moved Permanently, 308 Permanent Redirect | A permanent move: clients and search engines can replace the old URL. |
| 302 Found, 307 Temporary Redirect | A temporary move: clients keep using the old URL. |

Rules apply only to `GET` and `HEAD` requests: other methods get 405 Method Not Allowed before the rules run, and with [`cors`](./headers-and-cors.md) an `OPTIONS` request is answered as a preflight, also before the rules. For `GET` and `HEAD`, 301 and 308 behave alike, as do 302 and 307.

## Order the rules and check them

The first matching rule wins, so put specific rules before general ones. Run [`statica config validate`](../reference/cli.md#statica-config-validate): it warns on standard error when an earlier rule may match every request of a later one. In this configuration, the second rule can never apply, because the pattern of the first one accepts `2020`:

```yaml [config.yaml]
s3:
  bucket: example-site
  region: eu-west-1
redirects:
  - from: /blog/{year}/{slug}
    to: /posts/{year}/{slug}/
    status: 301
    captures:
      year:
        pattern: "[0-9]{4}"
  - from: /blog/2020/{slug}
    to: /archive/{slug}/
    status: 301
```

```text
statica: warning: redirects[1] may be shadowed by redirects[0]
configuration valid
```

Put the `/blog/2020/{slug}` rule first to let it apply. Warnings do not fail validation, and `statica serve` does not print them.

Two kinds of rule sets are errors. Two rules whose `from` templates are equivalent, such as `/blog/{slug}` and `/blog/{post}`, give:

```text
statica: redirects[1].from (line 9, column 11): duplicate equivalent source
```

Literal rules that redirect into each other, such as `/old` to `/new` and `/new` to `/old`, give:

```text
statica: redirects[0].to (line 7, column 9): literal redirect cycle
```

## Verify it

The output is abridged: `...` stands for the headers left out.

```sh
curl -sI 'http://127.0.0.1:8080/blog/2024/hello-world?ref=feed'
```

```text
HTTP/1.1 301 Moved Permanently
Cache-Control: no-store
Content-Length: 18
Content-Type: text/plain; charset=utf-8
Location: /posts/2024/hello-world/
X-Content-Type-Options: nosniff
X-Request-Id: d2996382a4450aebec2d76c6404c546b
...
```

The rule's default `query: drop` removed `ref=feed`.

The access event of a redirect has its status and `outcome=success`, and no `resolution` or `origin_key`, because nothing was read:

```text
level=info event=http_request host=127.0.0.1 method=HEAD path="/blog/2024/hello-world?ref=feed" status=301 outcome=success bytes_written=0 ...
```

A request whose values cannot be used gets 400 Bad Request, or 414 URI Too Long, logged at `warn` with `error_class=invalid_redirect_input`.

## Pitfalls

- **Hidden paths cannot be redirected.** A path with a segment that starts with a dot, other than a first `.well-known`, gets 404 Not Found before the rules run, so `config validate` rejects a `from` such as `/.old/page`.
- **A repeated query field is an error, not a mismatch.** When the path matches and a field listed in `from` appears more than once, as in `/search?q=a&q=b`, the request gets 400 Bad Request, even if a later rule could match.
- **Rules hide objects.** A rule answers before any lookup, so an object at its `from` path is never served.
- **Size limits.** Each template is at most 8 KiB. A request whose `Location` would exceed 8 KiB gets 414 URI Too Long, and a placeholder value longer than 256 bytes does not match.
- **The trailing-slash redirect is separate.** statica's own 308 from `/guide` to `/guide/` happens during resolution, after the rules, and only when `guide/index.html` exists ([Publish a site to S3](./publishing.md)).

## Related pages

- [`redirects` configuration reference](../reference/configuration.md#redirects): every rule of the template syntax.
- [Request flow and URL mapping](../architecture/request-flow.md#redirect-rules)
- [Publish a site to S3](./publishing.md)
