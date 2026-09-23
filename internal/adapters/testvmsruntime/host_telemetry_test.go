package testvmsruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKernelOOMEvidenceExcludesPrivatePathsAndCommands(t *testing.T) {
	body := []byte(`{"MESSAGE":"oom-kill: task_memcg=/private/customer/secret, command=secret-token","__REALTIME_TIMESTAMP":"12345"}
{"MESSAGE":"Out of memory: Killed process 123 (private-name)","__REALTIME_TIMESTAMP":"12346"}
{"MESSAGE":"unrelated private event","__REALTIME_TIMESTAMP":"12347"}`)
	got, err := kernelOOMEvidence(body)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(got)
	if len(got) != 2 || got[0].Kind != "oom_kill" || got[1].Kind != "killed_process" || strings.Contains(string(payload), "private") || strings.Contains(string(payload), "secret") {
		t.Fatalf("unsafe or missing evidence: %s", payload)
	}
	if _, err := kernelOOMEvidence([]byte(strings.Repeat("x", (1<<20)+1))); err == nil {
		t.Fatal("accepted unbounded evidence")
	}
}

func TestHostEvidencePersistsMissingMeasurementsAndDoesNotResampleRetry(t *testing.T) {
	calls := 0
	sink := HostSink{DataHome: t.TempDir(), Incus: "incus", OperatorGID: -1, Runner: &fakeRunner{handler: func(_ string, _, _ []string, _ io.Reader) ([]byte, []byte, error) {
		calls++
		return nil, nil, errors.New("private transport path")
	}}}
	incident := IncidentArtifact{IncidentID: strings.Repeat("a", 32)}
	instance := outerBrokerInstance{Name: "broker", Project: "test", Status: "RUNNING"}
	if err := sink.saveHostEvidence(context.Background(), instance, []IncidentArtifact{incident}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sink.DataHome, "logs", "test-vms-broker-incidents", "host", incident.IncidentID+".json")
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "allocation_state_and_cgroup") || !strings.Contains(string(first), "kernel_oom_log") || strings.Contains(string(first), "private transport") {
		t.Fatalf("missing explicit gaps: %s", first)
	}
	before := calls
	if err := sink.saveHostEvidence(context.Background(), instance, []IncidentArtifact{incident}); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if calls != before || string(first) != string(second) {
		t.Fatal("retry changed immutable observation")
	}
}
