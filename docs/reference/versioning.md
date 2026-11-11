# Versioning

statica follows [Semantic Versioning 2.0.0](https://semver.org/), applied to the public interface below. Before you move to a new major version, read the **⚠ BREAKING CHANGES** section of the [changelog](./changelog.md): each breaking change there says how to migrate.

## Public interface

These are the public interface. An incompatible change to any of them is a breaking change:

- **The configuration file**: keys, types, defaults and allowed values. A stricter validation that rejects a file accepted before is a breaking change ([configuration reference](./configuration.md)). A breaking change never gives an existing key a new meaning: it renames or removes the key, so that statica rejects a file written for the old format instead of reading it differently.
- **The CLI**: commands, flags, the `STATICA_CONFIG` environment variable and exit statuses ([CLI reference](./cli.md)).
- **The documented HTTP behavior**: statuses, headers, redirects, ranges and conditional requests ([HTTP responses](./http-responses.md)).
- **The admin endpoints**: `/livez`, `/readyz`, `/metrics` and their responses ([admin endpoints](./admin-endpoints.md)).
- **Metrics and log events**: metric names, labels and buckets, and event names and fields, which alerts and dashboards depend on ([metrics](./metrics.md), [logs](./logs.md)).
- **The container image**: entrypoint, user, default configuration path, exposed ports and platforms ([container image](./container-image.md)).
- **The names of the release assets**, which installation scripts depend on.

These are not: the Go packages in the repository, which are internal, the wording of the documentation, performance, and the exact text of error messages on standard error.

## Release candidates

Before some releases, a release candidate `X.Y.Z-rc.N` lets you try the next version. A release candidate is a GitHub pre-release with its own image tag, such as `1.5.0-rc.1`. It never becomes the latest release, never moves the floating image tags, and is not for production. Its documentation is under `/statica/next/` until the version is released.

## Supported versions

Only the latest release receives fixes, as a new patch release. Published releases never change: a defect found after a release is fixed in a new version, never by replacing the files or the image of a published one. The [container image tags](./container-image.md#tags) say which tags follow new releases.
