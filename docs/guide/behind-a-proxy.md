# Run behind a reverse proxy, CDN or ingress

Put **statica** behind a load balancer, reverse proxy, CDN or ingress controller, so that the access log records the real client, request IDs follow each request across both, and both sides agree on hostnames and time limits.

## Prerequisites

- The addresses from which your proxies connect to statica: their own IP addresses, or the narrowest ranges that contain them.
- The header in which your proxy passes the client address, `X-Forwarded-For` or `X-Real-IP`, and the header in which it passes a request ID, if it sets one.
- A network where only your proxies can reach statica's public listener.

## Listen on a private address

When the proxy terminates TLS, statica can listen in cleartext on an address that only the proxy reaches:

```yaml [config.yaml]
server:
  listen: 10.0.12.20:8080
```

Without [`server.tls`](../reference/configuration.md#server-tls), statica speaks HTTP/1.1 to the proxy. If the connection between the proxy and statica must be encrypted, keep `server.tls` as described in [Serve HTTPS directly](./tls.md); the proxy then connects with HTTPS, and HTTP/2 when it offers it.

Restrict the public listener with your network controls. A client that reaches statica directly bypasses whatever the proxy enforces, such as rate limits or a web application firewall: statica applies neither.

## Trust only your proxies

statica takes the client address from a forwarded header, and reuses a request ID, only when the immediate peer of the connection is inside [`server.trusted_proxies`](../reference/configuration.md#server-trusted-proxies). From any other peer, it ignores both headers and uses the peer address.

```yaml [config.yaml]
server:
  trusted_proxies: [10.0.12.0/28]
  client_ip_header: X-Forwarded-For
  max_forwarded_hops: 4
```

::: danger Broad ranges let clients choose their address
Every host inside `server.trusted_proxies` can set the client IP that statica logs and the request ID that it reuses. A range such as `10.0.0.0/8`, shared with other workloads, lets any of them forge both. List your proxies' own addresses, or the narrowest ranges that contain them. A range that trusts every address, such as `0.0.0.0/0`, is rejected.
:::

[Trusted proxies and client IP attribution](../architecture/security.md#trusted-proxies-and-client-ip-attribution) describes the rules in full.

## Choose the client IP header

Set [`server.client_ip_header`](../reference/configuration.md#server-client-ip-header) to the header your proxy sets:

| Header | How statica reads it | Configure the proxy to |
| --- | --- | --- |
| `X-Forwarded-For` (default) | Every line as one list of bare IP addresses, walked from the right. The client is the first address that is not in `server.trusted_proxies`, or the leftmost when all are. | Append the address it sees, without a port. |
| `X-Real-IP` | Exactly one IP address. | Overwrite any value the client sent. |

With a chain of proxies, for example a CDN in front of a load balancer, the walk from the right stops at the first address that is not trusted. If an intermediate proxy's address is not in `server.trusted_proxies`, statica logs that proxy as the client.

[`server.max_forwarded_hops`](../reference/configuration.md#server-max-forwarded-hops) bounds the length of `X-Forwarded-For`, `16` by default. Set it to cover the hops you expect. A longer chain, a chain longer than 64 bytes per allowed hop, or an entry that is not a bare IP address (an `address:port`, for example) is ignored as a whole, and statica uses the peer address.

## Pass request IDs

Set [`server.request_id_header`](../reference/configuration.md#server-request-id-header) to the header in which your proxy sends its request ID, `X-Request-ID` by default:

```yaml [config.yaml]
server:
  trusted_proxies: [10.0.12.0/28]
  request_id_header: X-Correlation-ID
```

statica reuses the value when the peer is trusted and the header appears once with 1 to 128 characters among letters, digits, `.`, `_`, `:` and `-`. Otherwise it generates a 32-character hexadecimal ID. A trace header with other characters, such as `=` or `;`, is never reused.

Whatever the request header is called, the response carries the ID in `X-Request-ID`, and the access log records it as `request_id`. Configure the proxy to overwrite the header rather than forward a client's value, and to log the same ID, so that you can follow one request through both logs.

## Preserve the Host header

statica reads the `Host` header, never `X-Forwarded-Host`. It uses the hostname to admit the request, and logs it and labels metrics with it; it never selects content by it ([Host never selects the origin](../architecture/security.md#host-never-selects-the-origin)). Configure the proxy to pass the client's original `Host` through.

List the names your site answers to in [`hosts`](../reference/configuration.md#hosts):

```yaml [config.yaml]
hosts: [www.example.com, example.com]
```

- A request for any other name gets 421 Misdirected Request before it reaches the memory cache or the origin.
- Without `hosts`, every valid hostname is admitted and becomes a value of the `host` label of the request metrics. A client that sends many names makes the number of series grow until the process restarts ([`host` label](../reference/metrics.md#host)).
- If your proxy rewrites `Host` to an internal name, that internal name is what statica admits, logs and counts.

statica ignores `X-Forwarded-Proto` too. The redirects it generates for a missing trailing slash carry a relative `Location`, a path and query, so they keep the scheme and host the client used. Redirect rules with a local destination do the same; rules with an `https://` destination use the host written in the rule.

## Align the time limits

The proxy and statica each have their own limits. When the proxy gives up first, statica records a client that went away (`termination_reason=disconnect`, with status `0` when the response headers were not sent yet), and the cause is visible only in the proxy's logs. [Timeouts and long downloads](../architecture/timeouts.md) describes statica's side.

| statica limit (default) | Proxy setting to align |
| --- | --- |
| [`server.response_header_timeout`](../reference/configuration.md#server-response-header-timeout) (`30s`): budget until the response headers, then 504 | The proxy's wait for response headers from statica: longer than this budget, so that statica answers the 504 itself and logs why. |
| [`s3.read_idle_timeout`](../reference/configuration.md#s3-read-idle-timeout) (`30s`): longest wait for origin data during a transfer | The proxy's inactivity limit between two reads from statica: longer than this, so that a stalled origin is reported by statica as `origin_read_timeout`. |
| [`server.idle_timeout`](../reference/configuration.md#server-idle-timeout) (`60s`): idle keep-alive connections are closed | The proxy's idle timeout for its pooled connections to statica: shorter, so that the proxy never sends a request on a connection that statica is closing. |
| [`server.write_idle_timeout`](../reference/configuration.md#server-write-idle-timeout) (`60s`): a client that accepts no data | Applies to the proxy as the client. A proxy that buffers responses reads quickly, and the end client's speed then matters only to the proxy. |
| [`server.request_timeout`](../reference/configuration.md#server-request-timeout) (`0s`, no total limit) | Any total limit in the proxy ends long downloads on its own. Raise it, or remove it, for routes that serve large files. |

Two capacity limits also apply to proxied traffic. Each request in progress holds one of [`server.max_concurrent_requests`](../reference/configuration.md#server-max-concurrent-requests) slots until its response ends, and every connection the proxy keeps open, idle or not, counts toward [`server.max_connections`](../reference/configuration.md#server-max-connections).

When statica shuts down, it stops accepting connections at once and closes idle keep-alive connections while active requests drain. Remove the instance from the proxy before the stop signal, as described in [Deploy, roll out and shut down](./deployment.md#shut-down-without-cutting-requests).

## Caches in front of statica

A CDN or caching proxy stores responses according to the `Cache-Control` that statica sends, which follows the origin's metadata and the [`cache_control`](../reference/configuration.md#cache-control) settings, not the memory cache ([Control browser and CDN caching](./browser-caching.md)). It must also respect `Vary`: statica adds `Accept-Encoding` to responses that come from resolving a path, `Accept` on single-page-app routes and `Origin` when `cors` is configured ([`Vary`](../reference/http-responses.md#vary)). A cache that ignores `Vary` can serve a Brotli body to a client that does not accept it, or the app shell to a script request.

## Verify it

Send a request through the proxy, then find its `http_request` event in statica's log, by the `X-Request-ID` that the response carries:

```sh
curl -sS -o /dev/null -D - https://www.example.com/ | grep -i '^x-request-id'
```

```text
x-request-id: lb-4f2a9c
```

```text
ts=2026-11-08T00:16:04.239Z level=info event=http_request request_id=lb-4f2a9c request_id_source=proxy host=www.example.com method=GET path=/ status=200 outcome=success resolution=index cache=miss origin_key=index.html duration_ms=13.059 ttfb_ms=13.005 bytes_written=14 client_ip=203.0.113.7 peer_ip=10.0.12.5 client_ip_source=x_forwarded_for
```

Check these fields:

| Field | Expected behind a proxy | If not |
| --- | --- | --- |
| `peer_ip` | The proxy's address | The client reached statica directly. |
| `client_ip_source` | `x_forwarded_for` or `x_real_ip` | `peer`: the proxy is not in `server.trusted_proxies`, or `client_ip_reason` says why its header was not used. |
| `client_ip_reason` | Absent | `missing_client_ip_header`: the proxy sends another header. `invalid_forwarded_chain`: an entry is not a bare IP address. `forwarded_chain_too_long`: raise `server.max_forwarded_hops`. `invalid_real_ip`: `X-Real-IP` is repeated or not one address. |
| `request_id_source` | `proxy`, when the proxy sets a request ID | `generated` with `request_id_reason=missing_request_id` (wrong header name) or `invalid_request_id` (repeated, or characters outside the allowed set). |
| `host` | Your public hostname | `authority` instead, with status 421 and `error_class=unknown_host`: the proxy changed `Host`, or the name is missing from `hosts`. |

The `startup` event reports how many ranges you trust (`trusted_proxies=1`) and whether a host allowlist is in force (`hosts=allowlist`). The [log reference](../reference/logs.md#client-ip-reason) lists every value.

## Pitfalls

- **The proxy's health checks hit the public listener.** They are logged and counted like visitors' requests, and get 421 when their `Host` is not in `hosts`. When the proxy can reach the admin listener, point its checks at `/readyz` instead ([Health checks and readiness](./health-checks.md)).
- **Unknown hosts take a request slot.** Capacity is checked before the host, so a flood of misdirected requests can still fill `server.max_concurrent_requests`.
- **`Forwarded` is not supported.** statica reads neither the RFC 7239 `Forwarded` header nor `X-Forwarded-Host` and `X-Forwarded-Proto`.
- **The client IP is attribution only.** statica makes no access decision from it. Set [`observability.log_client_ip`](../reference/configuration.md#observability-log-client-ip) to `false` to keep addresses out of the logs.

## Related pages

- [Security model](../architecture/security.md): trust boundaries, client IP attribution and request IDs.
- [Timeouts and long downloads](../architecture/timeouts.md).
- [Serve HTTPS directly](./tls.md).
- [Deploy, roll out and shut down](./deployment.md).
- [Work with the logs](./logs.md).
- [Troubleshooting](./troubleshooting.md#_421-misdirected-request): 421 responses, and [504 responses](./troubleshooting.md#_504-gateway-timeout) for clients that leave.
