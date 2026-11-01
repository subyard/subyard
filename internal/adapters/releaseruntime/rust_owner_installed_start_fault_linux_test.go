//go:build linux

package releaseruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/lxc/incus/v6/shared/api"
)

const installedStartProject = "subyard-test-yard"
const installedStartInstance = "yard-test-yard"

// Existing private create/closure records; no new admission or product schema.
type installedGraphCreateIntent struct {
	Version       int                  `json:"version"`
	Binding       releaseowner.Binding `json:"binding"`
	Recipe        string               `json:"recipe"`
	Source        string               `json:"source"`
	Scope         string               `json:"scope"`
	Kind          string               `json:"kind"`
	Domain        string               `json:"domain"`
	Index         uint64               `json:"index"`
	Token         string               `json:"token"`
	RequestDigest string               `json:"requestDigest"`
	Server        string               `json:"server"`
	Project       string               `json:"project"`
	Resource      string               `json:"resource"`
	Pool          string               `json:"pool"`
	Fingerprint   string               `json:"fingerprint,omitempty"`
}
type installedGraphCreateResult struct {
	Version        int                        `json:"version"`
	Intent         installedGraphCreateIntent `json:"intent"`
	OperationID    string                     `json:"operationID"`
	Status         int                        `json:"status"`
	InstanceUUID   string                     `json:"instanceUUID"`
	IdentityDigest string                     `json:"identityDigest"`
	ETagDigest     string                     `json:"etagDigest"`
}
type installedGraphCreateClosure struct {
	Version  int                           `json:"version"`
	Binding  releaseowner.Binding          `json:"binding"`
	Intent   string                        `json:"intent"`
	Accepted string                        `json:"accepted"`
	Terminal string                        `json:"terminal"`
	Commands []installedGraphCreateCommand `json:"commands"`
	Facts    []installedGraphCreateFact    `json:"facts"`
}
type installedGraphCreateCommand struct {
	Kind   string `json:"kind"`
	Source string `json:"source"`
}
type installedGraphCreateFact struct {
	Name   string `json:"name"`
	Size   uint64 `json:"size"`
	SHA256 string `json:"sha256"`
}

func installedGraphCanonical(value any) []byte {
	raw, _ := json.Marshal(value)
	return append(raw, '\n')
}

func installedGraphDecodeFact(raw []byte, value any) bool {
	return len(raw) <= 1<<20 && json.Unmarshal(raw, value) == nil && bytes.Equal(raw, installedGraphCanonical(value))
}

// This fixture owns one absent daemon runtime leaf before the real create.
// The original SDK still talks directly to the default local Incus endpoint.
type installedStartFault struct {
	client        *http.Client
	config        string
	pid           int
	start         string
	identity      string
	armed, closed bool
	began         time.Time
	uuid, tx      string
}

func (fault *installedStartFault) logStage(t *testing.T, stage string) {
	t.Logf("startup compensation stage=%s elapsed_ms=%d utc=%s", stage, time.Since(fault.began).Milliseconds(), time.Now().UTC().Format(time.RFC3339Nano))
}

func (fault *installedStartFault) get(ctx context.Context, path string, value any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://incus"+path, nil)
	if err != nil {
		return err
	}
	response, err := fault.client.Do(request)
	if err != nil {
		return errors.New("real daemon observation unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return os.ErrNotExist
	}
	if response.StatusCode != http.StatusOK {
		return errors.New("real daemon observation refused")
	}
	var envelope api.Response
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 || json.Unmarshal(raw, &envelope) != nil || envelope.Type != api.SyncResponse || envelope.StatusCode != 200 || json.Unmarshal(envelope.Metadata, value) != nil {
		return errors.New("real daemon observation malformed")
	}
	return nil
}

func (fault *installedStartFault) instance(ctx context.Context, project, name string) (api.Instance, error) {
	var instance api.Instance
	err := fault.get(ctx, "/1.0/instances/"+name+"?project="+project, &instance)
	return instance, err
}

// These are assertions over existing protected native facts, not an alternate
// admission path: the real owner/helper still validate and execute the capsule.
func installedGraphFacts(config string) (releasetransition.JournalRecord, map[string][]byte, error) {
	store, err := releasetransition.NewPOSIXV2Store(config)
	if err != nil {
		return releasetransition.JournalRecord{}, nil, err
	}
	current, err := store.ReadCurrentJournal()
	if err != nil || !current.Exists {
		return releasetransition.JournalRecord{}, nil, errors.New("original journal unavailable")
	}
	journal, err := releasetransition.ParseJournal(current.Payload)
	if err != nil {
		return journal, nil, err
	}
	directory := filepath.Join(config, "migrations/native-custody/v1", string(journal.Transaction))
	for path := directory; path != config; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
			return journal, nil, errors.New("unsafe native custody ancestry")
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) > 4096 {
		return journal, nil, errors.New("native custody census unavailable")
	}
	facts := map[string][]byte{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		file, err := os.OpenFile(filepath.Join(directory, entry.Name()), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return journal, nil, err
		}
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) || info.Sys().(*syscall.Stat_t).Nlink != 1 || info.Size() > 1<<20 {
			file.Close()
			return journal, nil, errors.New("unsafe native custody fact")
		}
		raw, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
		file.Close()
		if err != nil || len(raw) > 1<<20 {
			return journal, nil, errors.New("native custody fact bound")
		}
		facts[entry.Name()] = raw
	}
	return journal, facts, nil
}

func installedGraphCreated(facts map[string][]byte, tx, scope, project, instanceName, uuid, token string) bool {
	ok, _ := installedGraphCreatedDiagnostic(facts, tx, scope, project, instanceName, uuid, token)
	return ok
}

func installedGraphCreatedDiagnostic(facts map[string][]byte, tx, scope, project, instanceName, uuid, token string) (bool, string) {
	found := 0
	var ordinal, index uint64
	diagnostic := func(code string) (bool, string) {
		return code == "ok", fmt.Sprintf("code=%s candidates=%d ordinal=%d index=%d", code, found, ordinal, index)
	}
	for _, name := range slices.Sorted(maps.Keys(facts)) {
		if !strings.Contains(name, ".operation-") || !strings.HasSuffix(name, "-create-result.json") {
			continue
		}
		var result installedGraphCreateResult
		if !installedGraphDecodeFact(facts[name], &result) {
			return diagnostic("result-decode")
		}
		intent := result.Intent
		if intent.Scope != scope || intent.Project != project || intent.Resource != instanceName || result.InstanceUUID != uuid || intent.Token != token {
			continue
		}
		found++
		ordinal, index = intent.Binding.Ordinal, intent.Index
		var root releaseowner.Intent
		prefix := fmt.Sprintf("%03d.", intent.Binding.Ordinal)
		operation := fmt.Sprintf("%soperation-%03d-", prefix, intent.Index)
		if result.Version != 1 {
			return diagnostic("result-version")
		}
		if result.Status != 200 {
			return diagnostic("status")
		}
		if intent.Version != 1 {
			return diagnostic("intent-version")
		}
		if intent.Binding.Transaction != tx {
			return diagnostic("transaction")
		}
		if intent.Kind != "instance" || intent.Domain != "operation" {
			return diagnostic("kind-domain")
		}
		if len(token) != 32 || strings.Trim(token, "0123456789abcdef") != "" {
			return diagnostic("token-shape")
		}
		if intent.Index == 0 || intent.Index > 256 || intent.Pool != "" || !domain.SafeID(result.OperationID) || len(result.OperationID) > 256 || !domain.SafeID(uuid) || len(uuid) > 256 {
			return diagnostic("create-shape")
		}
		for _, hash := range []string{intent.Recipe, intent.Source, intent.Server, intent.RequestDigest, result.IdentityDigest, result.ETagDigest} {
			if len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "" {
				return diagnostic("create-digest")
			}
		}
		var original installedGraphCreateIntent
		if !installedGraphDecodeFact(facts[operation+"create-intent.json"], &original) || original != intent {
			return diagnostic("create-pair")
		}
		if _, exists := facts[prefix+"intent.json"]; !exists {
			return diagnostic("root-missing")
		}
		if !installedGraphDecodeFact(facts[prefix+"intent.json"], &root) {
			return diagnostic("root-decode")
		}
		if root.Binding != intent.Binding || root.Owner != "owner-registration" || root.Resource != "test-yard" || root.Recipe != intent.Recipe {
			return diagnostic("root-binding")
		}
		if root.Source != intent.Source {
			return diagnostic("root-source")
		}
		want := ports.NativeOperation{Kind: "owner-create-instance", Server: intent.Server, Project: project, Resource: intent.Resource, ID: result.OperationID, Status: 200}
		if _, compacted := facts[prefix+"closure.json"]; compacted {
			if code := installedGraphCreateClosed(facts, root, intent, want); code != "ok" {
				return diagnostic(code)
			}
		} else {
			for _, phase := range []string{"intent", "accepted", "terminal"} {
				expected := want
				if phase != "terminal" {
					expected.Status = 0
				}
				if phase == "intent" {
					expected.ID = ""
				}
				raw, exists := facts[operation+phase+".json"]
				if !exists {
					return diagnostic(phase + "-missing")
				}
				if !bytes.Equal(raw, installedGraphCanonical(expected)) {
					return diagnostic(phase + "-mismatch")
				}
			}
		}
	}
	if found == 0 {
		return diagnostic("candidate-absent")
	}
	if found != 1 {
		return diagnostic("candidate-duplicate")
	}
	return diagnostic("ok")
}

// Match the immutable Rust closure, including every original indexed witness
// and any residual left by an interrupted compaction. A create is not Ready.
func installedGraphCreateClosed(facts map[string][]byte, root releaseowner.Intent, create installedGraphCreateIntent, operation ports.NativeOperation) string {
	prefix := fmt.Sprintf("%03d.", create.Binding.Ordinal)
	accepted := releaseowner.Terminal{Version: 1, Binding: create.Binding, Owner: root.Owner, Resource: root.Resource, State: "accepted", Operations: []ports.NativeOperation{}}
	var whole releaseowner.Terminal
	if !bytes.Equal(facts[prefix+"accepted.json"], installedGraphCanonical(accepted)) || !installedGraphDecodeFact(facts[prefix+"terminal.json"], &whole) || whole.Version != 1 || whole.Binding != create.Binding || whole.Owner != root.Owner || whole.Resource != root.Resource || whole.State != "complete" && whole.State != "failed" {
		return "closure-whole"
	}
	if len(whole.Operations) > 256 || create.Index > uint64(len(whole.Operations)) || whole.Operations[create.Index-1] != operation {
		return "closure-operation"
	}
	var closure installedGraphCreateClosure
	if !installedGraphDecodeFact(facts[prefix+"closure.json"], &closure) || closure.Version != 1 || closure.Binding != create.Binding || closure.Commands == nil || closure.Facts == nil || len(closure.Commands) > 256 || len(closure.Facts) != 3*len(whole.Operations)+2*len(closure.Commands) {
		return "closure-binding"
	}
	hash := func(raw []byte) string { return fmt.Sprintf("%x", sha256.Sum256(raw)) }
	if closure.Intent != hash(facts[prefix+"intent.json"]) || closure.Accepted != hash(facts[prefix+"accepted.json"]) || closure.Terminal != hash(facts[prefix+"terminal.json"]) {
		return "closure-hash"
	}
	expected := map[string][]byte{}
	for index, final := range whole.Operations {
		for _, phase := range []string{"intent", "accepted", "terminal"} {
			value := final
			if phase != "terminal" {
				value.Status = 0
			}
			if phase == "intent" {
				value.ID = ""
			}
			expected[fmt.Sprintf("%soperation-%03d-%s.json", prefix, index+1, phase)] = installedGraphCanonical(value)
		}
	}
	for index, command := range closure.Commands {
		for _, phase := range []string{"intent", "terminal"} {
			expected[fmt.Sprintf("%scommand-%03d-%s.json", prefix, index+1, phase)] = installedGraphCanonical(command)
		}
	}
	previous := ""
	for _, fact := range closure.Facts {
		raw, exists := expected[fact.Name]
		if !exists || fact.Name <= previous || fact.Size != uint64(len(raw)) || fact.SHA256 != hash(raw) {
			return "closure-fact"
		}
		previous = fact.Name
	}
	for name, residual := range facts {
		parts := strings.Split(strings.TrimPrefix(name, prefix), "-")
		if !strings.HasPrefix(name, prefix) || len(parts) != 3 || parts[0] != "operation" && parts[0] != "command" || parts[2] != "intent.json" && parts[2] != "accepted.json" && parts[2] != "terminal.json" {
			continue
		}
		if raw, exists := expected[name]; !exists || !bytes.Equal(residual, raw) {
			return "closure-residual"
		}
	}
	return "ok"
}

func installedGraphCurrentCreatedUUID(facts map[string][]byte, tx string) (string, bool, string) {
	uuid := ""
	count, reason := 0, "code=candidate-absent candidates=0 ordinal=0 index=0"
	for _, name := range slices.Sorted(maps.Keys(facts)) {
		if !strings.Contains(name, ".operation-") || !strings.HasSuffix(name, "-create-result.json") {
			continue
		}
		var result struct {
			Intent       struct{ Scope, Project, Resource, Token string }
			InstanceUUID string
		}
		if json.Unmarshal(facts[name], &result) != nil {
			return "", false, fmt.Sprintf("code=result-decode candidates=%d ordinal=0 index=0", count)
		}
		if result.Intent.Scope == "current" && result.Intent.Project == installedStartProject && result.Intent.Resource == installedStartInstance {
			count++
			if uuid != "" {
				return "", false, fmt.Sprintf("code=candidate-duplicate candidates=%d ordinal=0 index=0", count)
			}
			var ok bool
			if ok, reason = installedGraphCreatedDiagnostic(facts, tx, "current", installedStartProject, installedStartInstance, result.InstanceUUID, result.Intent.Token); !ok {
				return "", false, reason
			}
			uuid = result.InstanceUUID
		}
	}
	if uuid == "" && count != 0 {
		reason = fmt.Sprintf("code=uuid-empty candidates=%d ordinal=0 index=0", count)
	}
	return uuid, uuid != "", reason
}

// One exact self-bind mount survives Incus Create's ignored runtime cleanup;
// the original SDK then fails when Start tries to write lxc.conf as a file.
const installedStartLeaf = `import json,os,stat,subprocess,sys
pid,start,action=sys.argv[1:4]
assert open('/proc/'+pid+'/stat').read().rsplit(')',1)[1].split()[19]==start
assert os.path.basename(os.readlink('/proc/'+pid+'/exe'))=='incusd'
assert os.stat('/proc/self/ns/mnt')==os.stat('/proc/'+pid+'/ns/mnt')
assert all(not x.startswith(b'INCUS_DIR=') or x==b'INCUS_DIR=' for x in open('/proc/'+pid+'/environ','rb').read().split(b'\0'))
flags=os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW
base=os.open('/run/incus',flags);s=os.fstat(base);assert s.st_uid==0 and s.st_mode&0o022==0
parent='subyard-test-yard_yard-test-yard'
path='/run/incus/'+parent+'/lxc.conf'
def mount_id():
 ids=[int(v[0]) for line in open('/proc/self/mountinfo') if len(v:=line.split())>4 and v[4]==path]
 assert len(ids)<=1
 return ids[0] if ids else 0
if action=='arm':
 os.mkdir(parent,0o700,dir_fd=base)
try:fd=os.open(parent,flags,dir_fd=base)
except FileNotFoundError:
 assert action=='absent';sys.exit(0)
s=os.fstat(fd);assert s.st_uid==0 and stat.S_IMODE(s.st_mode)==0o700
if action=='arm':
 try:os.mkdir('lxc.conf',0o700,dir_fd=fd)
 except BaseException:
  os.rmdir(parent,dir_fd=base);raise
 bound=False
 try:
  leaf=os.stat('lxc.conf',dir_fd=fd,follow_symlinks=False)
  assert stat.S_ISDIR(leaf.st_mode) and leaf.st_uid==0 and stat.S_IMODE(leaf.st_mode)==0o700 and mount_id()==0
  subprocess.run(['mount','--bind',path,path],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
  bound=True
  mounted=mount_id();assert mounted>0
 except BaseException:
  if bound:subprocess.run(['umount',path],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
  os.rmdir('lxc.conf',dir_fd=fd);os.rmdir(parent,dir_fd=base);raise
 print(json.dumps([s.st_dev,s.st_ino,leaf.st_dev,leaf.st_ino,mounted]))
else:
 assert action in ('held','disarm','absent')
 try:leaf=os.stat('lxc.conf',dir_fd=fd,follow_symlinks=False)
 except FileNotFoundError:leaf=None
 if action=='absent':assert leaf is None and mount_id()==0
 else:
  expected=json.loads(sys.argv[4]);assert len(expected)==5
  assert [s.st_dev,s.st_ino]==expected[:2] and leaf is not None
  assert [leaf.st_dev,leaf.st_ino]==expected[2:4] and stat.S_ISDIR(leaf.st_mode) and leaf.st_uid==0 and stat.S_IMODE(leaf.st_mode)==0o700
  assert mount_id()==expected[4]
  if action=='disarm':
   subprocess.run(['umount',path],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
   assert mount_id()==0
   os.rmdir('lxc.conf',dir_fd=fd);os.rmdir(parent,dir_fd=base)
`

func (fault *installedStartFault) leaf(ctx context.Context, action string) error {
	command := exec.CommandContext(ctx, "sudo", "-n", "nsenter", "--target", strconv.Itoa(fault.pid), "--mount", "--", "python3", "-c", installedStartLeaf, strconv.Itoa(fault.pid), fault.start, action, fault.identity)
	raw, err := command.Output()
	if err != nil {
		return fmt.Errorf("owned runtime leaf %s refused", action)
	}
	if action == "arm" {
		var identity []uint64
		if json.Unmarshal(raw, &identity) != nil || len(identity) != 5 || identity[1] == 0 || identity[3] == 0 || identity[4] == 0 {
			return errors.New("owned runtime leaf identity unavailable")
		}
		fault.identity = strings.TrimSpace(string(raw))
	}
	return nil
}

func newInstalledStartFault(t *testing.T, ctx context.Context, config string) *installedStartFault {
	t.Helper()
	if _, err := os.Stat("/run/subyard-e2e-lease.json"); err != nil {
		t.Fatal("startup fault requires a disposable lease marker")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", "/var/lib/incus/unix.socket")
	}}
	fault := &installedStartFault{client: &http.Client{Transport: transport}, config: config}
	var server api.Server
	if err := fault.get(ctx, "/1.0", &server); err != nil || server.Environment.ServerVersion != "6.0.6" || server.Environment.ServerClustered || server.Environment.ServerPid <= 1 {
		t.Fatal("startup fault requires the actual nonclustered Incus 6.0.6 daemon")
	}
	fault.pid = server.Environment.ServerPid
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", fault.pid))
	fields := strings.Fields(string(stat)[strings.LastIndex(string(stat), ")")+1:])
	if err != nil || len(fields) <= 19 {
		t.Fatal("daemon start identity unavailable")
	}
	fault.start = fields[19]
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		if fault.identity != "" && !fault.closed {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := fault.leaf(cleanup, "disarm"); err != nil {
				t.Error("startup fault cleanup refused")
			}
		}
	})
	return fault
}

// Reuse the capture helper's existing journal/capsule validation and fixed
// pending-evidence slots, also used by the current-partial recovery case.
const installedGraphCapsuleSnapshot = `import runpy,sys,pathlib,os
m=runpy.run_path(sys.argv[1]);c=pathlib.Path(sys.argv[2]);i,r=m['read_file'](c/'release-transition/v2/journal.json',m['MIB']);j=m['installed_journal'](i,r,owner=os.getuid());assert j is not None
info,data=m['installed_capsule'](c,j);assert info['status']=='present' and len(data)<=4*m['MIB'];m['validate_installed_capsule'](data,j,os.getuid())
f=os.open(sys.argv[3],os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
os.fchmod(f,0o600)
with os.fdopen(f,'wb') as o:o.write(data);o.flush();os.fsync(o.fileno())
d=os.open(pathlib.Path(sys.argv[3]).parent,os.O_RDONLY|os.O_DIRECTORY);os.fsync(d);os.close(d)
assert m['read_file'](c/'release-transition/v2/journal.json',m['MIB'])==(i,r)
`

func resumeInstalledCompensation(t *testing.T, ctx context.Context, fault *installedStartFault, first *rustOwnerSession, outcome releasetransition.Outcome, executeErr error, open func() *rustOwnerSession, descriptor, fixture, originalUUID string, originalConfig []byte) (*rustOwnerSession, releasetransition.Outcome) {
	t.Helper()
	if executeErr != nil || outcome.Status != releasetransition.StatusRecovering || outcome.Code != releasetransition.CodeVerificationFailed || outcome.Transaction == nil || outcome.ReachedGoal {
		t.Fatalf("startup failure did not close a retryable compensated attempt: status=%s code=%s transaction=%t call_error=%t message=%q", outcome.Status, outcome.Code, outcome.Transaction != nil, executeErr != nil, outcome.Message)
	}
	if err := closeInstalledGraphOwner(first); err != nil {
		t.Fatal("failed owner did not drain", err)
	}
	journal, facts, err := installedGraphFacts(fault.config)
	if err != nil || journal.Checkpoint == releasetransition.JournalComplete || journal.Transaction != *outcome.Transaction {
		t.Fatal("original pending transaction unavailable", err)
	}
	fault.tx = string(journal.Transaction)
	uuid, created, creationDiagnostic := installedGraphCurrentCreatedUUID(facts, fault.tx)
	fault.uuid = uuid
	if !fault.armed || fault.identity == "" || fault.uuid == "" {
		t.Fatalf("owned fault or actual canonical creation is unproved: armed=%t identity=%t uuid_nonempty=%t create_valid=%t facts=%d %s", fault.armed, fault.identity != "", fault.uuid != "", created, len(facts), creationDiagnostic)
	}
	failed, compensated := 0, false
	originalBundle := map[string][]byte{}
	for name, raw := range facts {
		if name == "bundle.json" || strings.HasPrefix(name, "bundle.") {
			originalBundle[name] = raw
		}
		if strings.HasSuffix(name, ".terminal.json") {
			var terminal releaseowner.Terminal
			if json.Unmarshal(raw, &terminal) == nil && terminal.State == "failed" && terminal.Owner == "owner-registration" && terminal.Resource == "test-yard" && terminal.Binding.Transaction == fault.tx {
				for _, operation := range terminal.Operations {
					if operation.ID != "" && operation.Kind == "instance-power" && operation.Project == installedStartProject && operation.Resource == installedStartInstance && operation.Status == 400 {
						failed++
					}
				}
			}
		}
		if strings.HasSuffix(name, ".recovery.json") {
			var edge struct {
				Binding     releaseowner.Binding
				Compensated *struct{ Group releaseowner.Binding }
			}
			if json.Unmarshal(raw, &edge) == nil && edge.Binding.Transaction == fault.tx && edge.Compensated != nil && edge.Compensated.Group.Transaction == fault.tx {
				compensated = true
			}
		}
	}
	legacy, err := fault.instance(ctx, "subyard-e2e-yard", "yard-e2e-yard")
	if err != nil || !strings.EqualFold(legacy.Status, "stopped") || legacy.Config["volatile.uuid"] == "" || legacy.Config["volatile.uuid"] == originalUUID || !installedGraphCreated(facts, fault.tx, "recreate", "subyard-e2e-yard", "yard-e2e-yard", legacy.Config["volatile.uuid"], legacy.Config["user.subyard.owner_create"]) || failed != 1 || !compensated || len(originalBundle) == 0 {
		t.Fatal("failed current start did not restore a causally created stopped legacy instance")
	}
	if _, err := fault.instance(ctx, installedStartProject, installedStartInstance); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("compensation did not remove the failed canonical instance")
	}
	if err := fault.leaf(ctx, "held"); err != nil {
		t.Fatal("exact owned runtime mount did not survive create, start and compensation", err)
	}
	if err := fault.leaf(ctx, "disarm"); err != nil {
		t.Fatal("owned runtime mount cleanup refused", err)
	}
	fault.closed = true
	if err := fault.leaf(ctx, "absent"); err != nil {
		t.Fatal("owned runtime mount cleanup did not reach absence", err)
	}
	config, err := os.ReadFile(filepath.Join(fault.config, "yards/e2e-yard/config.env"))
	if err != nil || !bytes.Equal(config, originalConfig) {
		t.Fatal("compensation changed original legacy registration")
	}
	journalRaw, err := os.ReadFile(filepath.Join(fault.config, "release-transition/v2/journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(fixture, "partial-pending-journal.json"), journalRaw, 0600)
	helper := os.Getenv("SUBYARD_RUST_INSTALLED_GRAPH_CAPTURE_HELPER")
	if helper == "" {
		t.Fatal("transported capture helper is required")
	}
	command := exec.CommandContext(ctx, "python3", "-c", installedGraphCapsuleSnapshot, helper, fault.config, filepath.Join(fixture, "partial-pending-capsule.tar"))
	if err := command.Run(); err != nil {
		t.Fatal("original compensated capsule validation failed", err)
	}
	fault.logStage(t, "closed-restored-proof")
	second := open()
	fresh, err := second.Inspect(ctx, nil)
	fault.logStage(t, "fresh-inspect-return")
	if err != nil || len(fresh.Blockers) != 0 || fresh.Resume == nil || *fresh.Resume != journal.Transaction {
		t.Fatal("fresh original owner did not admit exact compensated Resume", err)
	}
	resumed, err := second.Execute(ctx, fresh.Plan, descriptor)
	fault.logStage(t, "fresh-execute-return")
	if err != nil || resumed.Status != releasetransition.StatusReady || !resumed.ReachedGoal || resumed.Transaction == nil || *resumed.Transaction != journal.Transaction {
		t.Fatalf("same-transaction compensated Resume failed: status=%s code=%s transaction_match=%t call_error=%t", resumed.Status, resumed.Code, resumed.Transaction != nil && *resumed.Transaction == journal.Transaction, err != nil)
	}
	_, after, err := installedGraphFacts(fault.config)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range originalBundle {
		if !bytes.Equal(after[name], raw) {
			t.Fatal("Resume changed the original bundle")
		}
	}
	t.Log("real accepted SDK start failure400, compensated stopped legacy recreate, fresh original same-TX Resume Ready")
	return second, resumed
}

func TestInstalledGraphStartFaultRequiresRealCreateFacts(t *testing.T) {
	if err := exec.Command("python3", "-c", "import sys;compile(sys.argv[1],'<owned-runtime-leaf>','exec')", installedStartLeaf).Run(); err != nil {
		t.Fatal(err)
	}
	// Synthetic data tests only the assertion's refusal boundaries. The opt-in
	// physical case obtains these same fields solely from the real native helper.
	binding := releaseowner.Binding{Session: "session", Transaction: "tx", Ordinal: 7}
	token := strings.Repeat("a", 32)
	intent := installedGraphCreateIntent{Version: 1, Binding: binding, Recipe: strings.Repeat("b", 64), Source: strings.Repeat("c", 64), Scope: "current", Kind: "instance", Domain: "operation", Index: 2, Token: token, RequestDigest: strings.Repeat("d", 64), Server: strings.Repeat("d", 64), Project: installedStartProject, Resource: installedStartInstance}
	operation := ports.NativeOperation{Kind: "owner-create-instance", Server: intent.Server, Project: installedStartProject, Resource: installedStartInstance, ID: "operation", Status: 200}
	accepted := operation
	accepted.Status = 0
	before := accepted
	before.ID = ""
	facts := map[string][]byte{}
	put := func(name string, value any) {
		raw, _ := json.Marshal(value)
		facts[name] = append(raw, '\n')
	}
	for name, value := range map[string]any{
		"007.operation-002-create-result.json": installedGraphCreateResult{Version: 1, Intent: intent, OperationID: "operation", Status: 200, InstanceUUID: "uuid", IdentityDigest: strings.Repeat("e", 64), ETagDigest: strings.Repeat("f", 64)},
		"007.operation-002-create-intent.json": intent,
		"007.intent.json":                      releaseowner.Intent{Version: 1, Binding: binding, Owner: "owner-registration", Resource: "test-yard", Recipe: intent.Recipe, Source: intent.Source},
		"007.operation-002-intent.json":        before,
		"007.operation-002-accepted.json":      accepted,
		"007.operation-002-terminal.json":      operation,
	} {
		put(name, value)
	}
	check := func(scope, project, instance, uuid, token string) bool {
		return installedGraphCreated(facts, "tx", scope, project, instance, uuid, token)
	}
	if !check("current", installedStartProject, installedStartInstance, "uuid", token) {
		t.Fatal("matching existing create evidence refused")
	}
	if uuid, ok, diagnostic := installedGraphCurrentCreatedUUID(facts, "tx"); !ok || uuid != "uuid" || diagnostic != "code=ok candidates=1 ordinal=7 index=2" {
		t.Fatal("matching current create UUID or safe diagnostic was not derived")
	}
	for _, test := range []struct{ name, scope, project, instance, uuid, token string }{
		{"wrong-scope", "recreate", installedStartProject, installedStartInstance, "uuid", token},
		{"wrong-project", "current", "foreign", installedStartInstance, "uuid", token},
		{"wrong-instance", "current", installedStartProject, "foreign", "uuid", token},
		{"wrong-uuid", "current", installedStartProject, installedStartInstance, "foreign", token},
		{"wrong-token", "current", installedStartProject, installedStartInstance, "uuid", strings.Repeat("b", 32)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if check(test.scope, test.project, test.instance, test.uuid, test.token) {
				t.Fatal("foreign create evidence accepted")
			}
		})
	}
	for name, code := range map[string]string{"007.intent.json": "root-missing", "007.operation-002-accepted.json": "accepted-missing", "007.operation-002-terminal.json": "terminal-missing"} {
		raw := facts[name]
		delete(facts, name)
		if check("current", installedStartProject, installedStartInstance, "uuid", token) {
			t.Fatal("missing create closure accepted")
		}
		if _, ok, diagnostic := installedGraphCurrentCreatedUUID(facts, "tx"); ok || diagnostic != fmt.Sprintf("code=%s candidates=1 ordinal=7 index=2", code) {
			t.Fatal("missing create closure yielded a UUID or incorrect safe diagnostic")
		}
		facts[name] = raw
	}
	facts["008.operation-002-create-result.json"] = facts["007.operation-002-create-result.json"]
	if _, ok, diagnostic := installedGraphCurrentCreatedUUID(facts, "tx"); ok || diagnostic != "code=candidate-duplicate candidates=2 ordinal=7 index=2" {
		t.Fatal("duplicate current create yielded a UUID or incorrect safe diagnostic")
	}
	delete(facts, "008.operation-002-create-result.json")
	whole := releaseowner.Terminal{Version: 1, Binding: binding, Owner: "owner-registration", Resource: "test-yard", State: "failed", Operations: []ports.NativeOperation{operation, operation, operation}}
	whole.Operations[0].ID = "prior"
	whole.Operations[2].Kind, whole.Operations[2].ID, whole.Operations[2].Status = "instance-power", "start", 400
	put("007.terminal.json", whole)
	put("007.accepted.json", releaseowner.Terminal{Version: 1, Binding: binding, Owner: "owner-registration", Resource: "test-yard", State: "accepted", Operations: []ports.NativeOperation{}})
	var closure installedGraphCreateClosure
	closure.Version, closure.Binding, closure.Commands = 1, binding, []installedGraphCreateCommand{}
	hash := func(raw []byte) string { return fmt.Sprintf("%x", sha256.Sum256(raw)) }
	closure.Intent, closure.Accepted, closure.Terminal = hash(facts["007.intent.json"]), hash(facts["007.accepted.json"]), hash(facts["007.terminal.json"])
	for index, final := range whole.Operations {
		for _, phase := range []string{"intent", "accepted", "terminal"} {
			value := final
			if phase != "terminal" {
				value.Status = 0
			}
			if phase == "intent" {
				value.ID = ""
			}
			name := fmt.Sprintf("007.operation-%03d-%s.json", index+1, phase)
			put(name, value)
			closure.Facts = append(closure.Facts, installedGraphCreateFact{name, uint64(len(facts[name])), hash(facts[name])})
			delete(facts, name)
		}
	}
	slices.SortFunc(closure.Facts, func(a, b installedGraphCreateFact) int {
		return strings.Compare(a.Name, b.Name)
	})
	put("007.closure.json", closure)
	if _, ok, diagnostic := installedGraphCurrentCreatedUUID(facts, "tx"); !ok {
		t.Fatal("matching compacted create evidence refused:", diagnostic)
	}
	compacted := maps.Clone(facts)
	for _, failure := range []string{"missing-closure", "corrupt-closure", "null-commands", "null-facts", "binding", "top-hash", "fact-hash", "fact-size", "fact-order", "missing-accepted", "missing-whole", "whole-binding", "whole-operation", "create-intent", "create-pair", "residual", "extra-residual"} {
		t.Run("compacted-"+failure, func(t *testing.T) {
			facts = maps.Clone(compacted)
			changed := closure
			changed.Facts = slices.Clone(closure.Facts)
			switch failure {
			case "missing-closure":
				delete(facts, "007.closure.json")
			case "corrupt-closure":
				facts["007.closure.json"] = append(slices.Clone(facts["007.closure.json"]), ' ')
			case "null-commands":
				changed.Commands = nil
				put("007.closure.json", changed)
			case "null-facts":
				changed.Facts = nil
				put("007.closure.json", changed)
			case "binding":
				changed.Binding.Ordinal++
				put("007.closure.json", changed)
			case "top-hash":
				changed.Terminal = strings.Repeat("0", 64)
				put("007.closure.json", changed)
			case "fact-hash":
				changed.Facts[0].SHA256 = strings.Repeat("0", 64)
				put("007.closure.json", changed)
			case "fact-size":
				changed.Facts[0].Size++
				put("007.closure.json", changed)
			case "fact-order":
				changed.Facts[0], changed.Facts[1] = changed.Facts[1], changed.Facts[0]
				put("007.closure.json", changed)
			case "missing-accepted":
				delete(facts, "007.accepted.json")
			case "missing-whole":
				delete(facts, "007.terminal.json")
			case "whole-binding", "whole-operation":
				changedWhole := whole
				changedWhole.Operations = slices.Clone(whole.Operations)
				if failure == "whole-binding" {
					changedWhole.Binding.Ordinal++
				} else {
					changedWhole.Operations[1].ID = "foreign"
				}
				put("007.terminal.json", changedWhole)
			case "create-intent":
				delete(facts, "007.operation-002-create-intent.json")
			case "create-pair":
				changedIntent := intent
				changedIntent.RequestDigest = strings.Repeat("0", 64)
				put("007.operation-002-create-intent.json", changedIntent)
			case "residual":
				changedOperation := accepted
				changedOperation.ID = "foreign"
				put("007.operation-002-accepted.json", changedOperation)
			case "extra-residual":
				put("007.operation-004-accepted.json", accepted)
			}
			if _, ok, diagnostic := installedGraphCurrentCreatedUUID(facts, "tx"); ok {
				t.Fatal("unproved compacted create accepted:", diagnostic)
			}
		})
	}
	facts = maps.Clone(compacted)
	put("007.operation-002-accepted.json", accepted)
	if !check("current", installedStartProject, installedStartInstance, "uuid", token) {
		t.Fatal("matching residual beside immutable closure refused")
	}
}
