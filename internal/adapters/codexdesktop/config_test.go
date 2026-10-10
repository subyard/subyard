package codexdesktop

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/clientprojects"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func input() clientprojects.Export {
	return clientprojects.Export{HostID: "example", Yard: "dev", SSHHost: "yard-example", Projects: []clientprojects.Project{{Name: "web", Path: "/srv/workspaces/web/src"}, {Name: "api", Path: "/srv/workspaces/api/src"}}}
}

func target(t *testing.T) string {
	t.Helper()
	return filepath.Join(testkit.TempDir(t), "codex-app", "config.json")
}

func prepareOK(t *testing.T, filename string, in clientprojects.Export) clientprojects.Plan {
	t.Helper()
	p, err := Prepare(filename, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func read(t *testing.T, filename string) []byte {
	t.Helper()
	content, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	return content
}
func applyOK(t *testing.T, p clientprojects.Plan) {
	t.Helper()
	if err := p.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConfigPath(t *testing.T) {
	for _, tc := range []struct{ home, codex, override, want string }{
		{"/home/operator", "", "", "/home/operator/.codex/codex-app/config.json"},
		{"", "/opt/gui", "", "/opt/gui/codex-app/config.json"},
		{"", "", "/tmp/other.json", "/tmp/other.json"},
	} {
		got, err := ConfigPath(tc.home, tc.codex, tc.override)
		if err != nil || got != tc.want {
			t.Fatalf("got %q, %v", got, err)
		}
	}
	for _, tc := range []struct{ home, codex, override string }{{"relative", "", ""}, {"/home/operator", "relative", ""}, {"/home/operator", "", "relative"}, {"/home/operator\x00", "", ""}} {
		if _, err := ConfigPath(tc.home, tc.codex, tc.override); err == nil {
			t.Fatal("accepted invalid path")
		}
	}
}

func TestExactDeclarationAndNoop(t *testing.T) {
	filename := target(t)
	in := input()
	in.Projects[0].Name = `web "quoted"`
	p := prepareOK(t, filename, in)
	if !p.Changed || p.Added != 2 {
		t.Fatalf("plan %#v", p)
	}
	if _, err := os.Stat(filepath.Dir(filename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preview created config directory")
	}
	applyOK(t, p)
	want := `{
  "version": 1,
  "remoteConnections": [
    {
      "sshAlias": "yard-example",
      "projects": [
        {
          "remotePath": "/srv/workspaces/api/src",
          "label": "api / example/dev"
        },
        {
          "remotePath": "/srv/workspaces/web/src",
          "label": "web \"quoted\" / example/dev"
        }
      ]
    }
  ]
}
`
	if got := string(read(t, filename)); got != want {
		t.Fatalf("unexpected declaration:\n%s", got)
	}
	before, _, err := inspect(filename)
	if err != nil {
		t.Fatal(err)
	}
	p = prepareOK(t, filename, in)
	if p.Changed || p.Added != 0 {
		t.Fatal("repeat changes config")
	}
	applyOK(t, p)
	after, _, err := inspect(filename)
	if err != nil {
		t.Fatal(err)
	}
	if !sameSnapshot(before, after) {
		t.Fatal("noop rewrote config")
	}
	if _, err := os.Stat(filename + ".subyard-backup"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("noop created backup")
	}
}

func TestUnionPreservesConnectionsLabelsAndPreferences(t *testing.T) {
	filename := target(t)
	if err := os.Mkdir(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"version":1,"remoteConnectionMaxRetryAttempts":0,"sshConnectTimeoutSeconds":42,"remoteConnections":[{"sshAlias":"personal","projects":[{"remotePath":"/personal/path","label":"Custom label"}]},{"sshAlias":"yard-example","projects":[{"remotePath":"/srv/workspaces/api/src/.","label":"Custom API"},{"remotePath":"/retired/path"}]}]}`)
	testkit.WriteFile(t, filename, original, 0o640)
	p := prepareOK(t, filename, input())
	if !p.Changed || p.Added != 1 {
		t.Fatalf("plan %#v", p)
	}
	applyOK(t, p)
	if !bytes.Equal(read(t, filename+".subyard-backup"), original) {
		t.Fatal("backup did not preserve exact baseline")
	}
	var got declaration
	if err := json.Unmarshal(read(t, filename), &got); err != nil {
		t.Fatal(err)
	}
	if got.MaxRetries == nil || *got.MaxRetries != 0 || got.ConnectTimeout == nil || *got.ConnectTimeout != 42 {
		t.Fatal("lost preferences")
	}
	if len(got.Connections) != 2 || got.Connections[0].Alias != "personal" || got.Connections[0].Projects[0].Label != "Custom label" || got.Connections[1].Projects[0].Label != "Custom API" || len(got.Connections[1].Projects) != 3 {
		t.Fatalf("lost unrelated declaration: %#v", got)
	}
	other := input()
	other.HostID = "other"
	other.Yard = "named"
	other.SSHHost = "yard-other"
	applyOK(t, prepareOK(t, filename, other))
	if err := json.Unmarshal(read(t, filename), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Connections) != 3 || got.Connections[0].Alias != "personal" || got.Connections[2].Projects[0].Label != "api / other/named" {
		t.Fatal("second yard lost first")
	}
	noProjects := input()
	noProjects.Projects = nil
	p = prepareOK(t, filename, noProjects)
	if p.Changed || p.Added != 0 {
		t.Fatal("empty inventory mutated declaration")
	}
}

func TestRejectInvalidSchemaWithoutMutation(t *testing.T) {
	for name, content := range map[string]string{
		"malformed":           `{`,
		"version":             `{"version":2,"remoteConnections":[]}`,
		"missing-version":     `{"remoteConnections":[]}`,
		"missing-connections": `{"version":1}`,
		"unknown":             `{"version":1,"remoteConnections":[],"unknown":true}`,
		"null":                `{"version":1,"remoteConnections":null}`,
		"optional-null":       `{"version":1,"remoteConnections":[],"sshConnectTimeoutSeconds":null}`,
		"negative":            `{"version":1,"remoteConnections":[],"sshConnectTimeoutSeconds":-1}`,
		"fractional":          `{"version":1,"remoteConnections":[],"sshConnectTimeoutSeconds":0.5}`,
		"duplicate":           `{"version":1,"version":1,"remoteConnections":[]}`,
		"wrong-case":          `{"version":1,"Version":1,"remoteConnections":[]}`,
		"connection-case":     `{"version":1,"remoteConnections":[{"sshAlias":"host","SSHALIAS":"other","projects":[]}]}`,
		"project-case":        `{"version":1,"remoteConnections":[{"sshAlias":"host","projects":[{"remotePath":"/a","LABEL":"A"}]}]}`,
		"escaped-duplicate":   `{"version":1,"ver\u0073ion":1,"remoteConnections":[]}`,
		"nested-duplicate":    `{"version":1,"remoteConnections":[{"sshAlias":"host","projects":[{"remotePath":"/a","remotePath":"/b"}]}]}`,
		"missing-projects":    `{"version":1,"remoteConnections":[{"sshAlias":"host"}]}`,
		"null-label":          `{"version":1,"remoteConnections":[{"sshAlias":"host","projects":[{"remotePath":"/a","label":null}]}]}`,
		"unknown-project":     `{"version":1,"remoteConnections":[{"sshAlias":"host","projects":[{"remotePath":"/a","name":"A"}]}]}`,
		"alias-duplicates":    `{"version":1,"remoteConnections":[{"sshAlias":"host","projects":[]},{"sshAlias":"host","projects":[]}]}`,
		"alias-case":          `{"version":1,"remoteConnections":[{"sshAlias":"HOST","projects":[]},{"sshAlias":"host","projects":[]}]}`,
		"path-duplicates":     `{"version":1,"remoteConnections":[{"sshAlias":"host","projects":[{"remotePath":"/a/../b"},{"remotePath":"/b"}]}]}`,
		"path-case":           `{"version":1,"remoteConnections":[{"sshAlias":"host","projects":[{"remotePath":"/A"},{"remotePath":"/a"}]}]}`,
		"unicode-path-case":   `{"version":1,"remoteConnections":[{"sshAlias":"host","projects":[{"remotePath":"/Σ"},{"remotePath":"/ς"}]}]}`,
		"label-controls":      `{"version":1,"remoteConnections":[{"sshAlias":"host","projects":[{"remotePath":"/a","label":"A\nB"}]}]}`,
		"invalid-utf8":        `{"version":1,"remoteConnections":[{"sshAlias":"host","projects":[{"remotePath":"/a","label":"` + "\xff" + `"}]}]}`,
		"trailing":            `{"version":1,"remoteConnections":[]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			filename := filepath.Join(testkit.TempDir(t), "config.json")
			testkit.WriteFile(t, filename, []byte(content), 0o600)
			if _, err := Prepare(filename, input(), nil); err == nil {
				t.Fatal("accepted invalid schema")
			}
			if string(read(t, filename)) != content {
				t.Fatal("invalid config overwritten")
			}
			entries, err := os.ReadDir(filepath.Dir(filename))
			if err != nil || len(entries) != 1 {
				t.Fatal("assessment mutated directory")
			}
		})
	}
}

func TestRejectInputCollisions(t *testing.T) {
	for _, paths := range [][]string{{"/a", "/a/."}, {"/a", "/A"}, {"relative"}, {"/a\x00"}} {
		in := input()
		in.Projects = nil
		for _, p := range paths {
			in.Projects = append(in.Projects, clientprojects.Project{Name: "project", Path: p})
		}
		if _, err := Prepare(target(t), in, nil); err == nil {
			t.Fatalf("accepted paths %q", paths)
		}
	}
	filename := filepath.Join(testkit.TempDir(t), "config.json")
	testkit.WriteFile(t, filename, []byte(`{"version":1,"remoteConnections":[{"sshAlias":"yard-example","projects":[{"remotePath":"/srv/workspaces/API/src"}]}]}`), 0o600)
	if _, err := Prepare(filename, input(), nil); err == nil {
		t.Fatal("accepted existing case collision")
	}
	testkit.WriteFile(t, filename, []byte(`{"version":1,"remoteConnections":[{"sshAlias":"YARD-EXAMPLE","projects":[]}]}`), 0o600)
	if _, err := Prepare(filename, input(), nil); err == nil {
		t.Fatal("accepted selected alias case collision")
	}
}

func TestNoopApplyRejectsDrift(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			filename := target(t)
			applyOK(t, prepareOK(t, filename, input()))
			in := input()
			if empty {
				in.Projects = nil
			}
			p := prepareOK(t, filename, in)
			if p.Changed {
				t.Fatal("unexpected semantic change")
			}
			testkit.WriteFile(t, filename, []byte(`{"version":1,"remoteConnections":[],"unknown":true}`), 0o600)
			if err := p.Apply(context.Background()); !errors.Is(err, errStale) {
				t.Fatalf("noop drift accepted: %v", err)
			}
		})
	}
}

func TestNativeOpenerUsesExactURLAndReportsFailure(t *testing.T) {
	root := testkit.TempDir(t)
	stub := filepath.Join(root, "xdg-open")
	testkit.WriteFile(t, stub, []byte("#!/bin/sh\n[ \"$#\" = 1 ] && [ \"$1\" = 'codex://codex-app/apply-config' ]\n"), 0o700)
	t.Setenv("PATH", root)
	if err := OpenURL(context.Background(), ImportURL); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, stub, []byte("#!/bin/sh\nexit 1\n"), 0o700)
	if err := OpenURL(context.Background(), ImportURL); err == nil {
		t.Fatal("handler failure reported success")
	}
}

func TestOpenRequestsAndRetryWithoutChanges(t *testing.T) {
	filename := target(t)
	calls := 0
	opener := func(_ context.Context, url string) error {
		calls++
		if url != ImportURL {
			t.Fatal(url)
		}
		if calls == 1 {
			return errors.New("no desktop")
		}
		return nil
	}
	p, err := Prepare(filename, input(), opener)
	if err != nil {
		t.Fatal(err)
	}
	applyOK(t, p)
	if calls != 0 {
		t.Fatal("export launched GUI")
	}
	err = p.Open(context.Background())
	if err == nil || !strings.Contains(err.Error(), "saved") || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("bad opener error %v", err)
	}
	p, err = Prepare(filename, input(), opener)
	if err != nil {
		t.Fatal(err)
	}
	if p.Changed {
		t.Fatal("repeat changed declaration")
	}
	if err := p.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("unchanged plan skipped import retry")
	}
	if err := OpenURL(context.Background(), "https://example.invalid"); err == nil {
		t.Fatal("launched unsupported URL")
	}
}

func TestEmptyOpenFailureDoesNotClaimSavedDeclaration(t *testing.T) {
	for _, handler := range []bool{false, true} {
		t.Run(fmt.Sprint(handler), func(t *testing.T) {
			filename := target(t)
			var opener func(context.Context, string) error
			if handler {
				opener = func(context.Context, string) error { return errors.New("no desktop") }
			}
			p, err := Prepare(filename, clientprojects.Export{}, opener)
			if err != nil {
				t.Fatal(err)
			}
			applyOK(t, p)
			err = p.Open(context.Background())
			if err == nil || !strings.Contains(err.Error(), "no desktop declaration was written") || strings.Contains(err.Error(), "saved") {
				t.Fatalf("bad empty export error %v", err)
			}
			if _, err := os.Stat(filepath.Dir(filename)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("empty export created config state: %v", err)
			}
		})
	}
}

func TestProtectedCreationModes(t *testing.T) {
	filename := target(t)
	applyOK(t, prepareOK(t, filename, input()))
	for _, path := range []string{filepath.Dir(filename), filename} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if st.IsDir() {
			want = 0o700
		}
		if st.Mode().Perm() != want {
			t.Fatalf("%s mode %o", path, st.Mode().Perm())
		}
	}
	in := input()
	in.Projects = append(in.Projects, clientprojects.Project{Name: "new", Path: "/new"})
	applyOK(t, prepareOK(t, filename, in))
	for _, p := range []string{filename, filename + ".subyard-backup"} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("protected file %s: %v", p, err)
		}
	}
}

func TestRejectUnsafeTargetsAndAncestors(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo", "writable-file", "writable-directory", "symlink-directory", "unsafe-backup", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			root := testkit.TempDir(t)
			filename := filepath.Join(root, "config.json")
			valid := []byte(`{"version":1,"remoteConnections":[]}`)
			switch kind {
			case "symlink":
				testkit.WriteFile(t, filepath.Join(root, "other"), valid, 0o600)
				if err := os.Symlink("other", filename); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				testkit.WriteFile(t, filename, valid, 0o600)
				if err := os.Link(filename, filepath.Join(root, "other")); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(filename, 0o600); err != nil {
					t.Fatal(err)
				}
			case "writable-file":
				testkit.WriteFile(t, filename, valid, 0o620)
			case "writable-directory":
				if err := os.Chmod(root, 0o770); err != nil {
					t.Fatal(err)
				}
			case "symlink-directory":
				if err := os.Symlink(root, filepath.Join(root, "link")); err != nil {
					t.Fatal(err)
				}
				filename = filepath.Join(root, "link", "config.json")
			case "unsafe-backup":
				testkit.WriteFile(t, filename, valid, 0o600)
				if err := os.Symlink("config.json", filename+".subyard-backup"); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				testkit.WriteFile(t, filename, bytes.Repeat([]byte(" "), maxConfigBytes+1), 0o600)
			}
			if _, err := Prepare(filename, input(), nil); err == nil {
				t.Fatal("accepted unsafe target")
			}
		})
	}
	if os.Getuid() == 0 {
		t.Run("foreign-ownership", func(t *testing.T) {
			filename := filepath.Join(testkit.TempDir(t), "config.json")
			testkit.WriteFile(t, filename, []byte(`{"version":1,"remoteConnections":[]}`), 0o600)
			if err := os.Chown(filename, 1, -1); err != nil {
				t.Fatal(err)
			}
			if _, err := Prepare(filename, input(), nil); err == nil {
				t.Fatal("accepted foreign-owned file")
			}
		})
	}
}

func TestApplyRejectsConcurrentDrift(t *testing.T) {
	for _, kind := range []string{"bytes", "identity", "missing-created", "ancestor", "backup"} {
		t.Run(kind, func(t *testing.T) {
			filename := target(t)
			if kind != "missing-created" {
				applyOK(t, prepareOK(t, filename, input()))
			}
			in := input()
			in.Projects = append(in.Projects, clientprojects.Project{Name: "extra", Path: "/extra"})
			p := prepareOK(t, filename, in)
			switch kind {
			case "bytes":
				testkit.WriteFile(t, filename, []byte(`{"version":1,"remoteConnections":[],"sshConnectTimeoutSeconds":77}`), 0o600)
			case "identity":
				data := read(t, filename)
				replacement := filename + ".replacement"
				testkit.WriteFile(t, replacement, data, 0o600)
				if err := os.Rename(replacement, filename); err != nil {
					t.Fatal(err)
				}
			case "missing-created":
				if err := os.Mkdir(filepath.Dir(filename), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filename, []byte(`{"version":1,"remoteConnections":[]}`), 0o600)
			case "ancestor":
				if err := os.Rename(filepath.Dir(filename), filepath.Dir(filename)+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Dir(filename), 0o700); err != nil {
					t.Fatal(err)
				}
			case "backup":
				testkit.WriteFile(t, filename+".subyard-backup", []byte("operator backup"), 0o600)
			}
			before, _, err := inspect(filename)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Apply(context.Background()); err == nil {
				t.Fatal("stale plan applied")
			}
			after, _, err := inspect(filename)
			if err != nil {
				t.Fatal(err)
			}
			if !sameSnapshot(before, after) {
				t.Fatal("drift overwritten")
			}
		})
	}
}

func TestConcurrentPlansSerializeAndRejectStale(t *testing.T) {
	filename := target(t)
	applyOK(t, prepareOK(t, filename, input()))
	in := input()
	in.Projects = append(in.Projects, clientprojects.Project{Name: "extra", Path: "/extra"})
	one, two := prepareOK(t, filename, in), prepareOK(t, filename, in)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, p := range []clientprojects.Plan{one, two} {
		wg.Add(1)
		go func(p clientprojects.Plan) { defer wg.Done(); results <- p.Apply(context.Background()) }(p)
	}
	wg.Wait()
	close(results)
	success, stale := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, errStale) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("success=%d stale=%d", success, stale)
	}
}

func TestFailureBeforePublicationPreservesOriginal(t *testing.T) {
	for _, stage := range []string{"after-pending-fsync", "before-backup", "before-publish"} {
		t.Run(stage, func(t *testing.T) {
			filename := target(t)
			applyOK(t, prepareOK(t, filename, input()))
			original := read(t, filename)
			in := input()
			in.Projects = append(in.Projects, clientprojects.Project{Name: "extra", Path: "/extra"})
			failure := errors.New("injected write failure")
			p, err := prepare(filename, in, nil, func(at string) error {
				if at == stage {
					return failure
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Apply(context.Background()); !errors.Is(err, failure) {
				t.Fatalf("got %v", err)
			}
			if !bytes.Equal(read(t, filename), original) {
				t.Fatal("failed publish changed original")
			}
			entries, err := os.ReadDir(filepath.Dir(filename))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".") {
					t.Fatal("failed write leaked temporary file")
				}
			}
		})
	}
}

func TestAncestorReplacementDuringWriteRejected(t *testing.T) {
	filename := target(t)
	applyOK(t, prepareOK(t, filename, input()))
	original := read(t, filename)
	in := input()
	in.Projects = append(in.Projects, clientprojects.Project{Name: "extra", Path: "/extra"})
	p, err := prepare(filename, in, nil, func(stage string) error {
		if stage != "after-pending-fsync" {
			return nil
		}
		if err := os.Rename(filepath.Dir(filename), filepath.Dir(filename)+"-old"); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Dir(filename), 0o700); err != nil {
			return err
		}
		testkit.WriteFile(t, filename, original, 0o600)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(context.Background()); !errors.Is(err, errStale) {
		t.Fatalf("got %v", err)
	}
	if !bytes.Equal(read(t, filename), original) || !bytes.Equal(read(t, filepath.Join(filepath.Dir(filename)+"-old", filepath.Base(filename))), original) {
		t.Fatal("ancestor drift overwrote original")
	}
}

func TestCrossProcessLock(t *testing.T) {
	const helperEnv = "SUBYARD_TEST_CODEX_LOCK_DIRECTORY"
	if directory := os.Getenv(helperEnv); directory != "" {
		fd, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(fd)
		if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(os.Stdout, "ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	filename := target(t)
	applyOK(t, prepareOK(t, filename, input()))
	in := input()
	in.Projects = append(in.Projects, clientprojects.Project{Name: "extra", Path: "/extra"})
	p := prepareOK(t, filename, in)
	childCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestCrossProcessLock$")
	cmd.Env = append(os.Environ(), helperEnv+"="+filepath.Dir(filename))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		if err := cmd.Wait(); err != nil {
			t.Error(err)
		}
	}()
	ready, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || ready != "ready\n" {
		t.Fatalf("child lock: %q %v", ready, err)
	}
	ctx, cancelApply := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancelApply()
	if err := p.Apply(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cross-process lock not respected: %v", err)
	}
	if p = prepareOK(t, filename, in); !p.Changed {
		t.Fatal("lock timeout published config")
	}
}

func TestApplyCanceledAndLockBounded(t *testing.T) {
	filename := target(t)
	p := prepareOK(t, filename, input())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Apply(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Dir(filename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled apply created directory")
	}
	applyOK(t, p)
	in := input()
	in.Projects = append(in.Projects, clientprojects.Project{Name: "extra", Path: "/extra"})
	p = prepareOK(t, filename, in)
	fd, err := unix.Open(filepath.Dir(filename), unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	ctx, cancel = context.WithCancel(context.Background())
	go cancel()
	if err := p.Apply(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("lock wait failed: %v", err)
	}
}
