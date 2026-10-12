package testvmsruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestPressureEvidenceParsesOnlyBoundedNumericCounters(t *testing.T) {
	valid := "some avg10=0.01 avg60=1.23 avg300=4.56 total=789\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"
	counters, err := parsePressure([]byte(valid))
	if err != nil || counters["some"].Total != 789 || counters["some"].Avg60 != 1.23 {
		t.Fatalf("counters=%+v err=%v", counters, err)
	}
	for _, body := range []string{"", valid + valid, strings.Replace(valid, "0.01", "NaN", 1), strings.Replace(valid, "0.01", "Inf", 1), strings.Replace(valid, "0.01", "101", 1), strings.Replace(valid, "total=789", "total=-1", 1), valid + "secret=value\n", strings.Repeat("x", 4097)} {
		if _, err := parsePressure([]byte(body)); err == nil {
			t.Fatal("invalid pressure counters accepted")
		}
	}
	root := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, "pressure"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "pressure", "cpu"), []byte(valid), 0600)
	got, missing := pressureEvidence(root)
	if len(got) != 1 || len(missing) != 2 || missing[0] != "io" || missing[1] != "memory" {
		t.Fatalf("pressure=%+v missing=%v", got, missing)
	}
}

func TestInstanceEvidenceProjectsExpandedAllowlistAndPolicyIdentity(t *testing.T) {
	_, knownPolicy := vmSleepPolicy("x86_64")
	for _, policy := range []string{knownPolicy, "private unknown qemu config"} {
		body, _ := json.Marshal(map[string]any{
			"architecture": "x86_64", "config": map[string]string{"private": "secret"},
			"expanded_config":  map[string]string{"volatile.base_image": strings.Repeat("a", 64), "limits.cpu": "2", "limits.memory": "4GiB", "security.secureboot": "false", "raw.qemu.conf": policy, "cloud-init.user-data": "private secret", "user.private": "private secret"},
			"expanded_devices": map[string]any{"root": map[string]string{"type": "disk", "path": "/", "size": "10GiB", "source": "private disk"}, "secret": map[string]string{"source": "private key"}},
		})
		got := instanceEvidence(string(body))
		if strings.Contains(got, "private") || strings.Contains(got, "secret") || !strings.Contains(got, `"memory_bytes":4294967296`) || !strings.Contains(got, `"secure_boot":false`) || !strings.Contains(got, `"root_disk_bytes":10737418240`) {
			t.Fatalf("unsafe or incomplete projection: %s", got)
		}
		if policy == knownPolicy && !strings.Contains(got, "subyard-x86-s3-v1") {
			t.Fatal("known policy identity lost")
		}
		if policy != knownPolicy && !strings.Contains(got, "vm_policy_sha256") {
			t.Fatal("unknown policy digest lost")
		}
	}
	for _, body := range []string{`{}`, `{"architecture":"private","expanded_config":{"limits.cpu":"private secret","limits.memory":"private secret","security.secureboot":"private"}}`} {
		got := instanceEvidence(body)
		if !strings.Contains(got, "unavailable") || strings.Contains(got, "private") || strings.Contains(got, "secret") {
			t.Fatalf("missing or unsafe projection: %s", got)
		}
	}
	if got := instanceEvidence(strings.Repeat("x", (1<<20)+1)); !strings.HasPrefix(got, "unavailable:") {
		t.Fatal("accepted unbounded instance metadata")
	}
}

func TestQuarantineReadinessFailurePrioritizesFailedVMAndRetainsSafeTiming(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.AgentPublicKey = ""
	store := LeaseStore{Path: cfg.leaseState(), SlotCount: cfg.SlotCount}
	grant, err := store.AcquireSlot("client", "SHA256:key", "", "fixture", "slot-001")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Unix(1800000000, 0)
	failure := &readinessFailure{Label: "waiting for e2e-vm-2 Incus agent", VM: "e2e-vm-2", Phase: "incus-agent", Budget: time.Second, StartedAt: started, ObservedAt: started.Add(time.Second), LastProbe: "error_kind=command_error exit_code=23 duration=1s"}
	runner := &fakeRunner{handler: func(_ string, args, _ []string, _ io.Reader) ([]byte, []byte, error) {
		joined := strings.Join(args, " ")
		if joined == "project list --format csv -c n" {
			return nil, nil, nil
		}
		if strings.HasPrefix(joined, "exec ") {
			t.Fatal("diagnostics required the unavailable guest agent")
		}
		if strings.HasPrefix(joined, "list e2e-vm-") {
			return []byte("RUNNING"), nil, nil
		}
		return nil, nil, errors.New("private argument and stderr")
	}}
	rt := &Runtime{Config: cfg, Runner: runner, Stdout: io.Discard, Stderr: io.Discard}
	if err := rt.QuarantineSlot(context.Background(), store, grant, fmt.Errorf("provisioning: %w", failure)); err != nil {
		t.Fatal(err)
	}
	batch, err := rt.eventRecorder().Export()
	if err != nil || len(batch.Incidents) != 1 {
		t.Fatalf("incident=%+v err=%v", batch, err)
	}
	diagnostics := batch.Incidents[0].Diagnostics
	if !strings.Contains(diagnostics["readiness"], `"phase":"incus-agent"`) || !strings.Contains(diagnostics["readiness"], `"elapsed_ms":1000`) || !strings.Contains(diagnostics["readiness"], `"vm":"e2e-vm-2"`) {
		t.Fatalf("missing timing: %v", diagnostics)
	}
	payload, _ := json.Marshal(batch)
	if strings.Contains(string(payload), "private argument") {
		t.Fatal("raw native error entered spool")
	}
	consoles := map[string]int{}
	for i, call := range runner.calls {
		if len(call) > 1 && call[1] == "console" {
			consoles[call[2]]++
			if len(consoles) == 1 && (!strings.Contains(strings.Join(runner.calls[i-1], " "), "list e2e-vm-2") || call[2] != "e2e-vm-2") {
				t.Fatal("failed VM was not captured first")
			}
		}
	}
	if consoles["e2e-vm-1"] != 1 || consoles["e2e-vm-2"] != 1 {
		t.Fatalf("console reads=%v", consoles)
	}
}
