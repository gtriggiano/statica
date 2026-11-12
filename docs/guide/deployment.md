# Deploy, roll out and shut down

Run **statica** under a process supervisor such as systemd, apply configuration changes with restarts, and stop instances without cutting the requests they are serving. On Kubernetes, follow [Deploy on Kubernetes](./kubernetes.md) instead.

## Prerequisites

- statica installed ([Installation](./installation.md)), and a configuration that passes `statica config validate`.
- Credentials that can read the bucket, available to the process through the standard AWS credential chain ([Provide credentials](./s3-providers.md#provide-credentials)).
- A supervisor that stops a process with SIGTERM, and with SIGKILL after a grace period.

## What an instance needs

One statica process serves one site. Give it:

- **Its configuration file**, read once at startup ([configuration file](../reference/cli.md#configuration-file)).
- **Reachable listeners.** Both default to loopback addresses. Set [`server.listen`](../reference/configuration.md#server-listen) to an address that visitors or your proxy reach, and [`admin.listen`](../reference/configuration.md#admin-listen) to one that only your probes and Prometheus reach.
- **A reader for standard output and standard error.** Logs go to standard output, and the one-line reason of a failed command to standard error ([Work with the logs](./logs.md)).

```yaml [config.yaml]
server:
  listen: ":8080"
admin:
  listen: 10.0.12.20:9090
```

::: danger Keep the admin listener private
`/livez`, `/readyz` and `/metrics` have no authentication. Bind `admin.listen` to loopback or to a private address, and allow only your probes and your Prometheus to reach it ([the admin listener](../architecture/security.md#the-admin-listener)).
:::

## systemd

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
- **No reload.** Do not add an `ExecReload=` that sends SIGHUP: statica does not handle it, and SIGHUP ends the process at once.
- **Restarts.** `RestartPreventExitStatus=2` keeps a configuration error from restarting in a loop: fix the file instead. The Go runtime also exits with status 2 when the process crashes, with its report on standard error, so a crashed instance stays stopped too: remove the line to restart it. `Restart=always` also restarts an instance that a stray SIGHUP ended, which systemd counts as a clean exit. [Exit status of `serve`](../reference/cli.md#exit-status-of-serve) lists every status.

Point your load balancer's or supervisor's probes at `/livez` and `/readyz` on the admin listener ([Health checks and readiness](./health-checks.md)).

## Shut down without cutting requests

On the first SIGTERM or SIGINT, statica stops accepting connections at once, reports `not ready: draining` on `/readyz`, and lets active requests finish within [`server.shutdown_timeout`](../reference/configuration.md#server-shutdown-timeout), `30s` by default. Requests still active near the end of that budget are canceled ([one budget](../architecture/lifecycle.md#one-budget)). Three settings around it keep a stop clean:

- **Grace period.** Give your supervisor a grace period, the time between SIGTERM and SIGKILL, longer than `server.shutdown_timeout`. A SIGKILL ends the drain and loses the log lines still queued.
- **Pre-stop delay.** The public listener does not wait for load balancers to notice that `/readyz` turned 503. If yours keeps sending new connections to an instance that is stopping, those connections are refused: remove the instance from the load balancer first, or delay the signal with a pre-stop step, and add that delay to the grace period.
- **Long downloads.** A download still running at the end of the drain is cut, whatever its own limits. If some downloads must finish, raise `server.shutdown_timeout` and the grace period together ([long downloads](../architecture/lifecycle.md#long-downloads)).

### Signals

- **SIGTERM or SIGINT** starts the graceful shutdown. Send it once: a second one during the shutdown cuts the active requests at once ([signals of `serve`](../reference/cli.md#signals-of-serve)).
- **SIGHUP** is not handled: it ends the process at once, without a drain or a log flush, and reloads nothing. So does SIGKILL.

## Change the configuration

The configuration file and the TLS files are read once, at startup. To apply any change, a new publication prefix included, restart the process:

1. Validate the new file where it will run: `statica config validate`, then `statica config check-origin`, which reads one object with the same credentials, region and endpoint that `serve` will use.
2. Restart the instances one at a time, waiting for each new instance to report ready before stopping the next one.

Two side effects of a restart:

- **A cold memory cache.** A restarted instance starts with an empty memory cache and reads every object from the origin again until it warms up. Restarting all instances at once sends all of that traffic to the origin together.
- **Mixed versions during a rollout.** While a rollout runs, old and new instances serve side by side. When the change is a new `s3.prefix` for a new release of the site, visitors can receive pages from either release until the rollout ends ([Publish a site to S3](./publishing.md)).

::: warning Credentials are not checked at startup
An instance with missing credentials or a wrong bucket starts, becomes ready and answers every request that needs the origin with an error. `statica config check-origin` catches it before the rollout, and the [origin health check](./health-checks.md#decide-whether-readiness-follows-the-origin) keeps such an instance from becoming ready.
:::

Content published under the current prefix needs no restart: statica serves it once the memory cache's entries for the changed objects expire ([Tune the memory cache](./cache-tuning.md)).

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

Then stop one instance with your supervisor, for example `systemctl stop statica`, and read its last events:

```text
ts=2026-11-08T00:16:58.867Z level=info event=shutdown_started reason=signal shutdown_timeout=30s
ts=2026-11-08T00:16:58.870Z level=info event=shutdown_complete reason=signal outcome=drained duration_ms=2.665
```

`outcome=forced`, at warn level, means that requests were cut: its `forced_by` field names the cause ([`shutdown_complete`](../reference/logs.md#shutdown-complete)). A log that ends without `shutdown_complete` means that the process ended without a graceful shutdown, for example by SIGKILL or SIGHUP.
