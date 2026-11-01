package cli

import (
	"context"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
	"golang.org/x/sys/unix"
)

// This checks the actual installed ordinary stop's barrier placement, with a
// stopped SDK peer. It does not claim physical stop or manager convergence.
func TestPublicationInstalledStopOwnerBarrier(t *testing.T) {
	testPublicationInstalledStopOwnerBarrier(t, false)
}

func TestPublicationSealedChildStopOwnerBarrier(t *testing.T) {
	testPublicationInstalledStopOwnerBarrier(t, true)
}

func testPublicationInstalledStopOwnerBarrier(t *testing.T, sealed bool) {
	program, ready, home, server := completedRustSourceAuthoringRuntimeFixture(t, false, nil)
	testkit.WriteFile(t, ready, []byte("ready\n"), 0600)
	server.SetInstance("subyard", "yard", map[string]any{"name": "yard", "type": "container", "status": "Stopped", "config": map[string]string{"volatile.uuid": "source-authoring-default", "user.subyard.managed": "true"}, "expanded_config": map[string]string{"volatile.uuid": "source-authoring-default", "user.subyard.managed": "true"}})
	engine := filepath.Join(program.options.RepositoryRoot, "bin/yard-engine")
	raw, err := os.ReadFile(engine)
	if err != nil {
		t.Fatal(err)
	}
	stage := os.Getenv("SUBYARD_RUST_SOURCE_QUERY_TOOLING_ROOT")
	if stage == "" {
		stage = filepath.Join(repositoryRoot(t), ".build/runtime/current")
	}
	supplied, err := os.ReadFile(filepath.Join(stage, "bin/yard-engine"))
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(raw) != sha256.Sum256(supplied) {
		t.Fatal("fixture changed the supplied installed engine")
	}
	t.Logf("actual installed stop engine sha256=%x", sha256.Sum256(raw))
	data := program.env["SUBYARD_HOME"]
	installation := uninstall.Installation{Schema: 1, OperatorUID: uint32(os.Getuid()), DataHome: data, ConfigHome: home, RuntimeRoot: program.env["YARD_RUNTIME_ROOT"], CacheRoot: filepath.Join(data, "releases")}
	body, err := uninstall.MarshalInstallation(installation)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(data, uninstall.ManifestName), body, 0600)
	_, release, err := uninstall.OwnerLock(context.Background(), data, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	evidence, err := os.MkdirTemp("/tmp", "subyard-installed-stop-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("private installed stop evidence:", evidence)
	output, err := os.OpenFile(filepath.Join(evidence, "output"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, engine, "stop", "--force", "--yes")
	if sealed {
		fd, err := unix.MemfdCreate("subyard-verified-yard-engine", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
		if err != nil {
			t.Fatal(err)
		}
		image := os.NewFile(uintptr(fd), "sealed installed engine")
		defer image.Close()
		if n, err := image.Write(raw); err != nil || n != len(raw) {
			t.Fatal("copy installed engine", err)
		}
		if _, err = unix.FcntlInt(image.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); err != nil {
			t.Fatal(err)
		}
		command = exec.CommandContext(ctx, "/proc/self/fd/3", "stop", "--force", "--yes")
		command.ExtraFiles = []*os.File{image}
	}
	for _, v := range program.options.Environment {
		if !strings.HasPrefix(v, "SUBYARD_NO_AUDIT=") {
			command.Env = append(command.Env, v)
		}
	}
	command.Stdout = output
	command.Stderr = output
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	drained := false
	defer func() {
		if !drained {
			if release != nil {
				release()
				release = nil
			}
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("owned stop did not drain")
			}
		}
	}()
	rootPID := command.Process.Pid
	if sealed {
		rootPID = os.Getpid()
	}
	start := facadeProcessStart(t, rootPID)
	lock := uninstall.OwnerLockPath(data)
	deadline := time.Now().Add(5 * time.Second)
	waiting := false
	var census []string
	for time.Now().Before(deadline) {
		waiting, census = publicationStopWaiter(rootPID, start, lock, sha256.Sum256(raw))
		if waiting {
			break
		}
		select {
		case err = <-done:
			drained = true
			t.Fatalf("ordinary installed stop exited before barrier: %v (private output retained)", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	testkit.WriteFile(t, filepath.Join(evidence, "processes"), []byte(strings.Join(census, "\n")+"\n"), 0600)
	if !waiting {
		t.Fatal("installed stop owner descriptor was not observed; private census retained")
	}
	t.Logf("installed stop owned process census: %v", census)
	probe, err := os.OpenFile(lock, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if err = syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != syscall.EWOULDBLOCK {
		t.Fatalf("producer owner exclusion: %v", err)
	}
	release()
	release = nil
	select {
	case err = <-done:
		drained = true
		if err != nil {
			t.Fatalf("ordinary stop after release: %v (private output retained)", err)
		}
	case <-ctx.Done():
		t.Fatal("ordinary stop did not finish after owner release")
	}
	if err = syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	_ = syscall.Flock(int(probe.Fd()), syscall.LOCK_UN)
}

func TestPublicationStopExecutableAdmission(t *testing.T) {
	content := []byte("bound installed engine image")
	expected := sha256.Sum256(content)
	for _, tc := range []struct {
		name              string
		seals             int
		root, wrong, want bool
	}{
		{"root-installed", 0, true, false, true},
		{"sealed-child", unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL, false, false, true},
		{"unsealed-child", 0, false, false, false},
		{"partial-seals", unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK, false, false, false},
		{"foreign-bytes", unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fd, err := unix.MemfdCreate("subyard-verified-yard-engine", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
			if err != nil {
				t.Fatal(err)
			}
			image := os.NewFile(uintptr(fd), "owned executable fixture")
			defer image.Close()
			value := content
			if tc.wrong {
				value = []byte("foreign executable")
			}
			if _, err = image.Write(value); err != nil {
				t.Fatal(err)
			}
			if tc.seals != 0 {
				if _, err = unix.FcntlInt(image.Fd(), unix.F_ADD_SEALS, tc.seals); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = image.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			matched, _, accepted, err := publicationStopExecutable(image, expected, tc.root)
			if err != nil || matched == tc.wrong || accepted != tc.want {
				t.Fatalf("digest=%t accepted=%t error=%v", matched, accepted, err)
			}
		})
	}
}
