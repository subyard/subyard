package profile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// RunServices is a physical leaf behind the core operation's approval. Hooks
// belong to shipped profiles; secret values never travel in the command line.
func RunServices(ctx context.Context, root string, arguments []string, environment map[string]string, stdout, stderr io.Writer) error {
	if environment["SUBYARD_ENGINE_CONTEXT"] != "1" || environment["SUBYARD_ENGINE_CONTEXT_SCHEMA"] != "1" {
		return errors.New("prepared engine context required")
	}
	if len(arguments) < 1 || len(arguments) > 2 {
		return errors.New("invalid profile service arguments")
	}
	action := arguments[0]
	if !slices.Contains([]string{"--check", "--yes", "--pause", "--resume", "--remove"}, action) || len(arguments) == 2 && action != "--resume" {
		return errors.New("invalid profile service action")
	}
	definitions, err := Load(root)
	if err != nil {
		return err
	}
	selectedResume := []string{}
	if len(arguments) == 2 {
		selectedResume = strings.Fields(arguments[1])
	}
	for _, name := range selectedResume {
		if !slices.ContainsFunc(definitions, func(d Definition) bool { return d.Name == name && d.OwnerService != "" }) {
			return errors.New("unknown paused profile service")
		}
	}
	paused := []Definition{}
	yard := environment["SUBYARD_YARD"]
	// The prepared shell context represents the default yard with an empty selector.
	if yard == "" {
		yard = "default"
	}
	run := func(definition Definition, action string, output io.Writer) error {
		env := make([]string, 0, len(environment)+1)
		for key, value := range environment {
			if key != "SUBYARD_PROFILE_SELECTED" {
				env = append(env, key+"="+value)
			}
		}
		selected := "0"
		if definition.Selected(yard, environment) {
			selected = "1"
		}
		env = append(env, "SUBYARD_PROFILE_SELECTED="+selected)
		command := exec.CommandContext(ctx, "bash", filepath.Join(definition.Root, definition.OwnerService), action)
		command.Dir = root
		command.Env = env
		command.Stdout = output
		command.Stderr = stderr
		if err := command.Run(); err != nil {
			return fmt.Errorf("profile %s owner service %s failed: %w", definition.Name, action, err)
		}
		return nil
	}
	for _, definition := range definitions {
		if definition.OwnerService == "" {
			continue
		}
		if action == "--resume" && len(arguments) == 2 && !slices.Contains(selectedResume, definition.Name) {
			continue
		}
		var captured bytes.Buffer
		output := stdout
		if action == "--pause" {
			output = &captured
		}
		if err := run(definition, action, output); err != nil {
			// A failed pause must not strand earlier services; the caller receives no
			// usable pause list on failure and cannot restore those itself.
			for _, previous := range paused {
				_ = run(previous, "--resume", stderr)
			}
			return err
		}
		if action == "--pause" {
			result := strings.TrimSpace(captured.String())
			if result != "" && result != "paused" {
				_ = run(definition, "--resume", stderr)
				for _, previous := range paused {
					_ = run(previous, "--resume", stderr)
				}
				return errors.New("invalid profile service pause response")
			}
			if result == "paused" {
				paused = append(paused, definition)
			}
		}
	}
	if action == "--pause" {
		for _, definition := range paused {
			fmt.Fprintln(stdout, definition.Name)
		}
	}
	return nil
}
