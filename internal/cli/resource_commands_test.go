package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestResourceCommandPreparesConfirmsRevalidatesThenApplies(t *testing.T) {
	root, environment, applyLog := resourceCommandFixture(t)
	prepareLog := filepath.Join(root, "resource-prepare.log")
	prompt := &testkit.Prompt{Answers: []bool{true}}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "run"},
		Environment: append(environment, "API_TOKEN=must-not-reach-prepare"), WorkingDir: root,
		Stdin: strings.NewReader("must-not-reach-prepare\n"), Stdout: &stdout, Stderr: &stderr,
		Prompt: prompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(prompt.Requests) != 1 || prompt.Requests[0].Default != "yes" ||
		!strings.Contains(strings.Join(prompt.Requests[0].Consequences, "\n"), "start fixture runtime") {
		t.Fatalf("unexpected prompt: %#v", prompt.Requests)
	}
	if got := readResourceApplyLog(t, applyLog); got != "run\n" {
		t.Fatalf("apply log=%q", got)
	}
	if got := readResourceApplyLog(t, prepareLog); got != "prepare\nprepare\n" {
		t.Fatalf("prepare log=%q", got)
	}
	if !strings.Contains(stdout.String(), "applied run") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestResourceCommandRejectsStaleAssessmentAfterConsent(t *testing.T) {
	for _, test := range []struct {
		name      string
		argument  string
		assumeYes bool
	}{
		{name: "action", argument: "--stale-action"},
		{name: "consequences", argument: "--stale-consequence"},
		{name: "automation consent", argument: "--stale-on-second", assumeYes: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, applyLog := resourceCommandFixture(t)
			arguments := []string{"demo", "run", test.argument}
			var prompt ports.Prompter = &callbackPrompt{callback: func() {
				writeCLIFile(t, filepath.Join(root, "resource-drift"), "changed\n", 0o600)
			}}
			if test.assumeYes {
				arguments = append(arguments, "--yes")
				prompt = &testkit.Prompt{}
			}
			var stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: arguments,
				Environment: environment, WorkingDir: root, Stderr: &stderr, Prompt: prompt,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 1 ||
				!strings.Contains(stderr.String(), "operation plan is stale") {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			if _, err := os.Stat(applyLog); !os.IsNotExist(err) {
				t.Fatalf("stale assessment reached apply: %v", err)
			}
		})
	}
}

func TestResourceCommandSkipsMutationThatBecameNoOpAfterConsent(t *testing.T) {
	root, environment, applyLog := resourceCommandFixture(t)
	prompt := &callbackPrompt{callback: func() {
		writeCLIFile(t, filepath.Join(root, "resource-drift"), "changed\n", 0o600)
	}}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"demo", "run", "--become-no-op"},
		Environment: environment, WorkingDir: root, Stderr: &stderr, Prompt: prompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(applyLog); !os.IsNotExist(err) {
		t.Fatalf("post-consent no-op reached apply: %v", err)
	}
}

func TestResourceCommandRejectsMalformedSecondPrepare(t *testing.T) {
	root, environment, applyLog := resourceCommandFixture(t)
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"demo", "run", "--malformed-on-second", "--yes"},
		Environment: environment, WorkingDir: root, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 ||
		!strings.Contains(stderr.String(), "resource_plan_invalid") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(applyLog); !os.IsNotExist(err) {
		t.Fatalf("malformed second prepare reached apply: %v", err)
	}
}

func TestResourceCommandDeclineDoesNotApplyDefaultNoAction(t *testing.T) {
	root, environment, applyLog := resourceCommandFixture(t)
	prompt := &testkit.Prompt{Answers: []bool{false}}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "purge"},
		Environment: environment, WorkingDir: root, Stderr: &stderr, Prompt: prompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(prompt.Requests) != 1 || prompt.Requests[0].Default != "no" {
		t.Fatalf("unexpected prompt: %#v", prompt.Requests)
	}
	if _, err := os.Stat(applyLog); !os.IsNotExist(err) {
		t.Fatalf("declined action reached apply: %v", err)
	}
}

func TestResourceCommandSessionDoesNotPrompt(t *testing.T) {
	root, environment, applyLog := resourceCommandFixture(t)
	prompt := &testkit.Prompt{}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "view"},
		Environment: environment, WorkingDir: root, Stderr: &stderr, Prompt: prompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(prompt.Requests) != 0 {
		t.Fatalf("unexpected prompt: %#v", prompt.Requests)
	}
	if got := readResourceApplyLog(t, applyLog); got != "view\n" {
		t.Fatalf("apply log=%q", got)
	}
}

func TestResourceCommandNoOpMutationDoesNotPromptOrApply(t *testing.T) {
	root, environment, applyLog := resourceCommandFixture(t)
	prompt := &testkit.Prompt{}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "run", "--no-op"},
		Environment: environment, WorkingDir: root, Stderr: &stderr, Prompt: prompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(prompt.Requests) != 0 {
		t.Fatalf("unexpected prompt: %#v", prompt.Requests)
	}
	if _, err := os.Stat(applyLog); !os.IsNotExist(err) {
		t.Fatalf("prepared no-op reached mutation apply: %v", err)
	}
}

func TestResourceSessionApplyPreservesOnlyInteractiveEnvironment(t *testing.T) {
	root, environment, _ := resourceCommandFixture(t)
	environment = append(environment,
		"HOME=/home/session-user",
		"USER=session-user",
		"LOGNAME=session-user",
		"SHELL=/bin/session-shell",
		"TERM=xterm-session",
		"COLORTERM=truecolor",
		"DISPLAY=:77",
		"WAYLAND_DISPLAY=wayland-77",
		"XDG_RUNTIME_DIR=/run/user/777",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/777/bus",
		"XAUTHORITY=/tmp/session-xauthority",
		"API_TOKEN=must-not-reach-resource",
	)
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "view"},
		Environment: environment, WorkingDir: root, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	value, err := os.ReadFile(filepath.Join(root, "resource-session-env.log"))
	if err != nil {
		t.Fatal(err)
	}
	want := "/home/session-user|session-user|session-user|/bin/session-shell|xterm-session|truecolor|:77|wayland-77|/run/user/777|unix:path=/run/user/777/bus|/tmp/session-xauthority\n"
	if string(value) != want {
		t.Fatalf("session environment=%q, want %q", value, want)
	}
}

func TestResourceCommandYesIsConsentOnly(t *testing.T) {
	root, environment, applyLog := resourceCommandFixture(t)
	prompt := &testkit.Prompt{}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "run", "--yes"},
		Environment: environment, WorkingDir: root, Stderr: &stderr, Prompt: prompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(prompt.Requests) != 0 {
		t.Fatalf("--yes unexpectedly prompted: %#v", prompt.Requests)
	}
	if got := readResourceApplyLog(t, applyLog); got != "run\n" {
		t.Fatalf("handler received consent flag: %q", got)
	}
}

func TestResourceCommandRejectsUnknownAndMismatchedActionsBeforeApply(t *testing.T) {
	for _, test := range []struct {
		name      string
		arguments []string
		want      string
	}{
		{name: "unknown verb", arguments: []string{"demo", "unknown"}, want: "resource_action_unknown"},
		{name: "handler mismatch", arguments: []string{"demo", "run", "--mismatch"}, want: "resource_action_unknown"},
		{name: "malformed result", arguments: []string{"demo", "status", "--malformed"}, want: "resource_plan_invalid"},
		{name: "oversize result", arguments: []string{"demo", "status", "--oversize"}, want: "resource_plan_invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, applyLog := resourceCommandFixture(t)
			var stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: test.arguments,
				Environment: environment, WorkingDir: root, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code == 0 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			if _, err := os.Stat(applyLog); !os.IsNotExist(err) {
				t.Fatalf("invalid action reached apply: %v", err)
			}
		})
	}
}

func TestResourceCommandPrepareHonorsParentDeadline(t *testing.T) {
	root, environment, applyLog := resourceCommandFixture(t)
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "status", "--timeout"},
		Environment: environment, WorkingDir: root, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := program.Run(ctx); code == 0 || !strings.Contains(stderr.String(), "resource_plan_invalid") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(applyLog); !os.IsNotExist(err) {
		t.Fatalf("cancelled prepare reached apply: %v", err)
	}
}

func TestResourceHelpAndHiddenSvcUseSafeSharedDispatch(t *testing.T) {
	t.Run("help", func(t *testing.T) {
		root, environment, applyLog := resourceCommandFixture(t)
		var stdout, stderr bytes.Buffer
		program, err := New(Options{
			RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "--help"},
			Environment: append(environment, "API_TOKEN=must-not-reach-help"), WorkingDir: root,
			Stdin: strings.NewReader("must-not-reach-help\n"), Stdout: &stdout, Stderr: &stderr,
		})
		if err != nil {
			t.Fatal(err)
		}
		if code := program.Run(context.Background()); code != 0 || !strings.Contains(stdout.String(), "Usage:") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		if _, err := os.Stat(applyLog); !os.IsNotExist(err) {
			t.Fatalf("help reached apply: %v", err)
		}
	})

	t.Run("svc", func(t *testing.T) {
		root, environment, applyLog := resourceCommandFixture(t)
		var stderr bytes.Buffer
		program, err := New(Options{
			RepositoryRoot: root, Program: "yard", Arguments: []string{"svc", "demo", "status"},
			Environment: environment, WorkingDir: root, Stderr: &stderr,
		})
		if err != nil {
			t.Fatal(err)
		}
		if code := program.Run(context.Background()); code != 0 {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
		if got := readResourceApplyLog(t, applyLog); got != "status\n" {
			t.Fatalf("svc apply log=%q", got)
		}
	})

	t.Run("positional help is a handler argument", func(t *testing.T) {
		root, environment, applyLog := resourceCommandFixture(t)
		var stderr bytes.Buffer
		program, err := New(Options{
			RepositoryRoot: root, Program: "yard", Arguments: []string{"demo", "status", "help"},
			Environment: environment, WorkingDir: root, Stderr: &stderr,
		})
		if err != nil {
			t.Fatal(err)
		}
		if code := program.Run(context.Background()); code != 0 {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
		if got := readResourceApplyLog(t, applyLog); got != "status help\n" {
			t.Fatalf("handler arguments=%q", got)
		}
	})

	t.Run("global yes precedes hidden svc", func(t *testing.T) {
		root, environment, applyLog := resourceCommandFixture(t)
		var stderr bytes.Buffer
		program, err := New(Options{
			RepositoryRoot: root, Program: "yard", Arguments: []string{"--yes", "svc", "demo", "run"},
			Environment: environment, WorkingDir: root, Stderr: &stderr,
		})
		if err != nil {
			t.Fatal(err)
		}
		if code := program.Run(context.Background()); code != 0 {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
		if got := readResourceApplyLog(t, applyLog); got != "run\n" {
			t.Fatalf("handler arguments=%q", got)
		}
	})
}

func TestRemoteResourceCommandIsPreparedOnlyByOwner(t *testing.T) {
	root, environment, applyLog := resourceCommandFixture(t)
	remoteDirectory := filepath.Join(root, "state", "yards", "remote")
	if err := os.MkdirAll(remoteDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(remoteDirectory, "config.env"),
		"YARD_TYPE=remote\nREMOTE_DEST=owner.example\nREMOTE_YARD=inner\nSSH_PORT=4444\n", 0o600)
	fakeBin := filepath.Join(root, "fake-bin")
	sshLog := filepath.Join(root, "remote-ssh.log")
	if err := os.MkdirAll(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(fakeBin, "ssh"), `#!/bin/sh
set -eu
root="$(cd "$(dirname "$0")/.." && pwd)"
printf '%s\n' "$@" >"$root/remote-ssh.log"
`, 0o700)
	path := fakeBin + ":" + os.Getenv("PATH")
	t.Setenv("PATH", path)
	environment = append(environment, "PATH="+path)
	prompt := &testkit.Prompt{}
	var stderr bytes.Buffer
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"-Y", "remote", "demo", "run"},
		Environment: environment, WorkingDir: root, Stderr: &stderr, Prompt: prompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if len(prompt.Requests) != 0 {
		t.Fatalf("controller prepared or prompted for owner action: %#v", prompt.Requests)
	}
	if _, err := os.Stat(applyLog); !os.IsNotExist(err) {
		t.Fatalf("controller ran resource handler: %v", err)
	}
	forwarded, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"owner.example", "'yard'", "'inner'", "'demo'", "'run'"} {
		if !strings.Contains(string(forwarded), expected) {
			t.Fatalf("forwarded command missing %q:\n%s", expected, forwarded)
		}
	}
}

func resourceCommandFixture(t *testing.T) (string, []string, string) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	manifestPath := filepath.Join(root, "config", "commands.registry")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest = append(manifest, []byte("svc|resource|@resource||forward|mutate|dynamic|hidden|internal|resource|svc <resource> <verb>|dispatch a profile resource by registry name|--yes --help|\n")...)
	writeCLIFile(t, manifestPath, string(manifest), 0o600)
	resourceRoot := filepath.Join(root, "config", "profiles", "fixture", "resources")
	if err := os.MkdirAll(filepath.Join(resourceRoot, "demo"), 0o700); err != nil {
		t.Fatal(err)
	}
	descriptor := strings.Join([]string{
		"COMMAND=demo",
		"HANDLER=resources/demo/handler.sh",
		"TITLE=Fixture resource",
		`ACTION="run run host-change reversible"`,
		`ACTION="run-purge run persistent-data-destruction irreversible"`,
		`ACTION="purge purge persistent-data-destruction irreversible"`,
		`ACTION="status status read-only not-needed"`,
		`ACTION="view view session not-needed"`,
		"BRINGUP=run",
		"SHUTDOWN=purge",
	}, "\n") + "\n"
	writeCLIFile(t, filepath.Join(resourceRoot, "demo.res"), descriptor, 0o600)
	applyLog := filepath.Join(root, "resource-apply.log")
	handler := `#!/bin/sh
set -eu
verb="${1:-}"
case "${SUBYARD_RESOURCE_MODE:-}" in
  prepare)
    [ -z "${API_TOKEN:-}" ] || { echo leaked-secret >&2; exit 70; }
    [ -z "${SUBYARD_OPERATION_ID:-}" ] || { echo leaked-operation >&2; exit 71; }
    [ -z "${SUBYARD_RESOURCE_ACTION:-}" ] || { echo leaked-action >&2; exit 72; }
    [ -z "${DISPLAY:-}${WAYLAND_DISPLAY:-}${XDG_RUNTIME_DIR:-}${DBUS_SESSION_BUS_ADDRESS:-}${XAUTHORITY:-}${TERM:-}${COLORTERM:-}" ] || { echo leaked-session-environment >&2; exit 78; }
    if IFS= read -r _input; then echo leaked-stdin >&2; exit 73; fi
	printf 'prepare\n' >>"$SUBYARD_REPOSITORY_ROOT/resource-prepare.log"
	prepare_count="$(wc -l <"$SUBYARD_REPOSITORY_ROOT/resource-prepare.log")"
    case "$verb:$*" in
      run:*--mismatch*) action=purge; changed=true; consequence='mismatched action' ;;
      run:*--no-op*) action=run; changed=false; consequence='' ;;
	  run:*--stale-action*)
		action=run; changed=true; consequence='start fixture runtime'
		[ ! -e "$SUBYARD_REPOSITORY_ROOT/resource-drift" ] || action=run-purge
		;;
	  run:*--stale-consequence*)
		action=run; changed=true; consequence='start fixture runtime'
		[ ! -e "$SUBYARD_REPOSITORY_ROOT/resource-drift" ] || consequence='start fixture runtime and open host network access'
		;;
	  run:*--stale-on-second*)
		action=run; changed=true; consequence='start fixture runtime'
		[ "$prepare_count" -lt 2 ] || consequence='start fixture runtime and open host network access'
		;;
	  run:*--malformed-on-second*)
		if [ "$prepare_count" -ge 2 ]; then printf 'not-json\n'; exit 0; fi
		action=run; changed=true; consequence='start fixture runtime'
		;;
	  run:*--become-no-op*)
		action=run; changed=true; consequence='start fixture runtime'
		if [ -e "$SUBYARD_REPOSITORY_ROOT/resource-drift" ]; then changed=false; consequence=''; fi
		;;
      run:*) action=run; changed=true; consequence='start fixture runtime' ;;
      purge:*) action=purge; changed=true; consequence='permanently erase fixture data' ;;
	  status:*--malformed*) printf 'not-json\n'; exit 0 ;;
	  status:*--oversize*) head -c 70000 /dev/zero | tr '\000' x; exit 0 ;;
	  status:*--timeout*) sleep 1 ;;
      status:*) action=status; changed=false; consequence='' ;;
      view:*) action=view; changed=true; consequence='open fixture session' ;;
      *) action=unknown; changed=false; consequence='' ;;
    esac
    if [ -n "$consequence" ]; then
      printf '{"schema":"yard.resource-action-assessment.v1","action":"%s","changed":%s,"consequences":["%s"]}\n' "$action" "$changed" "$consequence"
    else
      printf '{"schema":"yard.resource-action-assessment.v1","action":"%s","changed":%s,"consequences":[]}\n' "$action" "$changed"
    fi
    ;;
  apply)
    expected="$verb"
    [ "$expected" = "${SUBYARD_RESOURCE_ACTION:-}" ] || { echo action-mismatch >&2; exit 74; }
    [ -n "${SUBYARD_OPERATION_ID:-}" ] || { echo missing-operation >&2; exit 75; }
    [ -z "${API_TOKEN:-}" ] || { echo leaked-secret >&2; exit 79; }
    printf '%s\n' "$*" >>"$SUBYARD_REPOSITORY_ROOT/resource-apply.log"
    if [ "$verb" = view ]; then
      printf '%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s\n' \
        "${HOME:-}" "${USER:-}" "${LOGNAME:-}" "${SHELL:-}" "${TERM:-}" "${COLORTERM:-}" \
        "${DISPLAY:-}" "${WAYLAND_DISPLAY:-}" "${XDG_RUNTIME_DIR:-}" \
        "${DBUS_SESSION_BUS_ADDRESS:-}" "${XAUTHORITY:-}" \
        >"$SUBYARD_REPOSITORY_ROOT/resource-session-env.log"
    fi
    printf 'applied %s\n' "$verb"
    ;;
  *)
	[ -z "${API_TOKEN:-}" ] || { echo leaked-secret >&2; exit 76; }
	[ -z "${SUBYARD_OPERATION_ID:-}" ] || { echo leaked-operation >&2; exit 77; }
    printf 'Usage: yard demo <run|purge|status|view>\n'
    ;;
esac
`
	writeCLIFile(t, filepath.Join(resourceRoot, "demo", "handler.sh"), handler, 0o700)
	return root, environment, applyLog
}

func readResourceApplyLog(t *testing.T, path string) string {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(value)
}
