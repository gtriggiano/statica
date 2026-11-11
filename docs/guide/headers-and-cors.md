# Response headers and CORS

Add security headers such as a Content Security Policy to every response, and let scripts on other sites read your files with CORS.

## Prerequisites

- The values of the headers you want to send, for example the Content Security Policy that your pages need.
- For CORS, the exact origins, such as `https://app.example.com`, whose scripts should read the site.

## Add static headers

[`headers.set`](../reference/configuration.md#headers-set) adds the same headers to every response that **statica** writes on the public listener: objects, 404 responses, redirects, errors and CORS preflights. A common set:

```yaml [config.yaml]
headers:
  set:
    Content-Security-Policy: "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'"
    Referrer-Policy: strict-origin-when-cross-origin
    Strict-Transport-Security: max-age=300
    Permissions-Policy: camera=(), microphone=(), geolocation=()
```

- **`Content-Security-Policy`** limits where pages may load scripts, styles, images and frames from. A policy that is too strict breaks pages, so try a new one first as `Content-Security-Policy-Report-Only`: browsers do not enforce it, and only report violations, in the console or to an endpoint the policy names.
- **`Referrer-Policy`** limits the `Referer` that browsers send when visitors follow links to other sites.
- **`Strict-Transport-Security`** tells browsers to use only HTTPS for the host for `max-age` seconds. Browsers honor it only on HTTPS responses, so set it when visitors reach the site over HTTPS, through statica's own [TLS](../reference/configuration.md#server-tls) or a proxy in front of it ([Send Strict-Transport-Security](./tls.md#send-strict-transport-security)).
- **`Permissions-Policy`** turns off browser features that the site does not use.

::: warning HSTS cannot be withdrawn
A browser that has received `Strict-Transport-Security` refuses plain HTTP to the host until `max-age` runs out, and to every subdomain too with `includeSubDomains`. Removing the header does not undo it. Start with a short `max-age`, such as `300`, raise it once HTTPS works everywhere, and add `includeSubDomains` only when every subdomain serves HTTPS. `headers.set` applies to every hostname that the instance serves.
:::

Each value is sent exactly as written. Quote a value that YAML could read differently: one that starts with a quote or contains `: ` or ` #`, as the Content Security Policy above does, and one that YAML reads as a number or a boolean, such as `1` or `true`, which is otherwise rejected:

```text
statica: headers.set.X-Test (line 7, column 13): expected a string
```

A value can contain tabs but no other ASCII control characters, and it can take part of its text from an environment variable with `${NAME}`. Names are case-insensitive: two names that differ only in case are rejected.

Every response also carries `X-Content-Type-Options: nosniff`, which you cannot change or remove.

### Reserved names

Names that statica sets itself, or that another setting controls, are rejected:

| Names | Instead |
| --- | --- |
| `Cache-Control`, `Expires` | Use [`cache_control`](./browser-caching.md). |
| Every `Access-Control-*` name | Use `cors` ([Allow cross-origin reads](#allow-cross-origin-reads)). |
| `Content-Type`, `Content-Encoding`, `Content-Disposition`, `Content-Language`, `Content-Length`, `Content-Range`, `ETag`, `Last-Modified` | Set the object's metadata when you upload it ([Publish a site to S3](./publishing.md)). |
| `X-Request-ID`, `X-Content-Type-Options`, `Location`, `Vary`, `Date`, `Age`, `Accept-Ranges` | Set by statica. |
| Hop-by-hop headers, `Host`, `Authorization`, `WWW-Authenticate`, `Cookie`, `Set-Cookie` | Cannot be set. |

A reserved name is reported with its field path, while a syntactically invalid name is reported on `headers.set` without repeating the name:

```text
statica: headers.set.Cache-Control (line 6, column 5): reserved response header name
statica: headers.set (line 6, column 5): invalid response header name
```

## Allow cross-origin reads

By default, statica sends no CORS headers, so browsers do not let scripts on other sites read its responses. List the origins that may:

```yaml [config.yaml]
cors:
  allowed_origins:
    - https://www.example.com
    - https://app.example.com:8443
```

Each entry is an exact origin: a scheme, a host and an optional port, without a path (not even `/`) and without wildcards. Default ports do not count, so `https://www.example.com:443` is the same origin as `https://www.example.com`.

With the [`cors`](../reference/configuration.md#cors) block:

- a `GET` or `HEAD` request whose `Origin` is listed gets `Access-Control-Allow-Origin` with that `Origin`; a request from another origin is still served, without it;
- every response to a request that passes target validation carries `Vary: Origin`;
- `OPTIONS` requests are handled as preflights. A listed origin that asks for `GET` or `HEAD`, with request headers among `Range`, `If-Match`, `If-None-Match`, `If-Modified-Since`, `If-Unmodified-Since` and `If-Range`, gets 204 No Content. Every other `OPTIONS` request that passes target validation gets 403 Forbidden.

The grant is deliberately narrow: `Access-Control-Allow-Credentials`, `Access-Control-Expose-Headers` and `Access-Control-Max-Age` are never sent. So a script that sends cookies or other credentials cannot read the response, a script can read only the response headers that browsers always expose, such as `Content-Type` and `Content-Length` but not `ETag`, and browsers keep each preflight result for their own default time.

CORS decides what browsers let other sites read; it does not restrict who can fetch the files.

## Verify it

Request a page from an allowed origin. The outputs on this page are abridged: `...` stands for the headers left out.

```sh
curl -sI -H 'Origin: https://www.example.com' http://127.0.0.1:8080/
```

```text
HTTP/1.1 200 OK
Access-Control-Allow-Origin: https://www.example.com
Cache-Control: max-age=300
Content-Security-Policy: default-src 'self'; img-src 'self' data:; frame-ancestors 'none'
Content-Type: text/html; charset=utf-8
Permissions-Policy: camera=(), microphone=(), geolocation=()
Referrer-Policy: strict-origin-when-cross-origin
Strict-Transport-Security: max-age=300
Vary: Accept-Encoding
Vary: Origin
X-Content-Type-Options: nosniff
...
```

Then send a preflight that asks to use the `Range` header:

```sh
curl -si -X OPTIONS \
  -H 'Origin: https://www.example.com' \
  -H 'Access-Control-Request-Method: GET' \
  -H 'Access-Control-Request-Headers: range' \
  http://127.0.0.1:8080/assets/app.3f9c2b.js
```

```text
HTTP/1.1 204 No Content
Access-Control-Allow-Headers: Range
Access-Control-Allow-Methods: GET, HEAD
Access-Control-Allow-Origin: https://www.example.com
Cache-Control: no-store
Content-Security-Policy: default-src 'self'; img-src 'self' data:; frame-ancestors 'none'
Permissions-Policy: camera=(), microphone=(), geolocation=()
Referrer-Policy: strict-origin-when-cross-origin
Strict-Transport-Security: max-age=300
Vary: Origin
Vary: Access-Control-Request-Method
Vary: Access-Control-Request-Headers
X-Content-Type-Options: nosniff
...
```

The preflight carries the `headers.set` headers and `nosniff` too.

A preflight from an origin that is not listed gets 403 Forbidden:

```sh
curl -si -X OPTIONS \
  -H 'Origin: https://other.example.net' \
  -H 'Access-Control-Request-Method: GET' \
  http://127.0.0.1:8080/ | head -1
```

```text
HTTP/1.1 403 Forbidden
```

It is logged at `warn` with `error_class=cors_preflight_denied`, and counted in [`statica_http_rejected_requests_total`](../reference/metrics.md#statica-http-rejected-requests-total) with `reason="cors_preflight_denied"`:

```text
level=warn event=http_request host=127.0.0.1 method=OPTIONS path=/ status=403 ... error_class=cors_preflight_denied ...
```

## Pitfalls

- **Some responses carry none of these headers.** Go's HTTP server answers a few malformed requests before statica sees them, such as 431 Request Header Fields Too Large for oversized headers. Those responses have no `headers.set` headers, no `nosniff` and no CORS headers ([statuses from Go's HTTP server](../reference/http-responses.md#statuses-from-go-s-http-server)).
- **Enabling CORS changes the answer to `OPTIONS`.** Without `cors`, every `OPTIONS` request gets 405 Method Not Allowed. With it, an `OPTIONS` request that is not a valid preflight, such as one without `Origin`, gets 403 Forbidden.
- **`Vary: Origin` multiplies cached copies.** CDNs and other caches that honor `Vary` keep a separate copy of each response per `Origin` value.
- **Headers are the same for every path.** `headers.set` has no per-path rules: a policy written for HTML pages is also sent with images and scripts.
- **The admin listener is not covered.** `headers.set` applies to the public listener only.

## Related pages

- [Security model](../architecture/security.md#response-safety): response safety and the CORS model.
- [Request flow and URL mapping](../architecture/request-flow.md#methods-and-cors-preflight): where preflights are answered.
- [HTTP responses reference](../reference/http-responses.md#response-headers): every header statica sets.
- [`headers` configuration reference](../reference/configuration.md#headers) and [`cors` configuration reference](../reference/configuration.md#cors)
