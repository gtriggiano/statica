# Connect to S3 and S3-compatible providers

Point statica at a bucket on AWS S3 or on an S3-compatible service, give it read-only credentials, and check the connection before you serve traffic.

## Prerequisites

- A bucket with your site under a [publication prefix](./publishing.md), and its region. For an S3-compatible service, its endpoint URL and the region name it expects in request signatures.
- Credentials that can read the objects under the prefix.

This page names no particular provider. Only `GetObject` and `HeadObject` requests are sent to the origin, with the optional conditional, range and checksum headers that `s3.features` controls, so a service can serve a site when it answers those requests as S3 does. Check yours with `config check-origin`, as described in [Verify it](#verify-it).

## Configure AWS S3

On AWS, the bucket and its region are enough:

```yaml [config.yaml]
s3:
  bucket: example-site
  region: eu-west-1
  prefix: releases/2026-11-08
```

Set [`s3.region`](../reference/configuration.md#s3-region) explicitly. When you omit it, statica takes the region from the AWS configuration when it starts, such as `AWS_REGION` or the shared configuration files, and fails to start if it finds none.

## Provide credentials

The configuration file has no credential fields. Credentials come from the standard AWS credential chain: for example `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, a profile in the shared AWS files, or the role of the machine or container. The AWS SDK refreshes temporary credentials ([origin credentials](../architecture/security.md#origin-credentials)).

Origin access is read-only: no request lists, writes or deletes objects. On AWS, both kinds of request need the `s3:GetObject` permission on the objects under the prefix:

```json [iam-policy.json]
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ReadPublishedSite",
      "Effect": "Allow",
      "Action": "s3:GetObject",
      "Resource": "arn:aws:s3:::example-site/releases/*"
    }
  ]
}
```

On AWS, objects encrypted with SSE-KMS also need `kms:Decrypt` on their key. Without it, AWS answers 403 Forbidden, which statica treats as a denial: a 404 Not Found for visitors with the default `errors.access_denied`.

### The `s3:ListBucket` trade-off

Without `s3:ListBucket` on the bucket, AWS answers 403 Forbidden instead of 404 Not Found for an object that does not exist. Every 403 counts as a denial, not as a missing object, so with this minimal policy:

- `/guide` is no longer redirected to `/guide/`;
- the custom 404 page and the single-page-app fallback are never used;
- every request for a missing URL is logged at `error` level with `error_class=origin_denied`, and reaches the origin each time, because a denial is never cached;
- a missing [origin health](./health-checks.md#choose-the-key) key gives the result `denied`, and `statica config check-origin` reports `access denied` for a missing key.

Visitors still get 404 Not Found, because [`errors.access_denied`](../reference/configuration.md#errors-access-denied) is `not_found` by default. To keep these features, also add this statement, which grants `s3:ListBucket` on the bucket:

```json
{
  "Sid": "TellMissingFromDenied",
  "Effect": "Allow",
  "Action": "s3:ListBucket",
  "Resource": "arn:aws:s3:::example-site"
}
```

The permission is never used to list anything, but anyone who holds the same credentials can list every key in the bucket. Decide according to your bucket policy ([AWS without `s3:ListBucket`](../architecture/access-denied.md#aws-without-s3-listbucket)).

## Configure an S3-compatible service

For a service other than AWS, set [`s3.endpoint`](../reference/configuration.md#s3-endpoint) to its URL, a scheme and a host with an optional port. Endpoint settings from the AWS environment or shared files are ignored: only this field selects the endpoint.

This example is illustrative; use the endpoint, region and addressing style that your provider documents:

```yaml [config.yaml]
s3:
  bucket: example-site
  endpoint: https://objects.example.net
  region: example-1
  path_style: true
  prefix: releases/2026-11-08
```

- [`s3.region`](../reference/configuration.md#s3-region) is the region used to sign requests. It must contain only ASCII letters, digits, `-` and `_`.
- [`s3.path_style: true`](../reference/configuration.md#s3-path-style) sends `https://objects.example.net/example-site/<key>` instead of `https://example-site.objects.example.net/<key>`. Use it when the service does not resolve bucket names as subdomains.
- [`s3.allow_http: true`](../reference/configuration.md#s3-allow-http) is required for an `http://` endpoint. Requests and object data then cross the network unencrypted: use it only for a server on the same machine or a trusted network.

## Keep origin TLS verified

The origin's certificate and hostname are verified against the system's trusted certificate authorities, for AWS and for `s3.endpoint` alike. To trust an endpoint whose certificate comes from a private certificate authority, add that authority to the trust store of the host or container image.

You can also use two environment variables. `SSL_CERT_FILE` names a PEM bundle that replaces the default bundle file, and `SSL_CERT_DIR` names directories of certificates that replace the default directories; on Linux, set both to replace the whole trust store. On macOS and Windows, setting either one replaces the platform's certificate verifier. They apply to every outgoing connection, credential providers included, so keep the public authorities in them if statica also reaches AWS services.

[`s3.insecure_skip_verify: true`](../reference/configuration.md#s3-insecure-skip-verify) keeps the connection encrypted but stops verifying the certificate and hostname.

::: danger Unverified origin TLS
With `s3.insecure_skip_verify: true`, anyone who can intercept the traffic between statica and the origin can read and change the content statica serves. The setting also applies without `s3.endpoint`, to the connection to AWS S3 itself. Use it only for a test endpoint whose certificate you cannot otherwise trust. The `origin_tls_verification_disabled` event is logged at `warn` level after `startup`, and `config check-origin` prints a warning.
:::

## Match the provider's features

[`s3.features`](../reference/configuration.md#s3-features) switches off optional request headers that a provider may not support:

| Setting | Default | Turn it off or on when |
| --- | --- | --- |
| [`conditional_reads`](../reference/configuration.md#s3-features-conditional-reads) | `true` | Off when the provider rejects `If-None-Match` or `If-Match` on `GetObject`. Expired objects are then read again in full instead of being revalidated, and ranges are no longer read from the origin. |
| [`range_reads`](../reference/configuration.md#s3-features-range-reads) | `true` | Off when the provider does not honor `Range`. A range request for an object that is not in memory then gets the whole object with 200 OK. |
| [`checksums`](../reference/configuration.md#s3-features-checksums) | `false` | On to validate the checksums the provider stores with objects. Objects without a checksum are still served, and a mismatch fails the read. Leave it off for a provider without S3 checksums. |

```yaml [config.yaml]
s3:
  bucket: example-site
  endpoint: https://objects.example.net
  region: example-1
  features:
    range_reads: false
```

[Effect of `s3.features`](../architecture/ranges-and-conditionals.md#effect-of-s3-features) describes the resulting range behavior.

## Set timeouts and attempts

| Setting | Default | Bounds |
| --- | --- | --- |
| [`s3.response_header_timeout`](../reference/configuration.md#s3-response-header-timeout) | `10s` | Each origin attempt, until its response headers. |
| [`s3.max_attempts`](../reference/configuration.md#s3-max-attempts) | `2` | Attempts per origin request, 1–3, with the AWS SDK's retry rules. |
| [`s3.read_idle_timeout`](../reference/configuration.md#s3-read-idle-timeout) | `30s` | The longest wait for body data from the origin. |
| [`server.response_header_timeout`](../reference/configuration.md#server-response-header-timeout) | `30s` | Every attempt of a request together, and the whole `config check-origin` run. |

For a distant or slow provider, raise `s3.response_header_timeout`, and keep `s3.max_attempts` times its value below both [`cache.fill_timeout`](../reference/configuration.md#cache-fill-timeout), the limit of a shared read into the memory cache, and `server.response_header_timeout`, so that the last attempt can finish before the request answers 504 Gateway Timeout. A request can read several keys one after the other (the exact key, the index object, precompressed variants, the single-page-app shell, the 404 page), all within the same `server.response_header_timeout` ([Timeouts and long downloads](../architecture/timeouts.md)).

## Choose how denials look to visitors

[`errors.access_denied`](../reference/configuration.md#errors-access-denied) selects the public answer to an origin 403 Forbidden: `not_found` (the default) answers 404 Not Found, `bad_gateway` answers 502 Bad Gateway. Choose `bad_gateway` when a load balancer or CDN that watches 5xx rates should notice a broken credential or bucket policy:

```yaml [config.yaml]
errors:
  access_denied: bad_gateway
```

Logs and metrics record the denial the same way with either value, apart from the `status` ([Access denied](../architecture/access-denied.md)).

## Use a proxy for origin traffic

Origin and credential requests honor the `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` environment variables: `HTTPS_PROXY` for `https://` destinations, `HTTP_PROXY` for `http://` ones. List in `NO_PROXY` the hosts to reach directly, such as an S3-compatible endpoint on your own network. When `HTTP_PROXY` is set and credentials come from the platform, also list its credential endpoints: `169.254.169.254` for an EC2 instance role, `169.254.170.2` for an ECS task role and `169.254.170.23` for EKS Pod Identity.

```sh
HTTP_PROXY=http://10.0.0.8:3128 HTTPS_PROXY=http://10.0.0.8:3128 \
  NO_PROXY=169.254.169.254 statica serve --config config.yaml
```

## Verify it

Run [`statica config check-origin`](../reference/cli.md#statica-config-check-origin) with the same environment as the server. It resolves the credentials and the region as `serve` does, then reads the metadata of the `index` object under the prefix, or of `admin.origin_health.key` when that block is present:

```sh
statica config check-origin --config config.yaml
```

```text
origin object accessible
```

`statica serve` does not contact the origin when it starts, so this command is the check that catches missing credentials or a wrong region before traffic does. Once the server runs, the `startup` event shows the origin settings in use, never the endpoint URL or the bucket:

```text
level=info event=startup ... s3_endpoint=custom s3_region=example-1 s3_path_style=true s3_allow_http=false s3_insecure_skip_verify=false s3_conditional_reads=true s3_range_reads=false s3_checksums=false s3_max_attempts=2 s3_response_header_timeout=10s s3_read_idle_timeout=30s ...
```

Request a page, then look at the origin reads on the admin listener's `/metrics`. Every read is counted in [`statica_origin_operations_total`](../reference/metrics.md#statica-origin-operations-total) by `result`:

```sh
curl -s http://127.0.0.1:8080/ -o /dev/null
curl -s http://127.0.0.1:9090/metrics | grep '^statica_origin_operations_total'
```

```text
statica_origin_operations_total{operation="get",purpose="cache_fill",result="success"} 1
```

To make readiness follow the origin, enable the [origin health check](../reference/configuration.md#admin-origin-health).

## Common failures

`config check-origin` prints fixed messages, without the endpoint, key or provider text. The [CLI reference](../reference/cli.md#statica-config-check-origin) lists all of them.

| Symptom | Likely cause |
| --- | --- |
| `origin check failed: access denied` | The origin answered 403 Forbidden: the credentials were rejected (on AWS, an unknown access key or a wrong secret also gets 403), or the policy does not allow `s3:GetObject` under the prefix. On AWS without `s3:ListBucket`, also a missing object: a wrong `s3.prefix`, or a release that was not uploaded. Requests then get 404 Not Found with the default `errors.access_denied`, logged with `error_class=origin_denied`. |
| `origin check failed: origin unavailable` | No credentials were found in the chain, so nothing was sent; the endpoint cannot be reached; its certificate is not trusted; or, on AWS, the region is wrong. Requests get 502 Bad Gateway. |
| `origin check failed: origin configuration`, and `serve` logs `startup_failed` with `stage=origin_client` | No region was found in the AWS configuration, or the region found there contains characters other than letters, digits, `-` and `_`. `config validate` already rejects such characters in `s3.region`, and bucket names ending in `--x-s3`, `.mrap`, `--ol-s3`, `-s3alias` or `--table-s3`. |
| `origin check failed: origin configuration` only | The bucket does not exist. Requests get 502 Bad Gateway, logged with `error_class=origin_configuration`. |
| `origin check failed: object not found` | With `s3:ListBucket` on AWS, or on a provider that answers 404 for a missing key: the checked object is missing, because the prefix is wrong or the release was not uploaded. |
| `origin check timed out` | The check did not finish within `server.response_header_timeout`, or an origin request timed out. Without credentials, also after several seconds spent by the AWS SDK trying to reach a machine role. |
| Missing pages answer 404, but `/guide` is not redirected to `/guide/` and the 404 page is not used | The credentials lack `s3:ListBucket` on AWS (see [the trade-off](#the-s3-listbucket-trade-off)). |
| Range requests for objects that are not in memory get 502 Bad Gateway, logged with `error_class=origin_invalid_response` | The provider does not honor `Range`. Turn off `s3.features.range_reads`. |

## Pitfalls

- **Wrong credentials look like missing pages.** With the default `errors.access_denied: not_found`, a rejected key turns every URL that reads the bucket into a 404. Watch `statica_http_requests_total{outcome="origin_failure"}` and the `error` level access events, not only 5xx rates.
- **A server without credentials starts and reports ready.** Without the origin health check, readiness does not depend on the origin.
- **`config validate` is offline.** It checks the syntax of the bucket name and region, but not the region discovered from the AWS configuration, the credentials or the bucket. Run `config check-origin` in the deployment's environment.
- **Revoked access takes effect at expiry.** Fresh memory cache entries are still served until they expire.

## Related pages

- [Publish a site to S3](./publishing.md)
- [Health checks and readiness](./health-checks.md): make readiness follow the origin.
- [Troubleshooting](./troubleshooting.md): 404, 502, 503 and 504 responses traced to the origin.
- [Work with the logs](./logs.md) and [Monitor with Prometheus](./metrics.md): `error_class` in the access log and the origin series.
- [Access denied](../architecture/access-denied.md): how 403 answers are handled and reported.
- [Security model](../architecture/security.md#origin-connections): origin connections and credentials.
- [`s3` configuration reference](../reference/configuration.md#s3)
