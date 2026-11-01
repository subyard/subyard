package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/testkit"
)

// The fresh-activation fixture owns an empty guest filesystem. Its desired
// files come from the installed source, never from an incoming guest request.
type configObservationControl struct {
	Repository, Home, Config, Data, InventoryProgram, InventorySHA256 string
}

type configObservationEntry struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Format string `json:"format,omitempty"`
}

type configObservationRequest struct {
	Home    string                   `json:"home"`
	UID     int                      `json:"uid"`
	Entries []configObservationEntry `json:"entries"`
}

func (c configObservationControl) request() ([]byte, error) {
	raw, _, err := c.description()
	return raw, err
}

func (c configObservationControl) description() ([]byte, map[string][]byte, error) {
	loaded, err := config.Load(config.LoadOptions{RepositoryRoot: c.Repository, OperatorHome: c.Home, YardName: "default", DisablePrivate: true, ConfigLocked: true, AllowPendingTransaction: true, Environment: map[string]string{"HOME": c.Home, "SUBYARD_OPERATOR_HOME": c.Home, "SUBYARD_HOME": c.Data, "SUBYARD_CONFIG_HOME": c.Config}})
	if err != nil {
		return nil, nil, err
	}
	sources, err := config.SelectMaterializationSources(loaded.Environment, "dev", config.MaterializationRuntime, loaded.Catalog.Integrations()...)
	if err != nil {
		return nil, nil, err
	}
	value := configObservationRequest{Home: "/home/dev", UID: 1000, Entries: []configObservationEntry{}}
	payloads := map[string][]byte{}
	if loaded.Context.DevUser != "dev" || loaded.Context.DevUID != 1000 {
		return nil, nil, fmt.Errorf("unsupported fixture developer")
	}
	for _, source := range sources {
		raw, err := source.Asset.ReadSource()
		if err != nil && !os.IsNotExist(err) {
			return nil, nil, err
		}
		payload, present, err := config.PrepareMaterializationContent(config.MaterializationRuntime, false, config.MaterializationContent{Present: err == nil, Payload: raw}, nil)
		if err != nil {
			return nil, nil, err
		}
		if !present {
			continue
		}
		kind := "file"
		if source.Asset.OwnedFormat != "" {
			kind = "structured"
		}
		value.Entries = append(value.Entries, configObservationEntry{source.Integration, kind, source.Asset.Destination, fmt.Sprintf("%x", sha256.Sum256(payload)), source.Asset.OwnedFormat})
		payloads[source.Asset.Destination] = payload
	}
	raw, err := json.Marshal(value)
	return raw, payloads, err
}

func configObservationStep(c configObservationControl, call testkit.IncusServerExecCall, release <-chan struct{}) testkit.IncusServerExecStep {
	reject := testkit.IncusServerExecStep{ExitCode: 99, Stderr: []byte("unsupported materialized observation fixture request\n"), Release: release}
	if slices.Equal(call.Command, []string{"bash", "-s", "--", "--observe"}) {
		path := filepath.Join(c.Repository, "config/projects-changed.sh")
		raw, err := os.ReadFile(path)
		if err != nil {
			return reject
		}
		step := historyResponder(historyControl{ProjectHookProgram: path, ProjectHookSHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}, filepath.Join(c.Home, "fixture-observation"))(call)
		step.Release = release
		return step
	}
	if len(call.Command) == 11 {
		return structuredConfigObservationStep(c, call, release, reject)
	}
	raw, err := os.ReadFile(c.InventoryProgram)
	program := strings.NewReplacer("@STATE_ROOT@", "/var/lib/subyard/integrations", "@STATE_UID@", "0").Replace(string(raw))
	if err != nil || fmt.Sprintf("%x", sha256.Sum256([]byte(program))) != c.InventorySHA256 || call.Project != "subyard" || call.Name != "yard" || call.User != 0 || call.Group != 0 || len(call.Environment) != 0 || !slices.Equal(call.Command, []string{"python3", "-B", "-c", program, "configs-observe"}) {
		return reject
	}
	expected, err := c.request()
	if err != nil {
		return reject
	}
	var request configObservationRequest
	if json.Unmarshal(expected, &request) != nil {
		return reject
	}
	output, _ := json.Marshal(make([]historyConfigState, len(request.Entries)))
	return testkit.IncusServerExecStep{Stdout: output, Release: release, AfterInput: func(actual testkit.IncusServerExecCall) int {
		current, err := c.request()
		if err != nil || !bytes.Equal(current, expected) || !bytes.Equal(actual.Stdin, expected) || actual.Project != call.Project || actual.Name != call.Name || actual.User != call.User || actual.Group != call.Group || len(actual.Environment) != 0 || !slices.Equal(actual.Command, call.Command) {
			return 99
		}
		return 0
	}}
}

func structuredConfigObservationStep(c configObservationControl, call testkit.IncusServerExecCall, release <-chan struct{}, reject testkit.IncusServerExecStep) testkit.IncusServerExecStep {
	expected, payloads, err := c.description()
	var files configObservationRequest
	if err != nil || json.Unmarshal(expected, &files) != nil || call.Project != "subyard" || call.Name != "yard" || call.User != 0 || call.Group != 0 || len(call.Environment) != 0 {
		return reject
	}
	for _, file := range files.Entries {
		if file.Format == "" {
			continue
		}
		request, err := configmaterial.Request(file.Format, configmaterial.ModeObserve, "dev", file.Path, 1000, payloads[file.Path])
		if err != nil || !slices.Equal(request.Command, call.Command[:10]) {
			continue
		}
		index := strings.LastIndex(request.Command[3], "\ntry:\n    main()\n")
		if index < 0 {
			return reject
		}
		// Reuse the shipped pure observation function. Never run main or read a
		// guest pathname on the host; this fixture declares all guest files absent.
		program := request.Command[3][:index] + "\ndesired=parse_document(sys.stdin.buffer.read())\nreport=materialized_observation({},desired,None,None,False,None," + fmt.Sprintf("%q", request.Command[8]) + ")\nreport['state']={'baseline':'','receipt':''}\nreport['destination']={'exists':False,'digest':'','uid':0,'gid':0,'mode':0}\nprint(json.dumps(report))\n"
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		command := exec.CommandContext(ctx, "python3", "-B", "-c", program)
		command.Stdin = bytes.NewReader(request.Stdin)
		output, err := command.Output()
		cancel()
		observed, decodeErr := configmaterial.ParseObservation(output)
		if err != nil || len(output) > 4096 || decodeErr != nil {
			return reject
		}
		capture, _ := configmaterial.WithAfterOwnerObservation(request, nil)
		retained, _ := configmaterial.WithAfterOwnerObservation(request, &configmaterial.ApplyExpectedState{State: observed.State, Fingerprint: observed.Fingerprint})
		if !slices.Equal(call.Command, capture.Command) && !slices.Equal(call.Command, retained.Command) {
			return reject
		}
		return testkit.IncusServerExecStep{Stdout: output, Release: release, AfterInput: func(actual testkit.IncusServerExecCall) int {
			current, err := c.request()
			if err != nil || !bytes.Equal(current, expected) || !bytes.Equal(actual.Stdin, request.Stdin) || actual.Project != call.Project || actual.Name != call.Name || actual.User != call.User || actual.Group != call.Group || len(actual.Environment) != 0 || !slices.Equal(actual.Command, call.Command) {
				return 99
			}
			return 0
		}}
	}
	return reject
}
