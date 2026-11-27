package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/crypto/ssh"
)

func authorizedLine(key ssh.PublicKey, prefix, comment string) string {
	return prefix + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + " " + comment + "\n"
}

func certificateLine(t *testing.T, key ssh.PublicKey) string {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &ssh.Certificate{Key: key, CertType: ssh.UserCert, ValidBefore: ssh.CertTimeInfinity}
	if err := certificate.SignCert(rand.Reader, signer); err != nil {
		t.Fatal(err)
	}
	return authorizedLine(certificate, "", "cert")
}

func rsaKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(&private.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func statuses(keys []hostKey) string {
	var out []string
	for _, entry := range keys {
		out = append(out, entry.Label+"="+entry.Status)
	}
	return strings.Join(out, " ")
}

func TestHostKeysClassifyOnlyBareNonOwnerKeys(t *testing.T) {
	// A laptop RSA key the yard already accepts outside Herdr stays unmanaged.
	owner, fresh, granted, unmanaged := publicKey(t, 1), publicKey(t, 2), publicKey(t, 3), rsaKey(t)
	restricted, duplicate := publicKey(t, 5), publicKey(t, 6)
	host := "# comment\n\n" +
		authorizedLine(owner, "", "subyard-owner") +
		authorizedLine(fresh, "", "user@laptop") +
		authorizedLine(granted, "", "desk") +
		authorizedLine(unmanaged, "", "manual") +
		authorizedLine(restricted, `from="10.0.0.0/8" `, "office") +
		authorizedLine(restricted, `command="/bin/true",restrict `, "office") +
		authorizedLine(publicKey(t, 7), "cert-authority ", "ca") +
		certificateLine(t, publicKey(t, 8)) +
		"garbage line\n" +
		authorizedLine(duplicate, "restrict ", "first") +
		authorizedLine(duplicate, "", "second") +
		authorizedLine(duplicate, "", "third")
	ownerFP := ssh.FingerprintSHA256(owner)
	guest, _, err := planKeyChange([]byte(authorizedLine(owner, "", "owner")+authorizedLine(unmanaged, "", "manual")),
		ssh.MarshalAuthorizedKey(granted), ownerFP, "add", "desk")
	if err != nil {
		t.Fatal(err)
	}
	keys, unrecognized := classifyHostKeys([]byte(host), guest, true, ownerFP)
	want := "subyard-owner=skipped:owner user-laptop=new desk=granted manual=unmanaged office=skipped:options " +
		"ca=skipped:options cert=skipped:certificate second=new"
	if got := statuses(keys); got != want {
		t.Fatalf("statuses:\n got %s\nwant %s", got, want)
	}
	if fmt.Sprint(unrecognized) != "[11]" {
		t.Fatalf("unrecognized lines: %v", unrecognized)
	}
	if keys[1].Key != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(fresh))) || keys[4].Key != "" {
		t.Fatalf("offered key text: %+v", keys)
	}
	unknown, _ := classifyHostKeys([]byte(host), nil, false, ownerFP)
	if unknown[1].Status != "unknown" || unknown[2].Status != "unknown" || unknown[0].Status != "skipped:owner" {
		t.Fatalf("unreadable guest: %s", statuses(unknown))
	}
}

func TestHostKeyLabelsFitConsequences(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	for comment, want := range map[string]string{
		"user@laptop":                  "user-laptop",
		"  my phone (work) ":           "my-phone-work",
		"":                             "host-abababab",
		"---":                          "host-abababab",
		"ноутбук":                      "host-abababab",
		"ghp_example":                  "host-abababab",
		"eyJabcdefgh.abcdefgh.abcdef":  "host-abababab",
		"heyjoe":                       "heyjoe",
		"_lead.trail_":                 "lead.trail",
		strings.Repeat("a", 80):        strings.Repeat("a", 31),
		strings.Repeat("a", 30) + "-b": strings.Repeat("a", 30),
	} {
		got := hostKeyLabel(comment, digest)
		if got != want || len(got) > maxHostLabel || !safeLabel.MatchString(got) {
			t.Errorf("label %q = %q, want %q", comment, got, want)
		}
	}
}

func TestHostKeySelectionCollectsEveryRefusal(t *testing.T) {
	owner, fresh, unmanaged, restricted := publicKey(t, 1), publicKey(t, 2), publicKey(t, 4), publicKey(t, 5)
	ownerFP := ssh.FingerprintSHA256(owner)
	host := authorizedLine(owner, "", "owner") + authorizedLine(fresh, "", "laptop") +
		authorizedLine(unmanaged, "", "manual") + authorizedLine(restricted, "restrict ", "office")
	keys, _ := classifyHostKeys([]byte(host), []byte(authorizedLine(unmanaged, "", "manual")), true, ownerFP)
	missing := ssh.FingerprintSHA256(publicKey(t, 9))
	selected, err := selectHostKeys(keys, "select", []string{
		ssh.FingerprintSHA256(fresh), missing, ownerFP, ssh.FingerprintSHA256(unmanaged),
		ssh.FingerprintSHA256(restricted), ssh.FingerprintSHA256(fresh),
	})
	if len(selected) != 1 || selected[0].Label != "laptop" {
		t.Fatalf("selected: %+v", selected)
	}
	want := domain.CollectRefusals(
		hostKeyRefusal(keys[0]), hostKeyRefusal(keys[2]), hostKeyRefusal(keys[3]),
		fmt.Errorf("%s is not in the owner account's authorized_keys", missing),
	)
	if err == nil || err.Error() != want.Error() || strings.Count(err.Error(), "; ") != 3 {
		t.Fatalf("refusals:\n got %v\nwant %v", err, want)
	}
	all, err := selectHostKeys(keys, "all", nil)
	if err != nil || len(all) != 1 {
		t.Fatalf("all: %+v, %v", all, err)
	}
	if _, err := selectHostKeys(keys[2:], "all", nil); err == nil || strings.Count(err.Error(), "; ") != 2 ||
		!strings.Contains(err.Error(), "no key that can be paired") {
		t.Fatalf("all without grantable keys: %v", err)
	}
	var many []hostKey
	for index := range maxHostGrants + 1 {
		many = append(many, hostKey{Fingerprint: fmt.Sprintf("fp%d", index), Key: "key", Status: "new"})
	}
	if _, err := selectHostKeys(many, "all", nil); err == nil || !strings.Contains(err.Error(), "exceed the limit") {
		t.Fatalf("over limit: %v", err)
	}
	if _, err := selectHostKeys(many[:maxHostGrants], "all", nil); err != nil {
		t.Fatalf("at limit: %v", err)
	}
}

func TestHostKeyGrantRoundTripsThroughGuestKeys(t *testing.T) {
	owner, fresh := publicKey(t, 1), publicKey(t, 2)
	ownerFP := ssh.FingerprintSHA256(owner)
	host := []byte(authorizedLine(owner, "", "owner") + authorizedLine(fresh, "", "user@laptop"))
	guest := []byte(authorizedLine(owner, "", "owner"))
	keys, _ := classifyHostKeys(host, guest, true, ownerFP)
	selected, err := selectHostKeys(keys, "all", nil)
	if err != nil || len(selected) != 1 {
		t.Fatalf("select: %+v, %v", selected, err)
	}
	next, change, err := planKeyChange(guest, []byte(selected[0].Key), ownerFP, "add", selected[0].Label)
	if err != nil || !change.Changed || change.Fingerprint != selected[0].Fingerprint ||
		!bytes.HasSuffix(next, []byte(" subyard-herdr:user-laptop\n")) {
		t.Fatalf("add: %+v, %v, %q", change, err, next)
	}
	again, _ := classifyHostKeys(host, next, true, ownerFP)
	if again[1].Status != "granted" {
		t.Fatalf("after grant: %s", statuses(again))
	}
	if _, change, err := planKeyChange(next, []byte(selected[0].Key), ownerFP, "existing", selected[0].Label); err != nil || change.Changed {
		t.Fatalf("existing: %+v, %v", change, err)
	}
}

func TestReadAuthorizedKeysFollowsStrictModes(t *testing.T) {
	home := testkit.TempDir(t)
	dir := filepath.Join(home, ".ssh")
	path := filepath.Join(dir, "authorized_keys")
	if _, _, err := readAuthorizedKeys(home); err == nil {
		t.Fatal("missing SSH directory accepted")
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte("ssh-ed25519 AAAA\n"), 0o620)
	if _, _, err := readAuthorizedKeys(home); err == nil {
		t.Fatal("group-writable authorized_keys accepted")
	}
	testkit.WriteFile(t, path, []byte("ssh-ed25519 AAAA\n"), 0o644)
	if _, contents, err := readAuthorizedKeys(home); err != nil || string(contents) != "ssh-ed25519 AAAA\n" {
		t.Fatalf("readable authorized_keys: %q, %v", contents, err)
	}
}
