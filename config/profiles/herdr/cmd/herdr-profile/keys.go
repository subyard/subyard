package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Subyard/Subyard/internal/safefs"
	"golang.org/x/crypto/ssh"
)

const mark = "subyard-herdr:"
const keyOptions = `from="127.0.0.1,::1",no-agent-forwarding `

var safeLabel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var publicFingerprint = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)

func bareKey(input []byte) (ssh.PublicKey, error) {
	input = bytes.TrimSuffix(input, []byte("\n"))
	if len(input) > 16384 || bytes.ContainsAny(input, "\r\n\x00") {
		return nil, errors.New("expected one bare OpenSSH public key")
	}
	key, _, options, rest, err := ssh.ParseAuthorizedKey(input)
	if err != nil || len(rest) != 0 || len(options) != 0 || !bytes.HasPrefix(input, []byte(key.Type()+" ")) {
		return nil, errors.New("expected one bare OpenSSH public key without authorization options")
	}
	return key, nil
}

type keyLine struct {
	fingerprint, label string
	marked             bool
}

func parseKeyLine(line []byte) keyLine {
	key, comment, _, _, err := ssh.ParseAuthorizedKey(line)
	if err != nil {
		return keyLine{}
	}
	label, marked := strings.CutPrefix(comment, mark)
	marked = marked && safeLabel.MatchString(label) && bytes.HasPrefix(line, []byte(keyOptions))
	if !marked {
		label = ""
	}
	return keyLine{ssh.FingerprintSHA256(key), label, marked}
}

type keyChange struct {
	Fingerprint string `json:"fingerprint"`
	Digest      string `json:"digest"`
	Changed     bool   `json:"changed"`
}

func planKeyChange(contents, input []byte, owner, operation, argument string) ([]byte, keyChange, error) {
	var result keyChange
	lines := bytes.SplitAfter(contents, []byte{10})
	switch operation {
	case "add", "existing":
		if !safeLabel.MatchString(argument) {
			return nil, result, errors.New("client label must contain 1–64 letters, digits, dots, underscores or hyphens")
		}
		key, err := bareKey(input)
		if err != nil {
			return nil, result, err
		}
		result.Fingerprint = ssh.FingerprintSHA256(key)
		if result.Fingerprint == owner {
			return nil, result, errors.New("the Subyard owner SSH key cannot be paired as a Herdr client")
		}
		for _, line := range lines {
			info := parseKeyLine(line)
			if info.fingerprint == result.Fingerprint {
				if !info.marked {
					return nil, result, errors.New("this key is already authorized outside Herdr")
				}
				return contents, result, nil
			}
		}
		if operation == "existing" {
			return nil, result, errors.New("the Herdr grant changed; retry pairing")
		}
		result.Changed = true
		next := bytes.Clone(contents)
		if len(next) != 0 && next[len(next)-1] != '\n' {
			next = append(next, '\n')
		}
		next = append(next, keyOptions+strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))+" "+mark+argument+"\n"...)
		return next, result, nil
	case "revoke":
		if !publicFingerprint.MatchString(argument) || argument == owner {
			return nil, result, errors.New("give a Herdr client SHA256 fingerprint; the owner key cannot be revoked")
		}
		result.Fingerprint = argument
		var kept bytes.Buffer
		for _, line := range lines {
			info := parseKeyLine(line)
			if info.fingerprint == argument && info.marked {
				result.Changed = true
				continue
			}
			kept.Write(line)
		}
		return kept.Bytes(), result, nil
	default:
		return nil, result, errors.New("unknown key operation")
	}
}

func manageKeys(args []string) error {
	if len(args) < 3 || !publicFingerprint.MatchString(args[2]) {
		return errors.New("managed owner SSH fingerprint is unavailable")
	}
	phase, operation, owner := args[0], args[1], args[2]
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".ssh")
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return errors.New("unsafe or missing development user SSH directory")
	}
	path := filepath.Join(dir, "authorized_keys")
	uid := uint32(os.Getuid())
	options := safefs.ReadOptions{MaxBytes: 1 << 20, OwnerUID: &uid, DenyPerm: 0o077, SingleLink: true}
	contents, err := safefs.ReadRegular(path, options)
	if err != nil {
		return err
	}
	if operation == "list" && len(args) == 3 && phase == "apply" {
		for i, line := range bytes.Split(contents, []byte{10}) {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 || trimmed[0] == '#' {
				continue
			}
			info := parseKeyLine(line)
			if info.fingerprint == owner {
				continue
			}
			switch {
			case info.marked:
				fmt.Printf("%d  %s  %s\n", i+1, info.fingerprint, info.label)
			case info.fingerprint != "":
				fmt.Printf("%d  %s  unmanaged\n", i+1, info.fingerprint)
			default:
				fmt.Printf("%d  unrecognized line\n", i+1)
			}
		}
		return nil
	}
	if len(args) != 4 || (phase != "prepare" && phase != "apply") {
		return errors.New("invalid key operation arguments")
	}
	input, err := io.ReadAll(io.LimitReader(os.Stdin, 16385))
	if err != nil {
		return err
	}
	next, assessment, err := planKeyChange(contents, input, owner, operation, args[3])
	if err != nil {
		return err
	}
	if phase == "apply" && assessment.Changed {
		fresh, err := safefs.ReadRegular(path, options)
		if err != nil || !bytes.Equal(fresh, contents) {
			return errors.New("authorized_keys changed; retry pairing")
		}
		if err := safefs.WriteFile(path, next, safefs.Options{Mode: 0o600}); err != nil {
			return err
		}
	}
	digest, _ := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(assessment.Fingerprint, "SHA256:"))
	assessment.Digest = hex.EncodeToString(digest)
	return json.NewEncoder(os.Stdout).Encode(assessment)
}
