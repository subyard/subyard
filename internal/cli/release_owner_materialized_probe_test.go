package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/releaseowner"
	"golang.org/x/sys/unix"
)

// Capture the original recipe from the production helper itself. The only
// requests are fixed readonly preparation and the existing first typed chunk.
func originalMaterializedHelperTarget(t *testing.T, helper string, environment []string, c releaseowner.Context, root, repository *os.File) (releaseowner.Target, int) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	local, remote := os.NewFile(uintptr(pair[0]), "original native preparation"), os.NewFile(uintptr(pair[1]), "native preparation child")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, helper, "_release-owner-native-v1")
	command.Env = environment
	command.Dir = c.RepositoryRoot
	command.Stdin = remote
	command.Stdout = remote
	if err := command.Start(); err != nil {
		local.Close()
		remote.Close()
		t.Fatal(err)
	}
	remote.Close()
	defer func() {
		local.Close()
		if err := command.Wait(); err != nil {
			t.Error("readonly helper close", err)
		}
	}()
	var sequence uint64
	exchange := func(request releaseowner.Request, files ...*os.File) releaseowner.Reply {
		t.Helper()
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		sequence++
		header := make([]byte, 52)
		copy(header, "SYC1")
		header[4] = 1
		header[5] = 15
		binary.BigEndian.PutUint64(header[8:16], sequence)
		sum := sha256.Sum256(body)
		copy(header[16:48], sum[:])
		binary.BigEndian.PutUint32(header[48:], uint32(len(body)))
		if len(files) != 0 {
			fds := make([]int, len(files))
			for index, file := range files {
				fds[index] = int(file.Fd())
			}
			if err := unix.Sendmsg(int(local.Fd()), header[:1], unix.UnixRights(fds...), nil, unix.MSG_NOSIGNAL); err != nil {
				t.Fatal(err)
			}
			header = header[1:]
		}
		if _, err := local.Write(append(header, body...)); err != nil {
			t.Fatal(err)
		}
		reply := make([]byte, 52)
		if _, err := io.ReadFull(local, reply); err != nil {
			t.Fatal("native reply header", err)
		}
		if !bytes.Equal(reply[:8], []byte{'S', 'Y', 'C', '1', 1, 15, 0, 0}) || binary.BigEndian.Uint64(reply[8:16]) != sequence || !bytes.Equal(reply[16:48], sum[:]) {
			t.Fatal("native preparation correlation")
		}
		size := binary.BigEndian.Uint32(reply[48:])
		if size > releaseowner.MaxReply {
			t.Fatal("native preparation reply bound")
		}
		raw := make([]byte, size)
		if _, err := io.ReadFull(local, raw); err != nil {
			t.Fatal(err)
		}
		var value releaseowner.Reply
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		if value.Code != "" {
			t.Fatal("native preparation refused", value.Code)
		}
		return value
	}
	exchange(releaseowner.Request{Op: "initialize", Context: &c}, root, repository)
	exchange(releaseowner.Request{Op: "capture-inventory", Session: c.Session})
	owner, _ := json.Marshal("materialized-config")
	values := exchange(releaseowner.Request{Op: "capture-activation", Session: c.Session, Owner: owner})
	for _, target := range values.Targets {
		if target.Class == "materialized-files" {
			chunk := exchange(releaseowner.Request{Op: "read-installed-recipe", Session: c.Session, RecipeHash: target.PreparedRecipe})
			if chunk.InstalledChunk == nil {
				t.Fatal("missing original typed recipe chunk")
			}
			return target, int(chunk.InstalledChunk.Total)
		}
	}
	t.Fatal("actual helper lacks original materialized recipe")
	return releaseowner.Target{}, 0
}
