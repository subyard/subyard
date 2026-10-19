package releaseowner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"
	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// The proxy always forwards to the actual local Incus daemon. Its only fault is
// withholding one marked resource's actual accepted exec response. Websocket
// data continues through the standard reverse proxy after the controller gate.
func TestNativePhysicalProxyChild(t *testing.T) {
	root := os.Getenv("SUBYARD_NATIVE_PHYSICAL_PROXY")
	if root == "" {
		return
	}
	if !strings.HasPrefix(root, "/tmp/subyard-native-physical-proxy.") {
		t.Fatal("private physical proxy root required")
	}
	if _, err := os.Stat("/run/subyard-e2e-lease.json"); err != nil {
		t.Fatal("disposable lease marker required")
	}
	target, _ := url.Parse("http://incus")
	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director
	proxy.Director = func(request *http.Request) {
		director(request)
		if request.Method != http.MethodPost || request.URL.Path != "/1.0/instances/native-remote/exec" {
			return
		}
		raw, err := io.ReadAll(io.LimitReader(request.Body, (1<<20)+1))
		request.Body.Close()
		request.Body = io.NopCloser(bytes.NewReader(raw))
		if err != nil || len(raw) > 1<<20 {
			return
		}
		var execRequest struct {
			Command []string `json:"command"`
		}
		if json.Unmarshal(raw, &execRequest) == nil {
			for _, argument := range execRequest.Command {
				if argument == "custody-refresh" {
					*request = *request.WithContext(context.WithValue(request.Context(), physicalGateKey{}, true))
				}
			}
		}
	}
	proxy.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", "/var/lib/incus/unix.socket")
	}}
	proxy.ModifyResponse = func(response *http.Response) error {
		request := response.Request
		if request.Method != http.MethodPost || request.URL.Path != "/1.0/instances/native-remote/exec" || request.URL.Query().Get("project") != "subyard-native-custody" {
			return nil
		}
		if request.Context().Value(physicalGateKey{}) != true {
			return nil
		}
		if err := os.Rename(filepath.Join(root, "arm"), filepath.Join(root, "armed")); err != nil {
			return nil
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		response.Body.Close()
		if err != nil || len(raw) > 1<<20 || response.StatusCode != http.StatusAccepted {
			return errors.New("real native acceptance response unavailable")
		}
		var accepted struct {
			Operation string `json:"operation"`
		}
		if err := json.Unmarshal(raw, &accepted); err != nil || !strings.HasPrefix(accepted.Operation, "/1.0/operations/") {
			return errors.New("real native operation identity unavailable")
		}
		fact, _ := json.Marshal(accepted)
		file, err := os.OpenFile(filepath.Join(root, "accepted.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = file.Write(fact)
		if err == nil {
			err = file.Sync()
		}
		file.Close()
		if err != nil {
			return err
		}
		deadline := time.Now().Add(90 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(root, "release")); err == nil {
				response.Body = io.NopCloser(bytes.NewReader(raw))
				return nil
			}
			if time.Now().After(deadline) {
				return errors.New("physical acceptance gate deadline")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	listener, err := net.Listen("unix", filepath.Join(root, "unix.socket"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := http.Serve(listener, proxy); err != nil {
		t.Fatal(err)
	}
}

type physicalGateKey struct{}

// This opt-in subprocess fixture runs only inside a broker-leased disposable VM
// and targets one exact marker-owned instance. It adds no production verb.
func TestNativePhysicalCustodianChild(t *testing.T) {
	if os.Getenv("SUBYARD_NATIVE_PHYSICAL_CHILD") != "native-custody-v1" {
		return
	}
	if _, err := os.Stat("/run/subyard-e2e-lease.json"); err != nil {
		t.Fatal("disposable lease marker required")
	}
	client := incusclient.New(os.Getenv("SUBYARD_NATIVE_PHYSICAL_SOCKET"))
	err := Serve(context.Background(), os.Stdin, os.Stdout, func(_ context.Context, c Context, _ *os.File) (Components, error) {
		marker, err := os.ReadFile(filepath.Join(c.ConfigHome, "native-physical.marker"))
		if err != nil || string(marker) != "native-custody-v1\n" || !strings.HasPrefix(c.ConfigHome, "/tmp/") {
			return nil, errors.New("private physical fixture root required")
		}
		return physicalNativeComponents{client: client}, nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "physical native helper refused: %v\n", err)
		os.Exit(3)
	}
	os.Exit(0)
}

type physicalNativeComponents struct {
	noEffects
	client *incusclient.Client
}

func (c physicalNativeComponents) ApplyIngress(ctx context.Context, step IngressStep) error {
	profileRoot := os.Getenv("SUBYARD_NATIVE_PHYSICAL_PROFILE_ROOT")
	if step.Kind != "source" && (profileRoot == "" || step.Kind != "fence") {
		return errors.New("physical fixture step")
	}
	instance, err := c.client.Instance(ctx, "subyard-native-custody", "native-remote")
	if err != nil {
		return err
	}
	if instance.Config["user.subyard.native_custody_fixture"] != "native-custody-v1" {
		return errors.New("physical instance marker required")
	}
	if profileRoot != "" {
		runtime := reconcileruntime.Runtime{RepositoryRoot: profileRoot, Environment: os.Environ(), Incus: c.client,
			Yard: domain.Context{IncusProject: "subyard-native-custody", YardInstanceName: "native-remote", DevUser: "dev"}, Stderr: os.Stderr}
		if step.Kind == "fence" {
			return runtime.FenceProfileRuntime(ctx, "git-fetch-runtime", step.Expected, step.Static, step.Desired)
		}
		expected := os.Getenv("SUBYARD_NATIVE_PHYSICAL_EXPECTED")
		if expected == "" {
			expected = "0"
		}
		return runtime.ApplyProfileRuntimeCustody(ctx, "git-fetch-runtime", "physical-refresh", step.Expected, step.Desired, expected, step.Static)
	}
	_, err = c.client.Exec(ctx, "subyard-native-custody", "native-remote", ports.InstanceExecRequest{Command: []string{"timeout", "30", "sh", "-c", `set -eu
root=/tmp/subyard-native-custody
mkdir -p "$root"
count=0
[ ! -f "$root/count" ] || count=$(cat "$root/count")
printf '%s\n' "$((count+1))" > "$root/count"
touch "$root/started"
while [ ! -f "$root/release" ]; do sleep .1; done
touch "$root/finished"
`}})
	return err
}

func (c physicalNativeComponents) QueryOperation(ctx context.Context, operation ports.NativeOperation) (ports.NativeOperation, error) {
	if operation.Project != "subyard-native-custody" || operation.Resource != "native-remote" {
		return ports.NativeOperation{}, errors.New("physical operation scope")
	}
	return c.client.NativeOperationState(ctx, operation)
}
