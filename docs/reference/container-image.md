# Container image

**statica** is published as the public image `ghcr.io/gtriggiano/statica`, one image index for linux/amd64 and linux/arm64. Each release builds it from the same binaries as the release archives. The image is part of statica's public interface: a change to anything on this page that breaks a deployment is a breaking change ([Versioning](./versioning.md)).

The image of the release that this documentation describes:

```text
ghcr.io/gtriggiano/statica:{{VERSION}}
```

[Installation](../guide/installation.md#container-image) shows how to run it.

## Tags

| Tag | Example | Points to | Moves? |
| --- | --- | --- | --- |
| `X.Y.Z` | `1.2.1` | That release | Never, once the release is published |
| `X.Y` | `1.2` | The latest patch release of that minor version | Yes, with each patch release |
| `X` | `1` | The latest release of that major version | Yes, with each release |
| `latest` | | The latest release | Yes, with each release |
| `X.Y.Z-rc.N` | `1.3.0-rc.1` | A release candidate, for testing only | Never |

Tags have no `v` prefix. No image is built from unreleased code. The floating tags `X.Y`, `X` and `latest` move to a new release only after its image has passed its checks on both platforms, and never to a release candidate. For a reference that never changes, use the digest given in the release notes: `ghcr.io/gtriggiano/statica@sha256:…`.

## Contents

| Property | Value |
| --- | --- |
| Base image | `gcr.io/distroless/static-debian13:nonroot`, pinned by digest: CA certificates, time zone data and an unprivileged user, with no shell and no package manager |
| Binary | `/usr/bin/statica` |
| Entrypoint | `/usr/bin/statica` |
| Default command | `serve` |
| Environment | `STATICA_CONFIG=/etc/statica/config.yaml` |
| User | `65532:65532` |
| Exposed ports | `8080` (public listener) and `9090` (admin listener) |
| Platforms | `linux/amd64`, `linux/arm64` |

- **Configuration.** statica reads `/etc/statica/config.yaml`, through `STATICA_CONFIG`; mount your file there, or pass `--config` after the command ([configuration file](./cli.md#configuration-file)).
- **Listen addresses.** The exposed ports are documentation only: statica's listeners default to `127.0.0.1:8080` and `127.0.0.1:9090`, unreachable from outside the container. Set [`server.listen`](./configuration.md#server-listen) and [`admin.listen`](./configuration.md#admin-listen), as in the [container example](./examples.md#container).
- **File system.** statica writes nothing to disk: run the container with a read-only root filesystem.
- **Arguments.** Arguments after the image name replace the default command, for example `version` or `config validate`.
- **No shell.** Container hooks and probes cannot run shell commands: use HTTP probes on the admin listener and the `sleep` action for a Kubernetes `preStop` hook ([Deploy on Kubernetes](../guide/kubernetes.md)).

## Labels

Each image carries these [OCI labels](https://github.com/opencontainers/image-spec/blob/main/annotations.md):

| Label | Value |
| --- | --- |
| `org.opencontainers.image.title` | `statica` |
| `org.opencontainers.image.description` | A one-line description of statica |
| `org.opencontainers.image.version` | The release version, such as `1.2.1` |
| `org.opencontainers.image.revision` | The full commit hash of the release |
| `org.opencontainers.image.created` | The date of that commit |
| `org.opencontainers.image.source` | `https://github.com/gtriggiano/statica` |
| `org.opencontainers.image.documentation` | `https://gtriggiano.github.io/statica/` |
| `org.opencontainers.image.licenses` | `MIT` |

## Provenance and SBOM

Each image index has a provenance attestation, signed by GitHub, that ties it to the release workflow and to the commit of the release. Check it with the [GitHub CLI](https://cli.github.com/):

```sh
gh attestation verify oci://ghcr.io/gtriggiano/statica:{{VERSION}} --repo gtriggiano/statica \
  --signer-workflow gtriggiano/statica/.github/workflows/release.yml
```

Each image also carries an SPDX software bill of materials, attached by the build as an attestation in the registry.

## Updates

Only the latest release receives fixes ([supported versions](./versioning.md#supported-versions)). A new base image, which Dependabot proposes when its digest changes, is released as a patch release. To follow fixes, track the `X.Y` tag of your minor version and restart your containers when it moves, or update a pinned version tag or digest yourself.
