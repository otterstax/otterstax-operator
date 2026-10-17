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

package crdinstall

import (
	"context"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/otterstax/otterstax-operator/api/v1alpha1"
)

// newEnv starts an apiserver with the generated CRDs and returns a client.
func newEnv(t *testing.T) client.Client {
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
		t.Fatalf("registering scheme: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	return c
}

func conn(alias string, mutate func(*v1alpha1.OtterStaxConnectionSpec)) *v1alpha1.OtterStaxConnection {
	spec := v1alpha1.OtterStaxConnectionSpec{Alias: alias}
	mutate(&spec)
	return &v1alpha1.OtterStaxConnection{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "c-", Namespace: "default"},
		Spec:       spec,
	}
}

func validMySQL() *v1alpha1.SQLBackend {
	return &v1alpha1.SQLBackend{
		Host: "mariadb", Port: 3306, Username: "u", Database: "d",
	}
}

// TestConnectorUnionIsEnforced pins the discriminated union. A connection with
// no connector, or with two, is meaningless — and the failure it would cause is
// silent: the renderer would emit a config that parses and simply omits or
// duplicates a backend.
func TestConnectorUnionIsEnforced(t *testing.T) {
	c := newEnv(t)
	ctx := context.Background()

	t.Run("none set is rejected", func(t *testing.T) {
		err := c.Create(ctx, conn("a", func(s *v1alpha1.OtterStaxConnectionSpec) {}))
		if err == nil {
			t.Fatal("apiserver accepted a connection with no connector")
		}
	})

	t.Run("two set is rejected", func(t *testing.T) {
		err := c.Create(ctx, conn("b", func(s *v1alpha1.OtterStaxConnectionSpec) {
			s.MySQL = validMySQL()
			s.ClickHouse = validMySQL()
		}))
		if err == nil {
			t.Fatal("apiserver accepted a connection with two connectors")
		}
	})

	t.Run("exactly one is accepted", func(t *testing.T) {
		if err := c.Create(ctx, conn("c", func(s *v1alpha1.OtterStaxConnectionSpec) {
			s.MySQL = validMySQL()
		})); err != nil {
			t.Fatalf("apiserver rejected a valid single-connector connection: %v", err)
		}
	})
}

// TestS3EndpointRejectsScheme guards a trap rather than a typo. The server
// forces plain HTTP and path-style addressing whenever endpoint is non-empty,
// so a user writing https:// would get clear-text traffic and no warning.
// Rejecting the scheme outright makes the limitation visible at apply time.
func TestS3EndpointRejectsScheme(t *testing.T) {
	c := newEnv(t)
	ctx := context.Background()

	for _, ep := range []string{"http://minio:9000", "https://minio:9000"} {
		if err := c.Create(ctx, conn("s3a", func(s *v1alpha1.OtterStaxConnectionSpec) {
			s.S3 = &v1alpha1.S3Backend{AccessKey: "k", SecretKey: "s", Endpoint: ep}
		})); err == nil {
			t.Fatalf("apiserver accepted an endpoint carrying a scheme: %q", ep)
		}
	}

	if err := c.Create(ctx, conn("s3b", func(s *v1alpha1.OtterStaxConnectionSpec) {
		s.S3 = &v1alpha1.S3Backend{AccessKey: "k", SecretKey: "s", Endpoint: "minio:9000"}
	})); err != nil {
		t.Fatalf("apiserver rejected a schemeless endpoint: %v", err)
	}
}

// TestAliasIsASQLIdentifier keeps the alias usable as the outermost qualifier in
// federated SQL. A dash or a dot here parses fine in YAML and then produces
// queries nobody can write.
func TestAliasIsASQLIdentifier(t *testing.T) {
	c := newEnv(t)
	ctx := context.Background()

	// Upper case is rejected by the server itself since upstream e489588: an
	// alias is written unquoted in SQL. Accepting it here moves the error from
	// kubectl apply to a crash loop.
	for _, bad := range []string{"my-alias", "my.alias", "1alias", "", "Billing"} {
		if err := c.Create(ctx, conn(bad, func(s *v1alpha1.OtterStaxConnectionSpec) {
			s.MySQL = validMySQL()
		})); err == nil {
			t.Fatalf("apiserver accepted alias %q", bad)
		}
	}

	if err := c.Create(ctx, conn("mysql_bill", func(s *v1alpha1.OtterStaxConnectionSpec) {
		s.MySQL = validMySQL()
	})); err != nil {
		t.Fatalf("apiserver rejected a valid alias: %v", err)
	}
}

// TestServerHasNoReplicasField is a regression test on something deliberately
// absent. It is the only mechanism that survives a fresh session: an assistant
// asked to "add high availability" will reach for replicas, and on this software
// a second pod is a second divergent database, not a replica.
func TestServerHasNoReplicasField(t *testing.T) {
	c := newEnv(t)
	ctx := context.Background()

	srv := &v1alpha1.OtterStaxServer{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "s-", Namespace: "default"},
		Spec:       v1alpha1.OtterStaxServerSpec{Image: "ghcr.io/otterstax/otterstax:test"},
	}
	if err := c.Create(ctx, srv); err != nil {
		t.Fatalf("creating a minimal server: %v", err)
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(v1alpha1.SchemeGroupVersion.WithKind("OtterStaxServer"))
	if err := c.Get(ctx, client.ObjectKeyFromObject(srv), u); err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	spec, found, err := unstructured.NestedMap(u.Object, "spec")
	if err != nil || !found {
		t.Fatalf("reading spec: found=%v err=%v", found, err)
	}
	if _, exists := spec["replicas"]; exists {
		t.Fatal("spec.replicas exists; it must not — see the type comment for why")
	}

	// Belt and braces: the scale subresource must not be enabled either, or
	// kubectl scale would succeed against a field the type does not have.
	if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "status", "replicas"); found {
		t.Fatal("status.replicas exists; the scale subresource must stay off")
	}
}

// TestExposureRequiresAcknowledgement.
//
// Moving off ClusterIP publishes unauthenticated read and write access to every
// federated backend. That is not a footgun to document — it is one to make
// somebody type a sentence about.
func TestExposureRequiresAcknowledgement(t *testing.T) {
	c := newEnv(t)
	ctx := context.Background()

	mk := func(name string, e v1alpha1.ExposureSpec) *v1alpha1.OtterStaxServer {
		return &v1alpha1.OtterStaxServer{
			ObjectMeta: metav1.ObjectMeta{GenerateName: name, Namespace: "default"},
			Spec: v1alpha1.OtterStaxServerSpec{
				Image:    "ghcr.io/otterstax/otterstax:test",
				Exposure: e,
			},
		}
	}

	for _, typ := range []corev1.ServiceType{corev1.ServiceTypeNodePort, corev1.ServiceTypeLoadBalancer} {
		err := c.Create(ctx, mk("x-", v1alpha1.ExposureSpec{Type: typ}))
		if err == nil {
			t.Fatalf("%s was accepted without acknowledgeUnauthenticated", typ)
		}
	}

	if err := c.Create(ctx, mk("ok-", v1alpha1.ExposureSpec{
		Type:                       corev1.ServiceTypeLoadBalancer,
		AcknowledgeUnauthenticated: true,
	})); err != nil {
		t.Fatalf("an acknowledged LoadBalancer was rejected: %v", err)
	}

	// And the default path stays frictionless.
	if err := c.Create(ctx, mk("def-", v1alpha1.ExposureSpec{})); err != nil {
		t.Fatalf("the default ClusterIP was rejected: %v", err)
	}
}

// testImage is any image reference; nothing here pulls it.
const testImage = "ghcr.io/otterstax/otterstax:test"

// TestNewServiceKeysAreDefaulted. The renderer writes these values verbatim, so
// a server created without them must come back from the API server with the
// same numbers the server itself would default to — not with zeroes.
func TestNewServiceKeysAreDefaulted(t *testing.T) {
	c := newEnv(t)
	ctx := context.Background()

	s := &v1alpha1.OtterStaxServer{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "d-", Namespace: "default"},
		Spec:       v1alpha1.OtterStaxServerSpec{Image: testImage},
	}
	if err := c.Create(ctx, s); err != nil {
		t.Fatalf("creating server: %v", err)
	}

	if got := s.Spec.Service.SparkConnectPort; got != 15002 {
		t.Errorf("sparkConnectPort defaulted to %d, want 15002", got)
	}
	if got := s.Spec.ConnectionRetry; got.MaxAttempts != 1 || got.DelayMilliseconds != 1000 {
		t.Errorf("connectionRetry defaulted to %+v, want maxAttempts=1 delayMilliseconds=1000", got)
	}
}

// TestConnectionRetryRejectsNegatives.
func TestConnectionRetryRejectsNegatives(t *testing.T) {
	c := newEnv(t)
	ctx := context.Background()

	for _, r := range []v1alpha1.ConnectionRetrySpec{
		{MaxAttempts: -1},
		{DelayMilliseconds: -1},
	} {
		err := c.Create(ctx, &v1alpha1.OtterStaxServer{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "r-", Namespace: "default"},
			Spec:       v1alpha1.OtterStaxServerSpec{Image: testImage, ConnectionRetry: r},
		})
		if err == nil {
			t.Errorf("connectionRetry %+v was accepted", r)
		}
	}
}
