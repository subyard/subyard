//go:build linux

package releaseruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestRustSourceQueryVerifiedTooling(t *testing.T) {
	toolingRoot := os.Getenv("SUBYARD_RUST_SOURCE_QUERY_TOOLING_ROOT")
	if toolingRoot == "" {
		t.Skip("normal verified tooling supplied by delivery gate")
	}
	for _, variant := range []string{"historical-go", "legacy-refusal"} {
		t.Run(variant, func(t *testing.T) {
			root := testkit.TempDir(t)
			configHome, runtimeRoot := filepath.Join(root, "config"), filepath.Join(root, "runtime")
			if err := os.Mkdir(configHome, 0700); err != nil {
				t.Fatal(err)
			}
			calls := filepath.Join(root, "operational-owner-calls")
			program := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in --version) printf 'yard-engine 1.0.0\\n';; *) printf called >> %q; exit 61;; esac\n", calls)
			active := writeDelegateRuntimeFixture(t, runtimeRoot, "1.0.0-source-data", "historical Go data, no Rust assets", []byte(sourceAdmissionRegistry), program)
			if err := os.Symlink("releases/"+string(active.release), filepath.Join(runtimeRoot, "current")); err != nil {
				t.Fatal(err)
			}
			if variant == "legacy-refusal" {
				if err := os.MkdirAll(filepath.Join(configHome, "migrations/transactions/0123456789abcdef0123456789abcdef"), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(configHome, "migrations/transactions/0123456789abcdef0123456789abcdef/transaction.json"), []byte("{bad\n"), 0600)
			}
			adapter := New(Config{RepositoryRoot: toolingRoot, Stderr: io.Discard})
			defer adapter.Close()
			proof, err := adapter.InspectRustSourceAdmission(context.Background(), runtimeRoot, configHome)
			if variant == "legacy-refusal" {
				if err == nil {
					t.Fatal("malformed legacy history acquired a source proof")
				}
			} else if err != nil || !lowerSHA256(proof.Binding) || proof.Metadata.Journal != nil {
				t.Fatal("normal Rust query over historical Go data", proof, err)
			}
			if _, err := os.Stat(calls); !os.IsNotExist(err) {
				t.Fatal("source query invoked original Go operational policy", err)
			}
			if _, err := os.Stat(filepath.Join(configHome, "migrations/update.lock")); !os.IsNotExist(err) {
				t.Fatal("source query created a mutation lease", err)
			}
		})
	}
}

func TestRustSourceQueryToolingIdentityGuards(t *testing.T) {
	from := os.Getenv("SUBYARD_RUST_SOURCE_QUERY_TOOLING_ROOT")
	if from == "" {
		t.Skip("normal verified tooling supplied by delivery gate")
	}
	if _, err := os.Stat(filepath.Join(from, "runtime-files.sha256")); os.IsNotExist(err) {
		from = filepath.Join(from, ".build/runtime/current")
	}
	from, err := filepath.EvalSymlinks(from)
	if err != nil {
		t.Fatal(err)
	}
	checkout := testkit.TempDir(t)
	stage := filepath.Join(checkout, ".build/runtime/releases", filepath.Base(from))
	if err := filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		to := filepath.Join(stage, rel)
		if entry.IsDir() {
			return os.MkdirAll(to, 0700)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("nonregular tooling fixture")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		testkit.WriteFile(t, to, raw, info.Mode().Perm())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(filepath.Join(stage, "bin/yard-engine"))
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(checkout, ".build/yard")
	testkit.WriteFile(t, helper, image, 0755)
	current := filepath.Join(checkout, ".build/runtime/current")
	link := "releases/" + filepath.Base(stage)
	if err := os.Symlink(link, current); err != nil {
		t.Fatal(err)
	}
	adapter := New(Config{RepositoryRoot: checkout, Stderr: io.Discard})
	defer adapter.Close()
	verified, check, err := adapter.sourceQueryTooling(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	// Same bytes in a replacement inode are another source build identity.
	if err := os.Rename(helper, helper+".original"); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, helper, image, 0755)
	if err := check(); err == nil {
		t.Fatal("replacement source helper inode admitted")
	}
	if err := os.Remove(helper); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(helper+".original", helper); err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatal("original helper restore", err)
	}
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/unrelated", current); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("different tooling selection admitted")
	}
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(link, current); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stage, stage+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("replacement tooling root admitted")
	}
}

func TestRustSourceQueryFiniteTransport(t *testing.T) {
	journal := []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}
	valid, err := json.Marshal(rustSourceProof{journal, strings.Repeat("a", 64), strings.Repeat("b", 64), []string{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name   string
		raw    []byte
		status int
		good   bool
	}{
		{"original-bytes", append(valid, '\n'), 0, true},
		{"missing-newline", valid, 0, false},
		{"partial", []byte("{\n"), 0, false},
		{"trailing", append(append(append([]byte{}, valid...), valid...), '\n'), 0, false},
		{"duplicate", []byte(fmt.Sprintf("{\"journalRaw\":null,\"metadataBinding\":%q,\"binding\":%q,\"binding\":%q,\"retainedReleases\":[]}\n", strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("b", 64))), 0, false},
		{"null-refs", []byte(fmt.Sprintf("{\"journalRaw\":null,\"metadataBinding\":%q,\"binding\":%q,\"retainedReleases\":null}\n", strings.Repeat("a", 64), strings.Repeat("b", 64))), 0, false},
		{"nonzero-after-response", append(valid, '\n'), 7, false},
		{"oversize", bytes.Repeat([]byte{'x'}, sourceQueryReplyLimit+1), 0, false},
	} {
		t.Run(item.name, func(t *testing.T) {
			runtime, tooling, request, configRoot, runtimeRoot := sourceQueryTransportFixture(t, item.raw, item.status, false)
			proof, err := runtime.queryRustSource(context.Background(), tooling, request, configRoot, runtimeRoot)
			if (err == nil) != item.good {
				t.Fatal("finite transport result", err)
			}
			if item.good && !bytes.Equal(proof.JournalRaw, journal) {
				t.Fatal("original journal bytes changed")
			}
			if _, err := os.Stat(filepath.Join(string(request.ConfigHome), "migrations")); !os.IsNotExist(err) {
				t.Fatal("read-only query created mutation namespace", err)
			}
		})
	}
}

func TestRustSourceQueryCancellationHasNoLeaseWait(t *testing.T) {
	runtime, tooling, request, configRoot, runtimeRoot := sourceQueryTransportFixture(t, nil, 0, true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := runtime.queryRustSource(ctx, tooling, request, configRoot, runtimeRoot); err == nil {
		t.Fatal("cancelled query succeeded")
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("read-only cancellation waited for mutation custody")
	}
}

// This fixture tests transport and descriptor custody only. The real Rust
// SourceQuery paired oracle separately proves protected-history decisions.
func sourceQueryTransportFixture(t *testing.T, reply []byte, status int, sleep bool) (*Runtime, *verifiedPublishedCandidate, rustSourceQuery, *os.File, *os.File) {
	t.Helper()
	root := testkit.TempDir(t)
	configPath := filepath.Join(root, "config-"+string([]byte{0xff}))
	runtimePath := filepath.Join(root, "runtime")
	for _, p := range []string{configPath, runtimePath} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	configRoot, err := os.Open(configPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { configRoot.Close() })
	runtimeRoot, err := os.Open(runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtimeRoot.Close() })
	// An independently held update/account lock must not delay a query.
	for _, name := range []string{"update.lock", "account.lock"} {
		f, err := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
	}
	helperRaw := []byte("exact sealed codec helper")
	helper := sealSourceQueryFixture(t, root, "helper", helperRaw)
	script := fmt.Sprintf(`#!/usr/bin/python3
import array,base64,fcntl,hashlib,json,os,socket,sys,time
peer=socket.socket(fileno=0)
byte,anc,flags,_=peer.recvmsg(1,socket.CMSG_SPACE(4*3))
assert byte==b'S' and not flags & (socket.MSG_CTRUNC|socket.MSG_TRUNC)
assert len(anc)==1 and anc[0][:2]==(socket.SOL_SOCKET,socket.SCM_RIGHTS)
fds=array.array('i');fds.frombytes(anc[0][2]);assert len(fds)==3
helper,config,runtime=fds
assert fcntl.fcntl(helper,1034)&15==15
assert os.fstat(helper).st_mode&511==320
assert hashlib.sha256(os.read(helper,1024)).hexdigest()==%q
request=json.loads(peer.makefile('rb').readline())
assert set(request)=={'configHome','runtimeRoot','configIdentity','runtimeIdentity','active','originalOwner','originalSources'}
def identity(fd):
 s=os.fstat(fd);return dict(device=s.st_dev,inode=s.st_ino,mode=s.st_mode,uid=s.st_uid,links=s.st_nlink)
assert request['configIdentity']==identity(config) and request['runtimeIdentity']==identity(runtime)
assert base64.b64decode(request['configHome'])==os.readlink('/proc/self/fd/'+str(config)).encode(errors='surrogateescape')
assert base64.b64decode(request['runtimeRoot'])==os.readlink('/proc/self/fd/'+str(runtime)).encode(errors='surrogateescape')
if %t:time.sleep(30)
peer.sendall(base64.b64decode(%q))
sys.exit(%d)
`, hex.EncodeToString(sha256Sum(helperRaw)), sleep, encodeSourceQueryFixture(reply), status)
	// Python uses True/False literals.
	script = strings.ReplaceAll(strings.ReplaceAll(script, "if true:", "if True:"), "if false:", "if False:")
	owner := sealSourceQueryFixture(t, root, "owner", []byte(script))
	ci, _ := releaseowner.IdentityOf(configRoot)
	ri, _ := releaseowner.IdentityOf(runtimeRoot)
	return New(Config{Stderr: io.Discard}), &verifiedPublishedCandidate{engine: helper, releaseOwner: owner}, rustSourceQuery{ConfigHome: []byte(configPath), RuntimeRoot: []byte(runtimePath), ConfigIdentity: ci, RuntimeIdentity: ri}, configRoot, runtimeRoot
}

func sealSourceQueryFixture(t *testing.T, root, name string, raw []byte) *os.File {
	t.Helper()
	p := filepath.Join(root, name)
	testkit.WriteFile(t, p, raw, 0700)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sealed, err := sealVerifiedEngine(f, sha256.Sum256(raw))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sealed.Close() })
	return sealed
}

func encodeSourceQueryFixture(raw []byte) string {
	encoded, _ := json.Marshal(raw)
	if raw == nil {
		return ""
	}
	return strings.Trim(string(encoded), "\"")
}
