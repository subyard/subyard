package sshtrust

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/testkit"
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
