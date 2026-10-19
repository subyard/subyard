//go:build linux

package releaseruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

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
	for _, scenario := range []string{"valid", "wrong-plan", "wrong-transaction", "wrong-session", "cleanup-inspection", "missing-retention", "ticket-plan-mismatch", "ticket-transaction-mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			runtime, verified, request := transportRustSessionFixture(t)
			script := strings.Split(transportOwnerPython, "\nplan=")[0] + receiptTransportPython
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
			if scenario == "cleanup-inspection" {
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
			if scenario == "valid" || scenario == "cleanup-inspection" {
				if err != nil || !outcome.ReachedGoal || outcome.Transaction == nil || *outcome.Transaction != session.admission.Transaction {
					t.Fatal("checked receipt did not resume without a fresh grant", outcome, err)
				}
			} else if err == nil {
				t.Fatal("changed receipt admission scope was accepted", scenario)
			}
		})
	}
}
