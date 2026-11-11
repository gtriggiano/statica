# Installation

Install **statica** from the container image, from a release archive, or by building it from source. The image and the archives of a release contain the same binaries.

| Method | Platforms |
| --- | --- |
| [Container image](#container-image) | linux/amd64, linux/arm64 |
| [Release archive](#release-archive) | Linux and macOS, amd64 and arm64 |
| [Build from source](#build-from-source) | Any platform with Go 1.27.4 or later |

The commands on this page install the release that this documentation describes. Set the `VERSION` variable that they use:

<!-- x-release-please-start-version -->
```sh
VERSION=1.0.0
```
<!-- x-release-please-end -->

To install another release, set `VERSION` to it. Every release is listed on the [releases page](https://github.com/gtriggiano/statica/releases), and [Versioning](../reference/versioning.md) explains what a version number promises.

## Container image

The image `ghcr.io/gtriggiano/statica` is public: pulling it needs no login.

```sh
docker pull "ghcr.io/gtriggiano/statica:${VERSION}"
```

The image runs `statica serve` and reads its configuration from `/etc/statica/config.yaml`. It runs as an unprivileged user, has no shell, and statica writes nothing to disk, so it works with a read-only root filesystem. The [container image reference](../reference/container-image.md) lists its tags, paths and labels.

::: warning Set the listen addresses in a container
statica's listeners default to `127.0.0.1:8080` and `127.0.0.1:9090`, which nothing outside the container can reach. Set [`server.listen`](../reference/configuration.md#server-listen) and [`admin.listen`](../reference/configuration.md#admin-listen) to listen on the container's interfaces, as in the example below.
:::

Start from this configuration, with your bucket and its region:

<<< @/examples/container.yaml{yaml}

Save it as `config.yaml`, mount it into the container and give statica your credentials through the standard AWS credential chain, here with the AWS environment variables of your shell:

```sh
docker run --rm --read-only \
  -p 8080:8080 -p 127.0.0.1:9090:9090 \
  -v "$PWD/config.yaml:/etc/statica/config.yaml:ro" \
  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_SESSION_TOKEN \
  "ghcr.io/gtriggiano/statica:${VERSION}"
```

The site is on `http://127.0.0.1:8080/`. The admin port is published on the host's loopback address only: the admin endpoints have no authentication ([the admin listener](../architecture/security.md#the-admin-listener)).

Run another command by giving it after the image name, for example to check the configuration and the access to the bucket:

```sh
docker run --rm \
  -v "$PWD/config.yaml:/etc/statica/config.yaml:ro" \
  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_SESSION_TOKEN \
  "ghcr.io/gtriggiano/statica:${VERSION}" config check-origin
```

In production, refer to the image by the digest given in the release notes, which never changes, or by the version tag: tags such as `latest` move to newer releases. [Deploy, roll out and shut down](./deployment.md#kubernetes) has a Kubernetes example.

## Release archive

Each release attaches an archive per platform, with the `statica` binary, the license, the README and the changelog. Download the archive for your system and the checksums file:

```sh
OS=linux     # or darwin for macOS
ARCH=amd64   # or arm64
curl -fLO "https://github.com/gtriggiano/statica/releases/download/v${VERSION}/statica_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -fLO "https://github.com/gtriggiano/statica/releases/download/v${VERSION}/statica_${VERSION}_checksums.txt"
```

Check the archive against the checksums file, with `sha256sum` on Linux or `shasum` on macOS. The command prints `OK` for the archive:

```sh
sha256sum --check --ignore-missing "statica_${VERSION}_checksums.txt"
shasum -a 256 --check --ignore-missing "statica_${VERSION}_checksums.txt"
```

Extract the binary, install it and check it:

```sh
tar -xzf "statica_${VERSION}_${OS}_${ARCH}.tar.gz" statica
sudo install -m 0755 statica /usr/local/bin/statica
statica version
```

`statica version` prints the version you downloaded, the commit it was built from, the Go version and the build time ([CLI reference](../reference/cli.md#statica-version)).

::: tip macOS and downloads from a browser
An archive downloaded with `curl` runs as is. If you download it with a browser instead, macOS marks the binary as quarantined and refuses to run it; remove the mark with `xattr -d com.apple.quarantine statica`.
:::

## Verify a release

Every archive and every image has a provenance attestation, signed by GitHub, that ties it to the release workflow of the statica repository and to the commit of the release. Check one with the [GitHub CLI](https://cli.github.com/):

```sh
gh attestation verify "statica_${VERSION}_${OS}_${ARCH}.tar.gz" --repo gtriggiano/statica \
  --signer-workflow gtriggiano/statica/.github/workflows/release.yml
gh attestation verify "oci://ghcr.io/gtriggiano/statica:${VERSION}" --repo gtriggiano/statica \
  --signer-workflow gtriggiano/statica/.github/workflows/release.yml
```

Releases are immutable: once published, their tag and their files never change. GitHub attests each release as a whole:

```sh
gh release verify "v${VERSION}" --repo gtriggiano/statica
```

Each archive also has an SPDX software bill of materials, `statica_<version>_<os>_<arch>.tar.gz.sbom.json`, attached to the release.

## Build from source

You need Go 1.27.4 or later, Git and Make:

```sh
git clone https://github.com/gtriggiano/statica.git
cd statica
make build
```

`make build` writes the binary to `bin/statica`. A build from source reports the version `dev` and the revision `unknown`. To build a release, check out its tag (`git checkout "v${VERSION}"`) before `make build`.

## Next steps

- [Quickstart](./quickstart.md): point statica at a bucket and serve it.
- [Configuration basics](./configuration.md): the configuration file.
- [Deploy, roll out and shut down](./deployment.md): run statica under systemd or Kubernetes.
