# Configuration basics

Learn how **statica** finds and reads its configuration file, what the defaults give you, and how to check a change before you apply it. The [configuration reference](../reference/configuration.md) describes every field.

## Where statica finds the file

Every command that needs a configuration reads exactly one file, the first of:

1. the `--config` flag: `statica serve --config /etc/statica/config.yaml`;
2. the `STATICA_CONFIG` environment variable, when it is set;
3. `config.yaml` in the working directory.

statica searches no other directory and never merges files. A file that does not exist is an error:

```text
statica: configuration file: cannot open
```

See [selecting the file](../reference/configuration.md#selecting-the-file).

## A minimal configuration

<<< @/examples/minimal.yaml{yaml}

[`s3.bucket`](../reference/configuration.md#s3-bucket) is the only required field. Everything else has a default:

- the public listener on `127.0.0.1:8080` and the admin listener on `127.0.0.1:9090`, both reachable from the same machine only ([`server.listen`](../reference/configuration.md#server-listen), [`admin.listen`](../reference/configuration.md#admin-listen));
- the whole bucket is the site, and `index.html` answers paths that end in `/` ([`s3.prefix`](../reference/configuration.md#s3-prefix), [`index`](../reference/configuration.md#index));
- a memory cache of up to 128 MiB and 10,000 entries, where objects stay fresh for up to 5 minutes ([`cache`](../reference/configuration.md#cache));
- the built-in 404 page ([`not_found`](../reference/configuration.md#not-found));
- every valid hostname accepted ([`hosts`](../reference/configuration.md#hosts));
- logs at `info` level on standard output ([`observability`](../reference/configuration.md#observability)).

## Defaults

statica applies a default only to a field you omit. A value you write is never replaced, even `false` or `0s`. Some defaults apply only when their block is present: the fields of `admin.origin_health`, for example. To see the configuration that statica will use, with every default applied and some values normalized (an `s3.prefix` of `/docs` becomes `docs/`), print it:

```sh
statica config print
```

See [defaults](../reference/configuration.md#defaults) and [normalization](../reference/configuration.md#normalization).

## Strict YAML

statica rejects what it does not understand instead of ignoring it. A misspelled key is an error, not a silently ignored setting. For this file:

```yaml
s3:
  bucket: example-websites
  regoin: eu-west-1
```

every command that reads it stops with:

```text
statica: s3.regoin (line 3, column 3): unknown field
```

It also rejects:

- duplicate keys;
- explicit nulls such as `index:` without a value: omit the field instead;
- YAML aliases, merge keys and custom tags;
- values of the wrong type. statica converts nothing: `yes` is not a boolean, and a string that YAML reads as a number needs quotes, as in `server.tls.min_version`, written `min_version: "1.3"`.

An invalid file stops the command before anything starts, with exit status 2. The message names the field and, when known, its position, never its value ([error messages](../reference/configuration.md#error-messages)).

## Units

- **Durations** are strings with a unit: `500ms`, `30s`, `5m`, `1h30m`. Write zero as `0s`. A bare number such as `300` is an error.
- **Byte sizes** are an integer followed by `KiB`, `MiB` or `GiB`: `512KiB`, `128MiB`. `MB`, decimals and bare numbers are errors.

With `ttl: 300` in the `cache` block, statica reports, for example:

```text
statica: cache.ttl (line 5, column 8): expected a string with explicit units
```

The [format table](../reference/configuration.md#format) lists what each type accepts.

## Environment variables

`${NAME}` in a string, duration or byte-size value is replaced with the value of the environment variable `NAME`. Use it for values that change between environments:

```yaml [config.yaml]
s3:
  bucket: ${SITE_BUCKET}
  region: eu-west-1
  prefix: releases/${RELEASE_ID}
```

- A variable that is not set is an error that names it:

  ```text
  statica: s3.bucket (line 2, column 11): missing environment variable SITE_BUCKET
  ```

- `$$` writes a literal `$`. A `$` that is not followed by `{` stays as it is.
- Keys, integers and booleans are never substituted.
- `config print` shows every substituted value as `[REDACTED]`:

  ```sh
  SITE_BUCKET=example-websites RELEASE_ID=2026-11-06 statica config print
  ```

  ```text
  ...
  s3:
      bucket: '[REDACTED]'
      region: eu-west-1
      prefix: '[REDACTED]'
  ...
  ```

See [environment variables](../reference/configuration.md#environment-variables).

## Credentials and region

The configuration file never holds credentials: there is no field for them, and an unknown key is an error. `statica serve` and `statica config check-origin` take them from the standard AWS credential chain, for example `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, a profile in the shared AWS configuration files, or the role of the machine or container ([origin credentials](../architecture/security.md#origin-credentials)).

Set the bucket's region in [`s3.region`](../reference/configuration.md#s3-region), or omit it to take the region from the AWS configuration, such as `AWS_REGION` or the shared configuration files. `config validate` then reminds you:

```text
configuration valid
S3 region deferred to AWS discovery at startup
```

When no region is found, `statica serve` logs `startup_failed stage=origin_client` and exits with status 1, and `config check-origin` reports `origin check failed: origin configuration`. See [Connect to S3 and S3-compatible providers](./s3-providers.md) for the permissions and the settings of each kind of origin.

## Check a configuration

Run these commands before you start or restart the server with a new file:

```sh
statica config validate
statica config check-origin
```

- [`config validate`](../reference/cli.md#statica-config-validate) checks the file and, when `server.tls` is set, the certificate and key. It is offline: it does not check that the bucket exists, that credentials are available, that a region can be discovered when `s3.region` is omitted, or that the listen addresses are free.
- [`config print`](../reference/cli.md#statica-config-print) shows the configuration with defaults applied. It is offline too.
- [`config check-origin`](../reference/cli.md#statica-config-check-origin) reads the metadata of one object with your credentials. It is the check that catches missing credentials, a wrong region or a missing object.

::: warning
`statica serve` does not check credentials or the bucket when it starts, and it reports ready unless [`admin.origin_health`](../reference/configuration.md#admin-origin-health) is enabled. Without credentials, it answers 502 Bad Gateway, or 504 Gateway Timeout while the AWS SDK looks for a machine role. With credentials that the origin rejects with 403 Forbidden, it answers 404 Not Found with the default `errors.access_denied`, or 502 Bad Gateway with `bad_gateway`, as for any denied object ([access denied](../architecture/access-denied.md)). Run `config check-origin` first.
:::

## Apply a change

statica reads its configuration once, when it starts. To apply a change, restart the process. The restarted process starts with an empty memory cache ([configuration changes and restarts](../architecture/lifecycle.md#configuration-changes-and-restarts)). [Change the configuration](./deployment.md#change-the-configuration) describes a rollout.

## Pitfalls

- **An `admin.origin_health` block without `enabled: true` does nothing**, apart from choosing the key that `config check-origin` reads. It is still validated, so an invalid value in it is still an error ([Health checks and readiness](./health-checks.md)).
- **Validation does not resolve names.** `config validate` rejects listen addresses with equal hosts, a host on every interface, or two loopback hosts such as `localhost:8080` and `127.0.0.1:8080` on the same port, but two different DNS names that resolve to the same address pass, then the second listener cannot bind and `statica serve` exits with status 1 ([`config validate` passes but `serve` fails](./troubleshooting.md#config-validate-passes-but-serve-fails)).

## Related pages

- [Quickstart](./quickstart.md): build statica and serve a bucket.
- [Configuration reference](../reference/configuration.md): every field, its type, default and rules.
- [Configuration examples](../reference/examples.md): complete files, from minimal to production-style.
- [CLI reference](../reference/cli.md): the commands, their output and exit statuses.
- [Memory cache](../architecture/memory-cache.md): what the `cache` settings control.
- [Timeouts and long downloads](../architecture/timeouts.md): how the timeouts combine.
- [Security model](../architecture/security.md): the admin listener, proxies and TLS.
- Guides for the next tasks: [Publish a site to S3](./publishing.md), [Connect to S3 and S3-compatible providers](./s3-providers.md), [Serve HTTPS directly](./tls.md), [Run behind a reverse proxy, CDN or ingress](./behind-a-proxy.md), [Deploy, roll out and shut down](./deployment.md), [Tune the memory cache](./cache-tuning.md), [Health checks and readiness](./health-checks.md) and [Troubleshooting](./troubleshooting.md).
