package application

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func TestProjectEnvironmentUpStagesProtectedInputAndNativeManifest(t *testing.T) {
	data := &projectDataStub{run: func(request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
		command := request.Command
		if len(command) >= 2 && command[0] == "docker" && command[1] == "inspect" ||
			slices.Equal(command, []string{"test", "-d", "/mnt/host/agent-sessions"}) {
			return ports.InstanceExecResult{}, errors.New("not found")
		}
		return ports.InstanceExecResult{}, nil
	}}
	runner := ProjectEnvironmentRunner{
		Data: data, Yard: domain.Context{DevUID: 1000}, Project: cloneRecord(), HasSecret: true,
		Profile: ProjectEnvironmentProfile{
			BaseImage: "ubuntu:24.04", Caches: []string{"/srv/cache/npm"},
			Features: []string{"browser"}, Devices: []string{"kvm"},
			Environment: map[string]string{"PUBLIC_VALUE": "visible"},
		},
	}
	result, diagnostics, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-env-up", Adapter: "project-env", Action: "up",
	}, strings.NewReader("SECRET_VALUE=hidden\n"))
	if err != nil || result.Status != "ok" || !strings.Contains(diagnostics, "box \"Demo\" up") {
		t.Fatalf("project environment up failed: result=%#v diagnostics=%q err=%v", result, diagnostics, err)
	}
	var streams [][]byte
	for _, request := range data.requests {
		if len(request.Stdin) != 0 {
			streams = append(streams, request.Stdin)
		}
	}
	if len(streams) != 2 || string(streams[0]) != "SECRET_VALUE=hidden\n" {
		t.Fatalf("protected input was not staged once: %#v", streams)
	}
	var manifest map[string]any
	if err := json.Unmarshal(streams[1], &manifest); err != nil || manifest["profile"] != "openclaw" {
		t.Fatalf("invalid native manifest: %q err=%v", streams[1], err)
	}
	if strings.Contains(string(streams[1]), "hidden") {
		t.Fatal("secret leaked into the public manifest")
	}
	var dockerRun []string
	for _, request := range data.requests {
		if len(request.Command) >= 2 && request.Command[0] == "docker" && request.Command[1] == "run" {
			dockerRun = request.Command
		}
	}
	joined := strings.Join(dockerRun, " ")
	if len(dockerRun) == 0 || !strings.Contains(joined, "PUBLIC_VALUE=visible") ||
		!strings.Contains(joined, "/run/subyard/profile.env:ro") || strings.Contains(joined, "hidden") {
		t.Fatalf("unsafe Docker invocation: %#v", dockerRun)
	}
}

func TestProjectEnvironmentManifestIsCanonical(t *testing.T) {
	payload, err := ProjectEnvironmentManifest(
		cloneRecord(), ProjectEnvironmentProfile{
			BaseImage: "ubuntu:24.04", Features: []string{"browser"},
			Environment: map[string]string{"PUBLIC_VALUE": "visible"},
		}, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Profile string              `json:"profile"`
		Image   string              `json:"image"`
		EnvKeys []string            `json:"envKeys"`
		Secrets []map[string]string `json:"secrets"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	if document.Profile != "openclaw" || document.Image != "ubuntu:24.04" ||
		!slices.Equal(document.EnvKeys, []string{"PUBLIC_VALUE"}) || len(document.Secrets) != 1 {
		t.Fatalf("manifest=%s", payload)
	}
}

func TestProjectEnvironmentRejectsControlSocketMount(t *testing.T) {
	data := &projectDataStub{}
	runner := ProjectEnvironmentRunner{
		Data: data, Yard: domain.Context{DevUID: 1000}, Project: cloneRecord(),
		Profile: ProjectEnvironmentProfile{
			BaseImage: "ubuntu:24.04", Mounts: []string{"/var/run/docker.sock:/var/run/docker.sock"},
		},
	}
	if _, _, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-env-unsafe", Adapter: "project-env", Action: "up",
	}, nil); err == nil || !strings.Contains(err.Error(), "control socket") {
		t.Fatalf("unsafe mount was accepted: %v", err)
	}
	for _, request := range data.requests {
		if len(request.Stdin) != 0 {
			t.Fatal("unsafe profile staged data before validation")
		}
	}
}

func TestProjectEnvironmentInfoAndDownUseDataPlane(t *testing.T) {
	manifestJSON := "{\"profile\":\"openclaw\"}\n"
	data := &projectDataStub{run: func(request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
		if len(request.Command) == 2 && request.Command[0] == "cat" {
			return ports.InstanceExecResult{Stdout: []byte(manifestJSON)}, nil
		}
		if len(request.Command) >= 3 && request.Command[0] == "docker" &&
			request.Command[1] == "inspect" && request.Command[2] == "-f" {
			return ports.InstanceExecResult{Stdout: []byte("sha256:owned\t1\tdemo-12345678\topenclaw\n")}, nil
		}
		return ports.InstanceExecResult{}, nil
	}}
	runner := ProjectEnvironmentRunner{Data: data, Project: cloneRecord()}
	_, output, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-env-info", Adapter: "project-env", Action: "info",
	}, nil)
	if err != nil || output != manifestJSON {
		t.Fatalf("project environment info failed: output=%q err=%v", output, err)
	}
	_, output, err = runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-env-down", Adapter: "project-env", Action: "down",
	}, nil)
	if err != nil || !strings.Contains(output, "stopped") {
		t.Fatalf("project environment down failed: output=%q err=%v", output, err)
	}
	last := data.requests[len(data.requests)-1].Command
	if !slices.Equal(last, []string{"docker", "stop", "sha256:owned"}) {
		t.Fatalf("unexpected down command: %#v", last)
	}
}

func TestProjectEnvironmentExistingBoxUsesImmutableIDForStartAndSessionLinks(t *testing.T) {
	data := &projectDataStub{run: func(request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
		command := request.Command
		if len(command) >= 3 && command[0] == "docker" &&
			command[1] == "inspect" && command[2] == "-f" {
			return ports.InstanceExecResult{
				Stdout: []byte("sha256:owned\t1\tdemo-12345678\topenclaw\n"),
			}, nil
		}
		return ports.InstanceExecResult{}, nil
	}}
	runner := ProjectEnvironmentRunner{
		Data: data, Yard: domain.Context{DevUID: 1000}, Project: cloneRecord(),
		Profile:   ProjectEnvironmentProfile{BaseImage: "ubuntu:24.04"},
		HostLinks: []string{".codex:/mnt/host/agent-sessions/codex:dir"},
	}
	if _, _, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-env-existing", Adapter: "project-env", Action: "up",
	}, nil); err != nil {
		t.Fatal(err)
	}
	foundStart, foundLink := false, false
	for _, request := range data.requests {
		command := request.Command
		if slices.Equal(command, []string{"docker", "start", "sha256:owned"}) {
			foundStart = true
		}
		if len(command) >= 6 && command[0] == "docker" && command[1] == "exec" {
			if command[4] != "sha256:owned" {
				t.Fatalf("session link used reusable container name: %#v", command)
			}
			foundLink = true
		}
	}
	if !foundStart || !foundLink {
		t.Fatalf("immutable start/link commands missing: %#v", data.requests)
	}
}

func TestProjectEnvironmentNewBoxUsesReturnedIDForSessionLinks(t *testing.T) {
	data := &projectDataStub{run: func(request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
		command := request.Command
		if slices.Equal(command, []string{"docker", "inspect", "subyard-box-demo-12345678"}) {
			return ports.InstanceExecResult{ExitCode: 1}, errors.New("not found")
		}
		if len(command) >= 2 && command[0] == "docker" && command[1] == "run" {
			return ports.InstanceExecResult{Stdout: []byte("sha256:new-container\n")}, nil
		}
		return ports.InstanceExecResult{}, nil
	}}
	runner := ProjectEnvironmentRunner{
		Data: data, Yard: domain.Context{DevUID: 1000}, Project: cloneRecord(),
		Profile:   ProjectEnvironmentProfile{BaseImage: "ubuntu:24.04"},
		HostLinks: []string{".codex:/mnt/host/agent-sessions/codex:dir"},
	}
	if _, _, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-env-new", Adapter: "project-env", Action: "up",
	}, nil); err != nil {
		t.Fatal(err)
	}
	for _, request := range data.requests {
		command := request.Command
		if len(command) >= 6 && command[0] == "docker" && command[1] == "exec" {
			if command[4] != "sha256:new-container" {
				t.Fatalf("new box session link used reusable container name: %#v", command)
			}
			return
		}
	}
	t.Fatalf("new box session link was not created: %#v", data.requests)
}

func TestProjectEnvironmentDownRejectsUnownedBox(t *testing.T) {
	data := &projectDataStub{run: func(request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
		if len(request.Command) >= 3 && request.Command[0] == "docker" &&
			request.Command[1] == "inspect" && request.Command[2] == "-f" {
			return ports.InstanceExecResult{Stdout: []byte("sha256:foreign\t\t\t\n")}, nil
		}
		return ports.InstanceExecResult{}, nil
	}}
	runner := ProjectEnvironmentRunner{Data: data, Project: cloneRecord()}
	if _, _, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-env-down", Adapter: "project-env", Action: "down",
	}, nil); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("unowned environment was stopped: %v", err)
	}
	for _, request := range data.requests {
		if slices.Equal(request.Command, []string{"docker", "stop", "subyard-box-demo-12345678"}) {
			t.Fatal("unowned environment received docker stop")
		}
	}
}

func TestProjectEnvironmentRebuildRecreatesOwnedBox(t *testing.T) {
	data := &projectDataStub{run: func(request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
		command := request.Command
		if slices.Equal(command, []string{"test", "-d", "/mnt/host/agent-sessions"}) {
			return ports.InstanceExecResult{ExitCode: 1}, errors.New("missing")
		}
		if len(command) >= 3 && command[0] == "docker" && command[1] == "inspect" && command[2] == "-f" {
			return ports.InstanceExecResult{Stdout: []byte("sha256:owned\t1\tdemo-12345678\topenclaw\n")}, nil
		}
		return ports.InstanceExecResult{}, nil
	}}
	runner := ProjectEnvironmentRunner{
		Data: data, Yard: domain.Context{DevUID: 1000}, Project: cloneRecord(), Rebuild: true,
		Profile: ProjectEnvironmentProfile{BaseImage: "ubuntu:24.04"},
	}
	if _, _, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-env-rebuild", Adapter: "project-env", Action: "up",
	}, nil); err != nil {
		t.Fatal(err)
	}
	removeIndex, runIndex, startIndex := -1, -1, -1
	for index, request := range data.requests {
		command := request.Command
		if len(command) >= 2 && command[0] == "docker" {
			switch command[1] {
			case "rm":
				if !slices.Equal(command, []string{"docker", "rm", "-f", "sha256:owned"}) {
					t.Fatalf("rebuild removal used reusable container name: %#v", command)
				}
				removeIndex = index
			case "run":
				runIndex = index
			case "start":
				startIndex = index
			}
		}
	}
	if removeIndex < 0 || runIndex <= removeIndex || startIndex >= 0 {
		t.Fatalf("rebuild did not recreate the box: %#v", data.requests)
	}
}

func TestProjectEnvironmentRebuildRejectsUnownedBox(t *testing.T) {
	data := &projectDataStub{run: func(request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
		command := request.Command
		if slices.Equal(command, []string{"test", "-d", "/mnt/host/agent-sessions"}) {
			return ports.InstanceExecResult{ExitCode: 1}, errors.New("missing")
		}
		if len(command) >= 3 && command[0] == "docker" && command[1] == "inspect" && command[2] == "-f" {
			return ports.InstanceExecResult{Stdout: []byte("sha256:foreign\t\t\t\n")}, nil
		}
		return ports.InstanceExecResult{}, nil
	}}
	runner := ProjectEnvironmentRunner{
		Data: data, Yard: domain.Context{DevUID: 1000}, Project: cloneRecord(), Rebuild: true,
		Profile: ProjectEnvironmentProfile{BaseImage: "ubuntu:24.04"},
	}
	if _, _, err := runner.Run(context.Background(), domain.AdapterRequest{
		Schema: 1, OperationID: "operation-env-rebuild", Adapter: "project-env", Action: "up",
	}, nil); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("unowned box was accepted: %v", err)
	}
	for _, request := range data.requests {
		if len(request.Command) >= 2 && request.Command[0] == "docker" &&
			(request.Command[1] == "rm" || request.Command[1] == "run") {
			t.Fatalf("unowned box was modified: %#v", data.requests)
		}
	}
}
