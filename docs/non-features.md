# Deliberately absent

Things this operator does not do, with the reason. Written down because the
absence is the decision, and an absent thing has no code to read.

Most entries exist because a reasonable-looking addition would make the system
worse in a way that only shows up under load or during an incident.

## No `replicas` field, and no scale subresource

OtterStax has no clustering of any kind: the scheduler is an in-process worker
pool and federation is client-side fan-out. A second pod is a second, divergent
database — not a replica. There is no leader election, no shared state, no
coordination.

The field is omitted rather than validated to be 1. A field that does not exist
cannot be set wrong: `kubectl scale`, HPA and KEDA fail at the API server with a
clear error instead of silently splitting the workload. Adding it later, if
clustering ever lands, is a backwards-compatible change.

## No liveness probe

The engine has open, acknowledged bugs about state not surviving a restart. A
false positive on a liveness probe is a SIGKILL in the middle of a WAL write.
The detection value is close to zero — the failure it would catch is a hung
process, which is not a failure mode anyone has observed here — and the cost is
corruption.

## No PodDisruptionBudget

`minAvailable: 1` on a single replica yields `disruptionsAllowed: 0`
permanently: node drains hang forever, the cluster autoscaler cannot scale down,
and consolidation stalls. For a single-instance service where a restart is
expected during node maintenance, no PDB is the correct answer.

## Readiness probes 8815 only

The server binds MySQL, then PostgreSQL, then Spark Connect, then FlightSQL,
and registers its backends before any of them. So 8815 opening means the engine recovered, WAL replay
finished and registration completed or exhausted its retries.

The wire ports are unusable as probes: their acceptor binds in the constructor,
one line before the accept handler is installed, so a TCP connect can succeed
microseconds before the server can answer anything.

## No fixed `initialDelaySeconds`

WAL replay is unbounded, and connection registration is serial and blocking — a
server with several unreachable backends opens its ports minutes late through no
fault of its own. The startup probe's `failureThreshold` is derived from
`spec.startup.budgetSeconds` instead. Any fixed delay is wrong for someone.

## The config volume is not optional

A missing config does not stop this server. It warns — at a level compiled out
of the production image — applies defaults, and serves on every port with
zero connections. A failed Secret mount would become an invisible outage.

Mounting with `optional: false` means the kubelet refuses to start the container
at all, converting a silent misconfiguration into a visible one. This is
prevention, not detection: the operator cannot observe the bad state, so it
makes it unreachable.

## The config is mounted as a directory, never via `subPath`

A `subPath`-mounted file is copied once and frozen: a rotated Secret would never
reach the container. It does not matter today because every change restarts the
pod anyway, but it costs nothing to keep the option open and is expensive to
notice later.

## Local storage is ephemeral

The engine's data directory is an `emptyDir`. Federated data lives in the remote
systems; the volume holds WAL churn, `.otbx` files for anything created locally,
and twenty unrotated log files per process start.

This is not a hedge — it is the honest encoding of what the software currently
is. It also removes any dependency on the cluster's StorageClass, which matters
when the deployment target is unknown.

## The operator never resolves secret references

Credentials reach the config as literals, or as an opaque reference the server
resolves itself. There is no `secretKeyRef` resolution, because the server reads
exactly one file and no environment variables — anything the operator resolved
would end up as literal bytes in that file regardless.

## Connection binding is same-namespace only

A cross-namespace reference is a privilege-escalation path: whoever can create a
connection in namespace A could otherwise inject credentials into a server in
namespace B. The answer to wanting one is to replicate the object.

## A nil `connectionSelector` matches nothing

Not everything. An operator that adopted every connection in the namespace would
make creating an unrelated object a live change to an unrelated server. An
explicit empty selector (`matchLabels: {}`) still matches everything, per
label-selector convention.

## Entries within a config section are not sorted

Section order is fixed; the order of entries inside a section follows the input.
Sorting would be a silent behaviour change: with a duplicate alias across
backend kinds the server resolves by probe order, so reordering can reroute a
query to a different backend.

## Only the config bytes go into the hash

Not the generation, not the resourceVersion, not a timestamp. Any of those would
make an unrelated update to the object restart a healthy server — and a restart
here is a SIGKILL mid-WAL-write, not an inconvenience.

## Garbage collection never deletes the mounted revision

Even when it is not the revision just rendered. A rollout can be mid-flight, and
deleting the Secret a live pod mounts leaves a healthy pod unable to restart —
a worse failure than keeping a few stale objects.

## No Gateway API support

TCPRoute reached the standard channel recently, but implementation support is
uneven, and the operator must install and work on a cluster with nothing on it.
`spec.exposure` covers ClusterIP, NodePort and LoadBalancer with user-supplied
annotations, which works everywhere.

## No per-cloud load-balancer annotations

`spec.exposure.annotations` is copied verbatim onto the Service. There are
hundreds of provider-specific annotations, they change, and a built-in list that
is merely out of date is worse than no list.
