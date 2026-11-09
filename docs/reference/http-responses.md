# HTTP responses reference

This page lists the status codes that the public listener sends and when, the headers that statica sets or controls, the origin metadata that reaches visitors, and the request headers that statica reads. The admin listener's responses are in the [admin endpoints reference](./admin-endpoints.md). [Request flow and URL mapping](../architecture/request-flow.md) describes the order of the checks.

## Statuses from statica

| Status | When |
| --- | --- |
| 200 OK | An object: the object at the exact key, the `index` object of a path ending in `/`, or the SPA shell. A `HEAD` gets the headers of the `GET` without a body. |
| 204 No Content | An allowed CORS preflight (`OPTIONS`). |
| 206 Partial Content | A satisfiable single byte range on a `GET`. See [Ranges and conditional requests](../architecture/ranges-and-conditionals.md). |
| 301, 302, 307, 308 | A [`redirects`](./configuration.md#redirects) rule matched; the status is the rule's. |
| 304 Not Modified | `If-None-Match`, or `If-Modified-Since` without `If-None-Match`, matched the object. |
| 308 Permanent Redirect | Also when the path has no trailing slash, the object at the exact key is missing and `<path>/<index>` exists. `Location` is the path with `/` added, query included. |
| 400 Bad Request | A malformed `Host`; a `GET` or `HEAD` with a request body; an invalid request target; a redirect rule whose destination cannot hold the request's values; an invalid object key. |
| 403 Forbidden | With `cors` configured, every `OPTIONS` request is a CORS preflight, and it gets 403 when its `Origin` is not exactly one value listed in `cors.allowed_origins`, when it does not carry exactly one `Access-Control-Request-Method`, when that method is not `GET` or `HEAD`, or when `Access-Control-Request-Headers` names a header other than `Range`, `If-Match`, `If-None-Match`, `If-Modified-Since`, `If-Unmodified-Since` and `If-Range`. This is the only 403. |
| 404 Not Found | The object is missing: the [`not_found`](./configuration.md#not-found) object, or the built-in body when there is none or it is missing too. Also a hidden path, without any origin read, and an origin denial with [`errors.access_denied: not_found`](./configuration.md#errors-access-denied); both always use the built-in body. |
| 405 Method Not Allowed | Any method other than `GET` and `HEAD`, and other than `OPTIONS` when `cors` is configured. Carries `Allow`. |
| 406 Not Acceptable | The object exists, but `Accept-Encoding` accepts none of its available codings. |
| 412 Precondition Failed | `If-Match`, or `If-Unmodified-Since` without `If-Match`, failed. No body. |
| 414 URI Too Long | A request target longer than 8 KiB, an object key longer than 1024 bytes, or a redirect `Location` longer than 8 KiB. |
| 416 Range Not Satisfiable | A valid byte range that the object cannot satisfy, such as one that starts beyond its end. Carries `Content-Range: bytes */<size>`. |
| 421 Misdirected Request | [`hosts`](./configuration.md#hosts) is set and the hostname is not listed. |
| 502 Bad Gateway | The origin failed before the response headers: for example it was unreachable, answered with a server error or a malformed response, or rejected the bucket or the endpoint. Also an origin denial with `errors.access_denied: bad_gateway`. |
| 503 Service Unavailable | Every request slot is in use (`server.max_concurrent_requests`); the origin throttled the read; a shutdown canceled the request before its headers; or the object kept changing during a range read from the origin. |
| 504 Gateway Timeout | `server.response_header_timeout` or `server.request_timeout` expired before the response headers, or an origin read timed out before them. See [Timeouts and long downloads](../architecture/timeouts.md#before-the-headers). |

A client that goes away before the response headers receives nothing. statica records the request with status `0`, `outcome=incomplete` and the [`termination_reason`](./logs.md#termination-reason) `disconnect`, whatever response it was preparing.

Once its headers are sent, a response keeps its status. A failure during the body, such as an origin that stops sending or a client that stops reading, ends the connection or the HTTP/2 stream early; the [access log](./logs.md#termination-reason) records why.

## Statuses from Go's HTTP server

Go's HTTP server answers some malformed requests before statica's handler sees them. These responses have a short plain-text body from Go and none of the headers below: no `X-Request-ID`, no `X-Content-Type-Options` and no `headers.set`. statica logs no `http_request` event for them and counts them in no metric.

| Status | When |
| --- | --- |
| 400 Bad Request | A malformed request line or header, an HTTP/1.1 request without a valid `Host` header, or a plain HTTP request sent to a TLS listener. |
| 417 Expectation Failed | An `Expect` header other than `100-continue`. |
| 431 Request Header Fields Too Large | Request headers, request line included, larger than `server.max_header_bytes` plus Go's 4 KiB allowance. |
| 501 Not Implemented | An unsupported `Transfer-Encoding`. |
| 505 HTTP Version Not Supported | A request line with an HTTP version other than 1.0 and 1.1. |

A failed TLS handshake gets no HTTP response.

## Statuses statica never sends

- 401 and 407: statica has no authentication.
- 429: statica has no rate limiting. A full set of request slots gets 503.
- 500: a failure inside statica's handler closes the connection, or resets the HTTP/2 stream, instead of answering.
- The origin's own status. An origin error becomes 404, 502, 503 or 504, and an origin 403 is never forwarded as 403.

## Generated responses

statica generates the response itself for rejections, redirects, the built-in 404, 406, 416, 502, 503, 504 and CORS preflights. A generated response has:

- a body that is the status text and a newline, such as `Not Found`, with `Content-Type: text/plain; charset=utf-8` and `Content-Length`; a `HEAD` gets the same headers without the body, and a 204 has neither body nor `Content-Type`;
- `Cache-Control: no-store` and no `Expires`;
- the headers that every response carries: `X-Request-ID`, `X-Content-Type-Options` and `headers.set`.

The body never contains origin error details or request values. A generated response that is ready only after `server.response_header_timeout` or `server.request_timeout` has expired becomes a 504, without `Location`.

The 404 for a hidden path, `/.git/config`, without `cors` configured:

```text
HTTP/1.1 404 Not Found
Cache-Control: no-store
Content-Length: 10
Content-Type: text/plain; charset=utf-8
X-Content-Type-Options: nosniff
X-Request-Id: 822c681a0572fdb34ea05df3219ea6f9
Date: Sat, 07 Nov 2026 23:47:12 GMT

Not Found
```

Responses for an object are not generated, including these:

| Response | Headers |
| --- | --- |
| 404 with the `not_found` object | The object's body, `Content-Type`, `Content-Length`, `Content-Encoding`, `Content-Disposition` and `Content-Language`, with `Cache-Control: no-store`. No `ETag`, `Last-Modified`, `Expires` or `Age`. |
| 200 SPA shell | The object's headers with `ETag` and `Last-Modified`, `Cache-Control: no-store` and `Vary: Accept`. |
| 304 | The headers of the 200, without `Content-Length`, `Content-Type`, `Content-Encoding` and `Accept-Ranges`. |
| 412 | The object's headers with `Content-Length: 0` and `Cache-Control: no-store`, without `Expires` and `Accept-Ranges`, and no body. |

## Response headers

Header names are case-insensitive; Go's HTTP/1.1 server writes them in its canonical form, such as `X-Request-Id` and `Etag`.

| Header | When and value |
| --- | --- |
| `X-Request-ID` | Every response. The request ID of a trusted proxy, read from `server.request_id_header` when it is valid, otherwise 32 hexadecimal characters that statica generates. The response header is always named `X-Request-ID`. See [Security model](../architecture/security.md#request-ids). |
| `X-Content-Type-Options` | Every response: `nosniff`. |
| [`headers.set`](./configuration.md#headers-set) | Every response, generated ones included. See [Response headers and CORS](../guide/headers-and-cors.md). |
| `Vary` | See [`Vary`](#vary). |
| `Cache-Control` | See [`Cache-Control`](#cache-control). |
| `Expires` | The origin's `Expires`, on a 200, 206 or 304 for an object other than the SPA shell, when no explicit `cache_control` value applies and the origin's cache metadata is well formed. |
| `Age` | On a 200, 206, 304 or 412 for an object other than the SPA shell, when the age is above 0, unless malformed origin cache metadata made the response `no-store`. See [Cache-Control and Age](../architecture/cache-control.md#age). |
| `Date` | On the same responses as `Age`, the origin's `Date` when it is valid, kept with the stored entry when the response comes from memory. Otherwise, the time of the response. |
| `ETag`, `Last-Modified` | On a 200, 206, 304 or 412 for an object, the SPA shell included, when the origin's values are valid. Not on a 404. |
| `Content-Type` | The object's type when it is a valid media type, otherwise a type chosen by file extension. A precompressed variant has the type of the object it encodes. See [`Content-Type`](#content-type). |
| `Content-Length` | The object's length when it is known, the range's length on a 206, `0` on a 412, absent on a 304. |
| `Content-Encoding` | `br` or `gzip`, in lowercase: the coding of a precompressed variant, or of an object stored with that encoding (`GZIP` is sent as `gzip`). A `.br` or `.gz` variant without stored encoding counts as `br` or `gzip`. |
| `Content-Disposition`, `Content-Language` | The object's values, when they are well formed. |
| `Accept-Ranges` | `bytes`, on 200 and 206 responses for an object when `s3.features.range_reads` and `s3.features.conditional_reads` are both `true` and the object has a known length and a strong entity tag. Absent otherwise, including on 304, 412 and the `not_found` object's 404; statica never sends `none`. See [Ranges and conditional requests](../architecture/ranges-and-conditionals.md#accept-ranges). |
| `Content-Range` | `bytes <first>-<last>/<size>` on a 206, `bytes */<size>` on a 416. |
| `Location` | On a redirect: the rule's destination, or the path with a trailing slash. |
| `Allow` | On a 405: `GET, HEAD`, or `GET, HEAD, OPTIONS` when `cors` is configured. |
| `Access-Control-Allow-Origin` | With `cors` configured, when the request's single `Origin` is allowed: that `Origin`, exactly as sent. Not on a denied preflight. |
| `Access-Control-Allow-Methods` | On an allowed preflight: `GET, HEAD`. |
| `Access-Control-Allow-Headers` | On an allowed preflight that asked for headers: the requested names, without duplicates, such as `Range, If-None-Match`. Only `Range`, `If-Match`, `If-None-Match`, `If-Modified-Since`, `If-Unmodified-Since` and `If-Range` are accepted; a preflight that asks for any other header gets 403. |
| `Connection` | `close` on an HTTP/1 request that carries a body, which statica does not read: its rejection, or an allowed preflight. |

statica never sends `Access-Control-Allow-Credentials`, `Access-Control-Max-Age`, `Access-Control-Expose-Headers` or `Set-Cookie`. [Allow cross-origin reads](../guide/headers-and-cors.md#allow-cross-origin-reads) shows how to configure `cors`.

### `Vary`

statica adds each member once, only when it applies:

| Member | Added to |
| --- | --- |
| `Origin` | Every response to a request that passed target validation, when `cors` is configured. Rejections for capacity, host, method, request body and target do not have it. |
| `Access-Control-Request-Method`, `Access-Control-Request-Headers` | CORS preflight responses, 204 and 403. |
| `Accept-Encoding` | Every response that comes from resolving the path: responses for an object, the 308 that adds a trailing slash, the built-in 404 for a missing object, 406 and 416. In both `compression.mode` values. Not on redirect rules, rejections or a denial masked as 404, and not on 502, 503 and 504 responses. |
| `Accept` | With `spa` configured, responses for a path that the SPA fallback covers once the object is missing: the shell and the 404. See [SPA fallback](../architecture/spa-fallback.md). |

### `Cache-Control`

| Response | `Cache-Control` |
| --- | --- |
| Generated responses, redirects included | `no-store` |
| 404 with the `not_found` object, the SPA shell, 412 | `no-store` |
| 200, 206 and 304 for an object | An explicit value from a `cache_control` rule or `cache_control.default`. With `auto`: `no-store` when the origin's cache metadata is malformed; otherwise the origin's `Cache-Control`, or none when the origin sent only `Expires`; otherwise `max-age=<ttl in seconds>` when the memory cache applies to the object with a `ttl` of at least 1 s, or `no-cache`. |

[Cache-Control and Age](../architecture/cache-control.md#precedence) describes the precedence and [`cache_control`](./configuration.md#cache-control) the settings; [Control browser and CDN caching](../guide/browser-caching.md) shows common policies.

### `Content-Type`

statica sends the object's stored `Content-Type` when it is a valid media type. When it is missing or invalid, the type comes from the extension of the object's name, ignoring case, for every response of that object: 200, 206, `HEAD` and the `not_found` object's 404. For a precompressed variant, the name is the canonical one, without `.br` or `.gz`.

| Extension | `Content-Type` |
| --- | --- |
| `.html`, `.htm` | `text/html; charset=utf-8` |
| `.css` | `text/css; charset=utf-8` |
| `.js`, `.mjs` | `text/javascript; charset=utf-8` |
| `.json`, `.map` | `application/json` |
| `.txt` | `text/plain; charset=utf-8` |
| `.csv` | `text/csv; charset=utf-8` |
| `.xml` | `application/xml` |
| `.svg` | `image/svg+xml` |
| `.png` | `image/png` |
| `.jpg`, `.jpeg` | `image/jpeg` |
| `.gif` | `image/gif` |
| `.webp` | `image/webp` |
| `.avif` | `image/avif` |
| `.ico` | `image/vnd.microsoft.icon` |
| `.pdf` | `application/pdf` |
| `.zip` | `application/zip` |
| `.gz` | `application/gzip` |
| `.wasm` | `application/wasm` |
| `.woff` | `font/woff` |
| `.woff2` | `font/woff2` |
| `.ttf` | `font/ttf` |
| `.otf` | `font/otf` |
| `.mp4` | `video/mp4` |
| `.webm` | `video/webm` |
| `.mp3` | `audio/mpeg` |
| `.ogg` | `audio/ogg` |
| Any other, or none | `application/octet-stream` |

## Origin metadata

statica forwards an allowlist of the origin's response metadata, after validating it:

| Origin header | Forwarded as |
| --- | --- |
| `Content-Type` | `Content-Type`, when it is a valid media type. |
| `Content-Length` | `Content-Length`. |
| `ETag`, `Last-Modified` | The same headers, when valid. |
| `Content-Encoding` | `br` and `gzip`, compared ignoring case and sent in lowercase. `identity` and an empty value mean no encoding. |
| `Content-Disposition`, `Content-Language` | The same headers, when well formed. |
| `Cache-Control`, `Expires`, `Date`, `Age` | Following [`Cache-Control`](#cache-control) and the rows above. |

Everything else stays at the origin:

- user-defined object metadata (`x-amz-meta-*`);
- website redirect metadata (`x-amz-website-redirect-location`): statica neither forwards nor follows it;
- every other provider header, such as version, encryption, storage class, checksum and request ID headers, `Server`, `Set-Cookie` and the bucket's CORS headers;
- the origin's status code, error messages and error bodies.

These origin values make the whole origin response invalid, and the request gets 502 with `error_class=origin_invalid_response`:

- an allowlisted value that contains CR, LF or NUL;
- a `Last-Modified` that is not a valid date;
- a `Content-Encoding` other than `identity`, `br` and `gzip`, ignoring case, or a `.br` or `.gz` variant whose stored `Content-Encoding` names another coding.

## Request headers

statica reads these request headers:

| Header | Use |
| --- | --- |
| `Host` | Host admission. In an absolute-form target, the target's authority. |
| `server.client_ip_header` | `X-Forwarded-For` or `X-Real-IP`, for the client IP, only from a peer in `server.trusted_proxies`. |
| `server.request_id_header` | The request ID, only from a peer in `server.trusted_proxies`. |
| `Origin`, `Access-Control-Request-Method`, `Access-Control-Request-Headers` | CORS, when `cors` is configured. |
| `Accept-Encoding` | Choice among the object and its precompressed variants, and 406. |
| `Accept` | Eligibility for the SPA fallback. |
| `If-Match`, `If-None-Match`, `If-Modified-Since`, `If-Unmodified-Since` | Preconditions. |
| `Range`, `If-Range` | Byte ranges, on `GET` only. |
| `Cache-Control` | The directives `no-store`, `no-cache` and `max-age`, for the memory cache, when it is enabled. |
| `Content-Length`, `Transfer-Encoding` | Detecting a request body on `GET` and `HEAD`. |

statica ignores every other request header, including:

- forwarding headers other than `server.client_ip_header`, such as `Forwarded`, `X-Forwarded-Host`, `X-Forwarded-Proto` and `Via`, and the configured header and `server.request_id_header` when the peer is not a trusted proxy;
- `Pragma`, and request `Cache-Control` directives other than `no-store`, `no-cache` and `max-age`, such as `max-stale` and `min-fresh`;
- `Cookie` and `Authorization`: statica has no visitor authentication or sessions;
- `Referer`, `User-Agent` and the rest.

Request headers are never sent to the origin, and the query string never selects an object.
