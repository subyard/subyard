package reconcileruntime

import (
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Only the non-default approval mode enters provisioned package identity.
func TestGitApprovalModeBindsOnlyProvisionedPackages(t *testing.T) {
	root := testkit.TempDir(t)
	provision := filepath.Join(root, "provision.sh")
	testkit.WriteFile(t, provision, []byte("#!/usr/bin/env bash\ntrue\n"), 0o755)
	digests := func(mode string) map[string]string {
		t.Helper()
		environment := []string{"CODING_TOOL_INTEGRATIONS=hooked plain", "AGENT_hooked_PROVISION=" + provision, "AGENT_plain_CHECK=plain-check"}
		if mode != "unset" {
			environment = append(environment, "CODING_TOOL_GIT_APPROVAL="+mode)
		}
		runtime := Runtime{RepositoryRoot: root, Integrations: []profile.IntegrationDefinition{}, Environment: environment}
		entries, err := runtime.integrationPackageArtifacts("inspect", nil)
		if err != nil || len(entries) != 2 {
			t.Fatalf("package artifacts for %s: %v %#v", mode, err, entries)
		}
		return map[string]string{entries[0].ID: entries[0].Digest, entries[1].ID: entries[1].Digest}
	}
	unset, prompt, off := digests("unset"), digests("prompt"), digests("off")
	if unset["hooked"] != prompt["hooked"] || unset["plain"] != prompt["plain"] {
		t.Fatalf("default mode changed package identity: %v %v", unset, prompt)
	}
	if off["hooked"] == prompt["hooked"] {
		t.Fatal("off mode did not change the provisioned package identity")
	}
	if off["plain"] != prompt["plain"] {
		t.Fatal("off mode changed a package without a provision hook")
	}
}
