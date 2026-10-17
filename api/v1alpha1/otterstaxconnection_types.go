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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// OtterStaxConnectionSpec is one federated backend, addressed in SQL by its alias.
//
// One alias, one object. A server picks connections up by label selector rather
// than naming them, so a connection created later is adopted without editing the
// server. The model is Stackable's TrinoCatalog; the reason is the same.
//
// Credentials are literal here, or an opaque reference the server resolves
// itself once it learns to. The operator does not resolve secret references: the
// server reads exactly one config file and no environment variables, so anything
// the operator resolved would end up as literal bytes in that file anyway.
// +kubebuilder:validation:XValidation:rule="[has(self.mysql), has(self.postgresql), has(self.clickhouse), has(self.s3)].filter(x, x).size() == 1",message="exactly one of mysql, postgresql, clickhouse or s3 must be set"
type OtterStaxConnectionSpec struct {
	// Alias is the outermost qualifier in federated SQL: alias.database.table for
	// backends, and the s3_alias in CREATE EXTERNAL TABLE and COPY TO.
	//
	// Uniqueness across a server is not expressible in CEL — it is a cross-object
	// invariant — so the controller refuses to render on a duplicate and the
	// server rejects duplicates at parse time. Neither alone is sufficient.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-zA-Z_][a-zA-Z0-9_]*$`
	Alias string `json:"alias"`

	// +optional
	MySQL *SQLBackend `json:"mysql,omitempty"`

	// +optional
	PostgreSQL *PostgresBackend `json:"postgresql,omitempty"`

	// +optional
	ClickHouse *SQLBackend `json:"clickhouse,omitempty"`

	// +optional
	S3 *S3Backend `json:"s3,omitempty"`
}

// SQLBackend is a MySQL/MariaDB or ClickHouse backend.
//
// Note for ClickHouse: the port is the native TCP protocol (9000 by default),
// not the HTTP interface on 8123. Every config shipped with the server uses the
// native port.
type SQLBackend struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Host string `json:"host"`

	// Port is rendered into config.yaml as a STRING, because that is what the
	// server's parser reads for backends — unlike the server's own listener
	// ports, which are integers. Modelled as an integer here so the API is
	// typed; the renderer quotes it.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Username string `json:"username"`

	// Password is literal. See the type comment on the spec for why there is no
	// secret reference here.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Password string `json:"password,omitempty"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Database string `json:"database"`

	// Table restricts the connection to a single table. Empty means discover
	// every table in the database, which is the usual case.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	Table string `json:"table,omitempty"`
}

// PostgresBackend is a PostgreSQL backend.
type PostgresBackend struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Host string `json:"host"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Username string `json:"username"`

	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Password string `json:"password,omitempty"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Database string `json:"database"`

	// Schema is the PostgreSQL schema. Left unset the server applies "public".
	//
	// Whether the renderer must OMIT this key rather than emit it empty is a
	// question about the parser, not about taste, and the answer is pinned by
	// the renderer's tests rather than assumed here.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	Schema string `json:"schema,omitempty"`

	// +kubebuilder:validation:MaxLength=128
	// +optional
	Table string `json:"table,omitempty"`
}

// S3Backend is an S3 or S3-compatible alias. Unlike the SQL backends it is only
// recorded at startup, never dialled, so an unreachable endpoint is not retried
// and does not delay the server coming up.
type S3Backend struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	AccessKey string `json:"accessKey"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	SecretKey string `json:"secretKey"`

	// +kubebuilder:validation:MaxLength=64
	// +optional
	Region string `json:"region,omitempty"`

	// +kubebuilder:validation:MaxLength=2048
	// +optional
	SessionToken string `json:"sessionToken,omitempty"`

	// Endpoint targets an S3-compatible service such as MinIO.
	//
	// Setting it is not neutral: the server switches the client to plain HTTP
	// and path-style addressing. There is therefore no way to reach an HTTPS
	// S3-compatible endpoint through this field today, and credentials travel
	// in clear text to it.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('http://') && !self.startsWith('https://')",message="endpoint must be host:port without a scheme; the server forces http and path-style addressing when this is set"
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
}

// OtterStaxConnectionStatus reports what the controller can observe about this
// connection. It cannot observe much: the server exposes no admin API, and it
// reaches Ready with zero working backends.
type OtterStaxConnectionStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the .metadata.generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// BoundServers are the OtterStaxServer names in this namespace whose
	// connectionSelector currently matches this object.
	// +kubebuilder:validation:MaxItems=32
	// +listType=set
	// +optional
	BoundServers []string `json:"boundServers,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=osconn
// +kubebuilder:printcolumn:name="Alias",type=string,JSONPath=`.spec.alias`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.status.conditions[?(@.type=="Accepted")].reason`
// +kubebuilder:printcolumn:name="Bound",type=string,JSONPath=`.status.boundServers`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// OtterStaxConnection is one federated backend addressed by its SQL alias.
type OtterStaxConnection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	Spec OtterStaxConnectionSpec `json:"spec"`

	// +optional
	Status OtterStaxConnectionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OtterStaxConnectionList contains a list of OtterStaxConnection.
type OtterStaxConnectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OtterStaxConnection `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &OtterStaxConnection{}, &OtterStaxConnectionList{})
		return nil
	})
}
