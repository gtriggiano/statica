# Deploy, roll out and shut down

Run **statica** as a supervised service, wire its probes, apply configuration changes with restarts, and stop instances without cutting the requests they are serving.

## Prerequisites

- statica installed: the container image or a release binary ([Installation](./installation.md)), and a configuration that passes `statica config validate`.
- Credentials that can read the bucket, available to the process through the standard AWS credential chain: for example a role attached to the machine or the workload, or the AWS environment variables and files of the service user. The configuration file never holds credentials ([Connect to S3 and S3-compatible providers](./s3-providers.md)).
- A process supervisor: systemd, Kubernetes or another one that sends SIGTERM to stop a process and SIGKILL after a grace period.

## What a deployment looks like

One statica process serves one site: one bucket and, optionally, one publication prefix. It needs:

- **Its configuration file**, read once at startup, from `--config`, the `STATICA_CONFIG` environment variable or `./config.yaml`.
- **The public listener**, [`server.listen`](../reference/configuration.md#server-listen), reachable by visitors or by your proxy.
- **The admin listener**, [`admin.listen`](../reference/configuration.md#admin-listen), reachable by your probes and by Prometheus, and by nothing else.
- **Standard output and standard error**, collected by the supervisor. Logs are logfmt events on standard output. Standard error carries one-line messages when a command fails, and the Go runtime's report if the process crashes.

Both listeners default to loopback addresses (`127.0.0.1:8080` and `127.0.0.1:9090`). Set them to the addresses that your network layout needs:

```yaml [config.yaml]
server:
  listen: ":8080"
admin:
  listen: 10.0.12.20:9090
```

::: danger Keep the admin listener private
`/livez`, `/readyz` and `/metrics` have no authentication. Bind `admin.listen` to loopback or to a private address, and allow only your probes and your Prometheus to reach it with your network controls ([the admin listener](../architecture/security.md#the-admin-listener)).
:::

## Run it under a supervisor

### systemd

An example unit for a release binary installed in `/usr/local/bin`, to adapt:

```ini [statica.service]
[Unit]
Description=statica static website server
Wants=network-online.target
After=network-online.target

[Service]
User=statica
Group=statica
ExecStart=/usr/local/bin/statica serve --config /etc/statica/config.yaml
# Stop with SIGTERM (the default), and allow more than server.shutdown_timeout.
KillSignal=SIGTERM
TimeoutStopSec=40s
# Restart after failures, but not after a configuration error (exit status 2).
Restart=always
RestartSec=2s
RestartPreventExitStatus=2
# Only needed to bind a port below 1024 without running as root.
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

- **Stop timeout.** `TimeoutStopSec` must exceed [`server.shutdown_timeout`](../reference/configuration.md#server-shutdown-timeout) (`30s` by default), so that systemd never sends SIGKILL during a drain.
- **No reload.** Do not add an `ExecReload=` that sends SIGHUP: statica does not handle it, and SIGHUP ends the process at once ([Signals](#signals)).
- **`Restart=always`.** systemd counts a process ended by SIGHUP as a clean exit, so `Restart=on-failure` would leave statica stopped after it. `RestartPreventExitStatus=2` keeps a configuration error from restarting in a loop: fix the file instead. The Go runtime also exits with status 2 when the process crashes, so with this setting a crashed instance stays stopped too; remove the line if you prefer automatic restarts in both cases.
- **Logs.** systemd sends standard output and standard error to the journal. journald applies its own rate limit per service and can drop lines that statica wrote successfully; statica cannot count those drops. Check your journald settings when access logs carry a high request rate.

### Kubernetes

A fragment of a Deployment's pod template, to adapt. statica's configuration file is mounted from a ConfigMap at the path the image reads, `/etc/statica/config.yaml`, and the image runs `statica serve` ([container image](../reference/container-image.md)):

<!-- x-release-please-start-version -->
```yaml [deployment.yaml]
spec:
  terminationGracePeriodSeconds: 45
  containers:
    - name: statica
      image: ghcr.io/gtriggiano/statica:0.0.0
      securityContext:
        runAsNonRoot: true
        readOnlyRootFilesystem: true
        allowPrivilegeEscalation: false
      ports:
        - name: http
          containerPort: 8080
        - name: admin
          containerPort: 9090
      livenessProbe:
        httpGet:
          path: /livez
          port: admin
      readinessProbe:
        httpGet:
          path: /readyz
          port: admin
        periodSeconds: 5
      lifecycle:
        preStop:
          sleep:
            seconds: 5
      volumeMounts:
        - name: config
          mountPath: /etc/statica
          readOnly: true
  volumes:
    - name: config
      configMap:
        name: statica-config
```
<!-- x-release-please-end -->

with this listener configuration:

```yaml [config.yaml]
server:
  listen: ":8080"
  shutdown_timeout: 30s
admin:
  listen: ":9090"
```

- **Listen on the pod's addresses.** The kubelet probes the pod's IP address, so the default `127.0.0.1:9090` is unreachable to it; the same holds for the public listener and your Service. With `:9090`, restrict access to the admin port with your network policies, keeping the kubelet's probes and Prometheus allowed.
- **Grace period.** `terminationGracePeriodSeconds` counts from the start of the pre-stop hook, so it must exceed the pre-stop delay plus `server.shutdown_timeout`, with some margin: 5 + 30 < 45 here.
- **Pre-stop delay.** The `sleep` action of a `preStop` hook needs a Kubernetes version that supports it. The statica image has no shell and no `sleep` binary, so an `exec` hook cannot replace it. See [Shut down without cutting requests](#shut-down-without-cutting-requests) for why the delay matters.

### Exit status

A supervisor can tell why statica stopped from its exit status ([exit status of `serve`](../reference/cli.md#exit-status-of-serve)):

| Exit status | Meaning | What to do |
| --- | --- | --- |
| 0 | Stopped after SIGTERM or SIGINT, including a shutdown that had to cut requests (`shutdown_complete outcome=forced`). | Nothing: the stop was requested. |
| 1 | A runtime failure: `startup_failed` (a component or a bind failed) or `listener_failed` while serving. | Restart with a delay, and read the events. |
| 2 | Invalid usage, configuration or TLS files: nothing started, and standard error has a `statica:` message. The Go runtime also exits with status 2 when the process crashes, with its report on standard error. | Fix the configuration; restarting does not help. For a crash, keep the report. |
| Killed by a signal | A signal that statica does not handle: SIGHUP or SIGKILL. A shell reports 128 plus the signal number (129 for SIGHUP, 137 for SIGKILL); systemd reports the signal's name. | Find what sent the signal. |

## Probes

| Probe | Path | Answer |
| --- | --- | --- |
| Liveness | `/livez` | 200 `ok` for as long as the admin listener runs: during origin outages, while not ready and throughout a drain. |
| Readiness | `/readyz` | 200 `ready`, or 503 with `not ready: <reason>` while draining or, with origin health enabled, while the origin is unknown, unhealthy or stale. |

Never use `/readyz` as a liveness probe. With origin health enabled, an origin outage would make your supervisor restart every instance, emptying their memory caches while the origin cannot refill them. [Health checks and readiness](./health-checks.md) explains readiness and when to tie it to the origin.

::: warning Credentials are not checked at startup
statica loads the AWS configuration at startup but fetches no credentials and sends no request to the bucket. Without [`admin.origin_health`](../reference/configuration.md#admin-origin-health) enabled, an instance with missing credentials or a wrong bucket becomes ready and answers every request that needs the origin with an error. Run `statica config check-origin` with the new configuration and credentials before a rollout, or enable the origin health check so that such instances never become ready.
:::

## Shut down without cutting requests

### What happens on SIGTERM

The first SIGTERM or SIGINT starts a graceful shutdown inside one budget, [`server.shutdown_timeout`](../reference/configuration.md#server-shutdown-timeout) (`30s` by default) ([one budget](../architecture/lifecycle.md#one-budget)):

1. At once, `/readyz` answers 503 `not ready: draining`, `/livez` still answers 200, and statica logs `shutdown_started`. The public listener stops accepting connections at the same moment, and idle keep-alive connections are closed.
2. Active requests continue until the budget minus `min(budget/4, 1s)`, 29 s with the default. The requests still active then are canceled, downloads included: they end with `termination_reason=shutdown`.
3. The admin listener stops last, `shutdown_complete` reports `outcome=drained` or `outcome=forced`, the remaining logs are written, and statica exits with status 0.

### Grace period and pre-stop delay

- **Grace period.** Give your supervisor a grace period, the time between SIGTERM and SIGKILL, longer than `server.shutdown_timeout`. A SIGKILL ends the drain and loses the log lines still queued.
- **Pre-stop delay.** The public listener closes as soon as the signal arrives: it does not wait for load balancers to notice that `/readyz` turned 503. If your load balancer or Service keeps sending new connections for a while after an instance starts stopping, those connections are refused. Delay the signal itself with a pre-stop step in your orchestrator, long enough for the instance to leave the load balancer, and add that delay to the grace period.
- **Long downloads.** A download still running at the drain deadline is cut, whatever its own limits. If some downloads must finish, raise `server.shutdown_timeout` and the grace period together ([long downloads](../architecture/lifecycle.md#long-downloads)).

### Signals

| Signal or event | Effect |
| --- | --- |
| First SIGTERM or SIGINT | Graceful shutdown within `server.shutdown_timeout`. |
| Another SIGTERM or SIGINT during the shutdown | Both listeners close at once, active requests are cut, and the logs get at most 250 ms; the access events of the cut requests can be lost ([a second signal](../architecture/lifecycle.md#a-second-signal)). |
| SIGHUP | statica does not handle it: the process ends at once, without a drain, shutdown events or a log flush. It does not reload anything. |
| Standard output closed (a pipe or socket whose reader went away) | statica keeps serving. Log lines that cannot be written are dropped and counted in [`statica_log_events_dropped_total`](../reference/metrics.md#statica-log-events-dropped-total). |
| SIGKILL | The process ends at once, without a drain. |
| SIGTERM or SIGINT during startup | The startup stops, nothing stays bound, `startup_aborted reason=signal` is logged and the exit status is 0. |

Keep the reader of standard output alive for the whole life of the process: after a log shipper restarts, or a pipe's consumer exits, statica keeps serving but every later log line is lost.

## Change the configuration

The configuration file and the TLS files are read once, at startup. To apply any change, a new publication prefix included, restart the process:

1. Validate the new file where it will run: `statica config validate`, then `statica config check-origin`, which reads one object with the same credentials, region and endpoint that `serve` will use.
2. Restart the instances one at a time, or let your orchestrator roll them, waiting for each new instance to report ready before stopping the next one.

Two side effects of a restart:

- **A cold memory cache.** A restarted instance starts with an empty memory cache and reads every object from the origin again until it warms up. Restarting all instances at once sends all of that traffic to the origin together.
- **Mixed versions during a rollout.** While a rollout runs, old and new instances serve side by side. When the change is a new `s3.prefix` for a new release of the site, visitors can receive pages from either release until the rollout ends ([Publish a site to S3](./publishing.md)).

Content published under the current prefix needs no restart: statica serves it once the memory cache's entries for the changed objects expire ([Tune the memory cache](./cache-tuning.md)).

## Logs

statica writes one logfmt event per line to standard output through a bounded queue of 4096 lines and 4 MiB. When the output cannot keep up, new lines are dropped and counted in `statica_log_events_dropped_total`, rather than slowing requests ([bounded output](../reference/logs.md#bounded-output)). Collect standard error too: it carries the one-line reason when `serve` fails, such as `statica: cannot bind the public listener`. [Work with the logs](./logs.md) describes the events.

## Verify it

Check both probes from a host allowed to reach the admin listener:

```sh
curl -sS -w '%{http_code}\n' http://10.0.12.20:9090/livez
curl -sS -w '%{http_code}\n' http://10.0.12.20:9090/readyz
```

Each command prints the body, then the status:

```text
ok
200
ready
200
```

Then stop one instance with your supervisor (`systemctl stop statica`, or by deleting a pod) and read its last events:

```text
ts=2026-11-08T00:16:58.867Z level=info event=shutdown_started reason=signal shutdown_timeout=30s
ts=2026-11-08T00:16:58.870Z level=info event=shutdown_complete reason=signal outcome=drained duration_ms=2.665
```

`outcome=forced` at warn level, with `forced_by=budget` and `cancelled_requests`, means that requests were still running at the drain deadline. `forced_by=signal` means that a second signal arrived: check that your supervisor's grace period is long enough and that nothing sends the signal twice. `forced_by=listener_failure` follows a `listener_failed` event for the public listener, whose failure cut every public connection at once. A log that ends without `shutdown_complete` means that the process ended without a graceful shutdown, for example by SIGKILL or SIGHUP.

## Pitfalls

- **Probes on loopback.** A probe from another host or from the kubelet cannot reach `127.0.0.1`. Bind the admin listener to an address the prober reaches.
- **Same port, different names.** `admin.listen` must not share a socket with `server.listen`: `config validate` rejects equal hosts, a host on every interface and two loopback hosts (`localhost`, `127.0.0.0/8`, `::1`) on the same port, but two different DNS names that resolve to the same address pass validation and then fail to bind with `startup_failed stage=bind_admin`.
- **A short grace period.** A SIGKILL during the drain cuts requests and loses queued log lines, including `shutdown_complete`.
- **`systemctl reload`.** Any reload action that sends SIGHUP stops statica.

## Related pages

- [Startup, readiness and shutdown](../architecture/lifecycle.md): the startup sequence, readiness and the shutdown budget.
- [CLI reference: `statica serve`](../reference/cli.md#statica-serve): signals and exit status.
- [Admin endpoints reference](../reference/admin-endpoints.md).
- [Configuration basics](./configuration.md).
- [Health checks and readiness](./health-checks.md).
- [Run behind a reverse proxy, CDN or ingress](./behind-a-proxy.md).
- [Monitor with Prometheus](./metrics.md).
- [Troubleshooting](./troubleshooting.md#statica-exits-with-status-2-at-startup): failed starts and exit statuses.
