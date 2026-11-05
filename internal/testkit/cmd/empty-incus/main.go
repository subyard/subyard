// empty-incus serves the native empty-instance Incus fixture for release tests.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

func main() {
	if len(os.Args) != 2 && len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "empty-incus: expected a private fixture root and optional observation file")
		os.Exit(2)
	}
	server, err := testkit.NewIncusServer(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "empty-incus: cannot start fixture server")
		os.Exit(1)
	}
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if len(os.Args) == 3 {
		server.SetExtensions("projects", "instances", "instance_all_projects", "storage", "network", "container_exec_user_group_cwd")
		go observeFixture(ctx, server, os.Args[2])
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	fmt.Println("ready")
	<-stop
}

// The optional control file changes only canned API observations. Guest commands
// are never executed. The history control selects exact canned command responses
// and records simulated effects on fixture-owned files.
func observeFixture(ctx context.Context, server *testkit.IncusServer, path string) {
	type instance struct {
		Project string         `json:"project"`
		Name    string         `json:"name"`
		Info    map[string]any `json:"info"`
	}
	type control struct {
		Instances          []instance                `json:"instances"`
		Journal            string                    `json:"holdJournal"`
		History            *historyControl           `json:"migrationHistory"`
		Projects           map[string]map[string]any `json:"projects"`
		DiskTmpCheckSHA256 string                    `json:"diskTmpCheckSHA256"`
		ActivationFailure  string                    `json:"activationFailure"`
	}
	var observationMu sync.Mutex
	read := func() control {
		observationMu.Lock()
		defer observationMu.Unlock()
		var value control
		payload, err := os.ReadFile(path)
		if err == nil && json.Unmarshal(payload, &value) == nil {
			projects := make(map[string]map[string]any)
			instances := make(map[string]map[string]any)
			for _, instance := range value.Instances {
				projects[instance.Project] = map[string]any{"name": instance.Project, "config": map[string]string{}}
				instances[instance.Project+"/"+instance.Name] = instance.Info
			}
			for name, project := range value.Projects {
				projects[name] = project
			}
			server.SetInventory(projects, instances)
			if value.History != nil {
				server.SetExecResponder(historyResponder(*value.History, path))
			} else if value.DiskTmpCheckSHA256 != "" {
				server.SetExecResponder(directOwnedResponder(value.DiskTmpCheckSHA256, path, value.ActivationFailure))
			} else {
				server.SetExecResponder(nil)
			}
			_ = os.WriteFile(path+".applied", payload, 0o600)
		}
		return value
	}
	go func() {
		for {
			read()
			select {
			case <-ctx.Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	queue := func() chan struct{} {
		release := make(chan struct{})
		server.QueueExec(testkit.IncusServerExecStep{
			Stdout:  []byte(`{"converged":false,"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`),
			Release: release,
		})
		return release
	}
	release := queue()
	for count := 1; ; count++ {
		if err := server.WaitForExecCount(ctx, count); err != nil {
			close(release)
			return
		}
		for {
			value := read()
			var journal struct {
				Checkpoint string `json:"checkpoint"`
			}
			payload, _ := os.ReadFile(value.Journal)
			_ = json.Unmarshal(payload, &journal)
			if value.Journal == "" || journal.Checkpoint != "reconciling" {
				break
			}
			_ = os.WriteFile(path+".held", []byte("reconciling\n"), 0o600)
			select {
			case <-ctx.Done():
				close(release)
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
		// Queue the next canned observation before the current request completes.
		next := queue()
		close(release)
		release = next
	}
}

// This opt-in models the direct-caller case: owned, empty integrations and an
// already installed disk policy. Unknown commands and mutations fail closed.
func directOwnedResponder(digest string, paths ...string) func(testkit.IncusServerExecCall) testkit.IncusServerExecStep {
	return func(call testkit.IncusServerExecCall) testkit.IncusServerExecStep {
		if call.Project != "subyard" || call.Name != "yard" {
			return testkit.IncusServerExecStep{ExitCode: 99}
		}
		// Same exact read-only integration substrate observation as historyResponder.
		if slices.Equal(call.Command, []string{"sh", "-eu", "-c", `command -v python3 >/dev/null; id "$1" >/dev/null; test -d /srv/agents`, "subyard", "dev"}) {
			return testkit.IncusServerExecStep{}
		}
		if len(call.Command) == 11 && slices.Equal(call.Command[:3], []string{"sh", "-eu", "-c"}) && call.Command[4] == "subyard" {
			program := fmt.Sprintf("%x", sha256.Sum256([]byte(call.Command[3])))
			if (program == "a01443d0ba01dfecb656b59ab5c0e32e84c9a57445354d1832c37e2b63ea1851" || program == "61f1199f9be495a3427b873b7a677f10bc663908324357062657d7aa7890be48") &&
				(call.Command[5] == "1a99d38f52be1c6aead189ccdbd2f16bd24be92e3110c9915ee6624e7e0b1ddf" || call.Command[5] == "bfcc059c108b8bc9af4b06b01beba490357d72ef02b697b49a796386478423e0" || call.Command[5] == "1abec2e90dd14f1a2e860eb35bba95e97a50ee7958020c9578a394b02ca70d53") &&
				slices.Equal(call.Command[6:], []string{"01ba4719c80b6fe911b091a7c05124b64eeece964e09c058ef8f9805daca546b", "/usr/local/libexec/subyard/projects-changed", "/etc/subyard/agent-project-hooks", "/usr/local/libexec/subyard/projects-changed.d", "0:0"}) {
				return testkit.IncusServerExecStep{}
			}
		}
		if len(call.Command) == 7 && slices.Equal(call.Command[:3], []string{"sh", "-eu", "-c"}) && slices.Equal(call.Command[4:], []string{"subyard", "observe", "true"}) &&
			fmt.Sprintf("%x", sha256.Sum256([]byte(call.Command[3]))) == "8195718501349e512581cbebc2b569cce3f86ab129873b6b5f78def1a99a9a2f" {
			return testkit.IncusServerExecStep{Stdout: []byte("changed=0\n")}
		}
		if len(call.Command) == 8 && slices.Equal(call.Command[:3], []string{"sh", "-eu", "-c"}) && slices.Equal(call.Command[4:], []string{"subyard", "observe", "", "true"}) {
			program := fmt.Sprintf("%x", sha256.Sum256([]byte(call.Command[3])))
			if program == "a320d966cbd1e55825c506d7de86331caab144c3b18b5acc442191decd8531de" || program == "fb927909c321199bd8846421bc0e29fc2b62c8642fd0aa2d24c84323839037f2" || program == "4ba7a0856a03cb4fd293c9aa217be900b3f1e1df433b466cc5de94bd35ef6a26" {
				return testkit.IncusServerExecStep{Stdout: []byte("changed=0\n")}
			}
		}
		// The direct-caller case selects no integrations: only the two project
		// hook artifacts may be observed, with the exact released/current reader.
		if len(call.Command) == 5 && slices.Equal(call.Command[:3], []string{"python3", "-B", "-c"}) && (call.Command[4] == "observe" || call.Command[4] == "observe-applied") {
			program := fmt.Sprintf("%x", sha256.Sum256([]byte(call.Command[3])))
			if program == "2193bc75e21ea70024008739bb74004f92a2d3ffc5894de7ca017f2d4291be2e" || program == "b0000549b3ec4cd53824d5143b79b05b1b78a253d0e4aceb30342321535e3372" || program == "f13739c75b5a44dd280e3464d9fb2fc7a4ff7a24a6d4fc9f7998e9cab1edb5ce" || program == "853f6adfd1780ea684f1211a3f2a33b35d762d8bedd99993f5a0e5bea2e9e473" {
				if len(paths) > 1 && paths[1] != "" {
					payload, err := os.ReadFile(paths[1])
					if err == nil {
						var failure struct{ Journal, Marker string }
						if json.Unmarshal(payload, &failure) != nil {
							return testkit.IncusServerExecStep{ExitCode: 99}
						}
						payload, err = os.ReadFile(failure.Journal)
						var journal struct{ Checkpoint string }
						if err != nil || json.Unmarshal(payload, &journal) != nil {
							return testkit.IncusServerExecStep{ExitCode: 99}
						}
						if journal.Checkpoint == "reconciling" {
							if os.WriteFile(failure.Marker, []byte("controlled core activation observation failure after migration effects\n"), 0600) != nil {
								return testkit.IncusServerExecStep{ExitCode: 99}
							}
							return testkit.IncusServerExecStep{ExitCode: 65, Stderr: []byte("controlled core activation observation failure after migration effects\n")}
						}
					} else if !os.IsNotExist(err) {
						return testkit.IncusServerExecStep{ExitCode: 99}
					}
				}
				return testkit.IncusServerExecStep{Stdout: []byte(`{"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","changed":false,"facts":[]}`), AfterInput: func(actual testkit.IncusServerExecCall) int {
					var input struct {
						Home, Developer string
						UID             int
						Entries         []struct{ ID, Kind, Path string }
					}
					if json.Unmarshal(actual.Stdin, &input) != nil || input.Home != "/home/dev" || input.Developer != "dev" || input.UID != 1000 || len(input.Entries) != 2 {
						return 99
					}
					seen := map[string]bool{}
					for _, entry := range input.Entries {
						if entry.ID != "_projects" || entry.Kind != "file" || (entry.Path != "/usr/local/libexec/subyard/projects-changed" && entry.Path != "/etc/subyard/agent-project-hooks") || seen[entry.Path] {
							return 99
						}
						seen[entry.Path] = true
					}
					return 0
				}}
			}
		}
		if !slices.Equal(call.Command, []string{"sh", "-s", "--", "check"}) {
			if len(paths) != 0 {
				command := slices.Clone(call.Command)
				if len(command) > 3 {
					command[3] = fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(command[3])))
				}
				payload, _ := json.Marshal(map[string]any{"project": call.Project, "name": call.Name, "command": command})
				_ = os.WriteFile(paths[0]+".unexpected", payload, 0600)
			}
			return testkit.IncusServerExecStep{ExitCode: 99}
		}
		return diskTmpCheckResponse(digest)
	}
}

// The fixture models an already installed policy; only its exact read-only check is supported.
func diskTmpCheckResponse(digest string) testkit.IncusServerExecStep {
	return testkit.IncusServerExecStep{AfterInput: func(actual testkit.IncusServerExecCall) int {
		if len(actual.Stdin) == 0 || fmt.Sprintf("%x", sha256.Sum256(actual.Stdin)) != digest {
			return 99
		}
		return 0
	}}
}
