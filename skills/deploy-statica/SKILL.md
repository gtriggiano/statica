---
name: deploy-statica
description: Configure and deploy statica, the web server that serves one S3 bucket or prefix as one static website, on Kubernetes, Docker or systemd, then verify the result. Use when asked to serve a bucket as a website with statica, to write or review its config.yaml or manifests, or to find out why a statica instance is not ready or answers errors.
license: MIT
---

# Deploy statica

statica serves one S3 bucket, or a prefix inside it, as one static website. It is a single binary, also published as the container image `ghcr.io/gtriggiano/statica`, configured by one YAML file. This skill takes you from "serve this bucket" to a deployment you have checked.

The documentation is the source of truth, and every page has a Markdown version. Start from the index, `https://gtriggiano.github.io/statica/llms.txt`: it lists every page and names the current release. Read a page before you use a field or a command from it; do not write configuration keys from memory.

| You need | Read |
| --- | --- |
| Complete, tested configuration files | `https://gtriggiano.github.io/statica/reference/examples.md` |
| Every field, with type, default and allowed values | `https://gtriggiano.github.io/statica/reference/configuration.md` |
| Kubernetes manifests | `https://gtriggiano.github.io/statica/guide/kubernetes.md` |
| systemd unit, restarts and shutdown | `https://gtriggiano.github.io/statica/guide/deployment.md` |
| Docker commands, release archives | `https://gtriggiano.github.io/statica/guide/installation.md` |
| Credentials, IAM policy, S3-compatible services | `https://gtriggiano.github.io/statica/guide/s3-providers.md` |
| Symptoms, causes and fixes | `https://gtriggiano.github.io/statica/guide/troubleshooting.md` |

## 1. Gather the facts

Ask for what you cannot find in the user's repository or environment. Do not guess a value in this list.

- **Origin:** the bucket, its region, and the prefix when the site is not at the bucket's root. For a service other than AWS S3: the endpoint URL and whether it needs path-style requests.
- **Credentials:** how the instance gets them. A role of the workload (IRSA or EKS Pod Identity, an instance role), or an access key in the environment or in a Secret.
- **Where it runs:** Kubernetes, Docker or another container runtime, or a host with systemd.
- **Hostnames** that visitors use.
- **What is in front:** an ingress controller, a load balancer or a CDN, or nothing, in which case ask whether statica terminates TLS itself.
- **The kind of site:** whether it is a single-page application that routes its URLs in the browser, and whether it needs redirects.

## 2. Start from the matching example

Copy the complete example that is closest to the deployment, from the examples page, and change it. Do not assemble a file from fragments.

| Deployment | Example |
| --- | --- |
| Kubernetes | Kubernetes, with the manifests of the Kubernetes guide |
| Docker or another container runtime | Container |
| systemd on a host, behind nothing or behind a local proxy | Minimal configuration |
| A local or self-hosted S3-compatible server | Local S3-compatible server |
| A review of what a production file can set | Advanced configuration |

Keep the file small: a setting that you leave out keeps its default, and the defaults are documented in the configuration reference.

## 3. Apply the rules that prevent real mistakes

- **One instance serves one site.** The `Host` header never selects content. Several sites need several deployments, each with its own configuration.
- **No credentials in the file.** The configuration has no credential fields. statica reads credentials through the standard AWS credential chain. A value written as `${NAME}` is read from the environment.
- **Grant read access only.** statica sends only `GetObject` and `HeadObject`. The IAM policy and the `s3:ListBucket` trade-off are in the credentials guide.
- **Listen addresses in a container.** Both listeners default to the loopback address, which nothing outside the container reaches. Set `server.listen` and `admin.listen` as the Container and Kubernetes examples do.
- **The admin port is never public.** `/livez`, `/readyz` and `/metrics` have no authentication. Publish that port only to the probes and to Prometheus: keep it out of the Service, the ingress and the load balancer.
- **Set `hosts`** to the site's hostnames. Other hostnames are refused, and the `host` label of the metrics stays bounded.
- **Trust proxies only behind a network restriction.** Every address in `server.trusted_proxies` can choose the client IP that statica logs. List only proxies that nothing else can impersonate. On Kubernetes, trust the pod network only with a NetworkPolicy that the cluster enforces; otherwise leave the field out.
- **Size the memory from the cache limits.** The container's memory limit follows `cache.max_bytes` and `cache.max_fill_bytes`, with the rule and the `GOMEMLIMIT` value given in the Kubernetes guide. Change them together.
- **Keep the shutdown timings consistent.** On Kubernetes, `terminationGracePeriodSeconds` exceeds the `preStop` delay plus `server.shutdown_timeout`. Under systemd, the stop timeout exceeds `server.shutdown_timeout`.
- **Probes:** liveness on `/livez`, readiness on `/readyz`, both on the admin port. Never use `/readyz` for liveness.
- **Readiness does not check the origin by default.** Credentials are not checked at startup either: an instance with wrong credentials becomes ready and then fails requests. Enable `admin.origin_health` when readiness must wait for a successful read of the bucket, and read its trade-off in the health checks guide first.
- **Configuration is read once.** There is no reload: restart the instances to apply a change. On Kubernetes, a changed ConfigMap needs a rollout.
- **Single-page applications** need the `spa` block; redirects, response headers, CORS and browser caching each have a guide, listed in the index.
- **Name the release.** Use the image tag or the download URL of the current release, as the documentation pages write it. Never `latest`.

## 4. Verify, in this order

Do not report the deployment as done before each check that you can run has passed. Say which checks you could not run.

1. **The file is valid.** `statica config validate --config config.yaml` prints `configuration valid` and exits 0. It works offline. With the image: `docker run --rm -v "$PWD/config.yaml:/etc/statica/config.yaml:ro" ghcr.io/gtriggiano/statica:<version> config validate`. The JSON Schema at `https://gtriggiano.github.io/statica/config.schema.json` gives an editor or a YAML validator a partial check (field names, types, allowed values); `config validate` is the authority. When the file sets `server.tls`, `config validate` reads the certificate and the key: mount them at the configured paths.
2. **The origin is readable.** `statica config check-origin --config config.yaml`, with the credentials that the instance will have, prints `origin object accessible`. It reads one object: the index object, or the key of `admin.origin_health`. A failure message names the cause; the CLI reference lists every message.
3. **The instance is ready.** `/readyz` on the admin port answers 200 `ready`. A 503 carries the reason. On Kubernetes, the pods are `Running` and ready, and the log has a `startup` event and no `startup_failed`.
4. **The site answers.** Request `/` with one of the configured hostnames in the `Host` header, through the public port, and expect 200. A 421 means the hostname is not in `hosts`; a 404 for every path with a `level=error` access event means the origin denied the read.
5. **The traffic path works.** Request the site through the ingress, proxy or CDN that visitors use.

When a check fails, find the symptom in the troubleshooting page before you change the configuration.
