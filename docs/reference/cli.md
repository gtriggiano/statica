# CLI reference

The statica binary has commands that validate and inspect a configuration, check the origin and run the server.

| Command | What it does | Network access |
| --- | --- | --- |
| [`statica`, `statica help`](#statica-and-statica-help) | Print help | None |
| [`statica version`](#statica-version) | Print build information | None |
| [`statica config validate`](#statica-config-validate) | Validate the configuration and the TLS files | None |
| [`statica config print`](#statica-config-print) | Print the configuration with defaults applied | None |
| [`statica config check-origin`](#statica-config-check-origin) | Read the metadata of one origin object | AWS credentials and region discovery, S3 reads |
| [`statica serve`](#statica-serve) | Run the server | AWS credentials and region discovery, S3 reads |

The offline commands do no credential discovery, DNS lookup or S3 access.

## Synopsis

```text
statica [--help]
statica help [command]
statica version
statica config validate [--config <path>]
statica config print [--config <path>]
statica config check-origin [--config <path>]
statica serve [--config <path>]
```

## Common behavior

### Flags and arguments

| Flag | Commands | Meaning |
| --- | --- | --- |
| `--config <path>` | `config` and its subcommands, `serve` | Configuration file. Overrides `STATICA_CONFIG` and `./config.yaml`. If repeated, the last value wins. |
| `-h`, `--help` | Every command | Print the command's help and exit 0. Extra positional arguments are ignored; an unknown flag still fails with exit status 2. |

No command takes positional arguments. An extra argument, including an unknown command name after `statica`, fails with `unexpected arguments; see --help`; an unknown flag fails with `invalid command flags; see --help`. Both exit with status 2. Error messages do not repeat the offending argument.

### Configuration file

`config validate`, `config print`, `config check-origin` and `serve` select exactly one file, the first of:

1. the `--config` flag;
2. the `STATICA_CONFIG` environment variable, when it is set;
3. `config.yaml` in the working directory.

An empty `--config` value or an empty `STATICA_CONFIG` is an error, and so is a file that cannot be opened. The [configuration reference](./configuration.md#loading) describes how the file is parsed and validated.

### Output streams

- Standard output carries the command's result: help, version, `configuration valid`, the printed configuration, `origin object accessible`, and the log events of `serve`.
- Standard error carries errors and warnings, one line each: `statica: <message>` and `statica: warning: <message>`.

### Exit status

| Status | Meaning |
| --- | --- |
| 0 | Success. For `serve`, also a shutdown or an aborted startup caused by a signal (see [`serve` exit status](#exit-status-of-serve)). |
| 1 | Runtime failure: the origin check failed or was canceled, the output could not be written, or `serve` failed after loading its configuration. |
| 2 | Usage or configuration error: unknown command, argument or flag; a configuration file that is missing, unreadable or invalid; TLS files that cannot be loaded. |

### Signals

The binary handles SIGINT and SIGTERM itself. `serve` drains on the first signal and closes at once on another (see [Signals of `serve`](#signals-of-serve)). For every other command, the first signal cancels the command: `config check-origin` stops with `origin check canceled` and exit status 1, and the offline commands, which do not wait on anything, finish normally. Further signals do not end a command sooner.

## `statica` and `statica help`

```sh
statica
statica help config
```

`statica`, `statica --help` and `statica help` print the general help. `statica help <command>` prints the help of a command, like `statica <command> --help`. An unknown command fails with `unknown command; see statica --help` and exit status 2. `statica config` without a subcommand prints the help of `config`.

```text
statica serves one S3 bucket over HTTP and inspects its configuration.
serve and config check-origin are online: they discover AWS credentials and
contact the origin (check-origin read-only). help, version, config validate and
config print are offline: no credential discovery, DNS, or S3 access.

Usage:
  statica [flags]
  statica [command]

Available Commands:
  config      Inspect configuration or check origin accessibility
  help        Help about any command
  serve       Serve the configured bucket (online)
  version     Print build information

Flags:
  -h, --help   help for statica

Use "statica [command] --help" for more information about a command.
```

## `statica version`

```sh
statica version
```

Prints one line on standard output: `statica <version> (revision <revision>; <Go version>[; built <time>])`. Version, revision and build time are set when the binary is built. A release binary or image reports its version, the full commit hash and the date of that commit, for example:

```text
statica 1.4.2 (revision 4f1c9a7e2b3d5f6a8c0e1d2b3a4f5e6d7c8b9a0f; go1.27.3; built 2026-11-20T10:00:00Z)
```

A binary built without them, such as the one `make build` produces, reports `dev` and `unknown` and omits the build time:

```text
statica dev (revision unknown; go1.27.3)
```

## `statica config validate`

```sh
statica config validate --config config.yaml
```

Loads and validates the configuration, then, when `server.tls` is present, loads the certificate chain and private key and checks that they match. It does not print the key material.

On success it prints `configuration valid` on standard output, followed by `S3 region deferred to AWS discovery at startup` when `s3.region` is omitted. It prints a warning on standard error for each redirect rule that an earlier rule may shadow; warnings do not fail the command.

```text
statica: warning: redirects[1] may be shadowed by redirects[0]
configuration valid
S3 region deferred to AWS discovery at startup
```

Validation applies the same syntax rules as the origin client: a configured `s3.region` with characters other than ASCII letters, digits, `-` and `_`, and a bucket name ending in `--x-s3`, `.mrap`, `--ol-s3`, `-s3alias` or `--table-s3`, are rejected here rather than at startup. It also rejects an `admin.listen` that would compete with `server.listen` for the same port: equal hosts, a host on every interface, or two loopback hosts. It rejects rules that could never apply: a `cache.rules[].match.exact` path ending in `/`, and a `redirects[].from` with a hidden segment ([`s3.region`](./configuration.md#s3-region), [`admin.listen`](./configuration.md#admin-listen), [`redirects[].from`](./configuration.md#redirects-from)).

`config validate` is offline. It does not check that the bucket exists, that credentials are available, that a region can be discovered when `s3.region` is omitted, that the endpoint is reachable or that the listen addresses can be bound.

## `statica config print`

```sh
statica config print --config config.yaml
```

Loads and validates the configuration, without reading the TLS files, and prints it as YAML on standard output: every field, with defaults applied and values normalized. Fields that are absent and have no default, such as `hosts`, `server.tls`, `s3.region` or `spa`, are left out. Durations are printed with hours, minutes and seconds (`1m0s`), byte sizes with the largest exact unit (`128MiB`), and TLS paths are absolute.

Every value that came from `${NAME}` substitution, and every default derived from one, is printed as `[REDACTED]`. The output is a diagnostic view: it is not guaranteed to load back as a configuration (a redacted value, for example, is not valid).

For a configuration that sets `s3.bucket: ${SITE_BUCKET}`, `s3.prefix: /site/` and `cache.max_bytes: 256MiB`, the output begins with:

```text
server:
    listen: 127.0.0.1:8080
    trusted_proxies: []
    client_ip_header: X-Forwarded-For
    request_id_header: X-Request-Id
    max_forwarded_hops: 16
    read_header_timeout: 5s
    response_header_timeout: 30s
    write_idle_timeout: 1m0s
    idle_timeout: 1m0s
    request_timeout: 0s
    shutdown_timeout: 30s
    max_header_bytes: 32768
    max_connections: 1024
    max_concurrent_requests: 256
admin:
    listen: 127.0.0.1:9090
s3:
    bucket: '[REDACTED]'
    prefix: site/
    path_style: false
```

`config print` is offline. [Check a configuration](../guide/configuration.md#check-a-configuration) shows how to use it with `config validate`.

## `statica config check-origin`

```sh
statica config check-origin --config config.yaml
```

Checks that statica can read one object of the origin. It is online and read-only.

1. Loads and validates the configuration. It does not read the TLS files.
2. Prints `statica: warning: origin TLS certificate verification is disabled` on standard error when `s3.insecure_skip_verify` is `true`.
3. Chooses the key: `admin.origin_health.key` when the `admin.origin_health` block is present (even with `enabled: false`), otherwise `index`, in both cases under `s3.prefix`.
4. Creates an origin client as `serve` does: AWS credential chain, region discovery when `s3.region` is omitted, `s3.endpoint` and the other `s3` settings.
5. Sends a `HEAD` request for the key. S3 cannot say in a `HEAD` response whether a 404 Not Found means a missing object, so on a 404 it sends one `GET` and closes the body without reading it.

The whole probe, client setup included, must finish within `server.response_header_timeout`. Each origin request also follows `s3.response_header_timeout` and `s3.max_attempts`.

On success it prints `origin object accessible` on standard output and exits 0. This means that the credentials and the region were resolved, the endpoint answered, and the object's metadata was readable with those credentials. It does not mean that other objects are readable, that the object's body is readable or intact, that the optional capabilities in `s3.features` work, or that `serve` can bind its listeners.

On failure it prints a message such as those below and exits 1. The messages are fixed text: they never contain the endpoint, the key, credentials or the provider's error text. A 403 Forbidden is reported whatever `errors.access_denied` says ([access denied](../architecture/access-denied.md#config-check-origin)). [Common failures](../guide/s3-providers.md#common-failures) gives the likely cause of each message for AWS S3 and S3-compatible services.

| Message | Cause |
| --- | --- |
| `origin check timed out` | The probe did not finish within `server.response_header_timeout`, or an origin request timed out. Without credentials, the check can also end here after several seconds, while the AWS SDK tries to reach a machine role (instance metadata). |
| `origin check canceled` | A signal canceled the command. |
| `origin check failed: access denied` | The origin answered 403 Forbidden: the credentials may not read the object, or the origin rejected them with a 403 (for example `InvalidAccessKeyId` or `SignatureDoesNotMatch`). |
| `origin check failed: object not found` | The `GET` after a 404 reported that the object does not exist (`NoSuchKey`). |
| `origin check failed: ambiguous object absence` | The `GET` after a 404 also answered 404, without saying that the object is missing. |
| `origin check failed: origin configuration` | The origin client could not be set up: no region or an invalid one, a bucket name the client rejects, or an AWS configuration that cannot be loaded. Also when the `GET` after a 404 reports that the bucket does not exist (`NoSuchBucket`). |
| `origin check failed: origin unavailable` | No credentials were found in the chain (no request is sent; the search for a machine role can also end with `origin check timed out` instead), the origin could not be reached, or it answered with an error that has no more specific message: a server error, or a redirect or 400 Bad Request caused by a wrong region or endpoint. |
| `origin check failed: origin throttled` | The origin kept answering 429 Too Many Requests or a throttling error after the configured attempts. |
| `origin check failed: invalid origin response` | The origin's response was malformed or incomplete. |
| `origin check failed` | Any other failure. |

## `statica serve`

```sh
statica serve --config config.yaml
```

Runs the server: the public listener, the admin listener with `/livez`, `/readyz` and `/metrics` ([admin endpoints](./admin-endpoints.md)), the memory cache and, when enabled, the origin health check. It is online: it discovers AWS credentials and the region and reads the origin. [Deploy, roll out and shut down](../guide/deployment.md) shows how to run it under systemd or Kubernetes.

### Startup

1. Loads and validates the configuration and, when `server.tls` is present, the certificate and key. An error here exits with status 2 before anything starts.
2. Builds every component, including the origin client. A failure logs [`startup_failed`](./logs.md#startup-failed) with its `stage` and exits with status 1; nothing is bound.
3. Binds the public listener, then the admin listener. If the public bind fails, nothing is bound; if the admin bind fails, the public listener is closed. Either way `startup_failed` is logged and the exit status is 1.
4. Marks readiness as serving, starts the origin health check, and logs [`startup`](./logs.md#startup) with the bound addresses and the operating settings, followed by [`origin_tls_verification_disabled`](./logs.md#origin-tls-verification-disabled) when `s3.insecure_skip_verify` is `true`.

The values of `stage` are `metrics`, `origin_client`, `health_client`, `cache`, `content`, `public_handler`, `public_server`, `health_worker`, `admin_server`, `bind_public` and `bind_admin`. [`startup_failed`, exit status 1](../guide/troubleshooting.md#startup-failed-exit-status-1) gives the likely cause of each.

### Output

Log events go to standard output as logfmt lines; `observability` controls them. When `serve` fails, it also prints one fixed-text line on standard error, for example:

```text
statica: cannot bind the public listener
```

See [Startup, readiness and shutdown](../architecture/lifecycle.md#startup) for the whole sequence.

### Signals of `serve`

- **During startup**, before both listeners are bound, a signal aborts the startup: nothing stays bound and [`startup_aborted`](./logs.md#startup-aborted) `reason=signal` is logged.
- **While serving**, the first signal starts a graceful shutdown within `server.shutdown_timeout`. `/readyz` answers 503 Service Unavailable while `/livez` still answers 200 OK, [`shutdown_started`](./logs.md#shutdown-started) is logged, active requests drain until the budget minus `min(budget/4, 1s)`, and the requests still active then are canceled ([one budget](../architecture/lifecycle.md#one-budget)). The admin listener stops last, and [`shutdown_complete`](./logs.md#shutdown-complete) reports `outcome=drained`, or `outcome=forced` (at warn level) with `cancelled_requests` and, when the budget ran out, `forced_by=budget`, `forced_by=error` when the drain ended with another error, or `forced_by=listener_failure` after a public listener failure ([listener failures](../architecture/lifecycle.md#listener-failures)).
- **During the shutdown**, another signal closes both listeners at once, cancels the remaining steps and gives the logs at most 250 ms to be written. `shutdown_complete` reports `outcome=forced forced_by=signal` ([a second signal](../architecture/lifecycle.md#a-second-signal)).

### Exit status of `serve`

| Situation | Logged event | Exit status |
| --- | --- | --- |
| Shutdown after a signal, all requests drained | `shutdown_complete outcome=drained` | 0 |
| Shutdown after a signal, cut by the budget or by another signal | `shutdown_complete outcome=forced` (warn) | 0 |
| Signal during startup, without a construction or bind failure | `startup_aborted reason=signal` | 0 |
| Logging could not be initialized (`statica: cannot initialize logging`) | None | 1 |
| Component or origin client construction failed | `startup_failed stage=<stage>` | 1 |
| A listener could not be bound | `startup_failed stage=bind_public` or `stage=bind_admin` | 1 |
| A failure while a signal was also pending | `startup_failed stage=<stage> signal_pending=true` | 1 |
| A listener failed while serving | `listener_failed`, then `shutdown_started reason=listener_failure` | 1 |
| Invalid usage, configuration or TLS files | None | 2 |

A failure takes precedence over a pending signal, so a failed start is never reported as a clean stop ([exit status](../architecture/lifecycle.md#exit-status)). Exit status 0 after a forced shutdown is deliberate: the shutdown was requested, and the warn event records that it was cut short.

[Shut down without cutting requests](../guide/deployment.md#shut-down-without-cutting-requests) shows how to size the grace period and which [signals](../guide/deployment.md#signals) to send, and [Exit status](../guide/deployment.md#exit-status) how a supervisor should treat each status. [Troubleshooting](../guide/troubleshooting.md) goes from a failed start or an unexpected status to its cause.
