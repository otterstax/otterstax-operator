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
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/otterstax/otterstax-operator/api/v1alpha1"
	"github.com/otterstax/otterstax-operator/internal/render"
)

// keptRevisions is how many superseded config Secrets survive.
//
// They exist so a rollback has something to roll back to. The mounted revision
// is never counted against this budget: deleting it would leave a healthy
// running pod unable to restart, which is a worse failure than keeping a few
// stale objects.
const keptRevisions = 3

// Condition types.
const (
	// ConditionConfigRendered says the desired config could be produced at all.
	// It is False when a connection is malformed or two connections claim the
	// same alias — cases where rendering something anyway would produce a file
	// that parses and misbehaves.
	ConditionConfigRendered = "ConfigRendered"

	// ConditionAvailable follows the StatefulSet.
	ConditionAvailable = "Available"
)

// OtterStaxServerReconciler reconciles an OtterStaxServer.
type OtterStaxServerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=otterstax.com,resources=otterstaxservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=otterstax.com,resources=otterstaxservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=otterstax.com,resources=otterstaxservers/finalizers,verbs=update
// +kubebuilder:rbac:groups=otterstax.com,resources=otterstaxconnections,verbs=get;list;watch
// +kubebuilder:rbac:groups=otterstax.com,resources=otterstaxconnections/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives one server towards its spec.
//
// The shape is: gather the connections, render the config, address it by its
// hash, and let the hash decide whether anything restarts. Every change is a
// restart because the server reads its configuration once and has no reload
// path — so the whole job is making sure a restart happens exactly when the
// bytes change, and never otherwise.
func (r *OtterStaxServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var server v1alpha1.OtterStaxServer
	if err := r.Get(ctx, req.NamespacedName, &server); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Snapshot for the status patch. Status is written as a merge patch against
	// this, not as an Update: an Update carries a resourceVersion and loses to
	// any concurrent write to the object — including a plain kubectl annotate.
	// That matters here beyond the retry, because the restart classification is
	// computed at the only moment it is knowable. A conflict on the write does
	// not merely delay it, it destroys it: by the next pass the StatefulSet
	// already carries the change and there is nothing left to compare.
	base := server.DeepCopy()

	if server.Spec.ClusterOperation.ReconciliationPaused {
		// Read and report, write nothing. The point is to let someone stop this
		// object's controller without stopping it for every object at once.
		l.Info("reconciliation paused by spec.clusterOperation")
		return ctrl.Result{}, r.updateStatus(ctx, &server, base, "", metav1.Condition{
			Type: ConditionConfigRendered, Status: metav1.ConditionUnknown,
			Reason:  "ReconciliationPaused",
			Message: "spec.clusterOperation.reconciliationPaused is set; the controller is reporting but not writing",
		})
	}

	conns, err := r.connectionsFor(ctx, &server)
	if err != nil {
		return ctrl.Result{}, err
	}

	config, renderErr := render.Render(render.Input{
		Service:         server.Spec.Service,
		ConnectionRetry: server.Spec.ConnectionRetry,
		Connections:     conns,
	})
	if renderErr != nil {
		// The live pod is deliberately left alone. A server that is serving
		// correctly from the previous config keeps serving; replacing it with
		// one built from a config we just judged unsound would turn a reported
		// problem into an outage.
		l.Info("refusing to render", "reason", renderErr.Error())
		meta := metav1.Condition{
			Type:    ConditionConfigRendered,
			Status:  metav1.ConditionFalse,
			Reason:  "InvalidConnections",
			Message: renderErr.Error(),
		}
		return ctrl.Result{}, r.updateStatus(ctx, &server, base, "", meta)
	}

	hash := ConfigHash(config)

	if err := r.ensureConfigSecret(ctx, &server, config, hash); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureServices(ctx, &server); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureNetworkPolicy(ctx, &server, conns); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureStatefulSet(ctx, &server, hash); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.collectOldRevisions(ctx, &server, hash); err != nil {
		// Not fatal: stale Secrets cost nothing but clutter, and failing the
		// whole reconcile over them would block real work.
		l.Error(err, "collecting superseded config revisions")
	}

	return ctrl.Result{}, r.updateStatus(ctx, &server, base, hash,
		metav1.Condition{
			Type:    ConditionConfigRendered,
			Status:  metav1.ConditionTrue,
			Reason:  "Rendered",
			Message: fmt.Sprintf("%d connection(s)", len(conns)),
		},
		networkPolicyCondition(&server),
	)
}

// connectionsFor returns the connections this server claims, in a stable order.
//
// A nil selector matches nothing. That is the safe reading: a controller that
// adopted every connection in the namespace would make creating an unrelated
// object a live change to an unrelated server.
func (r *OtterStaxServerReconciler) connectionsFor(ctx context.Context, server *v1alpha1.OtterStaxServer) ([]v1alpha1.OtterStaxConnection, error) {
	if server.Spec.ConnectionSelector == nil {
		return nil, nil
	}
	sel, err := metav1.LabelSelectorAsSelector(server.Spec.ConnectionSelector)
	if err != nil {
		return nil, fmt.Errorf("connectionSelector: %w", err)
	}

	var list v1alpha1.OtterStaxConnectionList
	if err := r.List(ctx, &list, client.InNamespace(server.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}

	// List order is not guaranteed, and the rendered config must be a pure
	// function of its inputs. Sorting by name makes the order an explicit
	// property of the objects rather than an accident of the API server.
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return list.Items, nil
}

// ensureConfigSecret creates the revision if it does not exist.
//
// It is never updated: the name is derived from the contents, so a Secret with
// this name already holds exactly these bytes. That is what makes rollback work
// — every revision that ever existed is still addressable by its hash.
func (r *OtterStaxServerReconciler) ensureConfigSecret(ctx context.Context, server *v1alpha1.OtterStaxServer, config []byte, hash string) error {
	desired := BuildConfigSecret(server, config, hash)
	if err := controllerutil.SetControllerReference(server, desired, r.Scheme); err != nil {
		return err
	}

	var existing corev1.Secret
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	switch {
	case apierrors.IsNotFound(err):
		return r.Create(ctx, desired)
	case err != nil:
		return err
	default:
		return nil
	}
}

func (r *OtterStaxServerReconciler) ensureServices(ctx context.Context, server *v1alpha1.OtterStaxServer) error {
	headless, clusterIP := BuildServices(server)
	for _, svc := range []*corev1.Service{headless, clusterIP} {
		if err := controllerutil.SetControllerReference(server, svc, r.Scheme); err != nil {
			return err
		}
		var existing corev1.Service
		err := r.Get(ctx, client.ObjectKeyFromObject(svc), &existing)
		switch {
		case apierrors.IsNotFound(err):
			if err := r.Create(ctx, svc); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			// ClusterIP is assigned by the API server and is immutable; carry it
			// across rather than fighting over it.
			svc.Spec.ClusterIP = existing.Spec.ClusterIP
			svc.ResourceVersion = existing.ResourceVersion
			if err := r.Update(ctx, svc); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureNetworkPolicy generates the only access control this service has.
func (r *OtterStaxServerReconciler) ensureNetworkPolicy(ctx context.Context, server *v1alpha1.OtterStaxServer, conns []v1alpha1.OtterStaxConnection) error {
	desired := BuildNetworkPolicy(server, conns)
	if desired == nil {
		return nil
	}
	if err := controllerutil.SetControllerReference(server, desired, r.Scheme); err != nil {
		return err
	}

	var existing networkingv1.NetworkPolicy
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	switch {
	case apierrors.IsNotFound(err):
		return r.Create(ctx, desired)
	case err != nil:
		return err
	default:
		desired.ResourceVersion = existing.ResourceVersion
		return r.Update(ctx, desired)
	}
}

func (r *OtterStaxServerReconciler) ensureStatefulSet(ctx context.Context, server *v1alpha1.OtterStaxServer, hash string) error {
	desired, err := BuildStatefulSet(server, hash)
	if err != nil {
		return err
	}
	if err := controllerutil.SetControllerReference(server, desired, r.Scheme); err != nil {
		return err
	}

	var existing appsv1.StatefulSet
	err = r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	switch {
	case apierrors.IsNotFound(err):
		r.recordRestart(server, v1alpha1.RestartInitialRollout, "first rollout")
		return r.Create(ctx, desired)
	case err != nil:
		return err
	default:
		// A StatefulSet's selector is immutable; changing it would need a
		// delete and recreate, which for this workload means downtime. Keep the
		// existing one and let everything else be updated.
		desired.Spec.Selector = existing.Spec.Selector
		desired.ResourceVersion = existing.ResourceVersion

		if reason, detail, rolls := classifyRoll(&existing, desired); rolls {
			r.recordRestart(server, reason, detail)
		}
		return r.Update(ctx, desired)
	}
}

// classifyRoll answers "why did it restart" before the restart happens.
//
// The question gets asked after an incident, when the only evidence left is a
// pod with a new start time. Classifying at the moment of the change is the
// only point where the answer is actually available.
func classifyRoll(existing, desired *appsv1.StatefulSet) (v1alpha1.RestartReason, string, bool) {
	oldTpl, newTpl := existing.Spec.Template, desired.Spec.Template

	if oldTpl.Annotations[v1alpha1.RestartAnnotation] != newTpl.Annotations[v1alpha1.RestartAnnotation] {
		return v1alpha1.RestartManual, fmt.Sprintf("%s annotation set to %q",
			v1alpha1.RestartAnnotation, newTpl.Annotations[v1alpha1.RestartAnnotation]), true
	}

	oldImg, newImg := containerImage(&oldTpl), containerImage(&newTpl)
	if oldImg != newImg {
		return v1alpha1.RestartImageChanged, fmt.Sprintf("%s -> %s", oldImg, newImg), true
	}

	oldHash := oldTpl.Annotations[ConfigHashAnnotation]
	newHash := newTpl.Annotations[ConfigHashAnnotation]
	if oldHash != newHash {
		return v1alpha1.RestartConfigChanged, fmt.Sprintf("config %s -> %s",
			shortHash(oldHash), shortHash(newHash)), true
	}
	return "", "", false
}

func containerImage(tpl *corev1.PodTemplateSpec) string {
	for i := range tpl.Spec.Containers {
		if tpl.Spec.Containers[i].Name == "otterstax" {
			return tpl.Spec.Containers[i].Image
		}
	}
	return ""
}

func shortHash(h string) string {
	if len(h) > 10 {
		return h[:10]
	}
	return h
}

// recordRestart stashes the classification for updateStatus to persist, and
// emits an Event so it also shows up in kubectl describe — which is where
// people actually look first.
func (r *OtterStaxServerReconciler) recordRestart(server *v1alpha1.OtterStaxServer, reason v1alpha1.RestartReason, detail string) {
	server.Status.LastRestartReason = &v1alpha1.RestartRecord{
		Reason: reason,
		Time:   metav1.Now(),
		Detail: detail,
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(server, corev1.EventTypeNormal, string(reason), "rolling the workload: %s", detail)
	}
}

// collectOldRevisions deletes superseded config Secrets, keeping a few for
// rollback and never touching the one currently in use.
func (r *OtterStaxServerReconciler) collectOldRevisions(ctx context.Context, server *v1alpha1.OtterStaxServer, currentHash string) error {
	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets,
		client.InNamespace(server.Namespace),
		client.MatchingLabels{InstanceLabel: server.Name},
	); err != nil {
		return err
	}

	// Whatever the live pod template references is off limits, even if it is
	// not the revision we just rendered — a rollout can be mid-flight, and
	// deleting the mounted Secret would leave a healthy pod unable to restart.
	mounted := map[string]struct{}{ConfigSecretName(server.Name, currentHash): {}}
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, client.ObjectKey{Namespace: server.Namespace, Name: server.Name}, &sts); err == nil {
		for _, v := range sts.Spec.Template.Spec.Volumes {
			if v.Secret != nil {
				mounted[v.Secret.SecretName] = struct{}{}
			}
		}
	}

	candidates := make([]corev1.Secret, 0, len(secrets.Items))
	for _, s := range secrets.Items {
		if _, ok := s.Labels[ConfigRevisionLabel]; !ok {
			continue
		}
		if _, inUse := mounted[s.Name]; inUse {
			continue
		}
		candidates = append(candidates, s)
	}

	// Newest first, so the ones kept are the most plausible rollback targets.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[j].CreationTimestamp.Before(&candidates[i].CreationTimestamp)
	})

	for i := keptRevisions; i < len(candidates); i++ {
		if err := r.Delete(ctx, &candidates[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// updateStatus records what was rendered and what is actually running.
//
// The two hashes are reported separately on purpose. Every change is a restart,
// so a window where they differ is normal; a window that does not close is a
// stuck rollout, and nothing else in the system would say so.
func (r *OtterStaxServerReconciler) updateStatus(ctx context.Context, server, base *v1alpha1.OtterStaxServer, renderedHash string, conds ...metav1.Condition) error {
	server.Status.ObservedGeneration = server.Generation
	if renderedHash != "" {
		server.Status.RenderedConfigHash = renderedHash
	}

	var sts appsv1.StatefulSet
	if err := r.Get(ctx, client.ObjectKey{Namespace: server.Namespace, Name: server.Name}, &sts); err == nil {
		server.Status.RunningConfigHash = sts.Spec.Template.Annotations[ConfigHashAnnotation]

		available := metav1.ConditionFalse
		reason := "Progressing"
		switch {
		case server.Spec.ClusterOperation.Stopped:
			reason = "Stopped"
		case sts.Status.ReadyReplicas > 0 && sts.Status.UpdatedReplicas > 0:
			available = metav1.ConditionTrue
			reason = "Ready"
		}
		conds = append(conds, r.diagnose(ctx, server.Namespace, server.Name))
		conds = append(conds, metav1.Condition{
			Type:   ConditionAvailable,
			Status: available,
			Reason: reason,
			Message: fmt.Sprintf("%d/%d ready, %d updated",
				sts.Status.ReadyReplicas, sts.Status.Replicas, sts.Status.UpdatedReplicas),
		})
	}

	for _, c := range conds {
		c.ObservedGeneration = server.Generation
		setCondition(&server.Status.Conditions, c)
	}
	return r.Status().Patch(ctx, server, client.MergeFrom(base))
}

// networkPolicyCondition never claims the policy works.
//
// A NetworkPolicy is accepted by every API server and enforced only by a CNI
// that implements one. The operator cannot tell the difference, and for a
// service that authenticates nobody, reporting an unenforced policy as
// protection would be worse than reporting nothing.
func networkPolicyCondition(server *v1alpha1.OtterStaxServer) metav1.Condition {
	if server.Spec.Network.Policy == v1alpha1.NetworkPolicyNone {
		return metav1.Condition{
			Type: ConditionNetworkPolicyEnforced, Status: metav1.ConditionFalse,
			Reason: ReasonPolicyDisabled,
			Message: "no NetworkPolicy generated by request; no wire protocol authenticates, " +
				"so anything able to open a TCP connection can read and write every " +
				"federated backend",
		}
	}
	return metav1.Condition{
		Type: ConditionNetworkPolicyEnforced, Status: metav1.ConditionUnknown,
		Reason: ReasonEnforcementUnverified,
		Message: fmt.Sprintf("NetworkPolicy generated with %d permitted ingress peer(s); whether it is "+
			"enforced depends on the cluster's CNI, which the operator cannot determine — a CNI that "+
			"does not implement NetworkPolicy accepts the object and blocks nothing",
			len(server.Spec.Network.AllowIngressFrom)),
	}
}

func setCondition(conds *[]metav1.Condition, c metav1.Condition) {
	if c.LastTransitionTime.IsZero() {
		c.LastTransitionTime = metav1.Now()
	}
	for i := range *conds {
		if (*conds)[i].Type != c.Type {
			continue
		}
		// Keep the original transition time when only the message moved, or
		// "how long has it been broken" becomes unanswerable.
		if (*conds)[i].Status == c.Status {
			c.LastTransitionTime = (*conds)[i].LastTransitionTime
		}
		(*conds)[i] = c
		return
	}
	*conds = append(*conds, c)
}

// SetupWithManager wires the watches.
//
// Watching connections is not optional. Without it a connection created or
// edited after the server would sit there doing nothing until some unrelated
// event happened to trigger a reconcile — which is indistinguishable from the
// operator being broken, and is exactly the bug users report.
func (r *OtterStaxServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.OtterStaxServer{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Watches(
			&v1alpha1.OtterStaxConnection{},
			handler.EnqueueRequestsFromMapFunc(r.serversForConnection),
		).
		Named("otterstaxserver").
		Complete(r)
}

// serversForConnection maps a changed connection back to every server whose
// selector currently claims it.
func (r *OtterStaxServerReconciler) serversForConnection(ctx context.Context, obj client.Object) []reconcile.Request {
	conn, ok := obj.(*v1alpha1.OtterStaxConnection)
	if !ok {
		return nil
	}

	var servers v1alpha1.OtterStaxServerList
	if err := r.List(ctx, &servers, client.InNamespace(conn.Namespace)); err != nil {
		return nil
	}

	var out []reconcile.Request
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
			out = append(out, reconcile.Request{
				NamespacedName: client.ObjectKey{Namespace: s.Namespace, Name: s.Name},
			})
		}
	}
	return out
}
