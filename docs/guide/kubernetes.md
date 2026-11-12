# Deploy on Kubernetes

Run **statica** as a Deployment behind a Service, with its configuration in a ConfigMap, probes on the admin listener and a shutdown that lets active requests finish. This page is self-contained: the configuration and the manifests below make a working deployment once you set your hostnames, bucket and credentials.

## Prerequisites

- A bucket with your site in it ([Publish a site to S3](./publishing.md)), and an identity that can read its objects ([Provide credentials](./s3-providers.md#provide-credentials)).
- `kubectl` access to a namespace of your cluster, and the `statica` binary ([Installation](./installation.md)) to check the configuration.
- Kubernetes 1.30 or later for the `sleep` action of the `preStop` hook. The image has no shell and no `sleep` binary, so an `exec` hook cannot replace it.

## The configuration

Save this file as `config.yaml`, with your hostnames, bucket and region:

<<< @/examples/kubernetes.yaml{yaml}

Check it, then create the ConfigMap that the pods mount:

```sh
statica config validate --config config.yaml
kubectl create configmap statica-config --from-file=config.yaml
```

The image reads `/etc/statica/config.yaml` and runs `statica serve` ([container image](../reference/container-image.md)).

## The manifests

Save these manifests as `statica.yaml`:

```yaml [statica.yaml]
apiVersion: v1
kind: ServiceAccount
metadata:
  name: statica
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: statica
  labels:
    app.kubernetes.io/name: statica
spec:
  replicas: 2
  selector:
    matchLabels:
      app.kubernetes.io/name: statica
  strategy:
    rollingUpdate:
      maxUnavailable: 0
      maxSurge: 1
  template:
    metadata:
      labels:
        app.kubernetes.io/name: statica
    spec:
      serviceAccountName: statica
      terminationGracePeriodSeconds: 45
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: statica
          image: ghcr.io/gtriggiano/statica:{{VERSION}}
          env:
            - name: GOMEMLIMIT
              value: 340MiB
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
          resources:
            requests:
              cpu: 100m
              memory: 384Mi
            limits:
              memory: 384Mi
          securityContext:
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            capabilities:
              drop: [ALL]
          volumeMounts:
            - name: config
              mountPath: /etc/statica
              readOnly: true
      volumes:
        - name: config
          configMap:
            name: statica-config
---
apiVersion: v1
kind: Service
metadata:
  name: statica
spec:
  selector:
    app.kubernetes.io/name: statica
  ports:
    - name: http
      port: 80
      targetPort: http
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: statica
spec:
  maxUnavailable: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: statica
```

Apply them and wait for the rollout:

```sh
kubectl apply -f statica.yaml
kubectl rollout status deployment/statica
```

Point your Ingress or Gateway route at the Service `statica`, port `80`. What the manifests rely on:

- **Probes.** `/livez` and `/readyz` are on the admin port and never contact the origin. Never use `/readyz` for liveness ([Health checks and readiness](./health-checks.md)).
- **Shutdown.** statica stops accepting connections as soon as it receives SIGTERM, so the `preStop` delay gives the Service time to stop sending new ones. `terminationGracePeriodSeconds` counts from the start of the hook and must exceed the delay plus `server.shutdown_timeout`: 5 + 30 < 45 here ([Shut down without cutting requests](./deployment.md#shut-down-without-cutting-requests)).
- **Rollouts.** `maxUnavailable: 0` replaces one pod at a time and waits for the new pod to be ready. A new pod starts with an empty memory cache.
- **The admin port.** It is a container port only, absent from the public Service: `/livez`, `/readyz` and `/metrics` have no authentication.
- **Security context.** The image runs as user `65532` and statica writes nothing to disk, so it needs no capability and no writable file system.

## Give the pods credentials

The configuration file never holds credentials: statica reads them through the standard AWS credential chain. The identity needs `s3:GetObject` on the site's objects ([Provide credentials](./s3-providers.md#provide-credentials)).

::: warning Credentials are not checked at startup
A pod with missing or wrong credentials becomes ready and then fails the requests that need the origin. To keep such pods out of a rollout, enable the origin health check: readiness then waits for one successful read of the bucket ([Decide whether readiness follows the origin](./health-checks.md#decide-whether-readiness-follows-the-origin)).
:::

### Amazon EKS

With IAM roles for service accounts, annotate the ServiceAccount with the role:

```yaml [statica.yaml]
apiVersion: v1
kind: ServiceAccount
metadata:
  name: statica
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/example-site-reader
```

With EKS Pod Identity, which needs the Pod Identity Agent add-on on the cluster, leave the ServiceAccount without annotation and associate it with the role:

```sh
aws eks create-pod-identity-association --cluster-name example-cluster \
  --namespace default --service-account statica \
  --role-arn arn:aws:iam::123456789012:role/example-site-reader
```

### Access keys in a Secret

For an S3-compatible service, or a cluster without workload identity, store the access key in a Secret:

```sh
kubectl create secret generic statica-origin \
  --from-literal=AWS_ACCESS_KEY_ID="$AWS_ACCESS_KEY_ID" \
  --from-literal=AWS_SECRET_ACCESS_KEY="$AWS_SECRET_ACCESS_KEY"
```

Then load it into the container's environment, next to `env`:

```yaml [statica.yaml]
          envFrom:
            - secretRef:
                name: statica-origin
```

[Configure an S3-compatible service](./s3-providers.md#configure-an-s3-compatible-service) lists the `s3` settings such a service needs.

## Size the memory

The cache limits bound the cache, not the process, and the Go heap can grow to about twice its live size between two garbage collections ([Leave room for the rest of the process](./cache-tuning.md#leave-room-for-the-rest-of-the-process)). Start from these two values, then compare `process_resident_memory_bytes` with the limit under real traffic:

| Setting | Starting value | With the example configuration |
| --- | --- | --- |
| Memory request and limit | Twice the sum of `cache.max_bytes` and `cache.max_fill_bytes`, plus room for the rest of the process | 2 × (128 + 32) MiB = 320 MiB, plus 64 MiB: `384Mi` |
| `GOMEMLIMIT` | About 90% of the memory limit | `340MiB` |

`GOMEMLIMIT` is the Go runtime's soft memory limit: as the heap approaches it, the garbage collector runs more often, which helps keep the process below the container's limit. It does not cap memory that is in use: a process whose live memory exceeds the container's limit is still killed. When you change the cache limits, change the three values together.

## Change the configuration

statica reads its configuration once, at startup, and never reloads it: a changed ConfigMap has no effect until the pods restart. Replace the ConfigMap and restart them:

```sh
kubectl create configmap statica-config --from-file=config.yaml --dry-run=client -o yaml | kubectl apply -f -
kubectl rollout restart deployment/statica
```

With Kustomize, generate the ConfigMap instead: its name then carries a hash of its content, so every change rolls the pods without a separate restart.

```yaml [kustomization.yaml]
resources:
  - statica.yaml
configMapGenerator:
  - name: statica-config
    files:
      - config.yaml
```

A value written as `${NAME}` in the configuration is read from the container's environment, which lets a Deployment choose, for example, the release prefix to serve ([Deploy with release prefixes](./publishing.md#deploy-with-release-prefixes)).

## Observability

### Metrics

Prometheus scrapes `/metrics` on the admin port of every pod. With the Prometheus Operator, declare the targets with a PodMonitor or a ServiceMonitor. Create the monitor in the namespace of the pods, and check that your Prometheus watches that namespace (`podMonitorNamespaceSelector` and `serviceMonitorNamespaceSelector`): by default it reads only the monitors of its own namespace. If it also selects monitors by label, add its label to the monitor's metadata.

A PodMonitor needs nothing else. Use it unless your Prometheus selects ServiceMonitors only:

```yaml [podmonitor.yaml]
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: statica
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: statica
  podMetricsEndpoints:
    - port: admin
```

A ServiceMonitor finds the pods through a Service. Give it a Service of its own, headless and with the admin port only: adding port `9090` to the Service behind your ingress would put the unauthenticated admin endpoints one route away from the internet.

```yaml [servicemonitor.yaml]
apiVersion: v1
kind: Service
metadata:
  name: statica-admin
  labels:
    app.kubernetes.io/name: statica
    app.kubernetes.io/component: admin
spec:
  clusterIP: None
  selector:
    app.kubernetes.io/name: statica
  ports:
    - name: admin
      port: 9090
      targetPort: admin
---
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: statica
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: statica
      app.kubernetes.io/component: admin
  endpoints:
    - port: admin
```

Either way, Prometheus connects to each pod on port `9090`, so the same [network policy](#restrict-the-network) covers both. [Monitor with Prometheus](./metrics.md) has the queries, and [Signals worth watching](./metrics.md#signals-worth-watching) lists the signals worth watching, among them requests that fail at the origin, `statica_ready` at 0 outside rollouts, and resident memory approaching the container's limit.

### Logs

statica writes its logs to standard output as logfmt events, one event per line, so the cluster's log collector picks them up without configuration, and `kubectl logs` shows them. Parse them as logfmt. [Work with the logs](./logs.md) covers what to log and how to read the events, and the [log reference](../reference/logs.md) lists every event and field.

When standard output cannot keep up, statica drops lines instead of slowing requests and counts them in [`statica_log_events_dropped_total`](../reference/metrics.md#statica-log-events-dropped-total) ([dropped events](./logs.md#dropped-events)).

### Health

The readiness probe and the [`statica_ready`](../reference/metrics.md#statica-ready) gauge report the same decision: the gauge is 1 while `/readyz` answers 200. When a pod is not ready, the body of `/readyz` names the reason ([`/readyz`](../reference/admin-endpoints.md#readyz)). With the origin health check enabled, the log records each change with an [`origin_health_changed`](../reference/logs.md#origin-health-changed) event ([Health checks and readiness](./health-checks.md)).

## Restrict the network

A NetworkPolicy keeps the admin port for Prometheus and the public port for your ingress controller. The namespace names are examples; the kubelet's probes come from the node and are not subject to the policy. The policy has an effect only if your network plugin enforces NetworkPolicy.

```yaml [networkpolicy.yaml]
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: statica
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: statica
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: ingress-nginx
      ports:
        - port: http
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: monitoring
      ports:
        - port: admin
```

::: danger Trust the pod network only behind this policy
An ingress controller connects from pod addresses that change, so `server.trusted_proxies` in the example names the whole pod network, `10.244.0.0/16`: set it to your cluster's. Every peer inside that range can choose the client IP that statica logs and the request ID that it reuses. That is safe only while the policy lets nothing but the ingress controller reach port `8080`; without such a policy, remove `server.trusted_proxies` ([Trust only your proxies](./behind-a-proxy.md#trust-only-your-proxies)).
:::

## Verify it

```sh
kubectl get pods -l app.kubernetes.io/name=statica
kubectl logs deployment/statica
kubectl port-forward deployment/statica 8080:8080
```

Every pod is `Running` and ready, and the log has a `startup` event and no `startup_failed`. With the port forwarded, request the site under one of its `hosts`:

```sh
curl -sS -o /dev/null -w '%{http_code}\n' -H 'Host: www.example.com' http://127.0.0.1:8080/
```

It prints `200`. [Troubleshooting](./troubleshooting.md#on-kubernetes) covers pods that never become ready, restart loops and denied reads.
