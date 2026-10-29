package reconcileruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestReadOnlyProbeCancellationDrainsChildrenAndInheritedPipes(t *testing.T) {
	for _, service := range []bool{false, true} {
		t.Run(map[bool]string{false: "stage", true: "scoped service"}[service], func(t *testing.T) {
			root := testkit.TempDir(t)
			if err := os.Mkdir(filepath.Join(root, "scripts"), 0o700); err != nil {
				t.Fatal(err)
			}
			ready, late := filepath.Join(root, "ready"), filepath.Join(root, "late")
			script := `#!/bin/sh
sh -c 'sh -c '\''printf ready > "$READY"; sleep .3; printf escaped > "$LATE"'\'' & wait' &
wait
`
			name := "06-network.sh"
			if service {
				name = "profile-services.sh"
			}
			testkit.WriteFile(t, filepath.Join(root, "scripts", name), []byte(script), 0o700)
			runtime := Runtime{RepositoryRoot: root, Profiles: []profile.Definition{}, Environment: []string{"PATH=/usr/bin:/bin", "READY=" + ready, "LATE=" + late}}
			if !service {
				runtime.Yard = domain.Context{IncusProject: "subyard", YardInstanceName: "yard"}
				runtime.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{"subyard/yard": {Status: "running"}}}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				if service {
					_, err := runtime.ProfileServiceObservation(ctx, "synthetic")
					finished <- err
				} else {
					_, err := runtime.CheckStage(ctx, ports.ReconcileStageNetwork)
					finished <- err
				}
			}()
			deadline := time.Now().Add(time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("grandchild did not start")
				}
				time.Sleep(time.Millisecond)
			}
			cancel()
			select {
			case err := <-finished:
				if service && (err == nil || !strings.Contains(err.Error(), "profile service check unavailable")) {
					t.Fatalf("cancellation changed the closed service diagnostic: %v", err)
				}
			case <-time.After(250 * time.Millisecond):
				t.Fatal("probe retained its child/grandchild or inherited pipes")
			}
			time.Sleep(350 * time.Millisecond)
			if _, err := os.Stat(late); !os.IsNotExist(err) {
				t.Fatalf("canceled grandchild wrote after the probe returned: %v", err)
			}
		})
	}
}
