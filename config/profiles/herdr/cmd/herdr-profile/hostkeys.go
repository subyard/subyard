package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"golang.org/x/crypto/ssh"
)

// maxHostGrants matches the engine's limit of consequences in one confirmation.
const maxHostGrants = 64

// maxHostLabel keeps a label below the engine's opaque-token length in consequences.
const maxHostLabel = 31

// hostKey is one fingerprint offered from the owner account's authorized_keys.
// Status is new, granted, unmanaged, unknown (guest unreadable) or skipped:<reason>.
type hostKey struct {
	Fingerprint string `json:"fingerprint"`
	Digest      string `json:"digest"`
	Label       string `json:"label"`
	Status      string `json:"status"`
	Key         string `json:"key,omitempty"`
}

type hostKeyReport struct {
	Keys         []hostKey `json:"keys"`
	Unrecognized []int     `json:"unrecognized"`
	Selected     []hostKey `json:"selected"`
	Refusal      string    `json:"refusal"`
}

// hostSkipReason offers only bare public keys: options such as from= or
// command= restrict a key in the host's sshd, and Herdr would bypass them.
func hostSkipReason(key ssh.PublicKey, options []string, owner string) string {
	switch {
	case ssh.FingerprintSHA256(key) == owner:
		return "owner"
	case len(options) != 0:
		return "options"
	}
	if _, certificate := key.(*ssh.Certificate); certificate {
		return "certificate"
	}
	return ""
}

// hostKeyLabel derives a Herdr label from a key comment.
func hostKeyLabel(comment, digest string) string {
	var label strings.Builder
	for _, character := range comment {
		switch {
		case character < 128 && (character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '.' || character == '_'):
			label.WriteRune(character)
		case !strings.HasSuffix(label.String(), "-"):
			label.WriteByte('-')
		}
	}
	value := strings.TrimLeft(label.String(), "-._")
	if len(value) > maxHostLabel {
		value = value[:maxHostLabel]
	}
	value = strings.TrimRight(value, "-._")
	if !safeLabel.MatchString(value) || credentialShaped(value) {
		return "host-" + digest[:8]
	}
	return value
}

var webToken = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{6,}\b`)

// credentialShaped mirrors the token shapes the engine refuses in consequence text.
func credentialShaped(label string) bool {
	lower := strings.ToLower(label)
	for _, prefix := range []string{"ghp_", "github_pat_", "sk-proj-", "sk-live-", "xoxb-", "xoxp-", "xoxa-", "xoxr-"} {
		if strings.Contains(lower, prefix) {
			return true
		}
	}
	return webToken.MatchString(label)
}

// parseGuestGrants reads the guest helper's bounded "keys apply list" output. The first
// line of a fingerprint decides, as in the guest key operation. A Herdr grant labeled
// "unmanaged" reads as unmanaged, which only refuses a key the guest would keep.
func parseGuestGrants(list []byte) map[string]keyLine {
	grants := map[string]keyLine{}
	for _, line := range strings.Split(string(list), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || !publicFingerprint.MatchString(fields[1]) {
			continue
		}
		if _, seen := grants[fields[1]]; !seen {
			grants[fields[1]] = keyLine{fields[1], fields[2], fields[2] != "unmanaged"}
		}
	}
	return grants
}

// classifyHostKeys lists one entry per fingerprint in first-seen order. The first
// bare line of a fingerprint wins over its lines with options. Nil grants mean the
// guest state is unknown.
func classifyHostKeys(host []byte, grants map[string]keyLine, owner string) ([]hostKey, []int) {
	var keys []hostKey
	var unrecognized []int
	index := map[string]int{}
	for number, line := range bytes.Split(host, []byte{'\n'}) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || trimmed[0] == '#' {
			continue
		}
		key, comment, options, rest, err := ssh.ParseAuthorizedKey(line)
		// sshd rejects an empty options field such as a lone comma before the key type.
		if err != nil || len(rest) != 0 || len(options) == 0 && !bytes.HasPrefix(trimmed, []byte(key.Type()+" ")) {
			unrecognized = append(unrecognized, number+1)
			continue
		}
		entry := hostKey{Fingerprint: ssh.FingerprintSHA256(key)}
		entry.Digest = fingerprintDigest(entry.Fingerprint)
		entry.Label = hostKeyLabel(comment, entry.Digest)
		if reason := hostSkipReason(key, options, owner); reason != "" {
			entry.Status = "skipped:" + reason
		} else {
			entry.Key = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
			grant, present := grants[entry.Fingerprint]
			switch {
			case grants == nil:
				entry.Status = "unknown"
			case !present:
				entry.Status = "new"
			case grant.marked:
				entry.Status, entry.Label = "granted", grant.label
			default:
				entry.Status = "unmanaged"
			}
		}
		position, seen := index[entry.Fingerprint]
		switch {
		case !seen:
			index[entry.Fingerprint] = len(keys)
			keys = append(keys, entry)
		case keys[position].Key == "" && entry.Key != "":
			keys[position] = entry
		}
	}
	return keys, unrecognized
}

func hostKeyRefusal(entry hostKey) error {
	switch entry.Status {
	case "skipped:owner":
		return fmt.Errorf("%s is the Subyard owner SSH key and cannot be paired", entry.Fingerprint)
	case "skipped:options":
		return fmt.Errorf("%s has authorization options in the host file and needs pair --key", entry.Fingerprint)
	case "skipped:certificate":
		return fmt.Errorf("%s is a certificate and needs a plain public key with pair --key", entry.Fingerprint)
	case "unmanaged":
		return fmt.Errorf("%s is already authorized in the yard outside Herdr", entry.Fingerprint)
	}
	return nil
}

func grantable(entry hostKey) bool {
	return entry.Key != "" && entry.Status != "unmanaged"
}

// selectHostKeys returns the keys to grant and every independent refusal.
func selectHostKeys(keys []hostKey, mode string, fingerprints []string) ([]hostKey, error) {
	var selected []hostKey
	var refusals []error
	switch mode {
	case "all":
		for _, entry := range keys {
			if grantable(entry) {
				selected = append(selected, entry)
			}
		}
		if len(selected) == 0 {
			refusals = append(refusals, errors.New("the owner account's authorized_keys has no key that can be paired"))
			for _, entry := range keys {
				refusals = append(refusals, hostKeyRefusal(entry))
			}
		}
	case "select":
		seen := map[string]bool{}
		for _, fingerprint := range fingerprints {
			if seen[fingerprint] {
				continue
			}
			seen[fingerprint] = true
			position := slices.IndexFunc(keys, func(entry hostKey) bool { return entry.Fingerprint == fingerprint })
			switch {
			case !publicFingerprint.MatchString(fingerprint):
				refusals = append(refusals, fmt.Errorf("%q is not a SHA256 public key fingerprint", fingerprint))
			case position < 0:
				refusals = append(refusals, fmt.Errorf("%s is not in the owner account's authorized_keys", fingerprint))
			case grantable(keys[position]):
				selected = append(selected, keys[position])
			default:
				refusals = append(refusals, hostKeyRefusal(keys[position]))
			}
		}
	}
	fresh := 0
	for _, entry := range selected {
		if entry.Status != "granted" {
			fresh++
		}
	}
	if fresh > maxHostGrants {
		refusals = append(refusals, fmt.Errorf("%d new keys exceed the limit of %d for one pairing; list their fingerprints in smaller groups", fresh, maxHostGrants))
	}
	return selected, domain.CollectRefusals(refusals...)
}

// hostKeys implements:
//
//	host-keys [--refusal TEXT]... HOME OWNER known|unknown list|all|select [FINGERPRINT...]
//
// The caller passes refusals it determined itself, so the boundary check reports
// them with the host findings. OWNER is empty when the managed owner key is unreadable.
// A known guest grant list arrives on stdin; it only refines the preview, because the
// guest key operation remains authoritative when it applies a grant.
func hostKeys(args []string) error {
	var refusals []error
	for len(args) >= 2 && args[0] == "--refusal" {
		refusals = append(refusals, errors.New(args[1]))
		args = args[2:]
	}
	if len(args) < 4 || (args[1] != "" && !publicFingerprint.MatchString(args[1])) || (args[2] != "known" && args[2] != "unknown") ||
		!slices.Contains([]string{"list", "all", "select"}, args[3]) || (args[3] == "select") != (len(args) > 4) {
		return errors.New("invalid host key arguments")
	}
	home, owner, mode := args[0], args[1], args[3]
	if owner == "" {
		refusals = append(refusals, errors.New("could not read the managed owner public key fingerprint"))
	}
	var grants map[string]keyLine
	if args[2] == "known" {
		list, err := io.ReadAll(io.LimitReader(os.Stdin, authorizedKeysLimit+1))
		if err != nil {
			return err
		}
		if len(list) <= authorizedKeysLimit {
			grants = parseGuestGrants(list)
		}
	}
	report := hostKeyReport{Keys: []hostKey{}, Unrecognized: []int{}, Selected: []hostKey{}}
	var contents []byte
	var err error
	if home == "" {
		err = errors.New("the owner account home is unavailable")
	} else if _, contents, err = readAuthorizedKeys(home, "owner account"); err != nil {
		err = fmt.Errorf("could not read the owner account's authorized_keys: %w", err)
	}
	if err != nil {
		refusals = append(refusals, err)
	} else {
		keys, unrecognized := classifyHostKeys(contents, grants, owner)
		report.Keys = append(report.Keys, keys...)
		report.Unrecognized = append(report.Unrecognized, unrecognized...)
		if mode != "list" {
			selected, err := selectHostKeys(keys, mode, args[4:])
			report.Selected = append(report.Selected, selected...)
			refusals = append(refusals, err)
		}
	}
	if err := domain.CollectRefusals(refusals...); err != nil {
		report.Refusal = err.Error()
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}
