package releaseowner

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type noEffects struct{}

func (noEffects) Close() error { return nil }
func (noEffects) CaptureInventory(context.Context) ([]string, []string, error) {
	return []string{}, []string{}, nil
}
func (noEffects) CaptureYard(context.Context, string, bool) (YardSettings, error) {
	return YardSettings{}, errors.New("fixture refuses effects")
}
func (noEffects) PrepareSettings(context.Context, string, []Edit) (PreparedSettings, error) {
	return PreparedSettings{}, errors.New("fixture refuses effects")
}
func (noEffects) CaptureOwner(context.Context) (OwnerBefore, error) {
	return OwnerBefore{State: "absent"}, nil
}
func (noEffects) CaptureActivation(context.Context, string, bool) ([]Target, error) {
	return []Target{}, nil
}
func (noEffects) CaptureSettings(context.Context, SettingsRecipe) (string, error) {
	return "", errors.New("effects forbidden")
}
func (noEffects) ProjectSettings(context.Context, SettingsRecipe, []Edit) (string, error) {
	return "", errors.New("effects forbidden")
}
func (noEffects) ObserveSettings(context.Context, SettingsRecipe) (string, error) {
	return "", errors.New("fixture refuses effects")
}
func (noEffects) ApplySettings(context.Context, SettingsRecipe) error {
	return errors.New("fixture refuses effects")
}
func (noEffects) ApplyOwner(context.Context, OwnerBefore, string) error {
	return errors.New("fixture refuses effects")
}
func (noEffects) ApplyActivation(context.Context, Target) error {
	return errors.New("fixture refuses effects")
}
func (noEffects) CaptureIngress(context.Context, *IngressBinding) (IngressFacts, error) {
	return IngressFacts{}, nil
}
func (noEffects) ObserveIngress(context.Context, IngressStep) (string, error) {
	return "", errors.New("fixture refuses effects")
}
func (noEffects) ApplyIngress(context.Context, IngressStep) error {
	return errors.New("fixture refuses effects")
}
func (noEffects) VerifyIngress(context.Context, IngressStep) error {
	return errors.New("fixture refuses effects")
}
func TestNativeCustodianChild(t *testing.T) {
	if os.Getenv("SUBYARD_NATIVE_CUSTODY_CHILD") != "1" {
		return
	}
	err := Serve(context.Background(), os.Stdin, os.Stdout, func(context.Context, Context, *os.File) (Components, error) { return noEffects{}, nil })
	if err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

type nativeTestPeer struct {
	t        *testing.T
	socket   *os.File
	sequence uint64
}

func (p *nativeTestPeer) frame(kind byte, value any, files ...*os.File) []byte {
	p.t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		p.t.Fatal(err)
	}
	p.sequence++
	header := make([]byte, 52)
	copy(header, "SYC1")
	header[4] = 1
	header[5] = kind
	binary.BigEndian.PutUint64(header[8:16], p.sequence)
	sum := sha256.Sum256(raw)
	copy(header[16:48], sum[:])
	binary.BigEndian.PutUint32(header[48:], uint32(len(raw)))
	if len(files) > 0 {
		fds := make([]int, len(files))
		for n, file := range files {
			fds[n] = int(file.Fd())
		}
		if err = unix.Sendmsg(int(p.socket.Fd()), header[:1], unix.UnixRights(fds...), nil, unix.MSG_NOSIGNAL); err != nil {
			p.t.Fatal(err)
		}
		header = header[1:]
	}
	if _, err = p.socket.Write(append(header, raw...)); err != nil {
		p.t.Fatal(err)
	}
	reply := make([]byte, 52)
	end := time.Now().Add(5 * time.Second)
	if err = readSocket(p.socket, reply, end); err != nil {
		p.t.Fatal(err)
	}
	if !bytesEqual(reply[:6], []byte{'S', 'Y', 'C', '1', 1, kind}) || binary.BigEndian.Uint64(reply[8:16]) != p.sequence || !bytesEqual(reply[16:48], sum[:]) {
		p.t.Fatal("reply correlation")
	}
	length := binary.BigEndian.Uint32(reply[48:])
	if length > MaxReply {
		p.t.Fatal("reply bound")
	}
	data := make([]byte, length)
	if err = readSocket(p.socket, data, end); err != nil {
		p.t.Fatal(err)
	}
	if reply[6] == 1 {
		return nil
	}
	if reply[6] != 0 {
		p.t.Fatal("reply status")
	}
	return data
}
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for n := range a {
		if a[n] != b[n] {
			return false
		}
	}
	return true
}
func (p *nativeTestPeer) request(request Request, files ...*os.File) Reply {
	p.t.Helper()
	raw := p.frame(15, request, files...)
	var reply Reply
	if err := strict(raw, &reply); err != nil {
		p.t.Fatal(err)
	}
	if reply.Code != "" {
		p.t.Fatalf("native refused: %s", reply.Code)
	}
	return reply
}
func TestNativeCustodyTransfersSameOFDAndClosesOnly(t *testing.T) {
	root := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, "migrations"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "migrations", "update.lock"), []byte{}, 0o600)
	config, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer config.Close()
	update, err := os.OpenFile(filepath.Join(root, "migrations", "update.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer update.Close()
	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	parent := os.NewFile(uintptr(sockets[0]), "native parent")
	child := os.NewFile(uintptr(sockets[1]), "native child")
	defer parent.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestNativeCustodianChild$")
	cmd.Env = append(os.Environ(), "SUBYARD_NATIVE_CUSTODY_CHILD=1")
	cmd.Stdin = child
	cmd.Stdout = child
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child.Close()
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	pgid, err := unix.Getpgid(cmd.Process.Pid)
	if err != nil || pgid == unix.Getpgrp() {
		t.Fatal("custodian inherited owner process group")
	}
	peer := nativeTestPeer{t: t, socket: parent}
	rootID, _ := IdentityOf(config)
	a := hex.EncodeToString(make([]byte, 32))
	context := Context{Session: a, DataHome: root, ConfigHome: root, RuntimeRoot: root, RepositoryRoot: root, Yard: "default", Target: "new", Artifact: a, Root: rootID, Repository: rootID}
	peer.request(Request{Op: "initialize", Context: &context}, config, config)
	if raw := peer.frame(16, map[string]any{"schemaVersion": 2, "contract": "unsupported"}); string(raw) != `{"schemaVersion":2,"contract":"unsupported"}` {
		t.Fatalf("checkpoint codec syntax reply: %s", raw)
	}
	if raw := peer.frame(17, map[string]any{"schemaVersion": 2, "files": []any{}, "unknown": true}); string(raw) != `{"schemaVersion":2,"files":[]}` {
		t.Fatalf("layout codec syntax reply: %s", raw)
	}
	layout := peer.request(Request{Op: "capture-config-layout", Session: a}).Layout
	if layout == nil || layout.Exists || layout.Raw != "" || layout.Digest != digest(nil) {
		t.Fatal("absent layout did not preserve exact byte binding", layout)
	}
	// Inspection does not acquire a mutation lease.
	peer.request(Request{Op: "capture-inventory", Session: a})
	if err = unix.Flock(int(update.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal("inspection held write lease", err)
	}
	image, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	imageSum := sha256.Sum256(image)
	identity, _ := IdentityOf(update)
	binding := Binding{Session: a, Transaction: "transaction", Plan: "plan-v1-" + a, Journal: a, Artifact: a, Helper: hex.EncodeToString(imageSum[:]), Ordinal: 1}
	peer.request(Request{Op: "claim-lease", Binding: &binding, Update: &identity}, update)
	update.Close()
	independent, err := os.OpenFile(filepath.Join(root, "migrations", "update.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer independent.Close()
	if err = unix.Flock(int(independent.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("helper did not retain OFD custody: %v", err)
	}
	// A legitimate frozen syntax refusal leaves this same native peer reusable.
	if reply := peer.frame(1, map[string]string{"unknown": "field"}); reply != nil {
		t.Fatal("syntax refusal accepted")
	}
	peer.request(Request{Op: "release-lease", Session: a})
	if err = unix.Flock(int(independent.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal("released helper retained lease", err)
	}
	parent.Close()
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}
func TestNativeAncillaryRefusalClosesAllReceivedDescriptors(t *testing.T) {
	root := testkit.TempDir(t)
	file, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for n := 0; n < 32; n++ {
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		sender := os.NewFile(uintptr(pair[0]), "sender")
		receiver := os.NewFile(uintptr(pair[1]), "receiver")
		before, _ := os.ReadDir("/proc/self/fd")
		if err = unix.Sendmsg(pair[0], []byte{1}, unix.UnixRights(int(file.Fd()), int(file.Fd()), int(file.Fd()), int(file.Fd())), nil, unix.MSG_NOSIGNAL); err != nil {
			t.Fatal(err)
		}
		_, received, err := receiveHeader(receiver)
		if err == nil || len(received) != 0 {
			t.Fatal("oversized transfer accepted")
		}
		after, _ := os.ReadDir("/proc/self/fd")
		if len(after) != len(before) {
			t.Fatalf("descriptor leak %d -> %d", len(before), len(after))
		}
		sender.Close()
		receiver.Close()
	}
}
