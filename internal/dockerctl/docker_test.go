package dockerctl

import "testing"

func TestRepositoryOf(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/org/repo:abc123":      "ghcr.io/org/repo",
		"ghcr.io/org/repo":             "ghcr.io/org/repo",
		"localhost:5000/app:v1":        "localhost:5000/app",
		"localhost:5000/app":           "localhost:5000/app",
		"ghcr.io/org/repo@sha256:abcd": "ghcr.io/org/repo",
	}
	for in, want := range cases {
		if got := RepositoryOf(in); got != want {
			t.Errorf("RepositoryOf(%q) = %q, want %q", in, got, want)
		}
	}
}
