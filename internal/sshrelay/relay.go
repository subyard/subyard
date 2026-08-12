package sshrelay

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Ready verifies the root-owned relay declaration and the corresponding active socket.
func Ready(ctx context.Context, unitDir string, expectedUID int, port int, target, systemctl string) bool {
	if port < 1 || port > 65535 || target == "" || systemctl == "" {
		return false
	}
	unit := "subyard-ssh-relay-" + strconv.Itoa(port)
	socket, ok := safeUnit(filepath.Join(unitDir, unit+".socket"), expectedUID)
	if !ok || directive(socket, "ListenStream") != "127.0.0.1:"+strconv.Itoa(port) ||
		directive(socket, "Accept") != "no" {
		return false
	}
	service, ok := safeUnit(filepath.Join(unitDir, unit+".service"), expectedUID)
	if !ok {
		return false
	}
	fields := strings.Fields(directive(service, "ExecStart"))
	if len(fields) != 2 || !allowedProxy(fields[0]) || fields[1] != target+":22" {
		return false
	}
	command := exec.CommandContext(ctx, systemctl, "is-active", "--quiet", unit+".socket")
	return command.Run() == nil
}

func SystemctlPath() string {
	for _, candidate := range []string{"/usr/bin/systemctl", "/bin/systemctl"} {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

func safeUnit(path string, expectedUID int) (string, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm() != 0o644 {
		return "", false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != expectedUID {
		return "", false
	}
	contents, err := os.ReadFile(path)
	return string(contents), err == nil
}

func directive(contents, name string) string {
	value := ""
	count := 0
	for _, line := range strings.Split(contents, "\n") {
		key, candidate, found := strings.Cut(strings.TrimSpace(line), "=")
		if found && key == name {
			count++
			value = strings.TrimSpace(candidate)
		}
	}
	if count != 1 {
		return ""
	}
	return value
}

func allowedProxy(path string) bool {
	return path == "/usr/lib/systemd/systemd-socket-proxyd" ||
		path == "/lib/systemd/systemd-socket-proxyd"
}
