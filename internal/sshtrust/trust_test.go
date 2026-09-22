package sshtrust

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/transport"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/shellquote"
	"golang.org/x/crypto/ssh"
)

func TestProxyJumpTargetsPreservePortsAndIPv6(t *testing.T) {
	for _, test := range []struct {
		input, target string
		options       []string
	}{
		{"owner-alias", "owner-alias", nil},
		{"dev@[192.0.2.1]", "dev@192.0.2.1", nil},
		{"dev@[owner.example]:2222", "dev@owner.example", []string{"-p", "2222"}},
		{"dev@owner.example:2222", "dev@owner.example", []string{"-p", "2222"}},
		{"dev@[::1]:2223", "dev@::1", []string{"-p", "2223"}},
		{"ssh://dev@owner.example:2224", "dev@owner.example", []string{"-p", "2224"}},
	} {
		target, options, err := parseJump(test.input)
		if err != nil || target != test.target || !reflect.DeepEqual(options, test.options) {
			t.Fatalf("jump %q=%q %v %v", test.input, target, options, err)
		}
	}
	for _, input := range []string{"-bad", "user:password@host", "host:0", "host:65536", "host/path", "dev@[host/path]", "dev@[host]suffix", "dev@[]"} {
		if _, _, err := parseJump(input); err == nil {
			t.Fatalf("unsafe jump accepted: %q", input)
		}
	}
}

func keyLine(t *testing.T, namespace string) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return namespace + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + "\n"
}

func fixture(t *testing.T, namespace string) (*Manager, string, string) {
	t.Helper()
	root := t.TempDir()
	known, candidate := filepath.Join(root, "known_hosts"), filepath.Join(root, "candidate")
	if err := os.WriteFile(candidate, []byte(keyLine(t, namespace)), 0o600); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(root, "ssh")
	// The transport double negotiates a real public key and enforces exact
	// candidate pinning. ssh-keygen itself checks the persistent trust store.
	script := `#!/bin/sh
if [ "$1" = -G ]; then
  printf 'hostname example.test\nport 2222\nhostkeyalias %s\nuserknownhostsfile %s\nglobalknownhostsfile /dev/null\n' "$NAMESPACE" "$KNOWN"
  exit 0
fi
known=; assess=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) shift; case "$1" in
      UserKnownHostsFile=*) known=${1#*=} ;;
      StrictHostKeyChecking=accept-new) assess=1 ;;
    esac ;;
  esac
  shift
done
if [ "$assess" = 1 ]; then cp "$CANDIDATE" "$known"; exit 255; fi
[ ! -e "$CANDIDATE.auth-failed" ] || { echo 'authentication rejected' >&2; exit 255; }
[ -n "$known" ] || known=$KNOWN
cmp -s "$known" "$CANDIDATE" || { echo 'changed server key' >&2; exit 255; }
printf 'verified\n' >> "$CANDIDATE.calls"
`
	if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Environment: append(os.Environ(), "KNOWN="+known, "CANDIDATE="+candidate, "NAMESPACE="+namespace)}
	t.Cleanup(manager.Close)
	return manager, program, known
}

func TestFirstTrustConsentAndVerification(t *testing.T) {
	for _, namespace := range []string{"subyard-remote-example", "[example.test]:2222"} {
		t.Run(namespace, func(t *testing.T) {
			manager, program, known := fixture(t, namespace)
			unrelated := keyLine(t, "unrelated.test")
			if err := os.WriteFile(known, []byte(unrelated), 0o600); err != nil {
				t.Fatal(err)
			}
			prompts := 0
			manager.Confirm = func(_ context.Context, proposals []Proposal) error {
				if len(proposals) != 1 {
					t.Fatalf("proposals=%v", proposals)
				}
				p := proposals[0]
				prompts++
				if p.Target != "example" || p.Namespace != namespace || p.File != known || p.Algorithm != "ssh-ed25519" || !strings.HasPrefix(p.Fingerprint, "SHA256:") {
					t.Fatalf("proposal=%+v", p)
				}
				data, _ := os.ReadFile(known)
				if string(data) != unrelated {
					t.Fatal("trust written before confirmation")
				}
				return nil
			}
			options, err := manager.Options(context.Background(), program, "example")
			if err != nil {
				t.Fatal(err)
			}
			if prompts != 1 || !strings.Contains(strings.Join(options, " "), "StrictHostKeyChecking=yes") {
				t.Fatalf("prompts=%d options=%v", prompts, options)
			}
			data, _ := os.ReadFile(known)
			if !strings.HasPrefix(string(data), unrelated) || strings.Count(string(data), namespace+" ") != 1 {
				t.Fatalf("unrelated trust lost or approved key duplicated: %s", data)
			}
			if _, err = manager.Options(context.Background(), program, "example"); err != nil || prompts != 1 {
				t.Fatalf("known key prompted again: %v, %d", err, prompts)
			}
		})
	}
}

func TestFirstTrustFailureLeavesStoreUnchanged(t *testing.T) {
	for _, scenario := range []string{"declined", "confirmation-required", "authentication", "rotation", "concurrent-trust", "witness-mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			manager, program, known := fixture(t, "example.test")
			candidate := filepath.Join(filepath.Dir(known), "candidate")
			prompts := 0
			manager.Confirm = func(context.Context, []Proposal) error {
				prompts++
				switch scenario {
				case "declined":
					return domain.ErrOperationDeclined
				case "confirmation-required":
					return domain.ErrConfirmationRequired
				case "rotation":
					return os.WriteFile(candidate, []byte(keyLine(t, "example.test")), 0o600)
				case "concurrent-trust":
					return os.WriteFile(known, []byte(keyLine(t, "example.test")), 0o600)
				}
				return nil
			}
			if scenario == "authentication" {
				if err := os.WriteFile(candidate+".auth-failed", nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "witness-mismatch" {
				manager.Witness = func(context.Context, string, string, ssh.PublicKey) error { return errors.New("owner scan differs") }
			}
			_, err := manager.Options(context.Background(), program, "example")
			if err == nil {
				t.Fatal("unsafe trust accepted")
			}
			if (scenario == "authentication" || scenario == "witness-mismatch") && prompts != 0 {
				t.Fatal("prompted despite failed assessment")
			}
			if scenario != "concurrent-trust" {
				if _, err := os.Stat(known); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed first trust changed persistent store")
				}
			} else if !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("concurrent trust not rejected as stale: %v", err)
			}
		})
	}
}

func TestExistingTrustNeverOffersFirstTrust(t *testing.T) {
	for _, marker := range []string{"", "@revoked "} {
		manager, program, known := fixture(t, "example.test")
		if err := os.WriteFile(known, []byte(marker+keyLine(t, "example.test")), 0o600); err != nil {
			t.Fatal(err)
		}
		manager.Confirm = func(context.Context, []Proposal) error { t.Fatal("existing key was treated as unknown"); return nil }
		options, err := manager.Options(context.Background(), program, "example")
		if err != nil || !strings.Contains(strings.Join(options, " "), "StrictHostKeyChecking=yes") {
			t.Fatalf("existing trust must remain strict: %v, %v", options, err)
		}
	}
}

func TestRegisteredOwnerPinOverridesAmbientTrust(t *testing.T) {
	manager, program, known := fixture(t, "example.test")
	line, err := os.ReadFile(filepath.Join(filepath.Dir(known), "candidate"))
	if err != nil {
		t.Fatal(err)
	}
	trust, err := ownerinventory.NewSSHHostTrust(strings.TrimSpace(string(line)))
	if err != nil {
		t.Fatal(err)
	}
	manager.Pin = func(string) (*ownerinventory.SSHHostTrust, error) { return &trust, nil }
	manager.Confirm = func(context.Context, []Proposal) error { t.Fatal("registered owner prompted"); return nil }
	options, err := manager.Options(context.Background(), program, "example")
	if err != nil {
		t.Fatal(err)
	}
	_, err = (transport.Process{Program: program, Arguments: append(options, "example", "--", "true"), Env: manager.Environment}).Call(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(known); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("registered pin changed ambient trust")
	}
}

func TestOwnerAndYardFirstTrustAreOneAction(t *testing.T) {
	for _, outcome := range []string{"accept", "decline", "yard-auth-failure", "owner-rotation"} {
		t.Run(outcome, func(t *testing.T) {
			manager, yardProgram, yardKnown := fixture(t, "yard-namespace")
			owner, ownerProgram, ownerKnown := fixture(t, "owner-namespace")
			// A second transport process with independent synthetic keys/stores.
			script, err := os.ReadFile(ownerProgram)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range owner.Environment {
				name, value, _ := strings.Cut(entry, "=")
				if name == "KNOWN" || name == "CANDIDATE" || name == "NAMESPACE" {
					script = []byte(strings.ReplaceAll(string(script), "\"$"+name+"\"", shellquote.Word(value)))
				}
			}
			if err := os.WriteFile(ownerProgram, script, 0o700); err != nil {
				t.Fatal(err)
			}
			manager.Witness = func(ctx context.Context, _, target string, _ ssh.PublicKey) error {
				if target != "yard" {
					return nil
				}
				_, err := manager.Options(ctx, ownerProgram, "owner")
				return err
			}
			prompts := 0
			manager.Confirm = func(_ context.Context, proposals []Proposal) error {
				prompts++
				if len(proposals) != 2 {
					t.Fatalf("proposals=%v", proposals)
				}
				for _, path := range []string{ownerKnown, yardKnown} {
					if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("partial trust before combined consent")
					}
				}
				if outcome == "decline" {
					return domain.ErrOperationDeclined
				}
				if outcome == "owner-rotation" {
					return os.WriteFile(filepath.Join(filepath.Dir(ownerKnown), "candidate"), []byte(keyLine(t, "owner-namespace")), 0o600)
				}
				return nil
			}
			if outcome == "yard-auth-failure" {
				if err := os.WriteFile(filepath.Join(filepath.Dir(yardKnown), "candidate.auth-failed"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err = manager.Options(context.Background(), yardProgram, "yard")
			if outcome == "accept" {
				if err != nil || prompts != 1 {
					t.Fatalf("accepted err=%v prompts=%d", err, prompts)
				}
			} else if err == nil {
				t.Fatal("failed combined trust accepted")
			}
			if outcome == "yard-auth-failure" && prompts != 0 {
				t.Fatal("prompted with invalid final target")
			}
			for _, path := range []string{ownerKnown, yardKnown} {
				_, err := os.Stat(path)
				if outcome == "accept" && err != nil {
					t.Fatal(err)
				}
				if outcome != "accept" && !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed combined action left partial trust")
				}
			}
		})
	}
}
