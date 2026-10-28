package releasetransition

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

// The oracle retains its own snapshot while the actual writer resumes. No
// serialized Go snapshot or timing guess substitutes for that process boundary.
const evidenceOracleProgram = `
import runpy, sys
from pathlib import Path
namespace = runpy.run_path(sys.argv[1])
root, destination, staged = Path(sys.argv[2]), sys.argv[3], sys.argv[4] == 'true'
capture, verify = namespace['capture_history_evidence'], namespace['verify_history_evidence']
snapshot = capture(root, [destination])
assert len(snapshot) == 2
assert sum(isinstance(value, namespace['StagedEvidence']) for value in snapshot.values()) == int(staged)
print('captured', flush=True)
assert sys.stdin.readline() == 'publish\n'
verify(root, snapshot, [destination])
snapshot = capture(root, [destination])
print('published', flush=True)
assert sys.stdin.readline() == 'retry\n'
verify(root, snapshot, [destination])
print('verified', flush=True)
`

func TestEvidenceOracleContract(t *testing.T) {
	for _, kind := range []string{"empty", "truncated", "full", "published"} {
		t.Run(kind, func(t *testing.T) {
			home := testkit.TempDir(t)
			store, err := NewPOSIXV2Store(home)
			if err != nil {
				t.Fatal(err)
			}
			transaction, step := TransactionID("tx-oracle"), "settings-v2"
			if err := store.CreateCheckpointEvidence(transaction, step, EvidenceCaptured, []byte(`{"schemaVersion":1,"captured":true}`)); err != nil {
				t.Fatal(err)
			}
			name, err := checkpointEvidenceName(transaction, step, EvidenceApplied)
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte(`{"schemaVersion":1,"applied":true}`)
			cut := errors.New("controlled evidence publication cut")
			point := "after-pending-fsync"
			if kind == "published" {
				point = "after-publish-before-dir-fsync"
			}
			store.fault = func(actual string) error {
				if actual == point {
					return cut
				}
				return nil
			}
			if kind == "empty" || kind == "truncated" {
				parent, present, err := store.openParent([]string{"release-transition", "v2", "transactions", string(transaction), "evidence"}, false)
				if err != nil || !present {
					t.Fatalf("evidence parent: present=%v err=%v", present, err)
				}
				defer unix.Close(parent)
				missing, err := readProtectedAt(parent, name)
				if err != nil {
					t.Fatal(err)
				}
				prefix := payload[:0]
				if kind == "truncated" {
					prefix = payload[:len(payload)/2]
				}
				// Private publication models a physically incomplete write; the
				// complete-record public API remains unchanged and is used to resume.
				err = store.publishAt(parent, name, missing, prefix, ErrProtectedStoreExists)
				if !errors.Is(err, cut) {
					t.Fatalf("partial staging cut: %v", err)
				}
			} else if err := store.CreateCheckpointEvidence(transaction, step, EvidenceApplied, payload); !errors.Is(err, cut) {
				t.Fatalf("publication cut: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			verifier, err := filepath.Abs("../../dev/verify-release-upgrades.py")
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(home, "release-transition", "v2", "transactions", string(transaction), "evidence")
			command := exec.CommandContext(ctx, "python3", "-B", "-c", evidenceOracleProgram, verifier, root, name, fmt.Sprint(kind != "published"))
			stdin, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			command.Stderr = &stderr
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = stdin.Close()
				if command.ProcessState == nil {
					_ = command.Process.Kill()
					_ = command.Wait()
				}
			}()
			reader := bufio.NewReader(stdout)
			handshake := func(want string) {
				t.Helper()
				line, err := reader.ReadString('\n')
				if err != nil || line != want+"\n" {
					_ = command.Process.Kill()
					_ = command.Wait()
					t.Fatalf("oracle handshake %q: got %q err=%v stderr=%s", want, line, err, stderr.String())
				}
			}
			handshake("captured")
			store.fault = nil
			if err := store.CreateCheckpointEvidence(transaction, step, EvidenceApplied, payload); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(stdin, "publish\n"); err != nil {
				t.Fatal(err)
			}
			handshake("published")
			if err := store.CreateCheckpointEvidence(transaction, step, EvidenceApplied, payload); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(stdin, "retry\n"); err != nil {
				t.Fatal(err)
			}
			handshake("verified")
			if err := command.Wait(); err != nil {
				t.Fatalf("oracle: %v stderr=%s", err, stderr.String())
			}
		})
	}
}

func TestEvidenceOracleContractRejectsMutants(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "python3", "-B", "../../tests/helpers/evidence-oracle-test.py")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("independent evidence negatives: %v\n%s", err, output)
	}
	t.Logf("%s", output)
}
