// empty-incus serves the native empty-instance Incus fixture for release tests.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
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
// are never executed; each receives the same read-only observation response.
func observeFixture(ctx context.Context, server *testkit.IncusServer, path string) {
	type instance struct {
		Project string         `json:"project"`
		Name    string         `json:"name"`
		Info    map[string]any `json:"info"`
	}
	type control struct {
		Instances []instance `json:"instances"`
		Journal   string     `json:"holdJournal"`
	}
	read := func() control {
		var value control
		payload, err := os.ReadFile(path)
		if err == nil && json.Unmarshal(payload, &value) == nil {
			for _, instance := range value.Instances {
				server.SetProject(instance.Project, map[string]any{"name": instance.Project, "config": map[string]string{}})
				server.SetInstance(instance.Project, instance.Name, instance.Info)
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
