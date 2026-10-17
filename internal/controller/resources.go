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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/strategicpatch"

	v1alpha1 "github.com/otterstax/otterstax-operator/api/v1alpha1"
)

const (
	// ConfigHashAnnotation carries the rendered config's hash on the pod
	// template. Changing it is what actually rolls the pod: Kubernetes does not
	// restart anything when a mounted Secret changes, and this server would not
	// notice if it did — it reads its config once and has no reload path.
	ConfigHashAnnotation = "otterstax.com/config-hash"

	// ConfigRevisionLabel marks the Secrets this controller owns so old
	// revisions can be found and collected.
	ConfigRevisionLabel = "otterstax.com/config-revision"

	// InstanceLabel ties every generated object back to its server.
	InstanceLabel = "app.kubernetes.io/instance"

	// NameLabel is the well-known name label.
	NameLabel = "app.kubernetes.io/name"

	// ConfigMountPath is where the rendered config is mounted, as a directory.
	//
	// Never via subPath: a subPath-mounted file is copied once and frozen, so a
	// rotated Secret would never reach the container. It does not matter today
	// because every change restarts the pod anyway, but it costs nothing to
	// keep the option open and it is a trap that is expensive to notice later.
	ConfigMountPath = "/etc/otterstax"

	// ConfigFileName is the file inside that directory.
	ConfigFileName = "config.yaml"

	// ContainerName is the server container in the pod template.
	ContainerName = "otterstax"

	// dataVolumeName is the engine's emptyDir data directory.
	dataVolumeName = "data"

	// DataMountPath is where the engine writes.
	//
	// The path is not a choice. main.cpp hardcodes
	// make_create_config("/tmp/test_collection_sql/base") and the process reads
	// no environment variables at all, so this is the only place a volume can
	// go. Making it configurable is a change to the server, tracked separately.
	DataMountPath = "/tmp/test_collection_sql"

	// ScratchMountPath covers the two S3 staging directories the server writes
	// outside its data directory. They are an unbounded download cache with no
	// eviction, which is why they get their own ephemeral volume rather than
	// sharing one with anything that matters.
	ScratchMountPath = "/tmp/otterstax_s3_cache"
	UploadMountPath  = "/tmp/otterstax_s3_upload"

	// FlightSQLPortName and friends name the ports so Services can target them
	// symbolically.
	FlightSQLPortName    = "flight-sql"
	MySQLPortName        = "mysql"
	PostgresPortName     = "pgwire"
	SparkConnectPortName = "spark-connect"
)

// ConfigHash is the content address of a rendered config.
//
// Only the bytes go in. Nothing about the object that produced them — no
// generation, no resourceVersion, no timestamp — or an unrelated update would
// change the hash and restart a healthy server for nothing.
func ConfigHash(config []byte) string {
	sum := sha256.Sum256(config)
	return hex.EncodeToString(sum[:])
}

// ConfigSecretName is content-addressed, so a config change is a new object
// rather than a mutation of an existing one. That is what makes rollout and
// rollback correct: the pod template references a name that changes with the
// content.
func ConfigSecretName(serverName, hash string) string {
	return fmt.Sprintf("%s-cfg-%s", serverName, hash[:10])
}

func labelsFor(server *v1alpha1.OtterStaxServer) map[string]string {
	return map[string]string{
		NameLabel:     "otterstax",
		InstanceLabel: server.Name,
	}
}

// BuildConfigSecret wraps the rendered config.
//
// A Secret rather than a ConfigMap because the file carries every backend
// credential in clear text. Immutable because the name is content-addressed, so
// the contents can never legitimately change — and immutability also stops the
// kubelet watching it, which is a real reduction in API server load.
func BuildConfigSecret(server *v1alpha1.OtterStaxServer, config []byte, hash string) *corev1.Secret {
	l := labelsFor(server)
	l[ConfigRevisionLabel] = hash[:10]

	immutable := true
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigSecretName(server.Name, hash),
			Namespace: server.Namespace,
			Labels:    l,
		},
		Immutable: &immutable,
		Data:      map[string][]byte{ConfigFileName: config},
	}
}

// BuildStatefulSet renders the workload.
//
// A StatefulSet with one replica, not a Deployment. Not for the volume — with
// an ephemeral volume each pod has its own — but for the update semantics: a
// Deployment's default rolling update would start a second pod alongside the
// first, and two OtterStax processes are two divergent databases rather than
// two replicas.
func BuildStatefulSet(server *v1alpha1.OtterStaxServer, hash string) (*appsv1.StatefulSet, error) {
	l := labelsFor(server)

	// Stopped keeps everything except the pod: config revisions, Services, the
	// NetworkPolicy and the status all survive, so bringing it back is a one
	// field edit rather than a restore from Git.
	replicas := int32(1)
	if server.Spec.ClusterOperation.Stopped {
		replicas = 0
	}

	// A fixed initialDelaySeconds is always wrong here: WAL replay is unbounded
	// and connection registration is serial and blocking, so a server with
	// several unreachable backends opens its ports minutes late through no
	// fault of its own. Derive the threshold from a declared budget instead.
	const probePeriod = 5
	budget := server.Spec.Startup.BudgetSeconds
	if budget <= 0 {
		budget = 900
	}
	failureThreshold := max(budget/probePeriod, 1)

	// Shutdown is unbounded in this server: gRPC is stopped without a deadline
	// and in-flight coroutines are destroyed rather than drained. The grace
	// period is therefore a guess informed by the only bounded component that
	// can be named from the source — the Kafka transaction timeout.
	grace := int64(120)

	container := corev1.Container{
		Name:            ContainerName,
		Image:           server.Spec.Image,
		ImagePullPolicy: server.Spec.ImagePullPolicy,
		// The image's CMD is ./server with no arguments, which makes it read
		// config.yaml relative to its working directory. Point it at the mount
		// explicitly rather than relying on that.
		Args: []string{"--config", ConfigMountPath + "/" + ConfigFileName},
		Ports: []corev1.ContainerPort{
			{Name: FlightSQLPortName, ContainerPort: server.Spec.Service.FlightSQLPort},
			{Name: MySQLPortName, ContainerPort: server.Spec.Service.MySQLPort},
			{Name: PostgresPortName, ContainerPort: server.Spec.Service.PostgresPort},
			{Name: SparkConnectPortName, ContainerPort: server.Spec.Service.SparkConnectPort},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "config", MountPath: ConfigMountPath, ReadOnly: true},
			{Name: dataVolumeName, MountPath: DataMountPath},
			{Name: "s3-cache", MountPath: ScratchMountPath},
			{Name: "s3-upload", MountPath: UploadMountPath},
		},
		Resources: server.Spec.Resources,

		// Readiness probes 8815 and nothing else. The server binds mysql, then
		// postgres, then Spark Connect, then FlightSQL, and registers its
		// backends before any of them, so 8815 opening means the engine
		// recovered and registration finished.
		// The wire ports are unusable as probes: their acceptor binds in the
		// constructor, one line before the accept handler is installed, so a
		// connect can succeed microseconds before the server can answer.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString(FlightSQLPortName)},
			},
			PeriodSeconds:    probePeriod,
			FailureThreshold: 3,
		},
		StartupProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString(FlightSQLPortName)},
			},
			PeriodSeconds:    probePeriod,
			FailureThreshold: failureThreshold,
		},
		// Deliberately no liveness probe. The engine has open, acknowledged bugs
		// about state not surviving a restart, so a false positive here is a
		// SIGKILL in the middle of a WAL write. The detection value is close to
		// zero and the cost is corruption.
	}

	tpl := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      l,
			Annotations: podAnnotations(server, hash),
		},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: &grace,
			ImagePullSecrets:              server.Spec.ImagePullSecrets,
			Containers:                    []corev1.Container{container},
			Volumes: []corev1.Volume{
				{
					Name: "config",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName:  ConfigSecretName(server.Name, hash),
							DefaultMode: ptr(int32(0o400)),
							Optional:    ptr(false),
						},
					},
				},
				{Name: dataVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "s3-cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "s3-upload", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		},
	}

	if err := applyPodOverrides(&tpl, server.Spec.PodOverrides); err != nil {
		return nil, err
	}

	// Re-asserted after the patch. The hash annotation is the restart trigger;
	// an override that dropped it — by replacing the annotations map rather
	// than merging into it — would leave a server that silently never picks up
	// a config change again.
	if tpl.Annotations == nil {
		tpl.Annotations = map[string]string{}
	}
	tpl.Annotations[ConfigHashAnnotation] = hash

	// Likewise the selector labels: a pod that no longer matches its own
	// StatefulSet's selector is rejected outright.
	if tpl.Labels == nil {
		tpl.Labels = map[string]string{}
	}
	maps.Copy(tpl.Labels, l)

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      server.Name,
			Namespace: server.Namespace,
			Labels:    l,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:            &replicas,
			ServiceName:         server.Name + "-headless",
			PodManagementPolicy: appsv1.OrderedReadyPodManagement,
			Selector:            &metav1.LabelSelector{MatchLabels: l},
			Template:            tpl,
		},
	}, nil
}

// podAnnotations carries the restart triggers onto the pod template.
func podAnnotations(server *v1alpha1.OtterStaxServer, hash string) map[string]string {
	a := map[string]string{ConfigHashAnnotation: hash}
	// Propagated from the object so that annotating the server rolls the pod.
	// Any new value works; a timestamp is the convention.
	if v, ok := server.Annotations[v1alpha1.RestartAnnotation]; ok {
		a[v1alpha1.RestartAnnotation] = v
	}
	return a
}

// applyPodOverrides merges the user's patch into the generated pod template.
//
// A strategic merge, not a replacement: that is what makes it possible to add a
// nodeSelector or a sidecar without restating the whole template, and what lets
// list fields like containers merge by name rather than being overwritten.
//
// This is the escape hatch that keeps the CRD from having to model every field
// anyone will ever need. It is deliberately unvalidated — the point is to allow
// what the API does not anticipate.
func applyPodOverrides(tpl *corev1.PodTemplateSpec, overrides *runtime.RawExtension) error {
	if overrides == nil || len(overrides.Raw) == 0 {
		return nil
	}

	original, err := json.Marshal(tpl)
	if err != nil {
		return fmt.Errorf("marshalling the generated pod template: %w", err)
	}
	patched, err := strategicpatch.StrategicMergePatch(original, overrides.Raw, corev1.PodTemplateSpec{})
	if err != nil {
		return fmt.Errorf("applying podOverrides: %w", err)
	}

	var out corev1.PodTemplateSpec
	if err := json.Unmarshal(patched, &out); err != nil {
		return fmt.Errorf("podOverrides produced a pod template that does not parse: %w", err)
	}
	*tpl = out
	return nil
}

// BuildServices returns the headless Service, which gives the StatefulSet its
// stable per-pod DNS, and the client-facing ClusterIP.
func BuildServices(server *v1alpha1.OtterStaxServer) (headless, clusterIP *corev1.Service) {
	l := labelsFor(server)
	ports := []corev1.ServicePort{
		{
			Name:       FlightSQLPortName,
			Port:       server.Spec.Service.FlightSQLPort,
			TargetPort: intstr.FromString(FlightSQLPortName),
			// Tells meshes and load balancers this is gRPC rather than opaque TCP.
			AppProtocol: ptr("kubernetes.io/grpc"),
		},
		{Name: MySQLPortName, Port: server.Spec.Service.MySQLPort, TargetPort: intstr.FromString(MySQLPortName)},
		{Name: PostgresPortName, Port: server.Spec.Service.PostgresPort, TargetPort: intstr.FromString(PostgresPortName)},
		{
			Name:        SparkConnectPortName,
			Port:        server.Spec.Service.SparkConnectPort,
			TargetPort:  intstr.FromString(SparkConnectPortName),
			AppProtocol: ptr("kubernetes.io/grpc"),
		},
	}

	headless = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: server.Name + "-headless", Namespace: server.Namespace, Labels: l},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  l,
			Ports:     ports,
			// The pod must be reachable while it is still starting: registration
			// failures happen before it is ever Ready, and that is exactly when
			// someone needs to look at it.
			PublishNotReadyAddresses: true,
		},
	}

	e := server.Spec.Exposure
	svcType := e.Type
	if svcType == "" {
		svcType = corev1.ServiceTypeClusterIP
	}

	clusterIP = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      server.Name,
			Namespace: server.Namespace,
			Labels:    l,
			// Copied rather than enumerated: per-cloud load-balancer
			// annotations number in the hundreds and change, and a built-in
			// list that is merely out of date is worse than none.
			Annotations: e.Annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:                     svcType,
			Selector:                 l,
			Ports:                    ports,
			LoadBalancerClass:        e.LoadBalancerClass,
			LoadBalancerSourceRanges: e.LoadBalancerSourceRanges,
			ExternalTrafficPolicy:    e.ExternalTrafficPolicy,
		},
	}
	return headless, clusterIP
}

func ptr[T any](v T) *T { return &v }
