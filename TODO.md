# TODO

Known gaps, in rough order of who hits them first.

## No published images

Upstream publishes no OtterStax server image, so the Quickstart only works with
an image you build yourself, and CI never runs the operator against a real
server — the live checks so far were done by hand on kind. The operator has no
release either: no image on ghcr.io, no `dist/install.yaml`, no release
workflow.

- Publish a server image upstream, or build one in CI.
- Add an e2e case that runs a real server and a federated query.
- Add a release workflow that pushes the operator image and attaches
  `dist/install.yaml` (`make build-installer`).

## Kafka sources are blocked by the egress policy

Kafka sources are created by SQL at runtime, not by an `OtterStaxConnection`, so
the operator never sees their brokers. The generated NetworkPolicy opens egress
only to ports named by connections, and the brokers are cut off under any CNI
that enforces policy.

Either document it, or add `spec.network.allowEgress` for destinations the
operator cannot derive.

## In-cluster backend whose Service port differs from targetPort

Egress is narrowed by the port the connection names (`egressPortsFor` in
`internal/controller/networkpolicy.go`). For a backend behind a Service, the CNI
matches the policy after DNAT, against the pod's `targetPort`. A Service on 3306
forwarding to 3307 is therefore blocked although the connection is correct.

Resolve the Service when the host is an in-cluster name and open its
`targetPort` as well, or document the restriction.

## `boundServers` is capped at 32

`OtterStaxConnectionStatus.BoundServers` has `MaxItems=32`. A connection
selected by a 33rd server makes the status patch fail. Either drop the cap, or
truncate the list and report the overflow in a condition.
