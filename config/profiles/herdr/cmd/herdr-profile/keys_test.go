package main

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func publicKey(t *testing.T, number byte) ssh.PublicKey {
	t.Helper()
	key, err := ssh.NewPublicKey(ed25519.PublicKey(bytes.Repeat([]byte{number}, ed25519.PublicKeySize)))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestClientKeysPreserveOwnerAndManualLines(t *testing.T) {
	owner := publicKey(t, 1)
	client := publicKey(t, 2)
	ownerFP := ssh.FingerprintSHA256(owner)
	contents := append(ssh.MarshalAuthorizedKey(owner), ssh.MarshalAuthorizedKey(publicKey(t, 3))...)
	key := ssh.MarshalAuthorizedKey(client)
	added, change, err := planKeyChange(contents, key, ownerFP, "add", "phone")
	if err != nil || !change.Changed || !bytes.HasPrefix(added, contents) || !bytes.Contains(added, []byte(keyOptions)) || !bytes.HasSuffix(added, []byte(" subyard-herdr:phone\n")) {
		t.Fatalf("first add: %v, %v, %q", change, err, added)
	}
	again, change, err := planKeyChange(added, key, ownerFP, "add", "phone")
	if err != nil || change.Changed || !bytes.Equal(added, again) {
		t.Fatalf("repeat add: %v, %v", change, err)
	}
	revoked, change, err := planKeyChange(added, nil, ownerFP, "revoke", ssh.FingerprintSHA256(client))
	if err != nil || !change.Changed || !bytes.Equal(revoked, contents) {
		t.Fatalf("revoke: %v, %v, %q", change, err, revoked)
	}
	if _, _, err := planKeyChange(contents, ssh.MarshalAuthorizedKey(owner), ownerFP, "add", "phone"); err == nil {
		t.Fatal("owner reuse accepted")
	}
	if _, err := bareKey(append([]byte(`no-agent-forwarding `), key...)); err == nil {
		t.Fatal("supplied authorization options accepted")
	}
}

func TestSSHPolicyReportsIndependentFailures(t *testing.T) {
	const keyOnly = `authenticationmethods any
passwordauthentication no
kbdinteractiveauthentication no
hostbasedauthentication no
gssapiauthentication no
permitemptypasswords no
pubkeyauthentication yes
authorizedkeysfile .ssh/authorized_keys
`
	allowed := "context global\n" + keyOnly + "context dev\n" + keyOnly
	if err := checkSSHPolicy(strings.NewReader(allowed), "dev"); err != nil {
		t.Fatal(err)
	}
	bad := strings.ReplaceAll(strings.ReplaceAll(allowed, "passwordauthentication no", "passwordauthentication yes"), "kbdinteractiveauthentication no", "kbdinteractiveauthentication yes")
	err := checkSSHPolicy(strings.NewReader(bad), "dev")
	for _, finding := range []string{"passwordauthentication for global", "kbdinteractiveauthentication for dev"} {
		if err == nil || !strings.Contains(err.Error(), finding) {
			t.Fatalf("missing %s: %v", finding, err)
		}
	}
}
