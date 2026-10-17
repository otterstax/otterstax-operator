# otterstax-operator

A Kubernetes operator for [OtterStax](https://github.com/otterstax/otterstax), a
single-process federated SQL engine.

## What it does, and what that is worth

OtterStax reads one configuration file at startup. It has no reload path, no
admin API, and no health endpoint. So the operator does two things that nothing
else does:

- **Turns a config change into a restart, exactly once and only when needed.**
  Kubernetes does not restart a pod when a mounted Secret changes, and this
  server would not notice if it did. The config is rendered from custom
  resources, addressed by the hash of its own bytes, and the hash on the pod
  template is what rolls the workload.
- **Reports what can be established, and says so when nothing can.** The server
  reaches Ready with every backend unreachable and mentions it nowhere.

Everything else here — the Services, the NetworkPolicy, the revision garbage
collection — is bookkeeping around those two.

## Install

There is no release yet. Build and deploy from source:

```sh
make docker-build docker-push IMG=<registry>/otterstax-operator:<tag>
make deploy IMG=<registry>/otterstax-operator:<tag>
```

No webhooks, no cert-manager, no CRDs outside this project. All validation is
CEL, which means installation is one command on a cluster with nothing on it.

## Quickstart

```yaml
apiVersion: otterstax.com/v1alpha1
kind: OtterStaxServer
metadata:
  name: demo
spec:
  image: ghcr.io/otterstax/otterstax:<tag>   # required; see Limitations
  connectionSelector:
    matchLabels:
      server: demo
---
apiVersion: otterstax.com/v1alpha1
kind: OtterStaxConnection
metadata:
  name: billing
  labels:
    server: demo          # must match the server's connectionSelector
spec:
  alias: mysql            # the outermost qualifier in SQL: mysql.bill.orders
  mysql:
    host: mariadb
    port: 3306
    username: demo
    password: demo
    database: bill
```

```sh
kubectl get otterstax          # servers
kubectl get osconn             # connections, and which server claims each
```

A connection is picked up by label selector, so adding a backend is one new
object and never an edit to the server. A connection whose labels match no
server reports `Bound=False` with reason `NoServerSelectsIt` — worth checking
first when a backend seems to be ignored, because nothing else will mention it.

## Limitations you should know before deploying

These are properties of the server, not oversights in the operator. They are
listed here because finding them during an incident is worse.

**No authentication on any wire protocol.** The MySQL handshake parses a
username and password and discards them; PostgreSQL sends an unconditional
auth-ok; FlightSQL runs a no-op auth handler; Spark Connect listens with
insecure gRPC credentials and checks nothing. Anything that can open a TCP
connection can read and write every federated backend, using credentials the
server holds on its behalf. The generated NetworkPolicy is the only access
control that exists, and moving `spec.exposure.type` off `ClusterIP` requires
setting `acknowledgeUnauthenticated: true`.

**NetworkPolicy enforcement cannot be verified.** A NetworkPolicy is accepted by
every API server and enforced only by a CNI that implements one. The operator
reports `NetworkPolicyEnforced=Unknown` rather than claiming protection it
cannot confirm.

**Backend reachability is not observable.** No metrics, no health endpoint, no
admin API — and the server's own logging is compiled at ERROR, so even the log
line is unreliable. `Degraded=Unknown/BackendHealthUnknown` means exactly what
it says.

**Local storage is ephemeral.** The engine's data directory is an `emptyDir`.
Federated data lives in the remote systems; what the volume holds is WAL churn
and logs. Anything created with local DDL does not survive a restart.

**No `replicas` field, and no scale subresource.** OtterStax has no clustering:
a second pod is a second divergent database, not a replica. `kubectl scale`,
HPA and KEDA fail at the API server rather than silently splitting the workload.

**No published server image yet.** `spec.image` is required and has no default
because upstream publishes none.

## Operating it

| Task | How |
|---|---|
| Force a restart | `kubectl annotate otterstaxserver <name> otterstax.com/restart="$(date -u +%FT%TZ)" --overwrite` |
| Why did it restart | `kubectl get otterstaxserver <name> -o jsonpath='{.status.lastRestartReason}'`, or the object's Events |
| Take out of service | `spec.clusterOperation.stopped: true` — scales to zero, keeps config revisions, Services and status |
| Make the operator hold still | `spec.clusterOperation.reconciliationPaused: true` — keeps reporting, stops writing |
| Change the pod without a new field | `spec.podOverrides`, strategic merge on the pod template |

See [docs/conditions.md](docs/conditions.md) for what each status condition
means, and [docs/non-features.md](docs/non-features.md) for what is deliberately
absent and why.

## Development

```sh
make manifests generate           # regenerate CRDs and deepcopy
make test                         # unit + envtest
make install deploy IMG=<image>   # onto the current kubecontext
make test-e2e-chainsaw            # end-to-end, needs a deployed operator
```

`make deploy` is not optional after changing RBAC markers. Loading a new image
and restarting the deployment leaves the old ClusterRole in place, and a
controller that cannot list a watched resource fails to sync its cache and stops
reconciling entirely — with no error anywhere except the manager's own log.

Licensed under the Apache License 2.0.
