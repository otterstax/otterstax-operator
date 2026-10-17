/*
Copyright 2026.

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
	"context"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/otterstax/otterstax-operator/api/v1alpha1"
)

// harness is a live apiserver plus a reconciler driven by hand.
//
// Reconcile is called explicitly rather than run as a manager: envtest has no
// kubelet and no controller-manager, so nothing would ever settle on its own,
// and a test that waits for convergence would only be testing its own timeout.
type harness struct {
	c   client.Client
	r   *OtterStaxServerReconciler
	ctx context.Context
	t   *testing.T
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	if err := v1alpha1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return &harness{
		c:   c,
		r:   &OtterStaxServerReconciler{Client: c, Scheme: scheme.Scheme},
		ctx: context.Background(),
		t:   t,
	}
}

func (h *harness) reconcile(name string) {
	h.t.Helper()
	if _, err := h.r.Reconcile(h.ctx, ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: "default", Name: name},
	}); err != nil {
		h.t.Fatalf("Reconcile: %v", err)
	}
}

func (h *harness) server(name string) *v1alpha1.OtterStaxServer {
	h.t.Helper()
	s := &v1alpha1.OtterStaxServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1alpha1.OtterStaxServerSpec{
			Image:              "ghcr.io/otterstax/otterstax:test",
			Service:            v1alpha1.ServiceSpec{FlightSQLPort: 8815, MySQLPort: 8816, PostgresPort: 8817},
			ConnectionSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"server": name}},
		},
	}
	if err := h.c.Create(h.ctx, s); err != nil {
		h.t.Fatalf("creating server: %v", err)
	}
	return s
}

func (h *harness) connection(name, server, alias, host string) *v1alpha1.OtterStaxConnection {
	h.t.Helper()
	c := &v1alpha1.OtterStaxConnection{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Labels: map[string]string{"server": server},
		},
		Spec: v1alpha1.OtterStaxConnectionSpec{
			Alias: alias,
			MySQL: &v1alpha1.SQLBackend{Host: host, Port: 3306, Username: "u", Database: "d"},
		},
	}
	if err := h.c.Create(h.ctx, c); err != nil {
		h.t.Fatalf("creating connection: %v", err)
	}
	return c
}

func (h *harness) serverObj(name string) *v1alpha1.OtterStaxServer {
	h.t.Helper()
	var s v1alpha1.OtterStaxServer
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: "default", Name: name}, &s); err != nil {
		h.t.Fatalf("getting server: %v", err)
	}
	return &s
}

func (h *harness) statefulSet(name string) *appsv1.StatefulSet {
	h.t.Helper()
	var sts appsv1.StatefulSet
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: "default", Name: name}, &sts); err != nil {
		h.t.Fatalf("getting statefulset: %v", err)
	}
	return &sts
}

// TestConfigChangeRollsThePod is the assertion the whole operator exists for.
//
// The server reads its config once and has no reload path, so a changed backend
// only takes effect if the pod is replaced. Nothing in Kubernetes does that on
// its own when a mounted Secret changes. If this test ever goes green while the
// mechanism is broken, the operator is decorative.
func TestConfigChangeRollsThePod(t *testing.T) {
	h := newHarness(t)
	h.server("srv")
	conn := h.connection("c1", "srv", "mysql", "mariadb-a")

	h.reconcile("srv")
	before := h.statefulSet("srv")
	hashBefore := before.Spec.Template.Annotations[ConfigHashAnnotation]
	secretBefore := before.Spec.Template.Spec.Volumes[0].Secret.SecretName
	if hashBefore == "" {
		t.Fatal("no config hash on the pod template")
	}

	// Move the backend to a different host: same shape, different bytes.
	conn.Spec.MySQL.Host = "mariadb-b"
	if err := h.c.Update(h.ctx, conn); err != nil {
		t.Fatalf("updating connection: %v", err)
	}
	h.reconcile("srv")

	after := h.statefulSet("srv")
	hashAfter := after.Spec.Template.Annotations[ConfigHashAnnotation]
	secretAfter := after.Spec.Template.Spec.Volumes[0].Secret.SecretName

	if hashAfter == hashBefore {
		t.Fatal("config hash did not change; the pod would not be replaced and the new backend would never take effect")
	}
	if secretAfter == secretBefore {
		t.Fatal("the pod template still references the old config Secret")
	}

	// And the new revision must actually contain the new host, not merely be a
	// different name.
	var sec corev1.Secret
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: "default", Name: secretAfter}, &sec); err != nil {
		t.Fatalf("getting the new config secret: %v", err)
	}
	if got := string(sec.Data[ConfigFileName]); !contains(got, "mariadb-b") {
		t.Fatalf("the new revision does not contain the new host:\n%s", got)
	}
}

// TestUnchangedSpecDoesNotRoll. A reconcile that changes nothing must write
// nothing. Spurious hash churn here is not cosmetic: a restart is a SIGKILL in
// the middle of a WAL write on this engine.
func TestUnchangedSpecDoesNotRoll(t *testing.T) {
	h := newHarness(t)
	h.server("srv")
	h.connection("c1", "srv", "mysql", "mariadb")

	h.reconcile("srv")
	first := h.statefulSet("srv").Spec.Template.Annotations[ConfigHashAnnotation]

	for i := range 5 {
		h.reconcile("srv")
		if got := h.statefulSet("srv").Spec.Template.Annotations[ConfigHashAnnotation]; got != first {
			t.Fatalf("hash changed on reconcile %d with no spec change: %s -> %s", i, first, got)
		}
	}
}

// TestDuplicateAliasLeavesTheRunningPodAlone.
//
// Two connections claiming one alias is unrenderable: the server would keep one
// and leak the other, silently. The controller must report that and stop —
// replacing a healthy pod with one built from a config we just judged unsound
// would turn a reported problem into an outage.
func TestDuplicateAliasLeavesTheRunningPodAlone(t *testing.T) {
	h := newHarness(t)
	h.server("srv")
	h.connection("c1", "srv", "shared", "mariadb-a")

	h.reconcile("srv")
	good := h.statefulSet("srv").Spec.Template.Annotations[ConfigHashAnnotation]

	h.connection("c2", "srv", "shared", "mariadb-b")
	h.reconcile("srv")

	if got := h.statefulSet("srv").Spec.Template.Annotations[ConfigHashAnnotation]; got != good {
		t.Fatalf("the running pod was rolled onto an unsound config: %s -> %s", good, got)
	}

	var srv v1alpha1.OtterStaxServer
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: "default", Name: "srv"}, &srv); err != nil {
		t.Fatalf("getting server: %v", err)
	}
	cond := findCondition(srv.Status.Conditions, ConditionConfigRendered)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected ConfigRendered=False, got %+v", cond)
	}
}

// TestMountedRevisionSurvivesCollection. Deleting the Secret a live pod mounts
// leaves a healthy pod unable to restart — a worse failure than keeping clutter.
func TestMountedRevisionSurvivesCollection(t *testing.T) {
	h := newHarness(t)
	h.server("srv")
	conn := h.connection("c1", "srv", "mysql", "host-0")
	h.reconcile("srv")

	// Churn well past the retention budget.
	for i := 1; i <= keptRevisions+3; i++ {
		conn.Spec.MySQL.Host = "host-" + string(rune('0'+i))
		if err := h.c.Update(h.ctx, conn); err != nil {
			t.Fatalf("updating connection: %v", err)
		}
		h.reconcile("srv")
	}

	mounted := h.statefulSet("srv").Spec.Template.Spec.Volumes[0].Secret.SecretName
	var sec corev1.Secret
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: "default", Name: mounted}, &sec); err != nil {
		t.Fatalf("the mounted config revision was collected: %v", err)
	}
}

func findCondition(conds []metav1.Condition, t string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// TestPodOverridesAreApplied. A field that appears in the schema and does
// nothing is worse than no field: it promises an escape hatch that is not
// there, and the user only finds out in an incident.
func TestPodOverridesAreApplied(t *testing.T) {
	h := newHarness(t)

	srv := &v1alpha1.OtterStaxServer{
		ObjectMeta: metav1.ObjectMeta{Name: "ovr", Namespace: "default"},
		Spec: v1alpha1.OtterStaxServerSpec{
			Image:   "ghcr.io/otterstax/otterstax:test",
			Service: v1alpha1.ServiceSpec{FlightSQLPort: 8815, MySQLPort: 8816, PostgresPort: 8817},
			PodOverrides: &runtime.RawExtension{Raw: []byte(`{
				"metadata": {"annotations": {"team": "data"}},
				"spec": {
					"nodeSelector": {"disktype": "ssd"},
					"containers": [{"name": "otterstax", "env": [{"name": "TZ", "value": "UTC"}]}]
				}
			}`)},
		},
	}
	if err := h.c.Create(h.ctx, srv); err != nil {
		t.Fatalf("creating server: %v", err)
	}
	h.reconcile("ovr")

	sts := h.statefulSet("ovr")
	pod := sts.Spec.Template

	if pod.Spec.NodeSelector["disktype"] != "ssd" {
		t.Errorf("nodeSelector not applied: %v", pod.Spec.NodeSelector)
	}
	if pod.Annotations["team"] != "data" {
		t.Errorf("annotation not applied: %v", pod.Annotations)
	}

	// A strategic merge, not a replacement: the generated container must still
	// be there, with its args, and merely gain the env var.
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("expected the containers list to merge by name, got %d containers", len(pod.Spec.Containers))
	}
	c := pod.Spec.Containers[0]
	if len(c.Args) == 0 {
		t.Error("the generated --config args were lost in the merge")
	}
	var sawTZ bool
	for _, e := range c.Env {
		if e.Name == "TZ" {
			sawTZ = true
		}
	}
	if !sawTZ {
		t.Error("env from podOverrides not applied")
	}

	// And the invariants must survive whatever the user wrote.
	if pod.Annotations[ConfigHashAnnotation] == "" {
		t.Error("the config-hash annotation was lost; the server would never pick up a config change again")
	}
	if pod.Labels[InstanceLabel] != "ovr" {
		t.Error("selector labels were lost; the pod would not match its own StatefulSet")
	}
}

// TestPodOverridesCannotDropTheRestartTrigger. The hash annotation is the whole
// restart mechanism. An override that replaced the annotations map instead of
// merging into it would silently disable config changes forever.
func TestPodOverridesCannotDropTheRestartTrigger(t *testing.T) {
	h := newHarness(t)

	srv := &v1alpha1.OtterStaxServer{
		ObjectMeta: metav1.ObjectMeta{Name: "ovr2", Namespace: "default"},
		Spec: v1alpha1.OtterStaxServerSpec{
			Image:   "ghcr.io/otterstax/otterstax:test",
			Service: v1alpha1.ServiceSpec{FlightSQLPort: 8815, MySQLPort: 8816, PostgresPort: 8817},
			PodOverrides: &runtime.RawExtension{Raw: []byte(
				`{"metadata": {"annotations": {"otterstax.com/config-hash": "forged"}}}`)},
		},
	}
	if err := h.c.Create(h.ctx, srv); err != nil {
		t.Fatalf("creating server: %v", err)
	}
	h.reconcile("ovr2")

	got := h.statefulSet("ovr2").Spec.Template.Annotations[ConfigHashAnnotation]
	if got == "forged" {
		t.Fatal("podOverrides overwrote the config hash; config changes would stop rolling the pod")
	}
}

// TestNetworkPolicyDefaultsToDenyingEverything.
//
// The wire protocols authenticate nobody, so the failure mode of getting this
// backwards is total: anything in the cluster reads and writes every federated
// system. An empty ingress rule list denies; a rule with an empty peer list
// allows from anywhere. They look nearly identical in Go and are opposites.
func TestNetworkPolicyDefaultsToDenyingEverything(t *testing.T) {
	h := newHarness(t)
	h.server("np")
	h.connection("c1", "np", "mysql", "mariadb")
	h.reconcile("np")

	var pol networkingv1.NetworkPolicy
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: "default", Name: "np"}, &pol); err != nil {
		t.Fatalf("no NetworkPolicy generated: %v", err)
	}

	if len(pol.Spec.Ingress) != 0 {
		t.Fatalf("with no permitted peers the ingress rule list must be empty (deny), got %d rule(s): %+v",
			len(pol.Spec.Ingress), pol.Spec.Ingress)
	}

	var hasIngress, hasEgress bool
	for _, pt := range pol.Spec.PolicyTypes {
		switch pt {
		case networkingv1.PolicyTypeIngress:
			hasIngress = true
		case networkingv1.PolicyTypeEgress:
			hasEgress = true
		}
	}
	if !hasIngress || !hasEgress {
		t.Errorf("both policy types must be set, got %v", pol.Spec.PolicyTypes)
	}

	// DNS must survive, or the server cannot resolve a backend and the symptom
	// looks like an unreachable database rather than a policy problem.
	var dns bool
	for _, e := range pol.Spec.Egress {
		for _, p := range e.Ports {
			if p.Port != nil && p.Port.IntValue() == 53 {
				dns = true
			}
		}
	}
	if !dns {
		t.Error("egress does not permit DNS")
	}
}

// TestNetworkPolicyOpensOnlyNamedPeers.
func TestNetworkPolicyOpensOnlyNamedPeers(t *testing.T) {
	h := newHarness(t)

	srv := &v1alpha1.OtterStaxServer{
		ObjectMeta: metav1.ObjectMeta{Name: "np2", Namespace: "default"},
		Spec: v1alpha1.OtterStaxServerSpec{
			Image:   "ghcr.io/otterstax/otterstax:test",
			Service: v1alpha1.ServiceSpec{FlightSQLPort: 8815, MySQLPort: 8816, PostgresPort: 8817},
			Network: v1alpha1.NetworkSpec{
				Policy: v1alpha1.NetworkPolicyDefaultDeny,
				AllowIngressFrom: []v1alpha1.IngressPeer{
					{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "bi"}}},
				},
			},
		},
	}
	if err := h.c.Create(h.ctx, srv); err != nil {
		t.Fatalf("creating server: %v", err)
	}
	h.reconcile("np2")

	var pol networkingv1.NetworkPolicy
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: "default", Name: "np2"}, &pol); err != nil {
		t.Fatalf("no NetworkPolicy: %v", err)
	}
	if len(pol.Spec.Ingress) != 1 || len(pol.Spec.Ingress[0].From) != 1 {
		t.Fatalf("expected exactly one peer, got %+v", pol.Spec.Ingress)
	}
	if got := pol.Spec.Ingress[0].From[0].PodSelector.MatchLabels["app"]; got != "bi" {
		t.Errorf("wrong peer selector: %v", pol.Spec.Ingress[0].From[0])
	}
	if len(pol.Spec.Ingress[0].Ports) != 4 {
		t.Errorf("expected the four wire ports, got %d", len(pol.Spec.Ingress[0].Ports))
	}
}

// TestSparkConnectPortIsWired. The server always opens Spark Connect, so the
// port has to reach every place the other three listeners do: a port missing
// from the Service is unreachable, and one missing from the NetworkPolicy is
// silently blocked under a CNI that enforces it.
func TestSparkConnectPortIsWired(t *testing.T) {
	h := newHarness(t)
	h.server("spark")
	h.reconcile("spark")

	// Left unset by the harness, so this is also the CRD default at work.
	const want = 15002

	var inContainer bool
	for _, p := range h.statefulSet("spark").Spec.Template.Spec.Containers[0].Ports {
		if p.Name == SparkConnectPortName && p.ContainerPort == want {
			inContainer = true
		}
	}
	if !inContainer {
		t.Errorf("container does not declare %s/%d", SparkConnectPortName, want)
	}

	var svc corev1.Service
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: "default", Name: "spark"}, &svc); err != nil {
		t.Fatalf("getting service: %v", err)
	}
	var inService bool
	for _, p := range svc.Spec.Ports {
		if p.Name == SparkConnectPortName && p.Port == want {
			inService = true
		}
	}
	if !inService {
		t.Errorf("service does not expose %s/%d: %+v", SparkConnectPortName, want, svc.Spec.Ports)
	}

	srv := h.serverObj("spark")
	srv.Spec.Network.AllowIngressFrom = []v1alpha1.IngressPeer{
		{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "spark-client"}}},
	}
	if err := h.c.Update(h.ctx, srv); err != nil {
		t.Fatalf("updating server: %v", err)
	}
	h.reconcile("spark")

	var pol networkingv1.NetworkPolicy
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: "default", Name: "spark"}, &pol); err != nil {
		t.Fatalf("no NetworkPolicy: %v", err)
	}
	var inPolicy bool
	for _, p := range pol.Spec.Ingress[0].Ports {
		if p.Port != nil && p.Port.IntValue() == want {
			inPolicy = true
		}
	}
	if !inPolicy {
		t.Errorf("NetworkPolicy does not admit %d", want)
	}
}

// TestNetworkPolicyEnforcementIsNeverClaimed. The operator cannot tell whether
// the CNI implements NetworkPolicy, so it must not report the policy as
// protection. Unknown is the honest status.
func TestNetworkPolicyEnforcementIsNeverClaimed(t *testing.T) {
	h := newHarness(t)
	h.server("np3")
	h.reconcile("np3")

	var srv v1alpha1.OtterStaxServer
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: "default", Name: "np3"}, &srv); err != nil {
		t.Fatalf("getting server: %v", err)
	}
	cond := findCondition(srv.Status.Conditions, ConditionNetworkPolicyEnforced)
	if cond == nil {
		t.Fatal("no NetworkPolicyEnforced condition")
	}
	if cond.Status == metav1.ConditionTrue {
		t.Fatal("enforcement reported as True; the operator has no way to establish that")
	}
}

// TestRestartReasonIsClassified. "Why did the database restart at 3am" is asked
// after the fact, when the only evidence left is a pod with a new start time.
// The moment of the change is the only point where the answer exists.
func TestRestartReasonIsClassified(t *testing.T) {
	h := newHarness(t)
	h.server("why")
	conn := h.connection("c1", "why", "mysql", "mariadb")

	h.reconcile("why")
	if r := h.serverObj("why").Status.LastRestartReason; r == nil || r.Reason != v1alpha1.RestartInitialRollout {
		t.Fatalf("expected InitialRollout, got %+v", r)
	}

	// A config change.
	conn.Spec.MySQL.Host = "mariadb-2"
	if err := h.c.Update(h.ctx, conn); err != nil {
		t.Fatalf("updating connection: %v", err)
	}
	h.reconcile("why")
	r := h.serverObj("why").Status.LastRestartReason
	if r == nil || r.Reason != v1alpha1.RestartConfigChanged {
		t.Fatalf("expected ConfigChanged, got %+v", r)
	}
	if r.Detail == "" {
		t.Error("the record should say what changed")
	}

	// An image change must be distinguished from a config change: they have
	// different causes and different people to talk to.
	srv := h.serverObj("why")
	srv.Spec.Image = "ghcr.io/otterstax/otterstax:v2"
	if err := h.c.Update(h.ctx, srv); err != nil {
		t.Fatalf("updating server: %v", err)
	}
	h.reconcile("why")
	if r := h.serverObj("why").Status.LastRestartReason; r == nil || r.Reason != v1alpha1.RestartImageChanged {
		t.Fatalf("expected ImageChanged, got %+v", r)
	}
}

// TestManualRestartAnnotationRolls. Twenty lines, and the feature people ask
// for first: force a restart without editing anything meaningful.
func TestManualRestartAnnotationRolls(t *testing.T) {
	h := newHarness(t)
	h.server("kick")
	h.reconcile("kick")
	before := h.statefulSet("kick").Spec.Template.Annotations[v1alpha1.RestartAnnotation]

	srv := h.serverObj("kick")
	if srv.Annotations == nil {
		srv.Annotations = map[string]string{}
	}
	srv.Annotations[v1alpha1.RestartAnnotation] = "2026-08-15T12:00:00Z"
	if err := h.c.Update(h.ctx, srv); err != nil {
		t.Fatalf("annotating server: %v", err)
	}
	h.reconcile("kick")

	after := h.statefulSet("kick").Spec.Template.Annotations[v1alpha1.RestartAnnotation]
	if after == before {
		t.Fatal("the restart annotation did not reach the pod template")
	}
	if r := h.serverObj("kick").Status.LastRestartReason; r == nil || r.Reason != v1alpha1.RestartManual {
		t.Fatalf("expected ManualRestart, got %+v", r)
	}
}

// TestStoppedScalesToZeroAndKeepsEverythingElse. The difference between "take
// this out of service" and "delete it and hope the config is in Git".
func TestStoppedScalesToZeroAndKeepsEverythingElse(t *testing.T) {
	h := newHarness(t)
	h.server("halt")
	h.connection("c1", "halt", "mysql", "mariadb")
	h.reconcile("halt")

	secretBefore := h.statefulSet("halt").Spec.Template.Spec.Volumes[0].Secret.SecretName

	srv := h.serverObj("halt")
	srv.Spec.ClusterOperation.Stopped = true
	if err := h.c.Update(h.ctx, srv); err != nil {
		t.Fatalf("stopping: %v", err)
	}
	h.reconcile("halt")

	sts := h.statefulSet("halt")
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0 {
		t.Fatalf("expected replicas 0, got %v", sts.Spec.Replicas)
	}
	if got := sts.Spec.Template.Spec.Volumes[0].Secret.SecretName; got != secretBefore {
		t.Errorf("the config revision changed while stopped: %s -> %s", secretBefore, got)
	}
	// Not available, but not on its way to being so either: Progressing would
	// send someone looking for a stuck rollout that does not exist.
	if c := findCondition(h.serverObj("halt").Status.Conditions, ConditionAvailable); c == nil ||
		c.Status != metav1.ConditionFalse || c.Reason != "Stopped" {
		t.Errorf("expected Available=False/Stopped while stopped, got %+v", c)
	}

	// And it comes back with one field.
	srv = h.serverObj("halt")
	srv.Spec.ClusterOperation.Stopped = false
	if err := h.c.Update(h.ctx, srv); err != nil {
		t.Fatalf("starting: %v", err)
	}
	h.reconcile("halt")
	if got := *h.statefulSet("halt").Spec.Replicas; got != 1 {
		t.Fatalf("expected replicas 1 after restarting, got %d", got)
	}
}

// TestPausedControllerWritesNothing. The escape hatch for an operator that is
// making things worse: hold this object still without stopping the controller
// for every object at once.
func TestPausedControllerWritesNothing(t *testing.T) {
	h := newHarness(t)
	h.server("frozen")
	conn := h.connection("c1", "frozen", "mysql", "mariadb")
	h.reconcile("frozen")
	pinned := h.statefulSet("frozen").Spec.Template.Annotations[ConfigHashAnnotation]

	srv := h.serverObj("frozen")
	srv.Spec.ClusterOperation.ReconciliationPaused = true
	if err := h.c.Update(h.ctx, srv); err != nil {
		t.Fatalf("pausing: %v", err)
	}

	// A change that would normally roll the pod.
	conn.Spec.MySQL.Host = "somewhere-else"
	if err := h.c.Update(h.ctx, conn); err != nil {
		t.Fatalf("updating connection: %v", err)
	}
	h.reconcile("frozen")

	if got := h.statefulSet("frozen").Spec.Template.Annotations[ConfigHashAnnotation]; got != pinned {
		t.Fatalf("the controller wrote while paused: %s -> %s", pinned, got)
	}
	// But it must still report, or pausing would look identical to a crash.
	if c := findCondition(h.serverObj("frozen").Status.Conditions, ConditionConfigRendered); c == nil ||
		c.Reason != "ReconciliationPaused" {
		t.Fatalf("paused state not reported: %+v", c)
	}
}

// TestRestartReasonSurvivesAConcurrentWrite.
//
// Found on a live cluster, not here: annotating the object with kubectl while
// the controller held a cached copy made the status write lose a conflict. The
// consequence is not a delayed update — the classification is computed at the
// only moment it is knowable, and by the next pass the StatefulSet already
// carries the change, so there is nothing left to compare. The reason was gone
// for good.
func TestRestartReasonSurvivesAConcurrentWrite(t *testing.T) {
	h := newHarness(t)
	h.server("race")
	h.reconcile("race")

	// Take a stale copy, the way a controller's cache holds one.
	stale := h.serverObj("race")

	// Somebody else writes to the object in between.
	fresh := h.serverObj("race")
	if fresh.Annotations == nil {
		fresh.Annotations = map[string]string{}
	}
	fresh.Annotations["someone-else"] = "was-here"
	if err := h.c.Update(h.ctx, fresh); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}

	// Now write status from the stale object exactly as the reconciler does:
	// the patch base is a snapshot of the same stale object, so the computed
	// diff carries no resourceVersion and therefore no precondition. Building
	// the base from a fresh Get instead would reintroduce the conflict — that
	// is the subtlety this test pins.
	base := stale.DeepCopy()
	stale.Status.LastRestartReason = &v1alpha1.RestartRecord{
		Reason: v1alpha1.RestartManual,
		Time:   metav1.Now(),
		Detail: "written from a stale copy",
	}
	if err := h.c.Status().Patch(h.ctx, stale, client.MergeFrom(base)); err != nil {
		t.Fatalf("status patch lost to a concurrent write: %v", err)
	}

	got := h.serverObj("race")
	if got.Status.LastRestartReason == nil || got.Status.LastRestartReason.Reason != v1alpha1.RestartManual {
		t.Fatalf("the restart reason did not survive: %+v", got.Status.LastRestartReason)
	}
}

// TestContainerRunsTheServerBinary. Args alone replace the image's CMD, and the
// upstream image has no ENTRYPOINT: the kubelet would exec "--config" and the
// container would never start. Found only against a real server image.
func TestContainerRunsTheServerBinary(t *testing.T) {
	h := newHarness(t)
	h.server("bin")
	h.reconcile("bin")

	c := h.statefulSet("bin").Spec.Template.Spec.Containers[0]
	if len(c.Command) == 0 || c.Command[0] != "./server" {
		t.Fatalf("command must name the server binary, got %q with args %q", c.Command, c.Args)
	}
}

// TestPodChangesReachTheServer. A crash changes the pod and nothing else: the
// StatefulSet's ready count stays at zero, so without a pod watch Degraded keeps
// saying "serving" for a container that has never started.
func TestPodChangesReachTheServer(t *testing.T) {
	r := &OtterStaxServerReconciler{}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "live", Name: "demo-0",
		Labels: map[string]string{NameLabel: "otterstax", InstanceLabel: "demo"},
	}}
	reqs := r.serverForPod(context.Background(), pod)
	if len(reqs) != 1 || reqs[0].Namespace != "live" || reqs[0].Name != "demo" {
		t.Fatalf("expected a request for live/demo, got %v", reqs)
	}

	stranger := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "live", Name: "x", Labels: map[string]string{InstanceLabel: "demo"}}}
	if reqs := r.serverForPod(context.Background(), stranger); len(reqs) != 0 {
		t.Errorf("a pod not labelled as an otterstax server must not enqueue anything, got %v", reqs)
	}
}

// TestBrokenRolloutIsReplaceable. Under OrderedReady the StatefulSet controller
// will not replace a pod that is not Running and Ready, so a bad image or config
// leaves the pod crash-looping on the old revision even after the spec is fixed,
// until someone deletes it by hand. With one replica ordering means nothing, and
// Parallel lets the fix roll out. Seen on kind 1.36 against a real server image.
func TestBrokenRolloutIsReplaceable(t *testing.T) {
	h := newHarness(t)
	h.server("fixable")
	h.reconcile("fixable")

	if p := h.statefulSet("fixable").Spec.PodManagementPolicy; p != appsv1.ParallelPodManagement {
		t.Fatalf("podManagementPolicy is %s; a crash-looping pod would block every later rollout", p)
	}
}
