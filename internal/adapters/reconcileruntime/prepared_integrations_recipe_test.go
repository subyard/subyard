package reconcileruntime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func materializedIntegrationFixture(t *testing.T, root string, create bool) (Runtime, *retryableIntegrationExecutor) {
	t.Helper()
	if create {
		files := map[string]string{
			"config/projects-changed.sh":            "#!/bin/sh\nexit 0\n",
			"scripts/reconcile-integrations.sh":     "#!/bin/sh\ncat \"$AGENT_sample_PROVISION\" >\"$RESULT\"\nprintf installed >\"$HOOK_STATE\"\n",
			"config/agents/sample/integration.json": `{"schema_version":1}`,
			"config/agents/sample/runtime.sh": `#!/bin/sh
case "$1" in
observe) cat "$LIFECYCLE_STATE";;
apply) printf approved-lifecycle >"$LIFECYCLE_RESULT"; cp "$LIFECYCLE_NEXT" "$LIFECYCLE_STATE"; cat "$LIFECYCLE_STATE";;
*) exit 92;;
esac
`,
			"config/agents/sample/payload.txt": "approved dependency\n",
			"provision.sh":                     "#!/bin/sh\n# approved original provision\n",
			"lifecycle-state":                  `{"state":"stale","actual":"` + testRuntimeOldDigest + `","desired":"` + testRuntimeNewDigest + `"}`,
			"lifecycle-next":                   `{"state":"current","actual":"` + testRuntimeNewDigest + `","desired":"` + testRuntimeNewDigest + `"}`,
		}
		for path, content := range files {
			full := filepath.Join(root, path)
			if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, full, []byte(content), 0o755)
		}
	}
	executor := &retryableIntegrationExecutor{pending: true}
	executor.observeHooks = func() hookObservation {
		observation := hookObservation{Projects: testRuntimeOldDigest, Wiring: testRuntimeNewDigest}
		if _, err := os.Stat(filepath.Join(root, "hook-state")); err == nil {
			observation.Hooks = []string{"sample-projects"}
			observation.Facts = map[string]string{"sample-projects": testRuntimeNewDigest}
			observation.Resolved = map[string]string{"sample-projects": "/usr/local/bin/sample-projects"}
		}
		return observation
	}
	incus := runningIncus(0)
	incus.Reconcile.Instance.Config = map[string]string{"volatile.uuid": "original-instance"}
	return Runtime{RepositoryRoot: root, Profiles: []profile.Definition{}, Integrations: []profile.IntegrationDefinition{{Name: "sample", Root: filepath.Join(root, "config/agents/sample"), Lifecycle: &profile.IntegrationLifecycle{Handler: "runtime.sh", SourceFiles: []string{"payload.txt"}}}}, Incus: incus, Executor: executor,
		Yard: domain.Context{IncusProject: "test", YardInstanceName: "yard", DevUser: "dev", DevUID: os.Getuid()}, Environment: []string{"PATH=/usr/bin:/bin", "SUBYARD_OPERATION_ID=materialized-fixture", "CODING_TOOL_INTEGRATIONS=sample", "AGENT_sample_COMMAND=sample", "AGENT_sample_PROJECTS_CHANGED=sample-projects", "AGENT_sample_PROVISION=" + filepath.Join(root, "provision.sh"), "RESULT=" + filepath.Join(root, "result"), "HOOK_STATE=" + filepath.Join(root, "hook-state"), "LIFECYCLE_STATE=" + filepath.Join(root, "lifecycle-state"), "LIFECYCLE_NEXT=" + filepath.Join(root, "lifecycle-next"), "LIFECYCLE_RESULT=" + filepath.Join(root, "lifecycle-result")}}, executor
}
