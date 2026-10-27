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

// Package crdinstall asserts that the generated CRDs can actually be installed
// into a real API server.
//
// This is the cheapest de-risking in the whole project. A CRD rejected at
// install time is not a failed validation rule — it is a total failure of the
// operator, because nothing can be created at all. The two ways to get there
// are the CEL cost estimator refusing an unbounded rule (kubernetes#121162) and
// a schema that trips an apiserver limit.
package crdinstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestCRDsInstall starts a real etcd + apiserver and installs every generated
// CRD. It fails if the API server rejects any of them.
func TestCRDsInstall(t *testing.T) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	var cfg *rest.Config
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("apiserver rejected the generated CRDs: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Logf("stopping envtest: %v", err)
		}
	})

	if cfg == nil {
		t.Fatal("envtest returned a nil rest.Config")
	}
	t.Log("all generated CRDs installed into a live apiserver")
}

// TestEveryCELRuleGuardsItsFields is a meta-test on the generated schema.
//
// A CEL rule that dereferences a field which is absent in its own default
// value is rejected at CRD install time, and the failure is total: nothing can
// be created because the operator cannot be installed. It has already happened
// once here, caught by TestCRDsInstall only after the rule was written. This
// asserts the property directly so the next one is caught in review instead.
func TestEveryCELRuleGuardsItsFields(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "..", "config", "crd", "bases"))
	if err != nil {
		t.Fatalf("reading CRD directory: %v", err)
	}

	var checked int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}

		for line := range strings.SplitSeq(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "- rule:") && !strings.HasPrefix(trimmed, "rule:") {
				continue
			}
			checked++
			// A rule that reads self.<field> without a has() guard is only safe
			// when the field is required. Rather than resolving that here, the
			// conservative check is that any rule mentioning self. also mentions
			// has( — which is the pattern that is always safe.
			if strings.Contains(trimmed, "self.") && !strings.Contains(trimmed, "has(") &&
				!strings.Contains(trimmed, "self.filter") && !strings.Contains(trimmed, "self.startsWith") {
				t.Errorf("%s: rule dereferences a field without a has() guard, which fails against an "+
					"object default and takes the whole CRD down at install time:\n  %s", e.Name(), trimmed)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no CEL rules found; the meta-test is not actually checking anything")
	}
	t.Logf("checked %d CEL rule(s)", checked)
}
