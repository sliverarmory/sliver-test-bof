package harness

import (
	"slices"
	"strings"
	"testing"
)

func TestSanitizedServerEnvExcludesCredentialsAndBuildControls(t *testing.T) {
	env := sanitizedServerEnv([]string{
		"PATH=/usr/bin",
		"LANG=en_US.UTF-8",
		"GITHUB_TOKEN=secret",
		"ACTIONS_RUNTIME_TOKEN=runtime-secret",
		"GOFLAGS=-toolexec=evil",
		"GOWORK=/tmp/evil.work",
		"UNRELATED=value",
	}, "/private/home", "/private/tmp", "/private/server", "/private/client")

	for _, forbidden := range []string{"GITHUB_TOKEN=", "ACTIONS_RUNTIME_TOKEN=", "GOFLAGS=", "GOWORK=", "UNRELATED="} {
		if slices.ContainsFunc(env, func(entry string) bool { return strings.HasPrefix(entry, forbidden) }) {
			t.Fatalf("sanitized server environment retained %q: %v", forbidden, env)
		}
	}
	for _, required := range []string{
		"PATH=/usr/bin", "LANG=en_US.UTF-8", "HOME=/private/home", "USERPROFILE=/private/home",
		"TMP=/private/tmp", "TEMP=/private/tmp", "TMPDIR=/private/tmp",
		"SLIVER_ROOT_DIR=/private/server", "SLIVER_CLIENT_ROOT_DIR=/private/client",
	} {
		if !slices.Contains(env, required) {
			t.Fatalf("sanitized server environment missing %q: %v", required, env)
		}
	}
}
