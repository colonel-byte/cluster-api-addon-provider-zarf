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

package controller

import "testing"

func TestCABundleFrom(t *testing.T) {
	tests := []struct {
		name string
		data map[string][]byte
		want string
		err  bool
	}{
		{
			name: "ca.crt wins over the other conventional keys",
			data: map[string][]byte{
				"ca.crt":  []byte("the ca"),
				"tls.crt": []byte("the serving cert"),
			},
			want: "the ca",
		},
		{
			name: "tls.crt is the last conventional key tried",
			data: map[string][]byte{
				"tls.crt": []byte("the serving cert"),
				"tls.key": []byte("the key"),
			},
			want: "the serving cert",
		},
		{
			name: "an operator-named single key is taken as the bundle",
			data: map[string][]byte{
				"internal-roots.pem": []byte("the ca"),
			},
			want: "the ca",
		},
		{
			name: "an empty conventional key falls through rather than returning nothing",
			data: map[string][]byte{
				"ca.crt": {},
				"ca.pem": []byte("the ca"),
			},
			want: "the ca",
		},
		{
			name: "several unconventional keys are ambiguous, not a guess",
			data: map[string][]byte{
				"first.pem":  []byte("one"),
				"second.pem": []byte("two"),
			},
			err: true,
		},
		{
			name: "no data at all",
			data: map[string][]byte{},
			err:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := caBundleFrom(test.data)
			if test.err {
				if err == nil {
					t.Fatalf("want an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("want %q, got error %v", test.want, err)
			}
			if string(got) != test.want {
				t.Fatalf("want %q, got %q", test.want, got)
			}
		})
	}
}

func TestRegistryHost(t *testing.T) {
	tests := []struct {
		name       string
		repository string
		want       string
	}{
		{
			name:       "host and path",
			repository: "registry.internal:5000/packages/app",
			want:       "registry.internal:5000",
		},
		{
			name:       "host alone",
			repository: "registry.internal",
			want:       "registry.internal",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := registryHost(test.repository); got != test.want {
				t.Fatalf("want %q, got %q", test.want, got)
			}
		})
	}
}

func TestDockerConfigJSON(t *testing.T) {
	// The shape oras' credential store reads, so a change to it fails here rather than at a
	// 401 against a real registry.
	got := string(dockerConfigJSON("registry.internal:5000", "zarf-pull", "hunter2"))
	want := `{"auths":{"registry.internal:5000":{"auth":"emFyZi1wdWxsOmh1bnRlcjI="}}}`
	if got != want {
		t.Fatalf("want %s, got %s", want, got)
	}
}
