package operatoraccess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ApprovedActor validates the captured UID against the elevated native context
// and the membership already approved by the owner installer.
func ApprovedActor(uid string) (uint32, error) {
	if os.Geteuid() != 0 || os.Getenv("SUBYARD_ENGINE_CONTEXT") != "1" || os.Getenv("SUBYARD_ENGINE_CONTEXT_SCHEMA") != "1" {
		return 0, errors.New("operator access requires the elevated prepared installer")
	}
	number, err := strconv.ParseUint(uid, 10, 32)
	if err != nil || strconv.FormatUint(number, 10) != uid {
		return 0, errors.New("invalid operator identity")
	}
	operator, err := user.LookupId(uid)
	if err != nil || operator.Username != os.Getenv("SUBYARD_USER") {
		return 0, errors.New("operator identity differs from the approved installer")
	}
	group, err := user.LookupGroup("incus-admin")
	if err != nil {
		return 0, errors.New("native Incus administrator group is unavailable")
	}
	groups, err := operator.GroupIds()
	if err != nil {
		return 0, errors.New("operator group membership is unavailable")
	}
	found := number == 0
	for _, gid := range groups {
		found = found || gid == group.Gid
	}
	if !found {
		return 0, errors.New("operator has not received the approved Incus administrator membership")
	}
	gid, err := strconv.ParseUint(group.Gid, 10, 32)
	if err != nil {
		return 0, errors.New("invalid Incus administrator group identity")
	}
	return uint32(gid), nil
}

// Tools operates only on a pinned native descriptor; an empty UID reads its ACL.
type Tools func(context.Context, *os.File, string) (string, error)

// GrantRW adds one named grant without changing modes, mask or unrelated entries.
// The owning adapter validates its fixed native object and supplies its CAS guard.
func GrantRW(ctx context.Context, file *os.File, uid string, checkIdentity func() error, tools Tools) error {
	number, err := strconv.ParseUint(uid, 10, 32)
	if err != nil || strconv.FormatUint(number, 10) != uid {
		return errors.New("invalid operator identity")
	}
	// setfacl may fall back to chmod without filesystem ACL support. Refuse first.
	bound := fmt.Sprintf("/proc/self/fd/%d", file.Fd())
	initialACL, err := accessACL(bound)
	if err != nil {
		return errors.New("native object does not support POSIX access ACLs")
	}
	initial, err := tools(ctx, file, "")
	if err != nil {
		return err
	}
	approved, err := parseACL(initial)
	if err != nil {
		return err
	}
	mask := approved["mask:"]
	if mask == "" {
		mask = approved["group:"]
	}
	if mask != "rw-" {
		return errors.New("native access ACL mask cannot grant access without widening permissions")
	}
	approved["user:"+uid] = "rw-"
	approved["mask:"] = mask
	if err := checkIdentity(); err != nil {
		return err
	}
	currentACL, err := accessACL(bound)
	if err != nil || !bytes.Equal(initialACL, currentACL) {
		return errors.New("native access ACL changed before operator grant")
	}
	if _, err := tools(ctx, file, uid); err != nil {
		return err
	}
	readback, err := tools(ctx, file, "")
	if err != nil {
		return err
	}
	actual, err := parseACL(readback)
	if err != nil || len(actual) != len(approved) {
		return errors.New("operator access ACL readback differs from the approved grant")
	}
	for key, value := range approved {
		if actual[key] != value {
			return errors.New("operator access ACL changed unrelated access")
		}
	}
	return checkIdentity()
}

func accessACL(path string) ([]byte, error) {
	payload := make([]byte, 64<<10)
	size, err := unix.Getxattr(path, "system.posix_acl_access", payload)
	if errors.Is(err, unix.ENODATA) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return payload[:size], nil
}

func parseACL(output string) (map[string]string, error) {
	entries := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		parts := strings.Split(line, ":")
		if len(parts) != 3 || len(parts[2]) != 3 {
			return nil, errors.New("invalid native access ACL observation")
		}
		switch parts[0] {
		case "user", "group":
			if parts[1] != "" {
				if _, err := strconv.ParseUint(parts[1], 10, 32); err != nil {
					return nil, errors.New("invalid native access ACL identity")
				}
			}
		case "mask", "other":
			if parts[1] != "" {
				return nil, errors.New("invalid native access ACL qualifier")
			}
		default:
			return nil, errors.New("invalid native access ACL entry")
		}
		for index, permission := range "rwx" {
			if parts[2][index] != byte(permission) && parts[2][index] != '-' {
				return nil, errors.New("invalid native access ACL permission")
			}
		}
		key := parts[0] + ":" + parts[1]
		if _, exists := entries[key]; exists {
			return nil, errors.New("duplicate native access ACL entry")
		}
		entries[key] = parts[2]
	}
	if entries["user:"] != "rw-" || entries["other:"] != "---" || entries["group:"] == "" {
		return nil, errors.New("native access base ACL is unsafe")
	}
	return entries, nil
}

// NativeACLToolsAvailable observes the fixed native prerequisites without invoking them.
// Missing tools are repairable; unsafe existing tools must not be replaced silently.
func NativeACLToolsAvailable() (bool, error) {
	return nativeACLToolsAvailableAt([]string{"/usr/bin/getfacl", "/usr/bin/setfacl"}, 0)
}

func nativeACLToolsAvailableAt(paths []string, ownerUID uint32) (bool, error) {
	available := true
	for _, path := range paths {
		present, err := nativeACLToolAvailable(path, ownerUID)
		if err != nil {
			return false, err
		}
		available = available && present
	}
	return available, nil
}

func nativeACLToolAvailable(tool string, ownerUID uint32) (bool, error) {
	info, err := os.Lstat(tool)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return false, errors.New("native ACL tool identity is unsafe")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != ownerUID || info.Mode().Perm()&0100 == 0 {
		return false, errors.New("native ACL tool ownership is unsafe")
	}
	return true, nil
}

func NativeACL(ctx context.Context, file *os.File, uid string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tool, arguments := "/usr/bin/getfacl", []string{"-cpnE", "--", "/proc/self/fd/3"}
	if uid != "" {
		tool, arguments = "/usr/bin/setfacl", []string{"-n", "-m", "u:" + uid + ":rw", "--", "/proc/self/fd/3"}
	}
	present, err := nativeACLToolAvailable(tool, 0)
	if err != nil {
		return "", err
	}
	if !present {
		return "", fmt.Errorf("native ACL tool %s is missing", tool)
	}
	command := exec.CommandContext(ctx, tool, arguments...)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	command.ExtraFiles = []*os.File{file}
	var output bytes.Buffer
	command.Stdout = &output
	if err := command.Run(); err != nil || output.Len() > 16<<10 {
		return "", errors.New("native operator access ACL command failed")
	}
	return output.String(), nil
}
