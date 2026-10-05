package testkit

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Exercise real kernel ACLs without elevated access or external ACL tools.
// The production adapter invokes the standard tools against this same pinned FD.
func KernelAccessACL(_ context.Context, file *os.File, uid string) (string, error) {
	path := fmt.Sprintf("/proc/self/fd/%d", file.Fd())
	data := make([]byte, 4096)
	n, err := unix.Getxattr(path, "system.posix_acl_access", data)
	entries := map[string]uint16{"user:": 6, "group:": 6, "other:": 0}
	if err != nil && !errors.Is(err, unix.ENODATA) {
		return "", err
	}
	if err == nil {
		entries = map[string]uint16{}
		for offset := 4; offset < n; offset += 8 {
			tag := binary.LittleEndian.Uint16(data[offset:])
			kind := map[uint16]string{1: "user:", 2: "user:", 4: "group:", 8: "group:", 16: "mask:", 32: "other:"}[tag]
			if tag == 2 || tag == 8 {
				kind += strconv.FormatUint(uint64(binary.LittleEndian.Uint32(data[offset+4:])), 10)
			}
			entries[kind] = binary.LittleEndian.Uint16(data[offset+2:])
		}
	}
	if uid != "" {
		entries["user:"+uid], entries["mask:"] = 6, 6
		data = make([]byte, 4)
		binary.LittleEndian.PutUint32(data, 2)
		for key, permissions := range entries {
			kind, qualifier, _ := strings.Cut(key, ":")
			tag := map[string]uint16{"user": 1, "group": 4, "mask": 16, "other": 32}[kind]
			id := uint32(0xffffffff)
			if qualifier != "" {
				tag *= 2
				number, _ := strconv.ParseUint(qualifier, 10, 32)
				id = uint32(number)
			}
			entry := make([]byte, 8)
			binary.LittleEndian.PutUint16(entry, tag)
			binary.LittleEndian.PutUint16(entry[2:], permissions)
			binary.LittleEndian.PutUint32(entry[4:], id)
			data = append(data, entry...)
		}
		// Linux requires canonical ACL tag/ID order.
		records := make([][]byte, 0, len(entries))
		for offset := 4; offset < len(data); offset += 8 {
			records = append(records, append([]byte(nil), data[offset:offset+8]...))
		}
		sort.Slice(records, func(i, j int) bool {
			left, right := binary.LittleEndian.Uint16(records[i]), binary.LittleEndian.Uint16(records[j])
			return left < right || left == right && binary.LittleEndian.Uint32(records[i][4:]) < binary.LittleEndian.Uint32(records[j][4:])
		})
		data = data[:4]
		for _, entry := range records {
			data = append(data, entry...)
		}
		return "", unix.Setxattr(path, "system.posix_acl_access", data, 0)
	}
	var output []string
	for key, permissions := range entries {
		mode := []byte("---")
		for index, bit := range []uint16{4, 2, 1} {
			if permissions&bit != 0 {
				mode[index] = "rwx"[index]
			}
		}
		output = append(output, key+":"+string(mode))
	}
	sort.Strings(output)
	return strings.Join(output, "\n"), nil
}
