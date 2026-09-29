package credentialruntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestSetupCredentialPreparationAndStaleSource(t *testing.T) {
	for _, name := range []string{"changed", "replaced", "symlink", "invalid-pem", "consumer-appeared"} {
		t.Run(name, func(t *testing.T) {
			runtime := profileCredentialFixture(t)
			source := filepath.Join(testkit.TempDir(t), "download.pem")
			testkit.WriteFile(t, source, []byte("synthetic invalid private key"), 0o644)
			prepared, err := runtime.PrepareSetupCredential(context.Background(), setupOptions(source))
			if err != nil {
				t.Fatal(err)
			}
			info, _ := os.Stat(source)
			if info.Mode().Perm() != 0o644 || runtime.Initialized() {
				t.Fatal("preparation mutated state")
			}
			installCredentialIdentity(t, runtime)
			switch name {
			case "consumer-appeared":
				writeCredentialFile(t, filepath.Join(runtime.config.ConsumerRoot, "fixture", "key.pem"), "concurrent consumer", 0o600)
			case "changed":
				testkit.WriteFile(t, source, []byte("changed bytes"), 0o644)
			case "replaced":
				if err := os.Rename(source, source+".old"); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, source, []byte("synthetic invalid private key"), 0o644)
			case "symlink":
				if err := os.Rename(source, source+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(source+".old", source); err != nil {
					t.Fatal(err)
				}
			}
			err = prepared.Execute(context.Background())
			if err == nil {
				t.Fatal("invalid or stale source accepted")
			}
			if name == "changed" || name == "replaced" || name == "consumer-appeared" {
				if !errors.Is(err, domain.ErrPlanStale) {
					t.Fatalf("error=%v", err)
				}
				info, _ = os.Stat(source)
				if info.Mode().Perm() != 0o644 {
					t.Fatal("changed source chmodded")
				}
			}
			if id, err := runtime.ConsumerCredentialID(context.Background(), "fixture-key", "global"); err != nil || id != "" {
				t.Fatalf("invalid key imported: %s %v", id, err)
			}
		})
	}
}

func TestSetupCredentialSourceRestrictions(t *testing.T) {
	runtime := profileCredentialFixture(t)
	root := testkit.TempDir(t)
	path := filepath.Join(root, "auth.json")
	testkit.WriteFile(t, path, []byte("private"), 0o644)
	if _, err := runtime.PrepareSetupCredential(context.Background(), setupOptions(path)); err == nil {
		t.Fatal("mutable auth store accepted")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o644 {
		t.Fatal("rejected source chmodded")
	}
	linked := filepath.Join(root, "linked.pem")
	if err := os.Link(path, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.PrepareSetupCredential(context.Background(), setupOptions(linked)); err == nil {
		t.Fatal("hardlink accepted")
	}
	if _, err := runtime.PrepareSetupCredential(context.Background(), setupOptions("")); err == nil {
		t.Fatal("empty source accepted")
	}
}

func TestSetupCredentialImportMaterializeAndResume(t *testing.T) {
	runtime := setupLedgerFixture(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	payload := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	source := filepath.Join(testkit.TempDir(t), "download.pem")
	testkit.WriteFile(t, source, payload, 0o644)
	ctx := context.Background()
	// Prepare before initialization, as the first yard init does.
	prepared, err := runtime.PrepareSetupCredential(ctx, setupOptions(source))
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	id, err := runtime.ConsumerCredentialID(ctx, "fixture-key", "global")
	if err != nil || id == "" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	destination := filepath.Join(runtime.config.ConsumerRoot, "fixture", "key.pem")
	if err := runtime.ValidateConsumerFile("fixture-key", "global", destination); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(destination, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runtime.ValidateConsumerFile("fixture-key", "global", destination); err == nil {
		t.Fatal("world-readable valid private key accepted")
	}
	if err := os.Chmod(destination, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("consumer mismatch: %v", err)
	}
	for _, path := range []string{source, destination} {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode=%o", path, info.Mode().Perm())
		}
	}
	if err := prepared.Execute(ctx); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("old import was reusable: %v", err)
	}
	// A failed/interrupted materialization can be retried without the download.
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	repair, err := runtime.PrepareSetupCredential(ctx, setupOptions(""))
	if err != nil {
		t.Fatal(err)
	}
	if err := repair.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	again, err := runtime.ConsumerCredentialID(ctx, "fixture-key", "global")
	if err != nil || again != id {
		t.Fatalf("retry imported a duplicate: %s %v", again, err)
	}
	got, err = os.ReadFile(destination)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("recovery mismatch: %v", err)
	}
	// The ledger contains only wrapped payloads; signatures and Git history use
	// real OpenSSH/Git, with age/SOPS doubles at the external crypto boundary.
	all, err := runtime.allRecords(ctx)
	if err != nil || len(all[sharedLedger]) != 1 {
		t.Fatalf("records=%v err=%v", all, err)
	}
	record, err := os.ReadFile(runtime.recordPath(sharedLedger, id, all[sharedLedger][0].RevisionID))
	if err != nil || bytes.Contains(record, []byte("PRIVATE KEY")) {
		t.Fatal("plaintext key leaked into ledger")
	}
}

func setupLedgerFixture(t *testing.T) *Runtime {
	t.Helper()
	runtime := profileCredentialFixture(t)
	runtime.git, runtime.sshKeygen = "git", "ssh-keygen"
	runtime.config.Environment = append(runtime.config.Environment, "PATH="+os.Getenv("PATH"))
	writeCredentialFile(t, runtime.ageKeygen, `#!/bin/sh
case "$1" in
-o) printf 'FAKE:age1fixture\n' > "$2" ;;
-y) printf 'age1fixture\n' ;;
*) exit 2 ;;
esac
`, 0o700)
	// Match the existing host-free ledger contract without network/tool downloads.
	writeCredentialFile(t, runtime.sops, `#!/usr/bin/env python3
import base64,json,sys
args=sys.argv[1:]
with open(args[-1]) as f: doc=json.load(f)
if args[0]=='encrypt':
    doc['payload']='ENC['+base64.b64encode(doc['payload'].encode()).decode()+']'
    recipients=args[args.index('--age')+1].split(',')
    doc['sops']={'age':[{'recipient':r,'enc':'fixture'} for r in recipients],'mac':'fixture'}
else:
    doc['payload']=base64.b64decode(doc['payload'][4:-1]).decode()
    del doc['sops']
print(json.dumps(doc))
`, 0o700)
	// Avoid inheriting operator git configuration while using real signed records.
	runtime.config.Environment = append(runtime.config.Environment, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	runtime.env["PATH"] = os.Getenv("PATH")
	runtime.config.Stdout = &strings.Builder{}
	return runtime
}

func setupOptions(source string) SetupCredentialOptions {
	return SetupCredentialOptions{Consumer: "fixture-key", Zone: "global", Label: "fixture", Source: source}
}

func profileCredentialFixture(t *testing.T) *Runtime {
	t.Helper()
	runtime := credentialFixture(t)
	writeCredentialFile(t, filepath.Join(runtime.config.RepositoryRoot, "config", "profiles", "fixture", "profile.json"), `{"schema_version":1,"consumers":[{"id":"fixture-key","zone":"global","path":"fixture/key.pem","format":"rsa-private-key"}]}`, 0o600)
	configured, err := New(runtime.config)
	if err != nil {
		t.Fatal(err)
	}
	return configured
}

func TestProfileConsumerMappingAndClassification(t *testing.T) {
	runtime := profileCredentialFixture(t)
	destination, err := runtime.ConsumerPath("fixture-key", "global")
	if err != nil || destination != filepath.Join(runtime.config.ConsumerRoot, "fixture", "key.pem") {
		t.Fatalf("path=%q err=%v", destination, err)
	}
	if runtime.detectConsumer(destination) != "fixture-key" || runtime.detectZone(destination) != "global" {
		t.Fatal("profile consumer was not detected")
	}
	for _, item := range []struct{ consumer, zone string }{{"fixture-key", "another"}, {"unknown-consumer", "global"}, {"fixture-key", "../outside"}} {
		if _, err := runtime.ConsumerPath(item.consumer, item.zone); err == nil {
			t.Fatalf("unsafe mapping accepted: %+v", item)
		}
		if _, err := runtime.ConsumerCredentialID(context.Background(), item.consumer, item.zone); err == nil {
			t.Fatalf("invalid setup consumer accepted: %+v", item)
		}
	}
	if _, err := runtime.Prepare(context.Background(), "", []string{"add", "fixture", "--consumer", "undeclared"}); err == nil {
		t.Fatal("undeclared consumer accepted")
	}
}

func TestValidateConsumerFileRejectsUnsafeFiles(t *testing.T) {
	runtime := profileCredentialFixture(t)
	path := filepath.Join(testkit.TempDir(t), "key.pem")
	for _, mode := range []os.FileMode{0o600, 0o644} {
		testkit.WriteFile(t, path, []byte("invalid private key"), mode)
		if err := runtime.ValidateConsumerFile("fixture-key", "global", path); err == nil {
			t.Fatalf("invalid file accepted with mode %o", mode)
		}
	}
	link := path + ".link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := runtime.ValidateConsumerFile("fixture-key", "global", link); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := runtime.ValidateConsumerFile("unknown", "global", path); err == nil {
		t.Fatal("unknown consumer accepted")
	}
	if err := runtime.ValidateConsumerFile("fixture-key", "other", path); err == nil {
		t.Fatal("wrong zone accepted")
	}
}

func TestProfileConsumersRejectMaterializationCollisions(t *testing.T) {
	for _, item := range []struct{ name, id, path string }{
		{"builtin-id", "staging-env", "other/key"},
		{"unmapped-id", "none", "other/key"},
		{"builtin-path", "another", "staging/fixture.env"},
		{"builtin-directory", "another", "qa-pool"},
		{"duplicate-path", "another", "fixture/key.pem"},
		{"nested-path", "another", "fixture/key.pem/child"},
		{"parent-path", "another", "fixture"},
	} {
		t.Run(item.name, func(t *testing.T) {
			runtime := profileCredentialFixture(t)
			declaration := map[string]any{"schema_version": 1, "consumers": []map[string]string{{"id": item.id, "zone": "global", "path": item.path, "format": "file"}}}
			data, err := json.Marshal(declaration)
			if err != nil {
				t.Fatal(err)
			}
			writeCredentialFile(t, filepath.Join(runtime.config.RepositoryRoot, "config", "profiles", "second", "profile.json"), string(data), 0o600)
			if _, err := New(runtime.config); err == nil {
				t.Fatal("consumer collision accepted")
			}
		})
	}
}
