package application

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func projectTreeExcluded(name string) bool {
	if name == "." {
		return true
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".git" {
			return true
		}
	}
	return false
}

func projectTreeDigest(entries map[string]string) string {
	payload, _ := json.Marshal(entries)
	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

// ProjectArchiveTreeDigest matches diff's .git exclusion without extracting
// controller input or writing to the yard during assessment.
func ProjectArchiveTreeDigest(input io.Reader) (string, error) {
	return ProjectArchiveContentDigest(input, true)
}

func ProjectArchiveContentDigest(input io.Reader, excludeGit bool) (string, error) {
	entries := make(map[string]string)
	links := make(map[string]string)
	reader := tar.NewReader(input)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		name, err := projectArchivePath(header.Name)
		if err != nil {
			return "", err
		}
		if name == "." || excludeGit && projectTreeExcluded(name) {
			continue
		}
		switch header.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			hash := sha256.New()
			if _, err := io.Copy(hash, reader); err != nil {
				return "", err
			}
			entries[name] = "f:" + hex.EncodeToString(hash.Sum(nil))
		case tar.TypeDir:
			entries[name] = "d:"
		case tar.TypeSymlink:
			entries[name] = "l:" + header.Linkname
		case tar.TypeLink:
			link, err := projectArchivePath(header.Linkname)
			if err != nil {
				return "", err
			}
			links[name] = link
		default:
			return "", errors.New("project snapshot contains unsupported file type")
		}
	}
	for name, target := range links {
		content, ok := entries[target]
		if !ok || !strings.HasPrefix(content, "f:") {
			return "", errors.New("project archive hardlink target is missing")
		}
		entries[name] = content
	}
	return projectTreeDigest(entries), nil
}

// ObserveProjectTree uses bounded read-only native probes. No guest temporary
// files, archive extraction or protected input transfer occurs here.
func ObserveProjectTree(ctx context.Context, data ports.YardExecutor, yard domain.Context, root string) (string, error) {
	return ObserveProjectContentDigest(ctx, data, yard, root, true)
}

func ObserveProjectContentDigest(ctx context.Context, data ports.YardExecutor, yard domain.Context, root string, excludeGit bool) (string, error) {
	probe := func(script string) ([]byte, error) {
		result, err := data.Execute(ctx, yard, ports.InstanceExecRequest{
			Command: []string{"sh", "-c", script, "subyard", root}, User: uint32(yard.DevUID), Group: uint32(yard.DevUID),
		})
		if err != nil || result.ExitCode != 0 {
			return nil, executionError("inspect project snapshot", result, err)
		}
		return result.Stdout, nil
	}
	prune := ""
	if excludeGit {
		prune = "-name .git -prune -o "
	}
	payload, err := probe(`set -eu; cd -- "$1"; find . ` + prune + `-printf '%y\0%P\0%l\0'`)
	if err != nil {
		return "", err
	}
	fields := strings.Split(string(payload), "\x00")
	if len(fields) == 0 || fields[len(fields)-1] != "" || (len(fields)-1)%3 != 0 {
		return "", errors.New("project type probe returned invalid manifest")
	}
	entries := make(map[string]string)
	for index := 0; index < len(fields)-1; index += 3 {
		name, err := projectArchivePath(fields[index+1])
		if err != nil {
			return "", err
		}
		if name == "." || excludeGit && projectTreeExcluded(name) {
			continue
		}
		switch fields[index] {
		case "f":
			entries[name] = "f:"
		case "d":
			entries[name] = "d:"
		case "l":
			entries[name] = "l:" + fields[index+2]
		default:
			return "", errors.New("project snapshot contains unsupported file type")
		}
	}
	payload, err = probe(`set -eu; cd -- "$1"; find . ` + prune + `-type f -exec sha256sum --zero -- {} +`)
	if err != nil {
		return "", err
	}
	for _, record := range strings.Split(string(payload), "\x00") {
		if record == "" {
			continue
		}
		if len(record) < 67 || record[64:66] != "  " {
			return "", errors.New("project hash probe returned invalid manifest")
		}
		name, err := projectArchivePath(record[66:])
		if err != nil {
			return "", err
		}
		if entries[name] != "f:" {
			return "", fmt.Errorf("%w: project file types changed during inspection", domain.ErrPlanStale)
		}
		if _, err := hex.DecodeString(record[:64]); err != nil {
			return "", errors.New("project hash probe returned invalid digest")
		}
		entries[name] = "f:" + record[:64]
	}
	for _, value := range entries {
		if value == "f:" {
			return "", fmt.Errorf("%w: project files changed during inspection", domain.ErrPlanStale)
		}
	}
	return projectTreeDigest(entries), nil
}
