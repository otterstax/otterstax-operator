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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The server has three documented failure modes and they are not equally
// visible. This file is about turning what can be observed into something a
// person can act on, and being explicit about what cannot be observed at all.
//
//  1. Invalid config — the parser throws and the process exits 1. Loud: the pod
//     crash-loops. Detectable from the pod's terminated state.
//
//  2. Valid config, backend unreachable — retried, then logged as ERROR, and
//     startup CONTINUES. The pod goes Ready with zero working backends. There
//     is no metric, no admin API and no health endpoint, and the server's own
//     log level is compiled to ERROR, so even the log line is unreliable.
//     NOT detectable. Saying so is better than implying otherwise.
//
//  3. Config file missing — the server warns (invisibly), applies defaults and
//     serves with zero connections. Prevented rather than detected: the config
//     volume is mounted with optional=false, so the kubelet refuses to start
//     the pod at all. That converts a silent misconfiguration into a visible
//     FailedMount.
const (
	// ConditionDegraded reports a server that is running but not healthy, or
	// one whose health cannot be established.
	ConditionDegraded = "Degraded"

	// ReasonConfigRejected: the process exited non-zero in a way consistent
	// with the config parser refusing the file.
	ReasonConfigRejected = "ConfigRejected"

	// ReasonCrashLooping: the container is restarting for some other reason.
	ReasonCrashLooping = "CrashLooping"

	// ReasonConfigNotMounted: the pod cannot start because its config Secret is
	// missing. This is mode 3, made visible by construction.
	ReasonConfigNotMounted = "ConfigNotMounted"

	// ReasonBackendHealthUnknown: the pod is serving, but whether any backend
	// is actually reachable cannot be established from outside the process.
	ReasonBackendHealthUnknown = "BackendHealthUnknown"
)

// exitCodeConfigError is what main returns when the config cannot be loaded.
// It is not a dedicated code — the same 1 is returned from argument parsing —
// so this is a strong hint rather than proof, and the message says so.
const exitCodeConfigError = 1

// diagnose inspects the pods behind a server and reports what can be
// established. It never guesses: an unobservable state is reported as unknown.
func (r *OtterStaxServerReconciler) diagnose(ctx context.Context, namespace, name string) metav1.Condition {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(namespace),
		client.MatchingLabels{InstanceLabel: name},
	); err != nil || len(pods.Items) == 0 {
		return metav1.Condition{
			Type: ConditionDegraded, Status: metav1.ConditionUnknown,
			Reason: ReasonBackendHealthUnknown, Message: "no pods yet",
		}
	}

	for i := range pods.Items {
		pod := &pods.Items[i]

		if c := diagnoseMount(pod); c != nil {
			return *c
		}
		if c := diagnoseCrash(pod); c != nil {
			return *c
		}
	}

	// Serving, as far as anything outside the process can tell.
	//
	// Deliberately not ConditionFalse: "not degraded" would assert that the
	// backends are reachable, which nothing here knows. The server reaches this
	// state with every backend down and reports nothing.
	return metav1.Condition{
		Type: ConditionDegraded, Status: metav1.ConditionUnknown,
		Reason: ReasonBackendHealthUnknown,
		Message: "serving; backend reachability is not observable — the server exposes no health endpoint, " +
			"no metrics and no admin API, and it serves happily with every backend unreachable",
	}
}

// diagnoseMount catches the pod that cannot start because its config is absent.
func diagnoseMount(pod *corev1.Pod) *metav1.Condition {
	for _, cs := range pod.Status.ContainerStatuses {
		w := cs.State.Waiting
		if w == nil {
			continue
		}
		switch w.Reason {
		case "CreateContainerConfigError", "CreateContainerError":
			return &metav1.Condition{
				Type: ConditionDegraded, Status: metav1.ConditionTrue,
				Reason: ReasonConfigNotMounted,
				Message: fmt.Sprintf("%s cannot start: %s. The config volume is mounted with optional=false "+
					"on purpose: without it the server would start with defaults and zero connections and "+
					"report nothing at all", pod.Name, w.Message),
			}
		}
	}
	return nil
}

// diagnoseCrash distinguishes a config rejection from any other crash.
func diagnoseCrash(pod *corev1.Pod) *metav1.Condition {
	for _, cs := range pod.Status.ContainerStatuses {
		// LastTerminationState outlives the recovery. A container that is back
		// up and serving is not crashing, whatever it did before.
		if cs.Ready && cs.State.Running != nil {
			continue
		}
		// Until the first restart the exit code is in State, not in
		// LastTerminationState.
		term := cs.State.Terminated
		if term == nil {
			term = cs.LastTerminationState.Terminated
		}
		if term == nil || term.ExitCode == 0 {
			continue
		}

		if term.ExitCode == exitCodeConfigError {
			return &metav1.Condition{
				Type: ConditionDegraded, Status: metav1.ConditionTrue,
				Reason: ReasonConfigRejected,
				Message: fmt.Sprintf("%s exited %d, which the server returns when it cannot load its "+
					"configuration. The rendered config is in the mounted Secret; note the exit code is "+
					"shared with argument-parsing failures, so this is a strong hint rather than proof",
					pod.Name, term.ExitCode),
			}
		}

		return &metav1.Condition{
			Type: ConditionDegraded, Status: metav1.ConditionTrue,
			Reason: ReasonCrashLooping,
			Message: fmt.Sprintf("%s exited %d (%s) after %d restart(s)",
				pod.Name, term.ExitCode, term.Reason, cs.RestartCount),
		}
	}
	return nil
}
