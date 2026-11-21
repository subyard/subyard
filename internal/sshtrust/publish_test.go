package sshtrust

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestPublishLocksNestedDirectoriesInConsistentOrder(t *testing.T) {
	root := testkit.TempDir(t)
	parentStore := filepath.Join(root, "a", "key")
	nestedStore := filepath.Join(root, "a", "x", "known")
	otherParentStore := filepath.Join(root, "a", "z")
	unrelated := keyLine(t, "unrelated.example")
	for _, path := range []string{parentStore, nestedStore, otherParentStore} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, []byte(unrelated), 0o600)
	}
	first := &Manager{Environment: os.Environ(), pending: []candidate{
		publicationCandidate(t, parentStore, "first-owner.example"),
		publicationCandidate(t, nestedStore, "first-yard.example"),
	}}
	second := &Manager{Environment: os.Environ(), pending: []candidate{
		publicationCandidate(t, nestedStore, "second-yard.example"),
		publicationCandidate(t, otherParentStore, "second-owner.example"),
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	unlock, err := LockKnownHosts(ctx, parentStore)
	if err != nil {
		t.Fatal(err)
	}
	releaseParent := sync.OnceFunc(unlock)
	defer releaseParent()
	results := make(chan error, 2)
	go func() { results <- first.publish(ctx) }()
	go func() { results <- second.publish(ctx) }()

	// Both publishers must reach the held parent lock. Sorting by filename
	// instead lets the second publisher hold the nested lock before this point.
	lockPath := filepath.Join(filepath.Dir(parentStore), ".subyard-known-hosts.lock")
	if err := waitForOpenLockFiles(ctx, lockPath, 3); err != nil {
		t.Errorf("publishers did not reach the parent lock: %v", err)
	} else {
		probeCtx, stopProbe := context.WithTimeout(ctx, 250*time.Millisecond)
		unlockNested, err := LockKnownHosts(probeCtx, nestedStore)
		stopProbe()
		if err != nil {
			t.Errorf("publisher acquired nested lock before parent lock: %v", err)
		} else {
			unlockNested()
		}
	}
	releaseParent()
	for range 2 {
		if err := <-results; err != nil {
			t.Errorf("concurrent publication failed: %v", err)
		}
	}
	for _, path := range []string{parentStore, nestedStore, otherParentStore} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want := unrelated
		for _, manager := range []*Manager{first, second} {
			for _, candidate := range manager.pending {
				if candidate.proposal.File == path {
					want += candidate.line + "\n"
					if !strings.Contains(string(data), candidate.line+"\n") {
						t.Errorf("approved key missing from %s", path)
					}
				}
			}
		}
		if !strings.HasPrefix(string(data), unrelated) || len(data) != len(want) {
			t.Errorf("publication lost unrelated entries or duplicated keys in %s", path)
		}
	}
}

func TestPublishCoalescesParentSymlinkAliases(t *testing.T) {
	root := testkit.TempDir(t)
	directory, alias := filepath.Join(root, "real"), filepath.Join(root, "alias")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(directory, alias); err != nil {
		t.Fatal(err)
	}
	store, aliasedStore := filepath.Join(directory, "known_hosts"), filepath.Join(alias, "known_hosts")
	unrelated := keyLine(t, "unrelated.example")
	testkit.WriteFile(t, store, []byte(unrelated), 0o600)
	first := publicationCandidate(t, store, "owner.example")
	second := publicationCandidate(t, aliasedStore, "yard.example")
	manager := &Manager{Environment: os.Environ(), pending: []candidate{first, second}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := manager.publish(ctx); err != nil {
		t.Fatalf("physical store aliases must not self-deadlock: %v", err)
	}
	data, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}
	want := unrelated + first.line + "\n" + second.line + "\n"
	if string(data) != want {
		t.Fatal("publication through aliases lost unrelated content or an approved key")
	}
	if manager.pending[0].proposal.File != store || manager.pending[1].proposal.File != aliasedStore {
		t.Fatal("publication changed the original confirmation paths")
	}
	if info, err := os.Lstat(alias); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("publication replaced the parent symlink: %v", err)
	}
}

func TestPublishPreservesFinalSymlink(t *testing.T) {
	for _, existing := range []bool{true, false} {
		name := "new-target"
		if existing {
			name = "existing-target"
		}
		t.Run(name, func(t *testing.T) {
			original := filepath.Join(testkit.TempDir(t), "known_hosts")
			target := filepath.Join(testkit.TempDir(t), "managed-known-hosts")
			link, err := filepath.Rel(filepath.Dir(original), target)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(link, original); err != nil {
				t.Fatal(err)
			}
			var unrelated string
			if existing {
				unrelated = keyLine(t, "unrelated.example")
				testkit.WriteFile(t, target, []byte(unrelated), 0o644)
			}
			first := publicationCandidate(t, original, "owner.example")
			second := publicationCandidate(t, target, "yard.example")
			manager := &Manager{Environment: os.Environ(), pending: []candidate{first, second}}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := manager.publish(ctx); err != nil {
				t.Fatal(err)
			}
			if result, err := os.Readlink(original); err != nil || result != link {
				t.Fatalf("symlink = %q, %v, want %q", result, err, link)
			}
			if data, err := os.ReadFile(target); err != nil || string(data) != unrelated+first.line+"\n"+second.line+"\n" {
				t.Fatalf("target contents = %q, %v", data, err)
			}
		})
	}
}

func TestPublishRejectsRetargetedStoreWithUnavailableParent(t *testing.T) {
	for _, parent := range []string{"missing-directory", "not-a-directory"} {
		t.Run(parent, func(t *testing.T) {
			directory := testkit.TempDir(t)
			store, target := filepath.Join(directory, "known_hosts"), filepath.Join(directory, "original")
			contents := keyLine(t, "unrelated.example")
			testkit.WriteFile(t, target, []byte(contents), 0o600)
			testkit.WriteFile(t, filepath.Join(directory, "not-a-directory"), nil, 0o600)
			if err := os.Symlink(target, store); err != nil {
				t.Fatal(err)
			}
			request := publicationCandidate(t, store, "owner.example")
			manager := &Manager{Environment: os.Environ(), pending: []candidate{request}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			unlock, err := LockKnownHosts(ctx, store)
			if err != nil {
				t.Fatal(err)
			}
			release := sync.OnceFunc(unlock)
			defer release()
			result := make(chan error, 1)
			go func() { result <- manager.publish(ctx) }()
			if err := waitForOpenLockFiles(ctx, filepath.Join(directory, ".subyard-known-hosts.lock"), 2); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(directory, parent, "known_hosts"), store+".next"); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(store+".next", store); err != nil {
				t.Fatal(err)
			}
			release()
			err = <-result
			want := domain.ErrPlanStale.Error() + ": SSH trust changed after assessment"
			if !errors.Is(err, domain.ErrPlanStale) || err.Error() != want {
				t.Fatalf("publication after retarget = %v, want %q", err, want)
			}
			if data, err := os.ReadFile(target); err != nil || string(data) != contents {
				t.Fatalf("original trust store changed: %q, %v", data, err)
			}
		})
	}
}

func TestPublishWithStationaryMissingStoreParentKeepsLockError(t *testing.T) {
	directory := testkit.TempDir(t)
	store := filepath.Join(directory, "known_hosts")
	target := filepath.Join(directory, "missing-directory", "known_hosts")
	if err := os.Symlink(target, store); err != nil {
		t.Fatal(err)
	}
	request := publicationCandidate(t, store, "owner.example")
	manager := &Manager{Environment: os.Environ(), pending: []candidate{request}}
	err := manager.publish(context.Background())
	if !errors.Is(err, unix.ENOENT) || err.Error() != "no such file or directory" {
		t.Fatalf("publication with missing store parent = %v", err)
	}
}

func TestPublishCoalescesBindMountedStoreAliases(t *testing.T) {
	const inNamespace = "SUBYARD_TEST_SSH_BIND_MOUNT"
	if os.Getenv(inNamespace) == "" {
		unshare, err := exec.LookPath("unshare")
		if err != nil {
			t.Skip("unshare is unavailable")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, unshare, "--user", "--map-root-user", "--mount",
			os.Args[0], "-test.v", "-test.run=^TestPublishCoalescesBindMountedStoreAliases$")
		command.Env = append(os.Environ(), inNamespace+"=1", "LC_ALL=C")
		output, err := command.CombinedOutput()
		if strings.HasPrefix(string(output), "unshare: unshare failed: Operation not permitted") ||
			strings.HasPrefix(string(output), "unshare: write failed /proc/self/uid_map: Operation not permitted") ||
			strings.Contains(string(output), "--- SKIP: TestPublishCoalescesBindMountedStoreAliases") {
			t.Skipf("private bind mount unavailable: %s", output)
		}
		if err != nil {
			t.Fatalf("bind-mount regression: %v\n%s", err, output)
		}
		return
	}

	root := testkit.TempDir(t)
	real, alias := filepath.Join(root, "real"), filepath.Join(root, "alias")
	for _, directory := range []string{real, alias} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mount(real, alias, "", unix.MS_BIND, ""); errors.Is(err, unix.EPERM) {
		t.Skip("mount permission unavailable in the private namespace")
	} else if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Unmount(alias, 0); err != nil {
			t.Error(err)
		}
	})
	first := publicationCandidate(t, filepath.Join(real, "known_hosts"), "first.example")
	second := publicationCandidate(t, filepath.Join(alias, "known_hosts"), "second.example")
	unrelated := keyLine(t, "unrelated.example")
	testkit.WriteFile(t, first.proposal.File, []byte(unrelated), 0o600)
	manager := &Manager{Environment: os.Environ(), pending: []candidate{first, second}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := manager.publish(ctx); err != nil {
		t.Fatal(err)
	}
	want := unrelated + first.line + "\n" + second.line + "\n"
	for _, path := range []string{first.proposal.File, second.proposal.File} {
		if data, err := os.ReadFile(path); err != nil || string(data) != want {
			t.Fatalf("trust store through %s = %q, %v, want both approved keys", path, data, err)
		}
	}
}

func TestPublishDeduplicatesHardLinkedLockInodes(t *testing.T) {
	configuredDir, targetDir := testkit.TempDir(t), testkit.TempDir(t)
	store, target := filepath.Join(configuredDir, "known_hosts"), filepath.Join(targetDir, "known_hosts")
	if err := os.Symlink(target, store); err != nil {
		t.Fatal(err)
	}
	lock := ".subyard-known-hosts.lock"
	testkit.WriteFile(t, filepath.Join(configuredDir, lock), nil, 0o600)
	if err := os.Link(filepath.Join(configuredDir, lock), filepath.Join(targetDir, lock)); err != nil {
		t.Fatal(err)
	}
	request := publicationCandidate(t, store, "owner.example")
	manager := &Manager{Environment: os.Environ(), pending: []candidate{request}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.publish(ctx); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != request.line+"\n" {
		t.Fatalf("published trust = %q, %v", data, err)
	}
}

func TestRemoveKnownHostsReportsRollbackSyncFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory read permission used to induce a sync failure")
	}
	directory := testkit.TempDir(t)
	path := filepath.Join(directory, "known_hosts")
	testkit.WriteFile(t, path, nil, 0o600)
	if err := os.Chmod(directory, 0o300); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(directory, 0o700)
	if err := removeKnownHosts(path); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("rollback without a readable parent = %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback left the new file: %v", err)
	}
}

func publicationCandidate(t *testing.T, path, namespace string) candidate {
	t.Helper()
	return candidate{
		proposal: Proposal{File: path, Namespace: namespace},
		line:     strings.TrimSuffix(keyLine(t, namespace), "\n"),
		files:    []string{path},
	}
}

func waitForOpenLockFiles(ctx context.Context, path string, count int) error {
	for {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			return err
		}
		opened := 0
		for _, entry := range entries {
			target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
			if err == nil && target == path {
				opened++
			}
		}
		if opened >= count {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("timed out waiting for publisher lock descriptors")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestPublishRefusesStoreLinkToDirectory(t *testing.T) {
	directory := testkit.TempDir(t)
	store := filepath.Join(directory, "known_hosts")
	if err := os.Symlink(".", store); err != nil {
		t.Fatal(err)
	}
	request := publicationCandidate(t, store, "owner.example")
	manager := &Manager{Environment: os.Environ(), pending: []candidate{request}}
	err := manager.publish(context.Background())
	if err == nil || err.Error() != "refusing non-regular SSH trust store" {
		t.Fatalf("publication through a link to a directory = %v", err)
	}
	if info, err := os.Lstat(store); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("store link replaced: %v", err)
	}
}
