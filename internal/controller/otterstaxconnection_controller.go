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
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/otterstax/otterstax-operator/api/v1alpha1"
)

// Condition types on a connection.
const (
	// ConditionAccepted says the object is well-formed. Its reason carries the
	// connector kind, which is what `kubectl get osconn` shows.
	ConditionAccepted = "Accepted"

	// ConditionBound says at least one server currently claims this connection.
	ConditionBound = "Bound"
)

const reasonOrphaned = "NoServerSelectsIt"

// OtterStaxConnectionReconciler reports which servers claim a connection.
//
// It owns nothing and creates nothing. Its entire job is to answer a question
// the user would otherwise have to answer by reading label selectors by hand:
// is this object actually being used, and by whom. A connection with a typo in
// its labels is otherwise indistinguishable from one that works — it simply
// never appears in any config, and nothing says so.
type OtterStaxConnectionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=otterstax.com,resources=otterstaxconnections,verbs=get;list;watch
// +kubebuilder:rbac:groups=otterstax.com,resources=otterstaxconnections/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=otterstax.com,resources=otterstaxservers,verbs=get;list;watch

func (r *OtterStaxConnectionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var conn v1alpha1.OtterStaxConnection
	if err := r.Get(ctx, req.NamespacedName, &conn); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Status goes out as a merge patch against this snapshot, as on the server:
	// an Update carries the resourceVersion and loses to any concurrent write.
	base := conn.DeepCopy()

	kind, err := connectorKind(&conn)
	if err != nil {
		// Should be unreachable: CEL enforces exactly one connector at admission.
		// Reported rather than ignored, because if it ever is reachable the
		// alternative is a connection that silently never renders.
		setCondition(&conn.Status.Conditions, metav1.Condition{
			Type: ConditionAccepted, Status: metav1.ConditionFalse,
			Reason: "InvalidConnector", Message: err.Error(),
			ObservedGeneration: conn.Generation,
		})
		return ctrl.Result{}, r.Status().Patch(ctx, &conn, client.MergeFrom(base))
	}

	bound, err := r.serversClaiming(ctx, &conn)
	if err != nil {
		return ctrl.Result{}, err
	}

	conn.Status.ObservedGeneration = conn.Generation
	conn.Status.BoundServers = bound

	setCondition(&conn.Status.Conditions, metav1.Condition{
		Type: ConditionAccepted, Status: metav1.ConditionTrue,
		Reason: kind, Message: fmt.Sprintf("alias %q", conn.Spec.Alias),
		ObservedGeneration: conn.Generation,
	})

	boundCond := metav1.Condition{
		Type: ConditionBound, Status: metav1.ConditionTrue,
		Reason:             "Claimed",
		Message:            fmt.Sprintf("claimed by %d server(s): %v", len(bound), bound),
		ObservedGeneration: conn.Generation,
	}
	if len(bound) == 0 {
		boundCond = metav1.Condition{
			Type: ConditionBound, Status: metav1.ConditionFalse,
			Reason: reasonOrphaned,
			Message: "no OtterStaxServer in this namespace selects it, so it is in no config and has " +
				"no effect; check the labels against the server's connectionSelector",
			ObservedGeneration: conn.Generation,
		}
	}
	setCondition(&conn.Status.Conditions, boundCond)

	return ctrl.Result{}, r.Status().Patch(ctx, &conn, client.MergeFrom(base))
}

// connectorKind returns the single connector set on a connection.
func connectorKind(c *v1alpha1.OtterStaxConnection) (string, error) {
	var kinds []string
	if c.Spec.MySQL != nil {
		kinds = append(kinds, "MySQL")
	}
	if c.Spec.PostgreSQL != nil {
		kinds = append(kinds, "PostgreSQL")
	}
	if c.Spec.ClickHouse != nil {
		kinds = append(kinds, "ClickHouse")
	}
	if c.Spec.S3 != nil {
		kinds = append(kinds, "S3")
	}
	switch len(kinds) {
	case 1:
		return kinds[0], nil
	case 0:
		return "", fmt.Errorf("no connector set")
	default:
		return "", fmt.Errorf("%d connectors set: %v", len(kinds), kinds)
	}
}

// serversClaiming lists, in a stable order, the servers whose selector matches.
func (r *OtterStaxConnectionReconciler) serversClaiming(ctx context.Context, conn *v1alpha1.OtterStaxConnection) ([]string, error) {
	var servers v1alpha1.OtterStaxServerList
	if err := r.List(ctx, &servers, client.InNamespace(conn.Namespace)); err != nil {
		return nil, err
	}

	var out []string
	for i := range servers.Items {
		s := &servers.Items[i]
		if s.Spec.ConnectionSelector == nil {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(s.Spec.ConnectionSelector)
		if err != nil {
			continue
		}
		if sel.Matches(labels.Set(conn.Labels)) {
			out = append(out, s.Name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// SetupWithManager watches servers as well as connections: a change to a
// server's selector can adopt or orphan a connection without the connection
// itself being touched.
func (r *OtterStaxConnectionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.OtterStaxConnection{}).
		Watches(
			&v1alpha1.OtterStaxServer{},
			handler.EnqueueRequestsFromMapFunc(r.connectionsInNamespace),
		).
		Named("otterstaxconnection").
		Complete(r)
}

func (r *OtterStaxConnectionReconciler) connectionsInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	var list v1alpha1.OtterStaxConnectionList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, reconcile.Request{
			NamespacedName: client.ObjectKey{Namespace: list.Items[i].Namespace, Name: list.Items[i].Name},
		})
	}
	return out
}
