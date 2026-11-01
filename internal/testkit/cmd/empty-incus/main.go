// empty-incus serves the native empty-instance Incus fixture for release tests.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/testkit"
)

func main() {
	if len(os.Args) == 5 && os.Args[1] == "--source-admission" {
		if err := sourceAdmissionProof(os.Args[2], os.Args[3], os.Args[4]); err != nil {
			fmt.Fprintln(os.Stderr, "empty-incus: source admission refused")
			os.Exit(1)
		}
		return
	}
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
		Instances []instance                `json:"instances"`
		Journal   string                    `json:"holdJournal"`
		History   *historyControl           `json:"migrationHistory"`
		Configs   *configObservationControl `json:"materializedObservation"`
	}
	var observationMu sync.Mutex
	var pending <-chan struct{}
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
			server.SetInventory(projects, instances)
			if value.History != nil {
				server.SetExecResponder(historyResponder(*value.History, path))
			} else {
				release := pending
				server.SetExecResponder(func(call testkit.IncusServerExecCall) testkit.IncusServerExecStep {
					if value.Configs != nil && len(call.Command) > 0 && (strings.HasPrefix(call.Command[len(call.Command)-1], "configs-") || len(call.Command) >= 11 && call.Command[0] == "python3" || slices.Equal(call.Command, []string{"bash", "-s", "--", "--observe"})) {
						return configObservationStep(*value.Configs, call, release)
					}
					return testkit.IncusServerExecStep{Stdout: []byte(`{"converged":false,"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`), Release: release}
				})
			}
			_ = os.WriteFile(path+".applied", payload, 0o600)
		}
		return value
	}
	queue := func() chan struct{} {
		release := make(chan struct{})
		observationMu.Lock()
		pending = release
		observationMu.Unlock()
		read()
		return release
	}
	release := queue()
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

// This companion mode returns only the existing finite source inspector's
// binding. It does not start an Incus server or provide an execution boundary.
func sourceAdmissionProof(runtimeRoot, configHome, repository string) error {
	inspector := releaseruntime.NewSourceInspector(repository, os.Stderr)
	defer inspector.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	proof, err := inspector.InspectSourceAdmission(ctx, runtimeRoot, configHome)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Binding string `json:"binding"`
	}{Binding: proof.Binding})
}
