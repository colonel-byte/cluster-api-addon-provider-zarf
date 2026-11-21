// Copyright 2026 colonel-byte
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build envtest

// Package envtest starts an API server with every CRD this provider's controller reads or writes,
// so a test can drive a whole reconcile against real API semantics - status subresources, patch
// conflicts, generateName, CEL validation rules - rather than against a fake client that
// approximates them. The shape mirrors cluster-api-provider-cargoship's internal/envtest.
//
// It is behind the envtest build tag because it needs the kubebuilder control-plane binaries, and
// plain `go test ./...` must stay runnable with nothing installed. The entry point is
// `go run ./magefiles/core test:envtest`.
package envtest

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// capiCRDEnvVar names the directory holding Cluster API's own core CRDs.
//
// They are not in this repository and they are not in vendor/ either: `go mod vendor` keeps the
// Go files a build needs and drops everything else, and the CRDs are YAML in the cluster-api
// module's source tree. The test:envtest mage target generates them from the vendored types and
// passes the directory in, which is why these tests run through that target rather than a bare
// `go test -tags=envtest`.
const capiCRDEnvVar = "CAPI_CRD_DIR"

// cfg is the running API server, set by Run before any test starts.
var cfg *rest.Config //nolint:gochecknoglobals // one API server per test binary

// Run boots the API server, runs the package's tests against it, and tears it down. A package with
// envtest tests calls it from TestMain, and the return value is the process exit code.
//
// One API server per test binary rather than one per test: a bring-up is a second or two, and the
// namespace each test makes with Namespace is what keeps tests out of each other's way.
func Run(m *testing.M) int {
	capiCRDs := os.Getenv(capiCRDEnvVar)
	if capiCRDs == "" {
		fmt.Fprintf(os.Stderr, "%s is not set; run these tests with `go run ./magefiles/core test:envtest`\n", capiCRDEnvVar)
		return 1
	}

	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			capiCRDs,
			filepath.Join(repoRoot(), "config", "crd", "bases"),
		},
		ErrorIfCRDPathMissing: true,
	}

	restConfig, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to start the test API server: %v\n", err)
		return 1
	}
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "unable to stop the test API server: %v\n", err)
		}
	}()

	cfg = restConfig
	return m.Run()
}

// Scheme builds the scheme the manager binary runs with: client-go's, this provider's, and
// Cluster API's v1beta2 types.
func Scheme() (*k8sruntime.Scheme, error) {
	scheme := k8sruntime.NewScheme()
	for _, add := range []func(*k8sruntime.Scheme) error{
		clientgoscheme.AddToScheme,
		addonsv1alpha1.AddToScheme,
		clusterv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			return nil, err
		}
	}
	return scheme, nil
}

// Client returns a client for the running API server, on the scheme the manager uses.
func Client(t *testing.T) client.Client {
	t.Helper()

	scheme, err := Scheme()
	if err != nil {
		t.Fatalf("unable to build the scheme: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("unable to build a client: %v", err)
	}
	return c
}

// Namespace creates a namespace for one test and returns its name.
//
// It is not deleted on the way out. A namespace deletion needs the controllers an envtest API
// server does not run, so it would hang rather than finish; the generated name is what keeps two
// tests apart, and the whole API server goes away at the end of the binary.
func Namespace(t *testing.T, c client.Client) string {
	t.Helper()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-"}}
	if err := c.Create(t.Context(), ns); err != nil {
		t.Fatalf("unable to create a namespace: %v", err)
	}
	return ns.Name
}

// repoRoot is this file's directory's parent's parent, which is the module root.
func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("unable to locate the repository root")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}
