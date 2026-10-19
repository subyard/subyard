//go:build linux

package releaseruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

// The fixture owns transport only. Actual Rust policy/native operations have
// separate opt-in process coverage using the production owner and helper.
const transportOwnerPython = `#!/usr/bin/python3
import array,base64,json,os,socket,struct
peer=socket.socket(fileno=0)
byte,anc,flags,addr=peer.recvmsg(1,socket.CMSG_SPACE(4*4))
rights=[]
for level,kind,data in anc:
 if level==socket.SOL_SOCKET and kind==socket.SCM_RIGHTS:
  values=array.array('i');values.frombytes(data);rights+=values.tolist()
assert byte==b'C' and len(rights)==4
helper,grant,custody,repository=rights
secret=os.read(grant,1024).decode()
custody=socket.socket(fileno=custody)
stream=peer.makefile('rb')
def send(value):
 if 'inspection' in value or 'outcome' in value:value.setdefault('retainedReleases',[])
 peer.sendall(json.dumps(value,separators=(',',':')).encode()+b'\n')
request=json.loads(stream.readline());options=request['options']
assert request['op']=='open' and secret
assert base64.b64decode(options['registryRaw'])==b'{}\n'
assert options['repositoryRoot']==options['runtimeRoot']+'/releases/'+options['releases']['target']
assert os.fstat(repository).st_ino==options['targetArtifact']['root']['inode']
def identity(stat):return dict(device=stat.st_dev,inode=stat.st_ino,mode=stat.st_mode,uid=stat.st_uid,links=stat.st_nlink)
scope=dict(session='b'*64,dataRoot=identity(os.stat(options['dataHome'])))
send(dict(custody=scope))
plan='plan-v1-'+'a'*64
while True:
 line=stream.readline()
 if not line:break
 request=json.loads(line)
 if request['op']=='inspect':
  send(dict(inspection=dict(plan=plan,assessment=dict(action='release.transition.v2',effect='mutation',changed=True,impacts=['local-metadata','persistent-data','yard-runtime'],recovery='reversible',consequences=['fixture transport']),outcome=dict(status='migration-required',reachedGoal=False,active=options['releases']['from'],target=options['releases']['target'],code='transition-required',message='fixture preparation',retry='confirm fixture'))))
 elif request['op']=='execute':
  assert request['plan']==plan and request['authorization']==secret
  marker,anc,flags,addr=custody.recvmsg(1,socket.CMSG_SPACE(4));assert marker==b'A'
  values=array.array('i');values.frombytes(anc[0][2]);assert len(values)==1
  length=struct.unpack('>I',custody.recv(4))[0]
  raw=b''
  while len(raw)<length:raw+=custody.recv(length-len(raw))
  admission=json.loads(raw)
  assert admission['session']==scope['session'] and admission['plan']==plan
  assert admission['lock']==identity(os.fstat(values[0]))
  custody.sendall(json.dumps(dict(version=1,session=scope['session'],plan=plan,transaction='',accepted=True)).encode()+b'\n')
  send(dict(outcome=dict(status='ready',reachedGoal=True,active=options['releases']['target'],previous=options['releases']['from'],target=options['releases']['target'],code='ready',message='fixture execution',transaction='tx-0123456789abcdef')))
  os.close(values[0])
 else:raise AssertionError(request)
`

func transportRustSessionFixture(t *testing.T) (*Runtime, *verifiedPublishedCandidate, releasetransition.ProcessRequest) {
	t.Helper()
	root := testkit.TempDir(t)
	for _, relative := range []string{"data", "state", "home"} {
		if err := os.Mkdir(filepath.Join(root, relative), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := writeDelegateRuntimeFixture(t, root, "1.2.3-old", "source", []byte("{}\n"), "#!/bin/sh\nprintf 'yard-engine 1.2.3\\n'\n")
	_ = old
	target := writeDelegateRuntimeFixture(t, root, "1.2.3-new", "target", []byte("{}\n"), "#!/bin/sh\nprintf 'yard-engine 1.2.3\\n'\n")
	if err := os.Symlink("releases/1.2.3-old", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	runtime := New(Config{Inputs: CaptureCommandInputs([]string{"HOME=" + filepath.Join(root, "home")}, nil), Context: RuntimeContext{DataHome: filepath.Join(root, "data")}, Stderr: io.Discard})
	t.Cleanup(func() { runtime.Close() })
	verified, err := runtime.verifyPublishedCandidate(context.Background(), target, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { verified.Close() })
	path := filepath.Join(root, "transport-owner.py")
	testkit.WriteFile(t, path, []byte(transportOwnerPython), 0o700)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	verified.releaseOwner, err = sealVerifiedEngine(file, sha256.Sum256([]byte(transportOwnerPython)))
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	request := releasetransition.ProcessRequest{SchemaVersion: 1, Mode: releasetransition.ProcessInspect,
		RuntimeRoot: root, ConfigHome: filepath.Join(root, "state"), Yard: "default", Target: "1.2.3-new",
		Direction: releasetransition.DirectionActivateTarget, ArtifactDigest: verified.manifestDigest, RegistryDigest: verified.registryDigest}
	return runtime, verified, request
}

func TestRustSessionSurvivesPrepareContextAndConsumesOneScope(t *testing.T) {
	runtime, verified, request := transportRustSessionFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	session, err := runtime.startRustOwner(ctx, verified, verified, verified, request, "")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	cancel()
	inspection, err := session.Inspect(context.Background(), nil)
	if err != nil {
		t.Fatal("preparation request cancellation killed retained owner", err)
	}
	if _, err := os.Stat(filepath.Join(request.ConfigHome, "migrations/update.lock")); !os.IsNotExist(err) {
		t.Fatal("read-only transport acquired a mutation lease")
	}
	if _, err := session.Execute(context.Background(), releasetransition.PlanToken("plan-v1-"+strings.Repeat("c", 64)), ""); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("different plan entered retained operation", err)
	}
	_, donorClose, err := uninstall.OwnerLock(context.Background(), session.options.DataHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer donorClose()
	outcome, err := session.Execute(context.Background(), inspection.Plan, "")
	if err != nil || outcome.Status != releasetransition.StatusReady {
		t.Fatalf("retained transport execution: %#v %v", outcome, err)
	}
	if _, err := session.Execute(context.Background(), inspection.Plan, ""); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("consumed retained scope replayed", err)
	}
}

func TestRustSessionExpiryAndCloseDoNotExtendPreparation(t *testing.T) {
	runtime, verified, request := transportRustSessionFixture(t)
	session, err := runtime.startRustOwner(context.Background(), verified, verified, verified, request, "")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	session.mu.Lock()
	session.expires = time.Now().Add(-time.Second)
	session.mu.Unlock()
	if _, err := session.Inspect(context.Background(), nil); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("expired original preparation acquired a new lifetime", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.done:
	case <-time.After(2 * time.Second):
		t.Fatal("read-only Close did not reap its retained owner")
	}
	if _, err := session.Inspect(context.Background(), nil); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("closed preparation reopened", err)
	}
}

func TestRustSessionInvalidInheritedBarrierCannotUseRegistrySubstitute(t *testing.T) {
	home := testkit.TempDir(t)
	_, release, err := uninstall.OwnerLock(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, descriptor := range []string{"invalid", "2", "999999"} {
		file, err := borrowRustOwnerBarrier(home, descriptor)
		if file != nil {
			file.Close()
		}
		if err == nil {
			t.Fatal("invalid supplied original barrier was replaced by registry borrower", descriptor)
		}
	}
}
