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
	"slices"
	"testing"

	v1alpha1 "github.com/otterstax/otterstax-operator/api/v1alpha1"
)

func egressPorts(conns ...v1alpha1.OtterStaxConnectionSpec) []int {
	in := make([]v1alpha1.OtterStaxConnection, len(conns))
	for i := range conns {
		in[i].Spec = conns[i]
	}
	ports := egressPortsFor(in)
	out := make([]int, 0, len(ports))
	for _, p := range ports {
		out = append(out, p.Port.IntValue())
	}
	slices.Sort(out)
	return out
}

// TestEgressCoversTheDriverDefaultPort. Port is optional on a connection; left
// unset, the renderer omits it and the server dials the driver's default. The
// egress rule has to open that same port, or under a CNI that enforces policy
// the backend is unreachable — and an unreachable backend is the one failure
// this server reports nowhere.
func TestEgressCoversTheDriverDefaultPort(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec v1alpha1.OtterStaxConnectionSpec
		want int
	}{
		{"mysql", v1alpha1.OtterStaxConnectionSpec{MySQL: &v1alpha1.SQLBackend{}}, 3306},
		{"postgresql", v1alpha1.OtterStaxConnectionSpec{PostgreSQL: &v1alpha1.PostgresBackend{}}, 5432},
		{"clickhouse", v1alpha1.OtterStaxConnectionSpec{ClickHouse: &v1alpha1.SQLBackend{}}, 9000},
		{"explicit", v1alpha1.OtterStaxConnectionSpec{MySQL: &v1alpha1.SQLBackend{Port: 3307}}, 3307},
	} {
		if got := egressPorts(tc.spec); !slices.Equal(got, []int{tc.want}) {
			t.Errorf("%s: egress ports %v, want [%d]", tc.name, got, tc.want)
		}
	}
}

// TestEgressFollowsTheS3Endpoint. With an endpoint set the server forces plain
// HTTP, so the port is the endpoint's own, or 80. Without one it is AWS over
// HTTPS. A MinIO on :9000 is the common case and must not be blocked.
func TestEgressFollowsTheS3Endpoint(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		want     int
	}{
		{"minio:9000", 9000},
		{"minio", 80},
		{"[fd00::1]:9001", 9001},
		{"", 443},
	} {
		got := egressPorts(v1alpha1.OtterStaxConnectionSpec{S3: &v1alpha1.S3Backend{Endpoint: tc.endpoint}})
		if !slices.Equal(got, []int{tc.want}) {
			t.Errorf("endpoint %q: egress ports %v, want [%d]", tc.endpoint, got, tc.want)
		}
	}
}
