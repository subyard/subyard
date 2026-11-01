//go:build linux

package releaseruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func TestRustFreshRecoveryRefusalsPreserveFrozenProtocolAndOriginalErrors(t *testing.T) {
	plan := releasetransition.PlanToken("plan-v1-" + strings.Repeat("a", 64))
	tx := releasetransition.TransactionID("tx-original")
	previous := releasetransition.ReleaseID("old")
	request := releasetransition.RecoveryProcessRequest{SchemaVersion: 2, Mode: releasetransition.RecoveryProcessConverge,
		RuntimeRoot: "/tmp/fixture-runtime", ConfigHome: "/tmp/fixture-config", Yard: "default", Target: "new", Direction: releasetransition.DirectionActivateTarget,
		ArtifactDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)), RegistryDigest: releasetransition.Fingerprint(strings.Repeat("b", 64)), Contract: releasetransition.ActivationOnlyRecoveryContractV2,
		Recovery: &releasetransition.ActivationOnlyRecoveryRequest{Transaction: tx, Fingerprint: releasetransition.Fingerprint(strings.Repeat("c", 64))}, Execution: &releasetransition.Execution{Plan: plan}}
	response := releasetransition.ProcessResponse{SchemaVersion: 1, ActivationReconciliationOwned: true,
		Inspection: &releasetransition.Inspection{Outcome: &releasetransition.Outcome{Status: releasetransition.StatusRecovering, Active: "new", Previous: &previous, Target: "new", Transaction: &tx}}}
	before, _ := json.Marshal(response)
	cause := errors.New("original guarded refusal")
	for _, code := range []releasetransition.OutcomeCode{releasetransition.CodePlanStale, releasetransition.CodeConfirmationRequired} {
		refused, err := rustFreshRecoveryRefusal(&request, response, code, "fresh recovery requires review", cause)
		if err != nil || refused.Outcome == nil || refused.Inspection != nil || refused.Outcome.Code != code || refused.Outcome.Transaction == nil || *refused.Outcome.Transaction != tx || refused.Outcome.Active != "new" || refused.Outcome.Previous == nil || *refused.Outcome.Previous != previous {
			t.Fatal("fresh recovery lost its typed refusal or inspected identities", err)
		}
		if err := releasetransition.ValidateProcessOutcome(releasetransition.Goal{Target: "new", Direction: releasetransition.DirectionActivateTarget}, *refused.Outcome); err != nil {
			t.Fatal("refusal violated the frozen outcome contract", err)
		}
		wire := releasetransition.RecoveryProcessResponse{SchemaVersion: 2, ActivationReconciliationOwned: refused.ActivationReconciliationOwned, Outcome: refused.Outcome}
		if _, err := releasetransition.MarshalRecoveryProcessResponse(wire); err != nil {
			t.Fatal("typed refusal cannot cross the existing recovery codec", err)
		}
	}
	for _, mode := range []string{"ordinary", "migration-owner", "malformed", "missing-inspection", "native-error"} {
		t.Run(mode, func(t *testing.T) {
			recovery, inspected, code := request, response, releasetransition.CodePlanStale
			selected := &recovery
			switch mode {
			case "ordinary":
				selected = nil
			case "migration-owner":
				recovery.Recovery = nil
				recovery.MigrationOwner = &releasetransition.MigrationOwnerReplacementRequest{}
			case "malformed":
				recovery.SchemaVersion = 0
			case "missing-inspection":
				inspected.Inspection = nil
			case "native-error":
				code = releasetransition.CodeDependencyUnavailable
			}
			refused, err := rustFreshRecoveryRefusal(selected, inspected, code, "guarded refusal", cause)
			if err != cause || refused.Outcome != nil {
				t.Fatal("ordinary or unvalidated failure became a recovery outcome")
			}
		})
	}
	after, _ := json.Marshal(response)
	if string(before) != string(after) {
		t.Fatal("refusal presentation changed the retained inspection")
	}
}

func TestRustPreparedRetainsOriginalSessionAcrossLaterInspection(t *testing.T) {
	runtime, verified, request := transportRustSessionFixture(t)
	request.RegistryDigest = ""
	response, err := runtime.invokeRustOwnerTransition(context.Background(), verified, verified, verified, request, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	original := runtime.findRustOwnerSession(verified.manifestDigest, request, nil)
	if original == nil {
		t.Fatal("original preparation was not retained")
	}
	verification := candidateVerification{candidate: verified.candidate, digest: verified.manifestDigest, version: verified.version, registryDigest: verified.registryDigest}
	prepared, err := runtime.prepareInspectedCandidateTransition(options{root: request.RuntimeRoot}, verification, verification, request, *response.Inspection, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	other := request
	other.Yard = "another"
	if _, err := runtime.invokeRustOwnerTransition(context.Background(), verified, verified, verified, other, nil, ""); err != nil {
		t.Fatal(err)
	}
	later := runtime.findRustOwnerSession(verified.manifestDigest, other, nil)
	if later == nil || later == original {
		t.Fatal("independent inspection replaced the original session")
	}
	_, release, err := uninstall.OwnerLock(context.Background(), original.options.DataHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := prepared.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !original.used || !original.closed || later.used || later.closed {
		t.Fatal("prepared execution did not consume and close only its original process")
	}
}

const receiptTransportPython = `
plan='resume-v1-'+'c'*64
transaction='tx-0123456789abcdef'
while True:
 line=stream.readline()
 if not line:break
 request=json.loads(line)
 if request['op']=='inspect-migration-owner':
  inspection=dict(plan=plan,resume=transaction,assessment=dict(action='release.transition.v2',effect='mutation',changed=True,impacts=['local-metadata','persistent-data','yard-runtime'],recovery='reversible',consequences=['resume the exact durable successor']),outcome=dict(status='recovering',reachedGoal=False,active=options['releases']['from'],target=options['releases']['target'],transaction=transaction,code='recovery-pending',message='fixture durable receipt',retry='resume fixture'))
  send(dict(inspection=inspection,admission=dict(plan=plan,transaction=transaction),retainedReleases=sorted(set([options['releases']['from'],options['releases']['target']]))))
 elif request['op']=='resume':
  assert 'authorization' not in request
  marker,anc,flags,addr=custody.recvmsg(1,socket.CMSG_SPACE(4));assert marker==b'A'
  values=array.array('i');values.frombytes(anc[0][2]);assert len(values)==1
  length=struct.unpack('>I',custody.recv(4))[0];raw=b''
  while len(raw)<length:raw+=custody.recv(length-len(raw))
  admission=json.loads(raw)
  assert admission['session']==scope['session'] and admission['dataRoot']==scope['dataRoot']
  assert admission['plan']==plan and admission['transaction']==transaction
  assert admission['lock']==identity(os.fstat(values[0]))
  custody.sendall(json.dumps(dict(version=1,session=scope['session'],plan=plan,transaction=transaction,accepted=True)).encode()+b'\n')
  send(dict(outcome=dict(status='ready',reachedGoal=True,active=options['releases']['target'],previous=options['releases']['from'],target=options['releases']['target'],transaction=transaction,code='ready',message='fixture durable resume')))
  os.close(values[0])
 else:raise AssertionError(request['op'])
`

func TestRustReceiptAdmissionRetainsScopeAndUsesNoFreshGrant(t *testing.T) {
	for _, scenario := range []string{"valid", "ordinary-pending", "wrong-plan", "wrong-transaction", "wrong-session", "cleanup-inspection", "missing-retention", "ticket-plan-mismatch", "ticket-transaction-mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			runtime, verified, request := transportRustSessionFixture(t)
			script := strings.Split(transportOwnerPython, "\nplan=")[0] + receiptTransportPython
			if scenario == "ordinary-pending" {
				script = strings.Replace(script, "request['op']=='inspect-migration-owner'", "request['op']=='inspect'", 1)
				script = strings.Replace(script, "admission=dict(plan=plan,transaction=transaction),", "", 1)
				script = strings.Replace(script, "request['op']=='resume'", "request['op']=='execute'", 1)
				script = strings.Replace(script, "assert 'authorization' not in request", "assert request['plan']==plan and request['authorization']==''", 1)
			}
			if scenario == "cleanup-inspection" {
				script = strings.Replace(script, "request['op']=='inspect-migration-owner'", "request['op']=='inspect'", 1)
			}
			if scenario == "missing-retention" {
				script = strings.Replace(script, "retainedReleases=sorted(set([options['releases']['from'],options['releases']['target']]))", "retainedReleases=None", 1)
			}
			if scenario == "ticket-plan-mismatch" {
				script = strings.Replace(script, "admission=dict(plan=plan,transaction=transaction)", "admission=dict(plan='resume-v1-'+'d'*64,transaction=transaction)", 1)
			}
			if scenario == "ticket-transaction-mismatch" {
				script = strings.Replace(script, "admission=dict(plan=plan,transaction=transaction)", "admission=dict(plan=plan,transaction='tx-another')", 1)
			}
			path := filepath.Join(request.RuntimeRoot, "receipt-owner.py")
			testkit.WriteFile(t, path, []byte(script), 0o700)
			image, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			verified.releaseOwner.Close()
			verified.releaseOwner, err = sealVerifiedEngine(image, sha256.Sum256([]byte(script)))
			image.Close()
			if err != nil {
				t.Fatal(err)
			}
			session, err := runtime.startRustOwner(context.Background(), verified, verified, verified, request, "")
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			recovery := &releasetransition.RecoveryProcessRequest{MigrationOwner: &releasetransition.MigrationOwnerReplacementRequest{Transaction: "tx-predecessor", Fingerprint: releasetransition.Fingerprint(strings.Repeat("a", 64))}}
			if scenario == "cleanup-inspection" || scenario == "ordinary-pending" {
				recovery = nil
			}
			inspection, err := session.Inspect(context.Background(), recovery)
			if scenario == "missing-retention" || strings.HasPrefix(scenario, "ticket-") {
				if err == nil {
					t.Fatal("inspection acquired an inconsistent migration receipt ticket")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "ordinary-pending" && (session.admission != nil || inspection.Resume == nil) {
				t.Fatal("ordinary pending Resume acquired a special ticket or lost its transaction")
			}
			// Exercise the actual frozen PV1 codec and semantic validator, including
			// Changed=true/Recovering plus an exact resume-v1 plan and transaction.
			raw, err := json.Marshal(releasetransition.ProcessResponse{SchemaVersion: 1, Inspection: &inspection})
			if err != nil {
				t.Fatal(err)
			}
			var decoded releasetransition.ProcessResponse
			if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Inspection.Resume == nil || decoded.Inspection.Plan != inspection.Plan {
				t.Fatal("durable receipt inspection violated frozen PV1", err)
			}
			session.grant = ""
			switch scenario {
			case "wrong-plan":
				session.admission.Plan = releasetransition.PlanToken("resume-v1-" + strings.Repeat("d", 64))
			case "wrong-transaction":
				session.admission.Transaction = "tx-another"
			case "wrong-session":
				session.scope.Session = strings.Repeat("d", 64)
			}
			_, release, err := uninstall.OwnerLock(context.Background(), session.options.DataHome, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			outcome, err := session.Execute(context.Background(), inspection.Plan, "")
			if scenario == "valid" || scenario == "cleanup-inspection" || scenario == "ordinary-pending" {
				if err != nil || !outcome.ReachedGoal || outcome.Transaction == nil || *outcome.Transaction != *inspection.Resume {
					t.Fatal("checked receipt did not resume without a fresh grant", outcome, err)
				}
			} else if err == nil {
				t.Fatal("changed receipt admission scope was accepted", scenario)
			}
		})
	}
}

func TestRustReportCheckNegotiatesBeforeDispatchAndRetainsNoSession(t *testing.T) {
	for _, scenario := range []string{"absent-marker", "check-marker", "unsupported-check"} {
		t.Run(scenario, func(t *testing.T) {
			runtime, verified, request := transportRustSessionFixture(t)
			runtime.currentCheck = true
			verified.readOnlyCheck = scenario != "absent-marker"
			capture := filepath.Join(request.RuntimeRoot, "check-dispatch.log")
			script := strings.Replace(transportOwnerPython, " request=json.loads(line)", " request=json.loads(line)\n with open("+fmt.Sprintf("%q", capture)+",'a') as record:record.write(request['op']+'\\n')", 1)
			if scenario == "check-marker" {
				script = strings.Replace(script, "request['op']=='inspect'", "request['op']=='check'", 1)
			}
			path := filepath.Join(request.RuntimeRoot, "check-owner.py")
			testkit.WriteFile(t, path, []byte(script), 0700)
			image, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			verified.releaseOwner.Close()
			verified.releaseOwner, err = sealVerifiedEngine(image, sha256.Sum256([]byte(script)))
			image.Close()
			if err != nil {
				t.Fatal(err)
			}
			response, err := runtime.invokeRustOwnerTransition(context.Background(), verified, verified, verified, request, nil, "")
			if scenario == "unsupported-check" {
				if err == nil {
					t.Fatal("unsupported Check retried through Inspect")
				}
			} else if err != nil || response.Inspection == nil {
				t.Fatal("report dispatch", err)
			}
			raw, readErr := os.ReadFile(capture)
			if readErr != nil {
				t.Fatal(readErr)
			}
			want := "check\n"
			if scenario == "absent-marker" {
				want = "inspect\n"
			}
			if string(raw) != want || len(runtime.rustOwners.sessions) != 0 {
				t.Fatal("report retained authority or changed dispatch after an error")
			}
		})
	}
}
