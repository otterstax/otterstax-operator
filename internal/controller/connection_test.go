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
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/otterstax/otterstax-operator/api/v1alpha1"
)

func (h *harness) reconcileConn(name string) {
	h.t.Helper()
	cr := &OtterStaxConnectionReconciler{Client: h.c, Scheme: h.r.Scheme}
	if _, err := cr.Reconcile(h.ctx, ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: "default", Name: name},
	}); err != nil {
		h.t.Fatalf("Reconcile: %v", err)
	}
}

func (h *harness) conn(name string) *v1alpha1.OtterStaxConnection {
	h.t.Helper()
	var c v1alpha1.OtterStaxConnection
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: "default", Name: name}, &c); err != nil {
		h.t.Fatalf("getting connection: %v", err)
	}
	return &c
}

// TestConnectionReportsWhoClaimsIt. Without this the only way to answer "is
// this connection actually in use" is to read label selectors by hand.
func TestConnectionReportsWhoClaimsIt(t *testing.T) {
	h := newHarness(t)
	h.server("srv")
	h.connection("c1", "srv", "mysql", "mariadb")

	h.reconcileConn("c1")
	got := h.conn("c1")

	if len(got.Status.BoundServers) != 1 || got.Status.BoundServers[0] != "srv" {
		t.Fatalf("expected boundServers [srv], got %v", got.Status.BoundServers)
	}
	if c := findCondition(got.Status.Conditions, ConditionBound); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("expected Bound=True, got %+v", c)
	}
	// The Accepted reason is what kubectl shows in the Kind column.
	if c := findCondition(got.Status.Conditions, ConditionAccepted); c == nil || c.Reason != "MySQL" {
		t.Fatalf("expected Accepted reason MySQL, got %+v", c)
	}
}

// TestOrphanedConnectionSaysSo.
//
// A connection whose labels do not match any server is silently absent from
// every config: it is not an error anywhere, it simply has no effect. That is
// the single most likely user mistake with a selector-based model, and the only
// place it can be reported is here.
func TestOrphanedConnectionSaysSo(t *testing.T) {
	h := newHarness(t)
	h.server("srv")

	// Deliberate typo in the label the server selects on.
	orphan := &v1alpha1.OtterStaxConnection{
		ObjectMeta: metav1.ObjectMeta{
			Name: "typo", Namespace: "default",
			Labels: map[string]string{"servers": "srv"},
		},
		Spec: v1alpha1.OtterStaxConnectionSpec{
			Alias: "lonely",
			MySQL: &v1alpha1.SQLBackend{Host: "h", Username: "u", Database: "d"},
		},
	}
	if err := h.c.Create(h.ctx, orphan); err != nil {
		t.Fatalf("creating connection: %v", err)
	}

	h.reconcileConn("typo")
	got := h.conn("typo")

	if len(got.Status.BoundServers) != 0 {
		t.Fatalf("expected no bound servers, got %v", got.Status.BoundServers)
	}
	c := findCondition(got.Status.Conditions, ConditionBound)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonOrphaned {
		t.Fatalf("expected Bound=False/%s, got %+v", reasonOrphaned, c)
	}

	// And it must be actionable, not merely negative.
	if c.Message == "" || len(c.Message) < 40 {
		t.Errorf("the message should say what to check; got %q", c.Message)
	}
}

// TestConnectionRebindsWhenTheServerSelectorChanges. Editing a server can adopt
// or orphan a connection without touching the connection, which is why the
// connection controller watches servers too.
func TestConnectionRebindsWhenTheServerSelectorChanges(t *testing.T) {
	h := newHarness(t)
	srv := h.server("srv")
	h.connection("c1", "srv", "mysql", "mariadb")

	h.reconcileConn("c1")
	if n := len(h.conn("c1").Status.BoundServers); n != 1 {
		t.Fatalf("expected 1 bound server, got %d", n)
	}

	// Narrow the selector so it no longer matches.
	srv.Spec.ConnectionSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"server": "somebody-else"},
	}
	if err := h.c.Update(h.ctx, srv); err != nil {
		t.Fatalf("updating server: %v", err)
	}

	h.reconcileConn("c1")
	got := h.conn("c1")
	if len(got.Status.BoundServers) != 0 {
		t.Fatalf("connection still reports itself bound: %v", got.Status.BoundServers)
	}
	if c := findCondition(got.Status.Conditions, ConditionBound); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("expected Bound=False after the selector narrowed, got %+v", c)
	}
}

// TestConnectionStatusSurvivesAConcurrentWrite. Somebody else writes to the
// connection between the reconciler's read and its status write — a kubectl
// label is enough. An Update carries the stale resourceVersion and loses; a
// merge patch built against the reconciler's own snapshot does not.
func TestConnectionStatusSurvivesAConcurrentWrite(t *testing.T) {
	h := newHarness(t)
	h.server("srv")
	h.connection("c1", "srv", "mysql", "mariadb")

	interfere := func(ctx context.Context, c client.Client, obj client.Object) {
		var fresh v1alpha1.OtterStaxConnection
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), &fresh); err != nil {
			t.Fatalf("concurrent read: %v", err)
		}
		fresh.Annotations = map[string]string{"someone-else": "was-here"}
		if err := c.Update(ctx, &fresh); err != nil {
			t.Fatalf("concurrent write: %v", err)
		}
	}
	racy := interceptor.NewClient(watchless{h.c}, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			interfere(ctx, c, obj)
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			interfere(ctx, c, obj)
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})

	cr := &OtterStaxConnectionReconciler{Client: racy, Scheme: h.r.Scheme}
	if _, err := cr.Reconcile(h.ctx, ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: "default", Name: "c1"},
	}); err != nil {
		t.Fatalf("status write lost to a concurrent write: %v", err)
	}
	if c := findCondition(h.conn("c1").Status.Conditions, ConditionBound); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("expected Bound=True, got %+v", c)
	}
}

// watchless lets the plain test client stand in for the client.WithWatch the
// interceptor wants. Nothing here watches.
type watchless struct{ client.Client }

func (watchless) Watch(context.Context, client.ObjectList, ...client.ListOption) (watch.Interface, error) {
	return nil, errors.New("watch is not used in this test")
}
