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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// OtterStaxServerSpec describes a single OtterStax process.
//
// There is deliberately no replicas field and no scale subresource. OtterStax
// has no clustering of any kind: the scheduler is an in-process worker pool and
// federation is client-side fan-out, so a second pod is a second, divergent
// database rather than a replica. Omitting the field makes kubectl scale, HPA
// and KEDA fail at the API server instead of silently splitting the workload.
// Adding it later, if clustering ever lands, is backwards compatible.
type OtterStaxServerSpec struct {
	// Image is the OtterStax server image. Required: upstream publishes no
	// image, so there is no defensible default to ship.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=511
	Image string `json:"image"`

	// ImagePullPolicy for the server container.
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	// +kubebuilder:default=IfNotPresent
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// ImagePullSecrets for private registries. Empty is the supported default:
	// public GHCR packages pull anonymously, so the quickstart needs no secret.
	// +kubebuilder:validation:MaxItems=8
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// Service holds the wire-protocol listener settings. These map 1:1 onto the
	// service: section of the server's config.yaml.
	// +kubebuilder:default={}
	// +optional
	Service ServiceSpec `json:"service,omitempty"`

	// ConnectionSelector picks the OtterStaxConnection objects that make up this
	// server's federated backends. Same namespace only: a cross-namespace
	// reference is a privilege-escalation path, and the answer to wanting one is
	// to replicate the object.
	//
	// A nil selector matches nothing, which is the safe reading — an operator
	// that silently adopted every connection in the namespace would make adding
	// an unrelated connection a live change to an unrelated server. An explicit
	// empty selector (matchLabels: {}) matches everything, per label-selector
	// convention.
	// +optional
	ConnectionSelector *metav1.LabelSelector `json:"connectionSelector,omitempty"`

	// Startup bounds how long the server may take to accept traffic. WAL replay
	// is unbounded and connection registration is serial and blocking, so a
	// fixed initialDelaySeconds is always wrong; the controller derives
	// failureThreshold from this budget instead.
	// +kubebuilder:default={}
	// +optional
	Startup StartupSpec `json:"startup,omitempty"`

	// ConnectionRetry maps onto service.connection_retry in the server's config.
	// +kubebuilder:default={}
	// +optional
	ConnectionRetry ConnectionRetrySpec `json:"connectionRetry,omitempty"`

	// Resources for the server container.
	//
	// Note: OtterStax sizes every thread pool from hardware_concurrency(), which
	// reports host cores and ignores the cgroup quota. A CPU limit therefore
	// throttles a process that has already sized itself for the whole node.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Exposure controls the client-facing Service.
	// +kubebuilder:default={}
	// +optional
	Exposure ExposureSpec `json:"exposure,omitempty"`

	// ClusterOperation are the two escape hatches every operator is eventually
	// asked for, shipped before the incident rather than after it.
	// +kubebuilder:default={}
	// +optional
	ClusterOperation ClusterOperationSpec `json:"clusterOperation,omitempty"`

	// Network controls the generated NetworkPolicy.
	//
	// This is not a hardening option, it is the only access control that
	// exists. No wire protocol authenticates: the MySQL handshake parses a
	// username and password and discards them, Postgres sends an unconditional
	// auth-ok, FlightSQL runs a no-op auth handler, and Spark Connect listens
	// with insecure gRPC credentials and checks nothing.
	// Anything that can open a TCP connection to this pod can read and write
	// every federated system, using credentials the server holds on its behalf.
	// +kubebuilder:default={}
	// +optional
	Network NetworkSpec `json:"network,omitempty"`

	// PodOverrides is a strategic-merge patch applied to the generated pod
	// template. Escape hatch for everything not modelled here: nodeSelector,
	// tolerations, sidecars, extra volumes. Shipping it from day one is a
	// lesson every operator learns the hard way.
	//
	// Deliberately RawExtension rather than a typed corev1.PodTemplateSpec:
	// embedding the typed schema inflated this CRD to 600 KB, which is both
	// poor practice and close enough to client and server limits to matter.
	// The field is a freeform patch by definition, so the schema bought
	// nothing it did not also cost.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	PodOverrides *runtime.RawExtension `json:"podOverrides,omitempty"`
}

// ServiceSpec are the wire-protocol listeners. Names match the server's config.
type ServiceSpec struct {
	// FlightSQLPort is the Arrow FlightSQL/gRPC listener.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=8815
	// +optional
	FlightSQLPort int32 `json:"flightSqlPort,omitempty"`

	// MySQLPort is the MySQL wire-protocol listener.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=8816
	// +optional
	MySQLPort int32 `json:"mysqlPort,omitempty"`

	// PostgresPort is the PostgreSQL wire-protocol listener.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=8817
	// +optional
	PostgresPort int32 `json:"postgresPort,omitempty"`

	// SparkConnectPort is the Spark Connect/gRPC listener. The server opens it
	// unconditionally, so it is modelled even when nothing uses it.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=15002
	// +optional
	SparkConnectPort int32 `json:"sparkConnectPort,omitempty"`
}

// ConnectionRetrySpec is how hard the server tries to reach each backend at
// startup. Registration is serial and blocking, so every attempt here is time
// the pod spends not Ready: raise startup.budgetSeconds alongside it.
type ConnectionRetrySpec struct {
	// MaxAttempts per backend. 1 means a single attempt, no retry.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	MaxAttempts int32 `json:"maxAttempts,omitempty"`

	// DelayMilliseconds between attempts. Zero is not expressible: an omitted
	// field and a zero one are the same thing on the wire, and both default.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1000
	// +optional
	DelayMilliseconds int32 `json:"delayMilliseconds,omitempty"`
}

// ExposureSpec shapes the client-facing Service.
//
// The default is ClusterIP, and moving off it is deliberately not free: no wire
// protocol authenticates, so a NodePort or LoadBalancer publishes
// unauthenticated read and write access to every federated backend to whatever
// can reach that address.
// The rule is guarded with has() on every field it touches. Without the guard
// it is evaluated against this struct's own default of {} during schema
// validation, where the fields do not exist yet, and the API server rejects the
// whole CRD at install time — which is not a failed validation but a total
// failure to install the operator.
// +kubebuilder:validation:XValidation:rule="!has(self.type) || self.type == 'ClusterIP' || (has(self.acknowledgeUnauthenticated) && self.acknowledgeUnauthenticated)",message="type NodePort or LoadBalancer publishes unauthenticated access to every federated backend; set acknowledgeUnauthenticated: true to confirm this is intended"
type ExposureSpec struct {
	// Type of the client-facing Service.
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	// +kubebuilder:default=ClusterIP
	// +optional
	Type corev1.ServiceType `json:"type,omitempty"`

	// AcknowledgeUnauthenticated must be set before the service leaves the
	// cluster. It is not a formality: the MySQL handshake parses a username and
	// password and discards them, Postgres sends an unconditional auth-ok,
	// FlightSQL runs a no-op auth handler, and Spark Connect listens with
	// insecure gRPC credentials and checks nothing.
	// +kubebuilder:default=false
	// +optional
	AcknowledgeUnauthenticated bool `json:"acknowledgeUnauthenticated,omitempty"`

	// Annotations are copied onto the client-facing Service.
	//
	// This is where cloud load-balancer configuration goes. The operator does
	// not enumerate per-cloud annotations: there are hundreds, they change, and
	// a list that is merely out of date is worse than none.
	// +kubebuilder:validation:MaxProperties=32
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// LoadBalancerClass selects a specific load-balancer implementation.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	LoadBalancerClass *string `json:"loadBalancerClass,omitempty"`

	// LoadBalancerSourceRanges restricts who may reach a LoadBalancer. Given
	// nothing authenticates, this is the closest thing to access control that
	// exists once the service leaves the cluster.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	LoadBalancerSourceRanges []string `json:"loadBalancerSourceRanges,omitempty"`

	// ExternalTrafficPolicy: Local preserves the client source address, which
	// matters if loadBalancerSourceRanges is doing the access control.
	// +kubebuilder:validation:Enum=Cluster;Local
	// +optional
	ExternalTrafficPolicy corev1.ServiceExternalTrafficPolicy `json:"externalTrafficPolicy,omitempty"`
}

// ClusterOperationSpec holds the two operational overrides.
type ClusterOperationSpec struct {
	// Stopped scales the workload to zero while leaving everything else in
	// place: the config revisions, the Services, the NetworkPolicy and the
	// status all survive. It is the difference between "take this out of
	// service for an hour" and "delete it and hope the config is in Git".
	// +kubebuilder:default=false
	// +optional
	Stopped bool `json:"stopped,omitempty"`

	// ReconciliationPaused stops the controller writing to the cluster while
	// leaving it reading and reporting. It exists for the case where an
	// operator is making things worse and someone needs it to hold still
	// without deleting it — the thing people otherwise achieve by scaling the
	// controller to zero, which stops it for every object at once.
	// +kubebuilder:default=false
	// +optional
	ReconciliationPaused bool `json:"reconciliationPaused,omitempty"`
}

// NetworkSpec configures the generated NetworkPolicy.
type NetworkSpec struct {
	// Policy selects what the operator generates.
	//
	// DefaultDeny is the default: nothing may reach the wire ports until it is
	// named in allowIngressFrom. None means the operator generates nothing and
	// the cluster's own defaults apply — appropriate only when something else
	// owns network policy, and never because the policy was inconvenient.
	// +kubebuilder:validation:Enum=DefaultDeny;None
	// +kubebuilder:default=DefaultDeny
	// +optional
	Policy NetworkPolicyMode `json:"policy,omitempty"`

	// AllowIngressFrom names the pods permitted to reach the wire ports. Each
	// entry selects pods; an entry with a namespaceSelector reaches across
	// namespaces. Empty means nothing may connect, which is the correct default
	// for a service that authenticates nobody.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	AllowIngressFrom []IngressPeer `json:"allowIngressFrom,omitempty"`
}

// NetworkPolicyMode is what the operator generates.
// +kubebuilder:validation:Enum=DefaultDeny;None
type NetworkPolicyMode string

const (
	NetworkPolicyDefaultDeny NetworkPolicyMode = "DefaultDeny"
	NetworkPolicyNone        NetworkPolicyMode = "None"
)

// IngressPeer names a source permitted to reach the wire ports.
type IngressPeer struct {
	// PodSelector selects pods. An empty selector matches every pod in scope,
	// so it is required here — an accidental empty object would open the
	// service to everything, which is exactly the state being prevented.
	// +kubebuilder:validation:Required
	PodSelector metav1.LabelSelector `json:"podSelector"`

	// NamespaceSelector widens the scope beyond this namespace. Omitted, the
	// peer is confined to the server's own namespace.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
}

// StartupSpec bounds startup instead of guessing a fixed delay.
type StartupSpec struct {
	// BudgetSeconds is the total time the server may take to open its listeners.
	// The controller computes the startup probe's failureThreshold from it.
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:validation:Maximum=7200
	// +kubebuilder:default=900
	// +optional
	BudgetSeconds int32 `json:"budgetSeconds,omitempty"`
}

// OtterStaxServerStatus reports what the controller can actually observe.
//
// The server exposes no health endpoint, no metrics and no admin API, and it
// reaches Ready with zero working backends. Status is therefore explicit about
// the difference between what was rendered and what is running, rather than
// implying knowledge the controller does not have.
type OtterStaxServerStatus struct {
	// Conditions follow the standard Available/Progressing/Degraded convention.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the .metadata.generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// RenderedConfigHash is the hash of the config the controller last rendered.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	RenderedConfigHash string `json:"renderedConfigHash,omitempty"`

	// RunningConfigHash is the hash of the config the live pod actually mounts.
	// Every spec change is a restart, so a window where these differ is normal;
	// a window that does not close means the rollout is stuck.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	RunningConfigHash string `json:"runningConfigHash,omitempty"`

	// LastRestartReason records why the workload was last rolled.
	//
	// Every spec change here is a process restart, and a restart is not free on
	// this engine, so "why did it restart" is a question that will be asked and
	// currently has no answer anywhere in the system.
	// +optional
	LastRestartReason *RestartRecord `json:"lastRestartReason,omitempty"`
}

// RestartRecord is a typed answer to "why did it restart".
type RestartRecord struct {
	// Reason is the classified cause.
	// +kubebuilder:validation:Enum=ConfigChanged;ImageChanged;ManualRestart;InitialRollout
	Reason RestartReason `json:"reason"`

	// Time is when the controller made the change.
	Time metav1.Time `json:"time"`

	// Detail names what specifically differed.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Detail string `json:"detail,omitempty"`
}

// RestartReason classifies a roll.
// +kubebuilder:validation:Enum=ConfigChanged;ImageChanged;ManualRestart;InitialRollout
type RestartReason string

const (
	RestartConfigChanged  RestartReason = "ConfigChanged"
	RestartImageChanged   RestartReason = "ImageChanged"
	RestartManual         RestartReason = "ManualRestart"
	RestartInitialRollout RestartReason = "InitialRollout"
)

// RestartAnnotation forces a roll out of band. Setting it to any new value
// restarts the pod; the convention is a timestamp.
//
// Twenty lines, and the single most requested operator feature there is.
const RestartAnnotation = "otterstax.com/restart"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=otterstax
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Available")].status`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`,priority=1
// +kubebuilder:printcolumn:name="Config",type=string,JSONPath=`.status.runningConfigHash`,priority=1
// +kubebuilder:printcolumn:name="Last-Restart",type=string,JSONPath=`.status.lastRestartReason.reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// OtterStaxServer is a single OtterStax federated SQL server.
type OtterStaxServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	Spec OtterStaxServerSpec `json:"spec"`

	// +optional
	Status OtterStaxServerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OtterStaxServerList contains a list of OtterStaxServer.
type OtterStaxServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OtterStaxServer `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &OtterStaxServer{}, &OtterStaxServerList{})
		return nil
	})
}
