package reconcileruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRefreshConfigsUsesTypedAtomicGuestWrites(t *testing.T) {
	root := t.TempDir()
	source := func(name, payload string) string {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	environment := []string{
		"CODING_TOOL_INTEGRATIONS=opencode",
		"HOST_CLAUDE_MD=" + source("CLAUDE.md", "claude\n"),
		"HOST_CODEX_AGENTS_MD=" + source("CODEX.md", "codex\n"),
		"HOST_OPENCODE_AGENTS_MD=" + source("OPENCODE.md", "opencode\n"),
		"AGENT_opencode_CONFIG=" + source("opencode.jsonc", "{}\n"),
		"AGENT_opencode_CONFIG_DEST=.config/opencode/opencode.jsonc",
		"AGENT_opencode_RULES=" + source("repo.rules", "rule\n"),
		"AGENT_opencode_RULES_DEST=.config/opencode/repo.rules",
	}
	incus := runningIncus(10)
	var output bytes.Buffer
	runtime := Runtime{
		Environment: environment, Incus: incus, Executor: incus, Stdout: &output,
		Yard: domain.Context{
			IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev", DevUID: 1001,
		},
	}
	if err := runtime.RefreshConfigs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RefreshConfigs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(incus.ExecCalls) != 10 {
		t.Fatalf("config refresh calls = %d, want 6 writes and 4 inventory operations", len(incus.ExecCalls))
	}
	for index, mode := range map[int]string{0: "configs-prepare", 4: "configs-commit", 5: "configs-prepare", 9: "configs-commit"} {
		if command := incus.ExecCalls[index].Request.Command; len(command) != 5 || command[4] != mode {
			t.Fatalf("inventory operation %d: %v", index, command)
		}
	}
	writes := append(incus.ExecCalls[1:4:4], incus.ExecCalls[6:9]...)
	wantDestinations := []string{
		"/home/dev/.config/opencode/AGENTS.md",
		"/home/dev/.config/opencode/opencode.jsonc",
		"/home/dev/.config/opencode/repo.rules",
	}
	for index, call := range writes {
		if call.Project != "subyard" || call.Name != "yard" ||
			len(call.Request.Command) != 7 ||
			call.Request.Command[0] != "sh" ||
			!strings.Contains(call.Request.Command[3], "mktemp") ||
			call.Request.Command[6] != "1001" {
			t.Fatalf("config write %d is not the atomic typed contract: %#v", index, call)
		}
		destination := call.Request.Command[5]
		if !strings.HasPrefix(destination, "/home/dev/") ||
			strings.Contains(destination, "auth.json") {
			t.Fatalf("unsafe config destination %q", destination)
		}
		if !slices.Contains(wantDestinations, destination) {
			t.Fatalf("unselected agent config destination %q", destination)
		}
		if index >= 3 {
			first := writes[index-3].Request
			if !slices.Equal(first.Command, call.Request.Command) ||
				!bytes.Equal(first.Stdin, call.Request.Stdin) {
				t.Fatalf("config refresh %d is not idempotent", index-3)
			}
		}
	}
	if !strings.Contains(output.String(), "Agent instructions and configs refreshed") {
		t.Fatalf("refresh output changed: %q", output.String())
	}
}

func TestConfigsConvergedComparesSourceAndGuestHashesReadOnly(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "AGENTS.md")
	payload := []byte("current instructions\n")
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	effective, err := (guestConfigFile{source: source, previewInstructions: true}).readSource()
	if err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(effective))
	for _, test := range []struct {
		name   string
		stdout string
		want   bool
	}{
		{name: "converged", stdout: hash + "  /home/dev/.codex/AGENTS.md\n", want: true},
		{name: "drifted", stdout: strings.Repeat("0", 64) + "  /home/dev/.codex/AGENTS.md\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			incus := runningIncus(1)
			incus.ExecSteps[0].Result.Stdout = []byte(test.stdout)
			runtime := Runtime{
				Environment: []string{"CODING_TOOL_INTEGRATIONS=codex", "HOST_CODEX_AGENTS_MD=" + source},
				Incus:       incus, Executor: incus,
				Yard: domain.Context{IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev"},
			}
			got, err := runtime.ConfigsConverged(context.Background())
			if err != nil || got != test.want {
				t.Fatalf("converged=%t err=%v", got, err)
			}
			if len(incus.ExecCalls) != 1 || !slices.Equal(
				incus.ExecCalls[0].Request.Command,
				[]string{"sha256sum", "--", "/home/dev/.codex/AGENTS.md"},
			) {
				t.Fatalf("hash probe=%#v", incus.ExecCalls)
			}
		})
	}
}

func TestRefreshConfigsRejectsPathsOutsideDeveloperHome(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config")
	if err := os.WriteFile(config, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	incus := runningIncus(0)
	runtime := Runtime{
		Environment: []string{
			"CODING_TOOL_INTEGRATIONS=opencode",
			"AGENT_opencode_CONFIG=" + config,
			"AGENT_opencode_CONFIG_DEST=../../root/.config",
		},
		Incus: incus, Executor: incus,
		Yard: domain.Context{
			IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev",
		},
	}
	if err := runtime.RefreshConfigs(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "leaves the developer home") {
		t.Fatalf("unsafe config destination error = %v", err)
	}
	if len(incus.ExecCalls) != 0 {
		t.Fatalf("unsafe config caused guest execution: %#v", incus.ExecCalls)
	}
}

func TestRefreshConfigsRejectsSymlinkSourceBeforeGuestMutation(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "config.toml")
	if err := os.WriteFile(target, []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "linked.toml")
	if err := os.Symlink(target, source); err != nil {
		t.Fatal(err)
	}
	incus := runningIncus(0)
	runtime := Runtime{
		Environment: []string{
			"CODING_TOOL_INTEGRATIONS=codex",
			"AGENT_codex_CONFIG=" + source,
			"AGENT_codex_CONFIG_DEST=.codex/config.toml",
		},
		Incus: incus, Executor: incus,
		Yard: domain.Context{
			IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev",
		},
	}
	if err := runtime.RefreshConfigs(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink source error = %v", err)
	}
	if len(incus.ExecCalls) != 0 {
		t.Fatalf("symlink source caused guest execution: %#v", incus.ExecCalls)
	}
}

func TestRefreshConfigsFollowsHostInstructionSymlink(t *testing.T) {
	root := t.TempDir()
	claudeDirectory := filepath.Join(root, ".claude")
	dotfilesDirectory := filepath.Join(root, "dotfiles", "agents")
	for _, directory := range []string{claudeDirectory, dotfilesDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	payload := []byte("operator instructions\n")
	target := filepath.Join(dotfilesDirectory, "AGENTS.md")
	if err := os.WriteFile(target, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(claudeDirectory, "CLAUDE.md")
	if err := os.Symlink("../dotfiles/agents/AGENTS.md", source); err != nil {
		t.Fatal(err)
	}
	incus := runningIncus(3)
	runtime := Runtime{
		Environment: []string{"CODING_TOOL_INTEGRATIONS=claude", "HOST_CLAUDE_MD=" + source},
		Incus:       incus, Executor: incus,
		Yard: domain.Context{
			IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev",
		},
	}
	if err := runtime.RefreshConfigs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(incus.ExecCalls) != 3 ||
		incus.ExecCalls[1].Request.Command[5] != "/home/dev/.claude/CLAUDE.md" ||
		!bytes.HasPrefix(incus.ExecCalls[1].Request.Stdin, payload) ||
		!bytes.Contains(incus.ExecCalls[1].Request.Stdin, []byte(previewInstructions)) {
		t.Fatalf("symlinked Claude instructions were not copied: %#v", incus.ExecCalls)
	}
}

func TestHostInstructionSourceRejectsDanglingSymlink(t *testing.T) {
	source := filepath.Join(t.TempDir(), "CLAUDE.md")
	if err := os.Symlink("missing.md", source); err != nil {
		t.Fatal(err)
	}
	file := guestConfigFile{
		label: "Claude instructions", source: source, followSymlinks: true,
	}
	for _, operation := range guestConfigSourceOperations() {
		t.Run(operation.name, func(t *testing.T) {
			err := operation.run(file)
			if err == nil || os.IsNotExist(err) || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("dangling symlink error = %v", err)
			}
		})
	}
}

func TestHostInstructionSourceRejectsFIFOWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "instructions.fifo")
	if err := syscall.Mkfifo(target, 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "CLAUDE.md")
	if err := os.Symlink("instructions.fifo", source); err != nil {
		t.Fatal(err)
	}
	file := guestConfigFile{
		label: "Claude instructions", source: source, followSymlinks: true,
	}
	for _, operation := range guestConfigSourceOperations() {
		t.Run(operation.name, func(t *testing.T) {
			result := make(chan error, 1)
			go func() { result <- operation.run(file) }()
			select {
			case err := <-result:
				if err == nil || !strings.Contains(err.Error(), "not a regular file") {
					t.Fatalf("FIFO error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("opening the FIFO source blocked")
			}
		})
	}
}

func guestConfigSourceOperations() []struct {
	name string
	run  func(guestConfigFile) error
} {
	return []struct {
		name string
		run  func(guestConfigFile) error
	}{
		{name: "read", run: func(file guestConfigFile) error {
			_, err := file.readSource()
			return err
		}},
		{name: "hash", run: func(file guestConfigFile) error {
			_, err := file.sourceHash()
			return err
		}},
	}
}

func TestApplyGitIdentityUsesTypedDeveloperCommands(t *testing.T) {
	incus := runningIncus(3)
	runtime := Runtime{
		Environment: []string{"GIT_USER_NAME=Developer", "GIT_USER_EMAIL=dev@example.test"},
		Incus:       incus, Executor: incus,
		Yard: domain.Context{
			IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev", DevUID: 1001,
			Paths: domain.RuntimePaths{DataHome: t.TempDir()},
		},
	}
	if err := runtime.applyGitIdentity(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"git", "config", "--global", "user.name", "Developer"},
		{"git", "config", "--global", "user.email", "dev@example.test"},
		{"git", "config", "--global", "--replace-all", "safe.directory", "*"},
	}
	if len(incus.ExecCalls) != len(want) {
		t.Fatalf("git identity calls = %#v", incus.ExecCalls)
	}
	for index, call := range incus.ExecCalls {
		if !slices.Equal(call.Request.Command, want[index]) ||
			call.Request.User != 1001 || call.Request.Group != 1001 ||
			call.Request.Environment["HOME"] != "/home/dev" {
			t.Fatalf("git identity call %d = %#v", index, call)
		}
	}
}

func TestApplyGitIdentityCopiesOperatorDropin(t *testing.T) {
	dataHome := t.TempDir()
	payload := []byte("[user]\n\tname = Operator\n")
	if err := os.WriteFile(filepath.Join(dataHome, "gitconfig"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	incus := runningIncus(2)
	runtime := Runtime{
		Incus: incus, Executor: incus,
		Yard: domain.Context{
			IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev",
			Paths: domain.RuntimePaths{DataHome: dataHome},
		},
	}
	if err := runtime.applyGitIdentity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(incus.ExecCalls) != 2 ||
		incus.ExecCalls[0].Request.Command[5] != "/home/dev/.gitconfig" ||
		!bytes.Equal(incus.ExecCalls[0].Request.Stdin, payload) ||
		!slices.Equal(incus.ExecCalls[1].Request.Command,
			[]string{"git", "config", "--global", "--replace-all", "safe.directory", "*"}) {
		t.Fatalf("operator gitconfig was not copied through typed execution: %#v", incus.ExecCalls)
	}
}

func runningIncus(execCount int) *testkit.Incus {
	steps := make([]testkit.IncusExecStep, execCount)
	for index := range steps {
		steps[index].Result = ports.InstanceExecResult{}
	}
	return &testkit.Incus{
		Reconcile: ports.ReconcileState{
			InstanceFound: true,
			Instance:      ports.InstanceInfo{Status: "Running"},
		},
		ExecSteps: steps,
	}
}

func TestConfigsConvergedUsesOwnedTOMLObservationForImportedSource(t *testing.T) {
	source := filepath.Join(t.TempDir(), "imported-config")
	if err := os.WriteFile(source, []byte("model = \"gpt-5\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	incus := runningIncus(2)
	incus.ExecSteps[0].Result.Stdout = []byte(fmt.Sprintf("%x  /home/dev/.codex/AGENTS.md\n", sha256.Sum256([]byte(previewInstructions))))
	incus.ExecSteps[1].Result.Stdout = []byte(`{"converged":true,"fingerprint":"` + strings.Repeat("a", 64) + `"}`)
	runtime := Runtime{
		Environment: []string{"CODING_TOOL_INTEGRATIONS=codex", "AGENT_codex_CONFIG=" + source, "AGENT_codex_CONFIG_DEST=.codex/config.toml"},
		Incus:       incus, Executor: incus,
		Yard: domain.Context{IncusProject: "subyard", YardInstanceName: "yard", DevUser: "dev"},
	}
	converged, err := runtime.ConfigsConverged(context.Background())
	if err != nil || !converged {
		t.Fatalf("owned TOML observation rejected: converged=%v err=%v", converged, err)
	}
	request := incus.ExecCalls[1].Request
	if len(request.Command) == 0 || request.Command[0] != "python3" || len(request.Stdin) == 0 {
		t.Fatal("TOML observation did not use the guest ownership adapter")
	}
}

func TestPreviewInstructionsComposeOnlySelectedEffectivePayloads(t *testing.T) {
	root := testkit.TempDir(t)
	source := filepath.Join(root, "instructions.md")
	host := []byte("Host instructions stay byte-for-byte intact.")
	testkit.WriteFile(t, source, host, 0o600)
	for _, agent := range []string{"claude", "codex", "opencode"} {
		t.Run(agent, func(t *testing.T) {
			key := map[string]string{"claude": "HOST_CLAUDE_MD", "codex": "HOST_CODEX_AGENTS_MD", "opencode": "HOST_OPENCODE_AGENTS_MD"}[agent]
			runtime := Runtime{Environment: []string{
				"CODING_TOOL_INTEGRATIONS=" + agent, key + "=" + source,
				"AGENT_" + agent + "_RULES=" + source, "AGENT_" + agent + "_RULES_DEST=rules",
			}}
			files, err := runtime.guestConfigFiles()
			if err != nil || len(files) != 2 || files[0].integration != agent {
				t.Fatalf("selected files=%#v err=%v", files, err)
			}
			effective, err := files[0].readSource()
			if err != nil || !bytes.HasPrefix(effective, host) || bytes.Count(effective, []byte("<!-- subyard-preview -->")) != 1 {
				t.Fatalf("effective instruction=%q err=%v", effective, err)
			}
			again, err := files[0].readSource()
			if err != nil || !bytes.Equal(effective, again) {
				t.Fatalf("repeat composition changed bytes: %v", err)
			}
			asset, err := files[1].readSource()
			if err != nil || !bytes.Equal(asset, host) {
				t.Fatalf("rules asset changed: %v", err)
			}
			digest, err := files[0].sourceHash()
			if err != nil || digest != fmt.Sprintf("%x", sha256.Sum256(effective)) || digest == fmt.Sprintf("%x", sha256.Sum256(host)) {
				t.Fatalf("effective hash=%q err=%v", digest, err)
			}
			legacy, err := files[0].legacySourceHash()
			if err != nil || legacy != fmt.Sprintf("%x", sha256.Sum256(host)) {
				t.Fatalf("legacy source hash=%q err=%v", legacy, err)
			}
			assetLegacy, err := files[1].legacySourceHash()
			if err != nil || assetLegacy != "" {
				t.Fatalf("config asset has legacy instruction evidence: %q %v", assetLegacy, err)
			}
			original, err := os.ReadFile(source)
			if err != nil || !bytes.Equal(original, host) {
				t.Fatal("host instructions changed")
			}
			files[0].source = filepath.Join(root, "missing")
			missing, err := files[0].readSource()
			if err != nil || string(missing) != previewInstructions {
				t.Fatalf("missing source effective instruction=%q err=%v", missing, err)
			}
			if legacy, err := files[0].legacySourceHash(); err != nil || legacy != "" {
				t.Fatalf("missing source has legacy evidence: %q %v", legacy, err)
			}
			files[0].source = ""
			empty, err := files[0].readSource()
			if err != nil || string(empty) != previewInstructions {
				t.Fatalf("unset source effective instruction=%q err=%v", empty, err)
			}
		})
	}
	for _, environment := range [][]string{
		{"CODING_TOOL_INTEGRATIONS=pi"},
		{"CODING_TOOL_INTEGRATIONS=codex", "ALLOWS_CODING_TOOLS=false"},
		{"CODING_TOOL_INTEGRATIONS="},
	} {
		files, err := (Runtime{Environment: environment}).guestConfigFiles()
		if err != nil || len(files) != 0 {
			t.Fatalf("unselected instruction files=%#v err=%v", files, err)
		}
	}
	composed := filepath.Join(root, "composed.md")
	testkit.WriteFile(t, composed, append(append([]byte(nil), host...), previewInstructions...), 0o600)
	result, err := (guestConfigFile{source: composed, previewInstructions: true}).readSource()
	if err != nil || bytes.Count(result, []byte(previewInstructions)) != 1 {
		t.Fatalf("duplicate preview paragraph: %v", err)
	}
}
