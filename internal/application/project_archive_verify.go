package application

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/shellquote"
)

type projectArchiveEntry struct {
	name string
	kind byte
	link string
}
type projectArchiveManifest struct {
	checksums string
	entries   []projectArchiveEntry
}

// Content hashes bind extracted bytes without requiring host uid/gid to match
// the yard user. Links and directories are checked separately without following
// them as archive destinations.
func readProjectArchiveManifest(input io.Reader) (*projectArchiveManifest, error) {
	reader := tar.NewReader(input)
	manifest := &projectArchiveManifest{}
	var checksums strings.Builder
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("inspect retained project archive: %w", err)
		}
		name, err := projectArchivePath(header.Name)
		if err != nil {
			return nil, err
		}
		entry := projectArchiveEntry{name: name, kind: header.Typeflag, link: header.Linkname}
		switch header.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			hash := sha256.New()
			if _, err := io.Copy(hash, reader); err != nil {
				return nil, err
			}
			checksums.WriteString(projectChecksumLine(hex.EncodeToString(hash.Sum(nil)), name))
		case tar.TypeDir:
		case tar.TypeSymlink:
		case tar.TypeLink:
			entry.link, err = projectArchivePath(header.Linkname)
			if err != nil {
				return nil, err
			}
		default:
			return nil, errors.New("project archive contains an unsupported file type")
		}
		manifest.entries = append(manifest.entries, entry)
	}
	manifest.checksums = checksums.String()
	return manifest, nil
}

func projectArchivePath(value string) (string, error) {
	name := path.Clean(value)
	if name == ".." || strings.HasPrefix(name, "../") || path.IsAbs(name) || strings.ContainsRune(name, '\x00') {
		return "", errors.New("project archive contains a path outside its workspace")
	}
	return name, nil
}

func projectChecksumLine(digest, name string) string {
	escaped := strings.ContainsAny(name, "\\\n")
	name = strings.ReplaceAll(strings.ReplaceAll(name, "\\", "\\\\"), "\n", "\\n")
	prefix := ""
	if escaped {
		prefix = "\\"
	}
	return prefix + digest + "  " + name + "\n"
}

func (manifest *projectArchiveManifest) verify(ctx context.Context, data ports.YardExecutor, yard domain.Context, directory string) error {
	dev := uint32(yard.DevUID)
	var checks strings.Builder
	checks.WriteString("set -eu\ncd -- \"$1\"\n")
	for _, entry := range manifest.entries {
		file := shellquote.Word(entry.name)
		switch entry.kind {
		case tar.TypeReg, tar.TypeRegA:
			fmt.Fprintf(&checks, "[ -f %s ] && [ ! -L %s ]\n", file, file)
		case tar.TypeDir:
			fmt.Fprintf(&checks, "[ -d %s ] && [ ! -L %s ]\n", file, file)
		case tar.TypeSymlink:
			fmt.Fprintf(&checks, "[ -L %s ] && [ \"$(readlink -- %s; printf '.')\" = %s ]\n", file, file, shellquote.Word(entry.link+"\n."))
		case tar.TypeLink:
			fmt.Fprintf(&checks, "[ %s -ef %s ]\n", file, shellquote.Word(entry.link))
		}
	}
	result, err := data.Stream(ctx, yard, ports.InstanceExecRequest{Command: []string{"sh", "-s", "--", directory}, User: dev, Group: dev}, strings.NewReader(checks.String()))
	if err != nil || result.ExitCode != 0 {
		return executionError("check extracted project entry types", result, err)
	}
	if manifest.checksums != "" {
		result, err := data.Stream(ctx, yard, ports.InstanceExecRequest{
			Command: []string{"sh", "-c", `set -eu; cd -- "$1"; exec sha256sum --check --strict --status`, "subyard", directory}, User: dev, Group: dev,
		}, strings.NewReader(manifest.checksums))
		if err != nil || result.ExitCode != 0 {
			return executionError("check extracted project content", result, err)
		}
	}
	return nil
}
