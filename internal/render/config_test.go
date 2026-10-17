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

package render

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/otterstax/otterstax-operator/api/v1alpha1"
)

func svc() v1alpha1.ServiceSpec {
	return v1alpha1.ServiceSpec{FlightSQLPort: 8815, MySQLPort: 8816, PostgresPort: 8817, SparkConnectPort: 15002}
}

func named(name string, spec v1alpha1.OtterStaxConnectionSpec) v1alpha1.OtterStaxConnection {
	return v1alpha1.OtterStaxConnection{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spec,
	}
}

func mysqlConn(name, alias string) v1alpha1.OtterStaxConnection {
	return named(name, v1alpha1.OtterStaxConnectionSpec{
		Alias: alias,
		MySQL: &v1alpha1.SQLBackend{
			Host: "mariadb", Port: 3306, Username: "u", Password: "p", Database: "bill",
		},
	})
}

func mustRender(t *testing.T, in Input) string {
	t.Helper()
	out, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return string(out)
}

// TestPortIsAQuotedString pins the asymmetry that is easiest to get wrong: the
// server's own listener ports are integers, while backend ports are strings.
func TestPortIsAQuotedString(t *testing.T) {
	got := mustRender(t, Input{Service: svc(), Connections: []v1alpha1.OtterStaxConnection{
		mysqlConn("m", "mysql"),
	}})

	if !strings.Contains(got, `port: "3306"`) {
		t.Errorf("backend port must be a quoted string; got:\n%s", got)
	}
	if !strings.Contains(got, "port: 8816\n") {
		t.Errorf("service port must be a bare int; got:\n%s", got)
	}
}

// TestNoBareKeysEver is the single most valuable assertion in this file.
//
// yaml-cpp does not throw on a defined-null node and does not yield an empty
// string: as_if<std::string> returns the four-character string "null". So a
// bare `password:` is a password, a bare `alias:` is a connection named null
// that passes validation, and a bare `schema:` defeats the "public" default.
// Nothing in the server would report any of it.
func TestNoBareKeysEver(t *testing.T) {
	// Every optional string left empty, which is exactly when a naive encoder
	// emits a bare key.
	got := mustRender(t, Input{Service: svc(), Connections: []v1alpha1.OtterStaxConnection{
		named("m", v1alpha1.OtterStaxConnectionSpec{
			Alias: "m",
			MySQL: &v1alpha1.SQLBackend{Host: "h", Username: "u", Database: "d"},
		}),
		named("s", v1alpha1.OtterStaxConnectionSpec{
			Alias: "s",
			S3:    &v1alpha1.S3Backend{AccessKey: "k", SecretKey: "s"},
		}),
	}})

	for i, line := range strings.Split(got, "\n") {
		t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasSuffix(t, ":") && !isSectionKey(t) {
			// A key with nothing after it.
			panicf(i, line, got)
		}
		for _, nullish := range []string{": ~", ": null", ": Null", ": NULL"} {
			if strings.HasSuffix(t, nullish) {
				panicf(i, line, got)
			}
		}
	}
}

// isSectionKey reports whether a trailing-colon line is a legitimate nesting
// key rather than a value-bearing field left empty.
func isSectionKey(t string) bool {
	switch t {
	case "service:", "flight_sql:", "mysql:", "postgres:", "postgresql:",
		"clickhouse:", "s3:", "connections:", "spark_connect:", "connection_retry:":
		return true
	}
	return false
}

func panicf(i int, line, full string) {
	panic("bare or null-like value at line " + itoa(i+1) + ": " + line + "\n\nfull document:\n" + full)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestSchemaIsAlwaysExplicit keeps the parser's default from ever being
// load-bearing. The default applies only when the key is ABSENT; an empty or
// null value does not get it.
func TestSchemaIsAlwaysExplicit(t *testing.T) {
	got := mustRender(t, Input{Service: svc(), Connections: []v1alpha1.OtterStaxConnection{
		named("p", v1alpha1.OtterStaxConnectionSpec{
			Alias:      "pg",
			PostgreSQL: &v1alpha1.PostgresBackend{Host: "h", Username: "u", Database: "d"},
		}),
	}})
	if !strings.Contains(got, `schema: "public"`) {
		t.Errorf("schema must be emitted explicitly; got:\n%s", got)
	}
}

// TestValuesAreQuotedAgainstYAMLTypeCoercion covers the class where an unquoted
// value changes meaning on the way in: a hash starts a comment, yes becomes a
// bool, a bare digit string becomes an int.
func TestValuesAreQuotedAgainstYAMLTypeCoercion(t *testing.T) {
	got := mustRender(t, Input{Service: svc(), Connections: []v1alpha1.OtterStaxConnection{
		named("m", v1alpha1.OtterStaxConnectionSpec{
			Alias: "m",
			MySQL: &v1alpha1.SQLBackend{
				Host: "h", Username: "yes", Password: "p@ss #1", Database: "d",
			},
		}),
	}})

	if !strings.Contains(got, `password: "p@ss #1"`) {
		t.Errorf("a value containing \" #\" must stay quoted or it is truncated; got:\n%s", got)
	}
	if !strings.Contains(got, `username: "yes"`) {
		t.Errorf("yes must stay a string; got:\n%s", got)
	}
}

// TestDuplicateAliasIsRefused. The server accepts duplicates: the last wins and
// the earlier connection is opened and leaked. Across kinds it is worse — the
// query routes by probe order, so it reaches a different backend than written.
// Neither is observable, so the renderer refuses.
func TestDuplicateAliasIsRefused(t *testing.T) {
	_, err := Render(Input{Service: svc(), Connections: []v1alpha1.OtterStaxConnection{
		mysqlConn("first", "shared"),
		mysqlConn("second", "shared"),
	}})
	if err == nil {
		t.Fatal("duplicate alias was accepted")
	}
	if !strings.Contains(err.Error(), "shared") {
		t.Errorf("error should name the alias; got %v", err)
	}
}

// TestRenderIsDeterministic. A renderer that drifts restarts the pod, and a
// restart is a SIGKILL mid-WAL-write on this software, not an inconvenience.
func TestRenderIsDeterministic(t *testing.T) {
	in := Input{Service: svc(), Connections: []v1alpha1.OtterStaxConnection{
		mysqlConn("a", "one"),
		named("b", v1alpha1.OtterStaxConnectionSpec{
			Alias: "two", S3: &v1alpha1.S3Backend{AccessKey: "k", SecretKey: "s", Region: "us-east-1"},
		}),
		named("c", v1alpha1.OtterStaxConnectionSpec{
			Alias: "three", PostgreSQL: &v1alpha1.PostgresBackend{Host: "h", Username: "u", Database: "d"},
		}),
	}}

	first := mustRender(t, in)
	for i := 0; i < 200; i++ {
		if got := mustRender(t, in); got != first {
			t.Fatalf("render %d differed from the first", i)
		}
	}
}

// TestSectionOrderIsFixed. Section order is part of the contract; entry order
// within a section follows the input, because with a duplicate alias across
// kinds the server resolves by probe order and reordering reroutes a query.
func TestSectionOrderIsFixed(t *testing.T) {
	got := mustRender(t, Input{Service: svc(), Connections: []v1alpha1.OtterStaxConnection{
		named("s", v1alpha1.OtterStaxConnectionSpec{Alias: "s3a", S3: &v1alpha1.S3Backend{AccessKey: "k", SecretKey: "s"}}),
		named("c", v1alpha1.OtterStaxConnectionSpec{Alias: "cha", ClickHouse: &v1alpha1.SQLBackend{Host: "h", Username: "u", Database: "d"}}),
		named("p", v1alpha1.OtterStaxConnectionSpec{Alias: "pga", PostgreSQL: &v1alpha1.PostgresBackend{Host: "h", Username: "u", Database: "d"}}),
		mysqlConn("m", "mya"),
	}})

	order := []string{"  mysql:", "  postgresql:", "  clickhouse:", "  s3:"}
	prev := -1
	for _, sec := range order {
		at := strings.Index(got, sec)
		if at < 0 {
			t.Fatalf("section %q missing from:\n%s", sec, got)
		}
		if at < prev {
			t.Fatalf("section %q out of order in:\n%s", sec, got)
		}
		prev = at
	}
}

// TestEmptyConnectionsOmitsTheBlock. An empty connections: block and no block
// both yield zero connections; omitting it is the honest encoding.
func TestEmptyConnectionsOmitsTheBlock(t *testing.T) {
	got := mustRender(t, Input{Service: svc()})
	if strings.Contains(got, "connections:") {
		t.Errorf("expected no connections block; got:\n%s", got)
	}
}

// TestS3EndpointWithSchemeIsRefused. Defence in depth: CEL rejects it at the
// API server, and the renderer refuses too, because the consequence is silent
// clear-text traffic.
func TestS3EndpointWithSchemeIsRefused(t *testing.T) {
	_, err := Render(Input{Service: svc(), Connections: []v1alpha1.OtterStaxConnection{
		named("s", v1alpha1.OtterStaxConnectionSpec{
			Alias: "s3a",
			S3:    &v1alpha1.S3Backend{AccessKey: "k", SecretKey: "s", Endpoint: "https://minio:9000"},
		}),
	}})
	if err == nil {
		t.Fatal("an endpoint carrying a scheme was accepted")
	}
}

// TestMissingRequiredFieldIsRefused. The parser throws and the process exits 1,
// which is loud — but failing here means it never reaches a pod.
func TestMissingRequiredFieldIsRefused(t *testing.T) {
	_, err := Render(Input{Service: svc(), Connections: []v1alpha1.OtterStaxConnection{
		named("m", v1alpha1.OtterStaxConnectionSpec{
			Alias: "m", MySQL: &v1alpha1.SQLBackend{Host: "", Username: "u", Database: "d"},
		}),
	}})
	if err == nil {
		t.Fatal("a backend with no host was accepted")
	}
	if !strings.Contains(err.Error(), "host") {
		t.Errorf("error should name the missing field; got %v", err)
	}
}

// TestSparkConnectAndRetryAreRendered. Both live under service: in the server's
// config, and both are plain integers there, like the listener ports.
func TestSparkConnectAndRetryAreRendered(t *testing.T) {
	s := svc()
	s.SparkConnectPort = 15003
	got := mustRender(t, Input{
		Service:         s,
		ConnectionRetry: v1alpha1.ConnectionRetrySpec{MaxAttempts: 5, DelayMilliseconds: 2000},
	})

	for _, want := range []string{
		"  spark_connect:\n    port: 15003\n",
		"  connection_retry:\n    max_attempts: 5\n    delay_ms: 2000\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
