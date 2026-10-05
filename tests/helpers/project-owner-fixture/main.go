// Command project-owner-fixture runs the production owner RPC boundary with a
// file-backed native project oracle for host-free project contract tests.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/cli"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// nativeIncus supplies only the observations required by these project tests.
type nativeIncus struct{}

func (nativeIncus) Server(context.Context) (ports.ServerInfo, error) {
	return ports.ServerInfo{}, nil
}
func (nativeIncus) Instance(_ context.Context, project, name string) (ports.InstanceInfo, error) {
	if project == "subyard" && (name == "yard" || name == "yard-inner") {
		return ports.InstanceInfo{Name: name, Project: project, Status: "Running"}, nil
	}
	return ports.InstanceInfo{}, ports.ErrInstanceNotFound
}
func (nativeIncus) ReconcileState(context.Context, string, string, string, string, string) (ports.ReconcileState, error) {
	return ports.ReconcileState{}, fmt.Errorf("project fixture does not support reconciliation")
}
func (nativeIncus) Events(context.Context, []string) (<-chan domain.OperationEvent, <-chan error) {
	events := make(chan domain.OperationEvent)
	failures := make(chan error)
	close(events)
	close(failures)
	return events, failures
}

type executor struct {
	root     string
	registry bool
}

func (e executor) exists(name string) bool {
	_, err := os.Stat(filepath.Join(e.root, name))
	return err == nil
}
func (e executor) mark(name string) error {
	target := map[string]string{"data-cleanup": "container", "staged-delete": "staged", "workspace-delete": "workspace"}[name]
	if err := os.RemoveAll(filepath.Join(e.root, target)); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(e.root, name), nil, 0600)
}
func (e executor) Exec(_ context.Context, _, _ string, r ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if e.registry {
		return e.projectExec(r)
	}
	command := strings.Join(r.Command, " ")
	result := ports.InstanceExecResult{}
	missing := func() (ports.InstanceExecResult, error) {
		return ports.InstanceExecResult{ExitCode: 1, Stderr: []byte("Error: No such object")}, nil
	}
	switch {
	case command == "docker info":
		mode, _ := os.ReadFile(filepath.Join(e.root, "cleanup-mode"))
		if strings.TrimSpace(string(mode)) == "fail" {
			result.ExitCode = 1
		}
	case strings.Contains(command, "docker inspect"):
		if strings.Contains(command, "printf present") {
			if !e.exists("container") {
				result.Stdout = []byte("missing")
			} else {
				result.Stdout = []byte("present")
			}
		} else if !e.exists("container") {
			return missing()
		} else if len(r.Command) > 2 && r.Command[2] == "-f" {
			result.Stdout = []byte("sha256:owned-container\t1\tdemo-12345678\tsynthetic\n")
		}
	case strings.HasPrefix(command, "docker rm "):
		return result, e.mark("data-cleanup")
	case strings.Contains(command, "/srv/env-secrets/"):
		if strings.Contains(command, "printf present") {
			if !e.exists("staged") {
				result.Stdout = []byte("missing")
			} else {
				result.Stdout = []byte("present")
			}
		} else if strings.HasPrefix(command, "rm -rf ") {
			return result, e.mark("staged-delete")
		} else if e.exists("staged") {
			result.ExitCode = 1
		}
	case strings.Contains(command, "/srv/workspaces/demo-12345678"):
		if strings.Contains(command, "printf present") {
			if !e.exists("workspace") {
				result.Stdout = []byte("missing")
			} else {
				result.Stdout = []byte("present")
			}
		} else if strings.HasPrefix(command, "rm -rf ") {
			return result, e.mark("workspace-delete")
		} else {
			return result, fmt.Errorf("unexpected workspace command")
		}
	case strings.Contains(command, "projects-changed"):
		// The synthetic profile declares no project hooks.
	default:
		return result, fmt.Errorf("unexpected native project command")
	}
	return result, nil
}

// Only native project tools run here; logical guest paths map into this
// disposable fixture root, and Git uses an actual retained revision file.
func (e executor) projectExec(r ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if len(r.Command) == 0 {
		return ports.InstanceExecResult{}, fmt.Errorf("empty fixture command")
	}
	args := append([]string(nil), r.Command...)
	for i, arg := range args {
		args[i] = strings.ReplaceAll(arg, "/srv/workspaces", filepath.Join(e.root, "guest", "workspaces"))
	}
	switch args[0] {
	case "git":
		revision := "1111111111111111111111111111111111111111"
		if len(args) > 1 && args[1] == "ls-remote" {
			return ports.InstanceExecResult{Stdout: []byte(revision + "\tHEAD\n")}, nil
		}
		if len(args) > 1 && args[1] == "clone" {
			target := args[len(args)-1]
			if err := os.MkdirAll(filepath.Join(target, ".git"), 0700); err != nil {
				return ports.InstanceExecResult{}, err
			}
			return ports.InstanceExecResult{}, os.WriteFile(filepath.Join(target, ".git", "HEAD"), []byte(revision+"\n"), 0600)
		}
		if len(args) > 3 && args[1] == "-C" {
			if args[3] == "checkout" {
				return ports.InstanceExecResult{}, nil
			}
			if args[3] == "rev-parse" {
				data, err := os.ReadFile(filepath.Join(args[2], ".git", "HEAD"))
				return ports.InstanceExecResult{Stdout: data}, err
			}
		}
	case "chown":
		// UID translation belongs to Incus; filesystem observations remain real.
		return ports.InstanceExecResult{}, nil
	case "tee":
		if len(args) != 2 {
			return ports.InstanceExecResult{}, fmt.Errorf("invalid metadata write")
		}
		if err := os.WriteFile(args[1], r.Stdin, 0600); err != nil {
			return ports.InstanceExecResult{}, err
		}
		if err := os.WriteFile(filepath.Join(e.root, "yard-meta.json"), r.Stdin, 0600); err != nil {
			return ports.InstanceExecResult{}, err
		}
		return ports.InstanceExecResult{Stdout: r.Stdin}, nil
	case "cat", "mkdir", "install", "chmod", "tar", "find", "rm", "sh":
		command := exec.Command(args[0], args[1:]...)
		command.Stdin = bytes.NewReader(r.Stdin)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		result := ports.InstanceExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				result.ExitCode = exit.ExitCode()
				return result, nil
			}
			return result, err
		}
		return result, nil
	}
	return ports.InstanceExecResult{}, fmt.Errorf("unexpected native fixture tool %s", args[0])
}

func main() {
	root := os.Getenv("REMOTE_TEST_STATE")
	registry := false
	if root == "" {
		root = os.Getenv("REGISTRY_TEST_STATE")
		registry = true
	}
	native := nativeIncus{}
	arguments := os.Args[1:]
	if len(arguments) == 0 {
		arguments = []string{"rpc", "--stdio"}
	}
	application, err := cli.New(cli.Options{RepositoryRoot: os.Getenv("PROJECT_OWNER_REPOSITORY"), Program: "yard", Arguments: arguments, Environment: os.Environ(), Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Incus: native, Executor: executor{root: root, registry: registry}})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(application.Run(context.Background()))
}
