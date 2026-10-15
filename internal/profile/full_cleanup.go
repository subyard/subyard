package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/ports"
)

type CleanupService struct {
	Scope string   `json:"scope"`
	Units []string `json:"units"`
}

// FullCleanupPlan is metadata-only. Each shipped owner assesses its own
// unit/helper identities; core drains the approved producers synchronously
// before removing these exact artifact bindings or any installation data.
type FullCleanupPlan struct {
	Conditional    bool                     `json:"conditional"`
	PreservedRoots []string                 `json:"preservedRoots"`
	MutableRoots   []string                 `json:"mutableRoots"`
	Blockers       []string                 `json:"blockers"`
	ManagedRoots   []string                 `json:"managedRoots"`
	Profile        string                   `json:"-"`
	Artifacts      []ports.TeardownArtifact `json:"artifacts"`
	Services       []CleanupService         `json:"services"`
	Preserved      []string                 `json:"preserved"`
}

func AssessFullCleanup(ctx context.Context, root string, environment map[string]string) ([]FullCleanupPlan, error) {
	definitions, err := Load(root)
	if err != nil {
		return nil, err
	}
	plans := []FullCleanupPlan{}
	for _, definition := range definitions {
		if definition.FullCleanup == "" {
			continue
		}
		childCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		command := exec.CommandContext(childCtx, filepath.Join(definition.Root, definition.FullCleanup), "--assess")
		command.Dir = root
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
		command.WaitDelay = time.Second
		for name, value := range environment {
			command.Env = append(command.Env, name+"="+value)
		}
		command.Stderr = io.Discard
		command.Env = append(command.Env, "PYTHONDONTWRITEBYTECODE=1")
		stdout, err := command.StdoutPipe()
		if err != nil {
			cancel()
			return nil, err
		}
		if err := command.Start(); err != nil {
			cancel()
			return nil, fmt.Errorf("profile %s full cleanup assessment is unavailable", definition.Name)
		}
		body, readErr := io.ReadAll(io.LimitReader(stdout, 65537))
		if len(body) > 65536 || readErr != nil {
			command.Cancel()
		}
		waitErr := command.Wait()
		cancel()
		if readErr != nil || waitErr != nil || len(body) > 65536 {
			return nil, fmt.Errorf("profile %s full cleanup assessment failed", definition.Name)
		}
		var plan FullCleanupPlan
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&plan); err != nil {
			return nil, fmt.Errorf("profile %s full cleanup metadata is invalid", definition.Name)
		}
		if decoder.Decode(new(any)) != io.EOF {
			return nil, errors.New("profile full cleanup metadata has trailing data")
		}
		if len(plan.Artifacts) > 256 || len(plan.Services) > 128 || len(plan.Preserved) > 128 {
			return nil, errors.New("profile full cleanup metadata is oversized")
		}
		names := map[string]bool{}
		for _, service := range plan.Services {
			if service.Scope != "user" && service.Scope != "system" {
				return nil, errors.New("profile full cleanup service has invalid scope")
			}
			if len(service.Units) > 4 {
				return nil, errors.New("profile full cleanup service is oversized")
			}
			for _, unit := range service.Units {
				if !regexp.MustCompile(`^subyard-[A-Za-z0-9_.@:+-]+\.(service|socket|timer)$`).MatchString(unit) || names[service.Scope+"/"+unit] {
					return nil, errors.New("profile full cleanup unit has invalid or duplicate identity")
				}
				names[service.Scope+"/"+unit] = true
			}
		}
		if len(plan.ManagedRoots) > 256 {
			return nil, errors.New("profile full cleanup roots are oversized")
		}
		for _, path := range append(append(append([]string{}, plan.ManagedRoots...), plan.PreservedRoots...), plan.MutableRoots...) {
			if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) == "/" {
				return nil, errors.New("profile full cleanup root is unsafe")
			}
		}
		paths := map[string]bool{}
		for _, artifact := range plan.Artifacts {
			if !filepath.IsAbs(artifact.Path) || filepath.Clean(artifact.Path) != artifact.Path || artifact.Path == "/" || paths[artifact.Path] || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(artifact.Binding) {
				return nil, errors.New("profile full cleanup artifact has invalid or duplicate identity")
			}
			paths[artifact.Path] = true
		}
		for _, text := range append(append([]string{}, plan.Preserved...), plan.Blockers...) {
			if !textSafe(text) {
				return nil, errors.New("profile full cleanup preserved diagnostic is invalid")
			}
		}
		plan.Profile = definition.Name
		plans = append(plans, plan)
	}
	return plans, nil
}
