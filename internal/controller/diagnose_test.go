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
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func podWith(cs corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{cs}}}
}

// TestRecoveredContainerIsNotDegraded. LastTerminationState outlives the
// recovery: a single OOM kill would otherwise report CrashLooping until the
// next roll, for a server that has been serving fine ever since.
func TestRecoveredContainerIsNotDegraded(t *testing.T) {
	pod := podWith(corev1.ContainerStatus{
		Ready:                true,
		State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}},
		RestartCount:         1,
	})
	if c := diagnoseCrash(pod); c != nil {
		t.Errorf("a running, ready container was reported as %s/%s", c.Status, c.Reason)
	}
}

// TestCrashLoopIsStillReported. The fix must not swallow the real thing.
func TestCrashLoopIsStillReported(t *testing.T) {
	for _, tc := range []struct {
		code int32
		want string
	}{
		{exitCodeConfigError, ReasonConfigRejected},
		{139, ReasonCrashLooping},
	} {
		pod := podWith(corev1.ContainerStatus{
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: tc.code}},
			RestartCount:         3,
		})
		c := diagnoseCrash(pod)
		if c == nil || c.Reason != tc.want {
			t.Errorf("exit %d: got %+v, want reason %s", tc.code, c, tc.want)
		}
	}
}

// TestFirstCrashIsReported. Until the first restart the exit code is in State,
// not LastTerminationState; a container that cannot even start must not be
// reported as serving in that window.
func TestFirstCrashIsReported(t *testing.T) {
	pod := podWith(corev1.ContainerStatus{
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 128, Reason: "StartError"}},
	})
	c := diagnoseCrash(pod)
	if c == nil || c.Reason != ReasonCrashLooping {
		t.Fatalf("got %+v, want reason %s", c, ReasonCrashLooping)
	}
}
