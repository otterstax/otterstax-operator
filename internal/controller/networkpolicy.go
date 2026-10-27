/*
Copyright 2026 OtterStax.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"net"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	v1alpha1 "github.com/otterstax/otterstax-operator/api/v1alpha1"
)

// ConditionNetworkPolicyEnforced reports whether the generated policy is known
// to be doing anything.
//
// It is a condition rather than an assumption because a NetworkPolicy is
// accepted by every API server and enforced only by a CNI that implements it.
// On a cluster whose CNI does not, the object exists, looks correct in kubectl,
// and blocks nothing. For a service with no authentication that is the
// difference between a control and a decoration, and the operator has no way to
// tell which one it created.
const ConditionNetworkPolicyEnforced = "NetworkPolicyEnforced"

const (
	ReasonEnforcementUnverified = "EnforcementUnverified"
	ReasonPolicyDisabled        = "PolicyDisabled"
)

// BuildNetworkPolicy generates the ingress and egress rules for one server.
//
// Ingress is default-deny: the policy selects the server's pods and lists only
// the peers named in the spec, so an empty list means nothing may connect. That
// is the right default for a service where opening a TCP connection is
// equivalent to holding every backend credential.
//
// Egress is derived from the connections rather than left open, so a compromised
// server cannot be used to reach arbitrary hosts. DNS is always allowed, or the
// server cannot resolve the backends it is configured for.
func BuildNetworkPolicy(server *v1alpha1.OtterStaxServer, conns []v1alpha1.OtterStaxConnection) *networkingv1.NetworkPolicy {
	if server.Spec.Network.Policy == v1alpha1.NetworkPolicyNone {
		return nil
	}

	l := labelsFor(server)

	ports := []networkingv1.NetworkPolicyPort{
		{Port: ptr(intstr.FromInt32(server.Spec.Service.FlightSQLPort))},
		{Port: ptr(intstr.FromInt32(server.Spec.Service.MySQLPort))},
		{Port: ptr(intstr.FromInt32(server.Spec.Service.PostgresPort))},
		{Port: ptr(intstr.FromInt32(server.Spec.Service.SparkConnectPort))},
	}

	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(server.Spec.Network.AllowIngressFrom))
	for i := range server.Spec.Network.AllowIngressFrom {
		p := server.Spec.Network.AllowIngressFrom[i]
		peer := networkingv1.NetworkPolicyPeer{PodSelector: &p.PodSelector}
		if p.NamespaceSelector != nil {
			peer.NamespaceSelector = p.NamespaceSelector
		}
		peers = append(peers, peer)
	}

	ingress := []networkingv1.NetworkPolicyIngressRule{}
	if len(peers) > 0 {
		// With no peers the rule list stays empty, which is how a NetworkPolicy
		// expresses "deny everything". Adding a rule with an empty peer list
		// would mean the opposite — allow from anywhere — and that inversion is
		// the classic way these policies are written backwards.
		ingress = append(ingress, networkingv1.NetworkPolicyIngressRule{
			From:  peers,
			Ports: ports,
		})
	}

	egress := []networkingv1.NetworkPolicyEgressRule{
		{
			// DNS. Without it the server cannot resolve a single backend host,
			// and the failure looks like an unreachable backend rather than a
			// policy problem.
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: ptr(corev1.ProtocolUDP), Port: ptr(intstr.FromInt32(53))},
				{Protocol: ptr(corev1.ProtocolTCP), Port: ptr(intstr.FromInt32(53))},
			},
		},
	}
	if backendPorts := egressPortsFor(conns); len(backendPorts) > 0 {
		// Ports only, without peers: the backends are addressed by hostname and
		// may live outside the cluster entirely, so there is no selector that
		// would name them. Narrowing by port is what can honestly be done here.
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{Ports: backendPorts})
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      server.Name,
			Namespace: server.Namespace,
			Labels:    l,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: l},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: ingress,
			Egress:  egress,
		},
	}
}

// egressPortsFor collects the distinct backend ports the connections name.
func egressPortsFor(conns []v1alpha1.OtterStaxConnection) []networkingv1.NetworkPolicyPort {
	seen := map[int32]struct{}{}
	var out []networkingv1.NetworkPolicyPort

	add := func(p int32) {
		if _, dup := seen[p]; dup {
			return
		}
		seen[p] = struct{}{}
		out = append(out, networkingv1.NetworkPolicyPort{
			Protocol: ptr(corev1.ProtocolTCP),
			Port:     ptr(intstr.FromInt32(p)),
		})
	}

	// An unset port is not rendered, and the server then dials the driver's
	// default. The rule has to open that same port, or the backend is blocked
	// under a CNI that enforces policy — and an unreachable backend is the one
	// failure this server reports nowhere.
	for _, c := range conns {
		switch {
		case c.Spec.MySQL != nil:
			add(portOr(c.Spec.MySQL.Port, 3306))
		case c.Spec.ClickHouse != nil:
			add(portOr(c.Spec.ClickHouse.Port, 9000))
		case c.Spec.PostgreSQL != nil:
			add(portOr(c.Spec.PostgreSQL.Port, 5432))
		case c.Spec.S3 != nil:
			add(s3Port(c.Spec.S3.Endpoint))
		}
	}
	return out
}

func portOr(p, def int32) int32 {
	if p == 0 {
		return def
	}
	return p
}

// s3Port is where the server sends S3 traffic. Without an endpoint that is AWS
// over HTTPS. With one, the server forces plain HTTP, so it is the endpoint's
// own port or 80 — a MinIO on :9000 is the common case.
func s3Port(endpoint string) int32 {
	if endpoint == "" {
		return 443
	}
	_, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return 80
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil || p == 0 {
		return 80
	}
	return int32(p)
}
