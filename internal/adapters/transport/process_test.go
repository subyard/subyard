package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/contracttest"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRPCProcessHelper(t *testing.T) {
	if os.Getenv("SUBYARD_RPC_PROCESS_HELPER") != "1" {
		return
	}
	handler := rpc.HandlerFunc(func(_ context.Context, call rpc.Call, _ rpc.Emit) (any, error) {
		return map[string]any{"method": call.Method}, nil
	})
	err := (rpc.Session{
		Handler: handler, Capabilities: []string{"snapshot"}, DrainOnEOF: true,
	}).Serve(context.Background(), os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestSSHTransportUsesFixedArgumentsAndPreservesFrames(t *testing.T) {
	root := t.TempDir()
	program := filepath.Join(root, "ssh")
	log := filepath.Join(root, "arguments")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$SSH_ARGUMENT_LOG\"\n" +
		"exec \"$SUBYARD_RPC_HELPER_BINARY\" -test.run=^TestRPCProcessHelper$\n"
	if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	transport, err := SSH(program, "owner@example.test", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	helper, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	transport.Env = append(os.Environ(), "SSH_ARGUMENT_LOG="+log,
		"SUBYARD_RPC_HELPER_BINARY="+helper, "SUBYARD_RPC_PROCESS_HELPER=1")
	request := rpcFrames(t,
		rpc.Request{Version: 1, Type: "request", ID: "n", Method: "rpc.negotiate"},
		rpc.Request{Version: 1, Type: "request", ID: "ping", OperationID: "operation-ping", Method: "system.ping"},
	)
	response, err := transport.Call(context.Background(), "owner", request)
	if err != nil {
		t.Fatal(err)
	}
	codec := rpc.NewCodec(bytes.NewReader(response), io.Discard)
	negotiated, err := codec.ReadResponse()
	if err != nil || negotiated.ID != "n" || negotiated.Error != nil {
		t.Fatalf("negotiation frame changed: %#v err=%v", negotiated, err)
	}
	ping, err := codec.ReadResponse()
	if err != nil || ping.ID != "ping" || ping.OperationID != "operation-ping" || ping.Error != nil {
		t.Fatalf("call frame changed: %#v err=%v", ping, err)
	}
	arguments, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(arguments), "BatchMode=yes\n") ||
		!strings.Contains(string(arguments), "bash\n-lc\n") ||
		!strings.Contains(string(arguments), "yard") ||
		!strings.Contains(string(arguments), "rpc") ||
		!strings.Contains(string(arguments), "--stdio") {
		t.Fatalf("unsafe or incomplete SSH arguments: %q", arguments)
	}
}

func rpcFrames(t *testing.T, requests ...rpc.Request) []byte {
	t.Helper()
	var framed bytes.Buffer
	codec := rpc.NewCodec(bytes.NewReader(nil), &framed)
	for _, request := range requests {
		if err := codec.Write(request); err != nil {
			t.Fatal(err)
		}
	}
	return framed.Bytes()
}

func TestScriptedTransportReconnectsWithFullResync(t *testing.T) {
	remote := &testkit.ScriptedRemote{Steps: []testkit.RemoteStep{
		{Err: errors.New("connection lost")},
		{Response: []byte("full snapshot revision 12")},
	}}
	if _, err := remote.Call(context.Background(), "owner", []byte("incremental revision 11")); err == nil {
		t.Fatal("disconnect was not reported")
	}
	response, err := remote.Call(context.Background(), "owner", []byte("system.snapshot"))
	if err != nil || string(response) != "full snapshot revision 12" {
		t.Fatalf("reconnect did not resync: response=%q err=%v", response, err)
	}
	if len(remote.Calls) != 2 || string(remote.Calls[1].Request) != "system.snapshot" {
		t.Fatalf("reconnect reused stale incremental request: %#v", remote.Calls)
	}
}

func TestTransportTimeoutAndTargetValidation(t *testing.T) {
	if _, err := SSH("ssh", "-oProxyCommand=bad", time.Second); err == nil {
		t.Fatal("unsafe SSH target was accepted")
	}
	root := t.TempDir()
	program := filepath.Join(root, "slow")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nsleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	transport := Process{Program: program, Timeout: 50 * time.Millisecond}
	started := time.Now()
	if _, err := transport.Call(context.Background(), "", nil); err == nil {
		t.Fatal("timeout was ignored")
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("timed-out transport was not killed")
	}
}

func TestProcessErrorPreservesStdoutAndExitCode(t *testing.T) {
	root := t.TempDir()
	program := filepath.Join(root, "fail")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf patch\nprintf diagnostic >&2\nexit 17\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := (Process{Program: program}).CallReader(context.Background(), strings.NewReader(""))
	var processError *ProcessError
	if string(output) != "patch" || !errors.As(err, &processError) || processError.ExitCode != 17 ||
		!strings.Contains(processError.Error(), "diagnostic") {
		t.Fatalf("exit result was lost: output=%q err=%#v", output, err)
	}
}

func TestProcessRunPassesLiteralArguments(t *testing.T) {
	output, err := (Process{Program: "/bin/printf"}).Run(context.Background(), "%s", "a b")
	if err != nil || string(output) != "a b" {
		t.Fatalf("literal argument changed: %q err=%v", output, err)
	}
}

func TestRemoteTransportConformance(t *testing.T) {
	root := t.TempDir()
	program := filepath.Join(root, "echo")
	if err := os.WriteFile(program, []byte("#!/bin/sh\ncat\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Run("process", func(t *testing.T) {
		contracttest.RemoteTransport(t, Process{Program: program}, "owner")
	})
	t.Run("scripted", func(t *testing.T) {
		contracttest.RemoteTransport(t, &testkit.ScriptedRemote{Steps: []testkit.RemoteStep{{Response: []byte("framed request")}}}, "owner")
	})
}

func TestSSHYardUsesLoginShellForUserInstalledCLI(t *testing.T) {
	process, err := SSHYard("ssh", "dev@owner.example", "test-yard", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=3",
		"dev@owner.example", "--", "bash", "-lc",
		"'exec '\\''yard'\\'' '\\''-Y'\\'' '\\''test-yard'\\'' '\\''rpc'\\'' '\\''--stdio'\\'''",
	}
	if !reflect.DeepEqual(process.Arguments, want) {
		t.Fatalf("unexpected SSH arguments:\n got: %#v\nwant: %#v", process.Arguments, want)
	}
}

func TestSSHPinnedUsesOnlyManagedKnownHosts(t *testing.T) {
	process, err := SSHPinned("ssh", "owner-alias", "/state/owner.known_hosts", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	wantOptions := []string{
		"BatchMode=yes", "ConnectTimeout=3", "StrictHostKeyChecking=yes",
		"UserKnownHostsFile=/state/owner.known_hosts", "GlobalKnownHostsFile=/dev/null",
		"UpdateHostKeys=no",
	}
	for _, option := range wantOptions {
		found := false
		for index := 0; index+1 < len(process.Arguments); index++ {
			if process.Arguments[index] == "-o" && process.Arguments[index+1] == option {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing strict SSH option %q in %#v", option, process.Arguments)
		}
	}
}

func TestSSHHostKeyAssessmentDelegatesAliasesToIsolatedOpenSSH(t *testing.T) {
	process, err := SSHHostKeyAssessment("ssh", "configured-owner-alias", "/tmp/candidate.known_hosts", 4*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	wantOptions := []string{
		"BatchMode=yes", "ConnectTimeout=4", "StrictHostKeyChecking=accept-new",
		"UserKnownHostsFile=/tmp/candidate.known_hosts", "GlobalKnownHostsFile=/dev/null",
		"HashKnownHosts=no", "UpdateHostKeys=no", "CheckHostIP=no",
		"ControlMaster=no", "ControlPath=none", "PreferredAuthentications=none",
	}
	for _, option := range wantOptions {
		found := false
		for index := 0; index+1 < len(process.Arguments); index++ {
			if process.Arguments[index] == "-o" && process.Arguments[index+1] == option {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing isolated assessment option %q in %#v", option, process.Arguments)
		}
	}
	if !slices.Contains(process.Arguments, "configured-owner-alias") {
		t.Fatalf("SSH alias was rewritten instead of delegated to OpenSSH: %#v", process.Arguments)
	}
}
