package sshidentity

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	Dedicated = "dedicated"
	Legacy    = "legacy"
	Drift     = "drift"
)

func Classify(operatorHome, dataHome, yardName string) string {
	canonical, ok := canonicalPath(dataHome)
	if !ok {
		return Drift
	}
	suffix := ""
	if yardName != "" && yardName != "default" {
		suffix = "-" + yardName
	}
	snippet := filepath.Join(operatorHome, ".ssh", "subyard"+suffix+".config")
	configured, ok := snippetIdentity(snippet)
	if !ok {
		return Drift
	}
	if configured != canonical {
		return Legacy
	}
	if !ValidCanonicalPair(dataHome) {
		return Drift
	}
	return Dedicated
}

func ValidCanonicalPair(dataHome string) bool {
	_, ok := CanonicalPublicKey(dataHome)
	return ok
}

func CanonicalPublicKey(dataHome string) (string, bool) {
	canonical, ok := canonicalPath(dataHome)
	if !ok {
		return "", false
	}
	keyDir := filepath.Dir(canonical)
	uid := os.Geteuid()
	if !safePath(keyDir, true, 0o700, uid) ||
		!safePath(canonical, false, 0o600, uid) ||
		!safePath(canonical+".pub", false, 0o644, uid) {
		return "", false
	}
	public, err := os.ReadFile(canonical + ".pub")
	if err != nil {
		return "", false
	}
	publicKey, ok := normalizedPublic(string(public))
	if !ok {
		return "", false
	}
	derived, err := exec.Command("ssh-keygen", "-y", "-P", "", "-f", canonical).Output()
	if err != nil {
		return "", false
	}
	privateKey, ok := normalizedPublic(string(derived))
	return publicKey, ok && privateKey == publicKey
}

func canonicalPath(dataHome string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(dataHome)
	if err != nil {
		return "", false
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", false
	}
	return filepath.Join(abs, "ssh", "id_ed25519"), true
}

func safePath(path string, directory bool, mode os.FileMode, uid int) bool {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode {
		return false
	}
	if directory != info.IsDir() || (!directory && !info.Mode().IsRegular()) {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == uid
}

func snippetIdentity(path string) (string, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm() != 0o600 {
		return "", false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return "", false
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	identity := ""
	identityCount := 0
	identitiesOnly := false
	identitiesOnlyCount := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		name, value, found := strings.Cut(line, " ")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.EqualFold(name, "IdentitiesOnly") {
			identitiesOnlyCount++
			identitiesOnly = strings.EqualFold(value, "yes")
			continue
		}
		if !strings.EqualFold(name, "IdentityFile") {
			continue
		}
		identityCount++
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		value = strings.ReplaceAll(value, "%%", "%")
		if filepath.IsAbs(value) {
			identity = filepath.Clean(value)
			continue
		}
		return "", false
	}
	return identity, scanner.Err() == nil && identityCount == 1 &&
		identitiesOnlyCount == 1 && identitiesOnly
}

func normalizedPublic(value string) (string, bool) {
	fields := strings.Fields(value)
	if len(fields) < 2 || fields[0] != "ssh-ed25519" {
		return "", false
	}
	return fields[0] + " " + fields[1], true
}
