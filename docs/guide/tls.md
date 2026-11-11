# Serve HTTPS directly

Terminate TLS in **statica** itself, so that clients connect to it over HTTPS without a proxy in front.

If a load balancer, CDN or ingress terminates TLS for you, statica can listen in cleartext behind it instead: see [Run behind a reverse proxy](./behind-a-proxy.md).

## Prerequisites

- A certificate chain and its private key, both in PEM format. The chain starts with the leaf certificate, followed by the intermediate certificates that clients need. The leaf must cover every hostname that clients use.
- Read access to both files for the user that runs statica.
- A port that this user may bind. Ports below 1024, such as 443, usually need a privilege: see [Binding port 443](#binding-port-443).

## Configure the listener

Add a [`server.tls`](../reference/configuration.md#server-tls) block. Its presence switches the public listener to HTTPS; nothing else is needed.

```yaml [config.yaml]
hosts: [www.example.com, example.com]
server:
  listen: ":8443"
  tls:
    cert_file: tls/fullchain.pem
    key_file: tls/privkey.pem
    min_version: "1.2"
```

- [`server.tls.cert_file`](../reference/configuration.md#server-tls-cert-file) and [`server.tls.key_file`](../reference/configuration.md#server-tls-key-file) may be absolute or relative. A relative path is resolved against the directory of the configuration file, not the working directory. `statica config print` shows the resolved absolute paths.
- [`server.tls.min_version`](../reference/configuration.md#server-tls-min-version) is `"1.2"` (the default) or `"1.3"`. Quote it: an unquoted `1.3` is a YAML number and is rejected. Cipher suites are Go's defaults and cannot be configured.
- One certificate chain serves every hostname. The name a client asks for in the TLS handshake does not select another certificate, so list in [`hosts`](../reference/configuration.md#hosts) only names that the certificate covers.

The listener now offers HTTP/2 and HTTP/1.1 through ALPN, and serves only HTTPS: a cleartext request on the same port gets 400 Bad Request from Go's HTTP server. Without `server.tls`, the listener serves HTTP/1.1 only; statica has no cleartext HTTP/2. The admin listener always stays cleartext HTTP/1.1.

## Validate the files

```sh
statica config validate --config /etc/statica/config.yaml
```

```text
configuration valid
```

Without `s3.region`, a second line follows, `S3 region deferred to AWS discovery at startup`: it concerns the origin, not TLS.

`config validate` loads the certificate chain and the key, checks that the key matches the first certificate of the chain and that every certificate parses. `statica serve` runs the same checks before it starts. A missing file or a key that does not match fails with exit status 2:

```text
statica: server.tls (line 5, column 5): cannot load a matching certificate and private key
```

::: warning What validation does not check
statica does not check that the certificate is still valid, that it covers your hostnames, or that the chain is complete and trusted. An expired certificate passes `config validate` and `serve` starts with it. Check these properties with your own tools, as in [Verify it](#verify-it).
:::

## Start statica

```sh
statica serve --config /etc/statica/config.yaml
```

The `startup` event reports TLS (abridged):

```text
ts=2026-11-08T00:15:41.269Z level=info event=startup ... public_listen=[::]:8443 admin_listen=127.0.0.1:9090 tls=on tls_min_version=1.2 hosts=allowlist hosts_count=2 ...
```

## Binding port 443

statica binds exactly `server.listen`. On most systems, an unprivileged process cannot bind a port below 1024. Rather than running statica as root:

- grant the process the capability to bind low ports, for example on Linux `CAP_NET_BIND_SERVICE` through your service manager (the [systemd example](./deployment.md#run-it-under-a-supervisor) shows the setting); or
- listen on a high port such as `8443` and forward 443 to it with your load balancer, a container port mapping or a firewall rule.

## Rotate the certificate

statica reads the certificate and key once, when it starts. Nothing reloads them, and SIGHUP ends the process instead of reloading (see [Deploy, roll out and shut down](./deployment.md#signals)). To install a renewed certificate:

1. Write the new chain and key to the configured paths, or to new paths that you set in the configuration.
2. Run `statica config validate` with the new files, as the user that runs statica.
3. Restart statica. With several instances, restart them one at a time, so that the others keep serving: each restart drains active requests and starts with an empty memory cache ([Configuration changes and restarts](../architecture/lifecycle.md#configuration-changes-and-restarts)).

statica exports no metric about the certificate. Watch its expiry date with your own monitoring, for example by connecting to the listener as in [Verify it](#verify-it).

## Send Strict-Transport-Security

statica never adds `Strict-Transport-Security` by itself. To send it, set it in [`headers.set`](../reference/configuration.md#headers-set), which adds it to every response on the public listener, for every hostname the instance serves ([Response headers and CORS](./headers-and-cors.md)):

```yaml [config.yaml]
headers:
  set:
    Strict-Transport-Security: max-age=300
```

::: warning HSTS cannot be withdrawn
A browser that has received `Strict-Transport-Security` refuses plain HTTP to the host until `max-age` runs out, and to every subdomain too with `includeSubDomains`. Removing the header does not undo it. Start with a short `max-age`, such as `300`, raise it once every name in `hosts` works over HTTPS, and add `includeSubDomains` only when every subdomain serves HTTPS.
:::

## What statica does not provide

| Missing feature | How to cover it |
| --- | --- |
| Automatic certificates (ACME) | Run an ACME client that writes the certificate files, then restart statica. Or terminate TLS at a proxy that manages certificates. |
| Certificate reload | Restart statica, as in [Rotate the certificate](#rotate-the-certificate). |
| Client certificates (mutual TLS) | Terminate TLS at a proxy that verifies client certificates. |
| A cleartext listener that redirects HTTP to HTTPS | Run the redirect on port 80 in a load balancer or proxy. statica binds only `server.listen`. |
| Several certificates, chosen by hostname | Use one certificate that covers every name, or run one instance per certificate. |

## Verify it

Request a page over HTTPS and check the protocol that the client negotiated:

```sh
curl -sS -o /dev/null -w '%{http_version} %{http_code}\n' https://www.example.com:8443/
curl -sS -o /dev/null -w '%{http_version} %{http_code}\n' --http1.1 https://www.example.com:8443/
```

```text
2 200
1.1 200
```

Show the certificate that statica presents, with its expiry date:

```sh
openssl s_client -connect www.example.com:8443 -servername www.example.com </dev/null 2>/dev/null \
  | openssl x509 -noout -subject -enddate
```

```text
subject=CN=www.example.com
notAfter=Dec  8 00:15:22 2026 GMT
```

With `min_version: "1.3"`, a client limited to TLS 1.2 fails the handshake:

```sh
curl -sS --tls-max 1.2 https://www.example.com:8443/
```

A failed handshake produces no response and no log event: statica discards the TLS errors of Go's HTTP server, because they carry client data ([safe logging](../architecture/security.md#safe-logging)). Successful requests are logged as usual, as `http_request` events.

## Pitfalls

- **Incomplete chain.** If `cert_file` holds only the leaf, browsers that do not already have the intermediate certificate fail to verify the site. statica does not detect this. Check the chain with `openssl s_client -connect www.example.com:8443 -servername www.example.com -showcerts`.
- **Chain in the wrong order.** The key must match the first certificate in the file. A chain that starts with an intermediate fails validation with `cannot load a matching certificate and private key`.
- **Unreadable key.** Validate as the user that runs statica, with the same file permissions, so that `config validate` reads the files the way `serve` will. Relative paths resolve against the configuration file's directory, whatever the working directory.
- **Health checks against the HTTPS port.** A cleartext check of the public listener gets 400 from Go's HTTP server. Point probes at the admin listener, which is cleartext: see [Health checks and readiness](./health-checks.md).
- **Slow handshakes.** [`server.read_header_timeout`](../reference/configuration.md#server-read-header-timeout) also bounds the TLS handshake of a new connection, and connections in a handshake count toward [`server.max_connections`](../reference/configuration.md#server-max-connections).

## Related pages

- [TLS on the public listener](../architecture/security.md#tls-on-the-public-listener): what statica does and does not do with TLS.
- [Configuration reference: `server.tls`](../reference/configuration.md#server-tls).
- [Run behind a reverse proxy, CDN or ingress](./behind-a-proxy.md).
- [Deploy, roll out and shut down](./deployment.md).
- [Response headers and CORS](./headers-and-cors.md).
- [Troubleshooting](./troubleshooting.md#statica-exits-with-status-2-at-startup): certificate and key files that fail to load.
