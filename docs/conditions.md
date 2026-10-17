# Status conditions

What the operator reports, and — more importantly — what it declines to report.

The recurring theme: this server exposes no health endpoint, no metrics and no
admin API, and it reaches Ready with every backend unreachable. A condition that
said "healthy" would be asserting something nothing here can establish. Several
conditions are therefore `Unknown` in the normal case, deliberately.

## OtterStaxServer

### `ConfigRendered`

| Status | Reason | Meaning |
|---|---|---|
| `True` | `Rendered` | The config was produced. The message carries the connection count |
| `False` | `InvalidConnections` | Refused to render. **The running pod is left alone** — replacing a working server with one built from a config just judged unsound turns a reported problem into an outage |
| `Unknown` | `ReconciliationPaused` | `spec.clusterOperation.reconciliationPaused` is set: reporting continues, writing stops |

`InvalidConnections` is most often a duplicate alias. The server would accept it,
keep one connection and leak the other, silently; across backend kinds it routes
queries by probe order, so a query reaches a different backend than written.
Neither is observable, which is why the operator refuses rather than reports.

### `Available`

Follows the StatefulSet. `True` once at least one replica is ready and updated.
This says the process is running and accepting connections on 8815 — nothing
about whether it can answer a query usefully.

With `spec.clusterOperation.stopped` set it is `False` with reason `Stopped`,
not `Progressing`: nothing is rolling out, and nothing is stuck.

### `Degraded`

| Status | Reason | Meaning |
|---|---|---|
| `True` | `ConfigRejected` | The container exited with the code the server returns when it cannot load its config. A hint rather than proof: the same code covers argument-parsing failures |
| `True` | `ConfigNotMounted` | The kubelet will not start the container because the config Secret is absent. This is by design — see [non-features](non-features.md) |
| `True` | `CrashLooping` | Exited non-zero for some other reason; the message carries the code and restart count |
| `Unknown` | `BackendHealthUnknown` | **The normal state of a working server.** It is serving; whether any backend is reachable cannot be established from outside the process |

`Degraded=Unknown` is not a defect in the operator. `False` would mean "not
degraded", which is a claim about backend reachability that nothing here can
make.

### `NetworkPolicyEnforced`

| Status | Reason | Meaning |
|---|---|---|
| `Unknown` | `EnforcementUnverified` | A policy was generated. Whether it is enforced depends on the cluster's CNI |
| `False` | `PolicyDisabled` | `spec.network.policy: None`. Nothing restricts access to a service that authenticates nobody |

This condition is never `True`. A CNI that does not implement NetworkPolicy
accepts the object and blocks nothing, and the operator cannot tell the
difference. Reporting an unenforced policy as protection would be worse than
reporting nothing.

### `lastRestartReason`

Not a condition — a record, because "why did it restart at 3am" is asked after
the fact, when the only evidence left is a pod with a new start time.

| Reason | Trigger |
|---|---|
| `InitialRollout` | The StatefulSet did not exist |
| `ConfigChanged` | The rendered config hash changed; the detail names both hashes |
| `ImageChanged` | `spec.image` changed; the detail names both images |
| `ManualRestart` | The `otterstax.com/restart` annotation changed |

Also emitted as an Event on the object, because that is where people look first.

## OtterStaxConnection

### `Accepted`

`True` with the connector kind as the reason (`MySQL`, `PostgreSQL`,
`ClickHouse`, `S3`) — which is what the `KIND` column shows.

### `Bound`

| Status | Reason | Meaning |
|---|---|---|
| `True` | `Claimed` | At least one server's `connectionSelector` matches; `status.boundServers` lists them |
| `False` | `NoServerSelectsIt` | Nothing selects it |

`NoServerSelectsIt` is the most likely mistake with selector-based binding, and
this is the only place it surfaces. A connection with a typo in its labels is
not an error anywhere: it is simply absent from every config and has no effect.

## Reading the two hashes

`status.renderedConfigHash` is what the operator last produced.
`status.runningConfigHash` is what the live pod template actually mounts.

Every change is a restart, so a window where they differ is normal. A window
that does not close is a stuck rollout, and nothing else in the system would
say so.
