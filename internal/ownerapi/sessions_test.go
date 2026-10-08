package ownerapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/state"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/crypto/ssh"
)

func TestVSCodeSessionUsesOrdinaryAliasAndPreservesGuestPin(t *testing.T) {
	root := testkit.TempDir(t)
	sshRoot := filepath.Join(root, "ssh")
	if err := os.Mkdir(sshRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	hostKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	pinPath := filepath.Join(sshRoot, "known_hosts")
	pin := "[127.0.0.1]:2222 " + hostKey + "\n"
	testkit.WriteFile(t, pinPath, []byte(pin), 0o600)
	yard := domain.Context{
		YardName: "default", SSHHost: "yard", SSHPort: 2222, DevUser: "dev",
		AccessKind: domain.AccessLocal, YardKind: domain.YardContainer,
		IncusProject: "subyard", YardInstanceName: "yard",
		Paths: domain.RuntimePaths{DataHome: root, StateDir: filepath.Join(root, "projects")},
	}
	store, err := state.NewFileStore(yard.Paths.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	record := domain.ProjectRecord{Schema: 1, ProjectID: "demo-id", Name: "Demo",
		HostPath: "/host/Demo", YardPath: state.YardPath("demo-id"), Mode: domain.ProjectSync,
		SSHHost: "yard", Target: "yard"}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"subyard/yard": {Status: "Running", Devices: map[string]map[string]string{"ssh": {"type": "proxy"}}},
	}}
	service := Service{Loaded: config.Loaded{Context: yard}, Instances: incus, ProjectsAllowed: true}
	params := SessionParams{Kind: "vscode", ProjectID: record.ProjectID}
	prepared, err := service.PrepareSession(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.VSCode == nil || prepared.VSCode.SSHAlias != yard.SSHHost ||
		prepared.VSCode.Port != yard.SSHPort || prepared.VSCode.HostKey != hostKey ||
		prepared.VSCode.HostKeyFingerprint != ssh.FingerprintSHA256(key) {
		t.Fatalf("session transport drifted: %#v", prepared.VSCode)
	}
	payload, err := json.Marshal(prepared)
	if err != nil || strings.Contains(string(payload), "previewPort") || strings.Contains(string(payload), "yard.code") {
		t.Fatalf("session retains preview coupling: %s err=%v", payload, err)
	}
	if after, err := os.ReadFile(pinPath); err != nil || string(after) != pin || len(incus.ExecCalls) != 0 {
		t.Fatalf("session preparation changed guest access: pin=%q err=%v exec=%#v", after, err, incus.ExecCalls)
	}
	service.Loaded.Context.SSHHost = "-invalid"
	_, err = service.PrepareSession(context.Background(), params)
	var fault *Error
	if !errors.As(err, &fault) || fault.Code != "session_ssh_unavailable" {
		t.Fatalf("unsafe SSH alias accepted: %v", err)
	}
	service.Loaded.Context.SSHHost = yard.SSHHost
	testkit.WriteFile(t, pinPath, []byte("invalid\n"), 0o600)
	_, err = service.PrepareSession(context.Background(), params)
	if !errors.As(err, &fault) || fault.Code != "session_host_key_unavailable" {
		t.Fatalf("missing protected guest pin accepted: %v", err)
	}
}
