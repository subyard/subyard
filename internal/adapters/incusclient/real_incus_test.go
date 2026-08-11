//go:build realincus

package incusclient

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/projectruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func TestRealIncusServerContract(t *testing.T) {
	socket := os.Getenv("SUBYARD_REAL_INCUS_SOCKET")
	if socket == "" {
		t.Skip("set SUBYARD_REAL_INCUS_SOCKET")
	}
	if !filepath.IsAbs(socket) {
		t.Fatal("real Incus socket must be absolute")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server, err := New(socket, "projects").Server(ctx)
	if err != nil || server.Environment == "" || server.Version == "" {
		t.Fatalf("server/extensions contract: %#v err=%v", server, err)
	}
}

// TestRealIncusConformance is opt-in and read-only apart from executing printf
// inside a dedicated acceptance instance. Run it once for a container and once
// for a VM; the ordinary yard-safe gate uses the fake Unix/WebSocket server.
func TestRealIncusConformance(t *testing.T) {
	socket := os.Getenv("SUBYARD_REAL_INCUS_SOCKET")
	project := os.Getenv("SUBYARD_REAL_INCUS_PROJECT")
	instanceName := os.Getenv("SUBYARD_REAL_INCUS_INSTANCE")
	expectedType := domain.YardKind(os.Getenv("SUBYARD_REAL_INCUS_TYPE"))
	if socket == "" || project == "" || instanceName == "" || expectedType == "" {
		t.Skip("set SUBYARD_REAL_INCUS_SOCKET, SUBYARD_REAL_INCUS_PROJECT, " +
			"SUBYARD_REAL_INCUS_INSTANCE and SUBYARD_REAL_INCUS_TYPE")
	}
	if !filepath.IsAbs(socket) || !domain.SafeName(project) || !domain.SafeName(instanceName) ||
		(expectedType != domain.YardContainer && expectedType != domain.YardVM) {
		t.Fatal("real Incus acceptance inputs are invalid")
	}
	client := New(socket, "projects")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server, err := client.Server(ctx)
	if err != nil || server.Environment == "" || server.Version == "" {
		t.Fatalf("server contract: %#v err=%v", server, err)
	}
	instance, err := client.Instance(ctx, project, instanceName)
	if err != nil || instance.Name != instanceName || instance.Type != expectedType ||
		!strings.EqualFold(instance.Status, "running") {
		t.Fatalf("running instance contract: %#v err=%v", instance, err)
	}
	eventContext, stopEvents := context.WithCancel(context.Background())
	events, errorsOut := client.Events(eventContext, []string{"lifecycle", "operation"})
	result, err := client.Exec(ctx, project, instanceName, ports.InstanceExecRequest{
		Command: []string{"sh", "-c", "printf subyard-real-incus"},
	})
	if err != nil || result.ExitCode != 0 || string(result.Stdout) != "subyard-real-incus" {
		stopEvents()
		t.Fatalf("exec contract: %#v err=%v", result, err)
	}
	result, err = client.StreamExec(ctx, project, instanceName, ports.InstanceExecRequest{
		Command: []string{"cat"},
	}, strings.NewReader("subyard-stream"))
	if err != nil || string(result.Stdout) != "subyard-stream" {
		stopEvents()
		t.Fatalf("stream exec contract: %#v err=%v", result, err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "payload"), []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, err := (projectruntime.TarArchiver{}).Open(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	destination := "/tmp/subyard-real-archive"
	if _, err := client.Exec(ctx, project, instanceName, ports.InstanceExecRequest{
		Command: []string{"install", "-d", destination},
	}); err != nil {
		t.Fatal(err)
	}
	result, err = client.StreamExec(ctx, project, instanceName, ports.InstanceExecRequest{
		Command: []string{"tar", "-C", destination, "-xf", "-"},
	}, archive)
	archiveErr := archive.Close()
	if err != nil || archiveErr != nil {
		stopEvents()
		t.Fatalf("archive stream contract: %#v stream=%v archive=%v", result, err, archiveErr)
	}
	result, err = client.Exec(ctx, project, instanceName, ports.InstanceExecRequest{
		Command: []string{"cat", filepath.Join(destination, "payload")},
	})
	if err != nil || string(result.Stdout) != "archive" {
		stopEvents()
		t.Fatalf("archive payload contract: %#v err=%v", result, err)
	}
	select {
	case event, ok := <-events:
		if !ok || event.Sequence == 0 || event.Revision == 0 ||
			(event.Kind != "operation" && event.Kind != "lifecycle") {
			stopEvents()
			t.Fatalf("event delivery contract: %#v", event)
		}
	case streamErr, ok := <-errorsOut:
		stopEvents()
		if !ok {
			t.Fatal("event stream closed before the exec event")
		}
		t.Fatalf("event delivery contract: %v", streamErr)
	case <-time.After(5 * time.Second):
		stopEvents()
		t.Fatal("event stream did not observe the exec operation")
	}
	stopEvents()
	for events != nil || errorsOut != nil {
		select {
		case _, ok := <-events:
			if !ok {
				events = nil
			}
		case streamErr, ok := <-errorsOut:
			if !ok {
				errorsOut = nil
			} else if streamErr != nil {
				t.Fatalf("event cancellation contract: %v", streamErr)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("event stream did not close after cancellation")
		}
	}
}

func TestRealIncusEffectiveConfigAndPowerWriterContract(t *testing.T) {
	const marker = "code-health-expanded-config-v1"
	if os.Getenv("SUBYARD_REAL_INCUS_MUTATION") != marker {
		t.Skip("set SUBYARD_REAL_INCUS_MUTATION=" + marker)
	}
	socket := os.Getenv("SUBYARD_REAL_INCUS_SOCKET")
	project := os.Getenv("SUBYARD_REAL_INCUS_PROJECT")
	instanceName := os.Getenv("SUBYARD_REAL_INCUS_INSTANCE")
	profileName := os.Getenv("SUBYARD_REAL_INCUS_PROFILE")
	if socket == "" || project == "" || instanceName == "" || profileName == "" {
		t.Skip("set real Incus socket, project, instance and profile inputs")
	}
	if !filepath.IsAbs(socket) || !domain.SafeName(project) || !domain.SafeName(instanceName) ||
		!domain.SafeName(profileName) {
		t.Fatal("real Incus mutation inputs are invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client := New(socket, "projects")
	server, err := client.connect(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	projectServer := server.UseProject(project)
	instance, _, err := projectServer.GetInstance(instanceName)
	if err != nil {
		t.Fatal(err)
	}
	if instance.Config["user.subyard.acceptance_fixture"] != marker {
		t.Fatalf("%s/%s is not marker-owned", project, instanceName)
	}
	if !slices.Contains(instance.Profiles, profileName) {
		t.Fatalf("%s/%s does not use acceptance profile %q", project, instanceName, profileName)
	}
	profile, _, err := projectServer.GetProfile(profileName)
	if err != nil {
		t.Fatal(err)
	}
	originalInstance := instance.Writable()
	originalInstance.Config = maps.Clone(originalInstance.Config)
	originalInstance.Profiles = slices.Clone(originalInstance.Profiles)
	originalProfile := profile.Writable()
	originalProfile.Config = maps.Clone(originalProfile.Config)
	defer func() {
		_, etag, readErr := projectServer.GetInstance(instanceName)
		if readErr == nil {
			operation, updateErr := projectServer.UpdateInstance(
				instanceName, originalInstance, etag,
			)
			if updateErr == nil {
				updateErr = operation.Wait()
			}
			if updateErr != nil {
				t.Errorf("restore acceptance instance: %v", updateErr)
			}
		} else {
			t.Errorf("read acceptance instance for restore: %v", readErr)
		}
		_, profileETag, readErr := projectServer.GetProfile(profileName)
		if readErr == nil {
			if updateErr := projectServer.UpdateProfile(profileName, originalProfile, profileETag); updateErr != nil {
				t.Errorf("restore acceptance profile: %v", updateErr)
			}
		} else {
			t.Errorf("read acceptance profile for restore: %v", readErr)
		}
	}()
	updateInstance := func(edit func(map[string]string)) {
		t.Helper()
		current, etag, readErr := projectServer.GetInstance(instanceName)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if current.Config == nil {
			current.Config = make(map[string]string)
		}
		edit(current.Config)
		operation, updateErr := projectServer.UpdateInstance(instanceName, current.Writable(), etag)
		if updateErr != nil {
			t.Fatal(updateErr)
		}
		if updateErr := operation.Wait(); updateErr != nil {
			t.Fatal(updateErr)
		}
	}
	updateProfile := func(edit func(map[string]string)) {
		t.Helper()
		current, etag, readErr := projectServer.GetProfile(profileName)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if current.Config == nil {
			current.Config = make(map[string]string)
		}
		edit(current.Config)
		if updateErr := projectServer.UpdateProfile(profileName, current.Writable(), etag); updateErr != nil {
			t.Fatal(updateErr)
		}
	}

	const effectiveKey = "user.subyard.acceptance_effective"
	updateInstance(func(config map[string]string) { delete(config, effectiveKey) })
	updateProfile(func(config map[string]string) { config[effectiveKey] = "profile" })
	info, err := client.Instance(ctx, project, instanceName)
	if err != nil {
		t.Fatal(err)
	}
	if value, present := info.EffectiveConfig(effectiveKey); !present || value != "profile" {
		t.Fatalf("inherited effective config=%q present=%v", value, present)
	}
	if _, present := info.LocalConfig[effectiveKey]; present {
		t.Fatalf("inherited key leaked into LocalConfig: %#v", info.LocalConfig)
	}
	updateInstance(func(config map[string]string) { config[effectiveKey] = "local" })
	info, err = client.Instance(ctx, project, instanceName)
	if err != nil {
		t.Fatal(err)
	}
	if value, present := info.EffectiveConfig(effectiveKey); !present || value != "local" ||
		info.LocalConfig[effectiveKey] != "local" {
		t.Fatalf("local override did not become effective: %#v", info)
	}
	updateInstance(func(config map[string]string) { config[effectiveKey] = "" })
	info, err = client.Instance(ctx, project, instanceName)
	if err != nil {
		t.Fatal(err)
	}
	if value, present := info.EffectiveConfig(effectiveKey); !present || value != "profile" {
		t.Fatalf("present empty local config did not retain profile fallback: %q present=%v",
			value, present)
	}
	if value, present := info.LocalConfig[effectiveKey]; present {
		t.Fatalf("Incus did not normalize empty local config away: %q", value)
	}

	const unownedKey = "user.subyard.acceptance_keep"
	updateInstance(func(config map[string]string) {
		config["boot.autostart"] = "false"
		config["user.subyard.managed"] = "true"
		config["user.subyard.name"] = "default"
		config["user.subyard.bridge"] = "incusbr0"
		config["user.subyard.desired_power"] = application.PowerStopped
		config["user.subyard.initialized"] = "true"
		config[unownedKey] = "keep"
	})
	yard := domain.Context{
		YardName: "default", IncusProject: project, YardInstanceName: instanceName, IncusBridge: "incusbr0",
	}
	power := application.PowerService{Instances: client, Config: client}
	if _, err := power.Ensure(ctx, yard); err != nil {
		t.Fatal(err)
	}
	if err := power.Set(ctx, yard, application.PowerStopped, false); err != nil {
		t.Fatal(err)
	}
	if err := power.Commit(ctx, yard, application.PowerRunning); err != nil {
		t.Fatal(err)
	}
	info, err = client.Instance(ctx, project, instanceName)
	if err != nil {
		t.Fatal(err)
	}
	if info.LocalConfig[unownedKey] != "keep" || info.Config[unownedKey] != "keep" {
		t.Fatalf("power writers removed unowned config: %#v", info)
	}
}
