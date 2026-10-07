package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode"
)

// SourceError retains config provenance without rendering input values or the
// wrapped cause. Call SourceDiagnostic at the operation boundary to localize it.
type SourceError struct {
	Path   string
	Line   int
	Key    string
	Role   string
	Reason string
	cause  error
}

func (err *SourceError) Error() string { return err.diagnostic("") }
func (err *SourceError) Unwrap() error { return err.cause }

// SourceDiagnostic returns only trusted source context, never arbitrary error
// text. Paths within configHome are relative to that root; external sources are
// identified without exposing an absolute operator or repository path.
func SourceDiagnostic(configHome string, err error) (string, bool) {
	var source *SourceError
	if !errors.As(err, &source) {
		return "", false
	}
	return source.diagnostic(configHome), true
}

func (err *SourceError) diagnostic(configHome string) string {
	path := err.Path
	role := err.Role
	if configHome != "" && path != "" {
		relative, relErr := filepath.Rel(filepath.Clean(configHome), filepath.Clean(path))
		if relErr == nil && relative != ".." && !filepath.IsAbs(relative) &&
			!strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			path = filepath.ToSlash(relative)
			if strings.HasPrefix(path, GitSettingsRelativePath+"/") {
				role = "Git " + role
			} else {
				role = "local " + role
			}
		} else {
			path = "configuration source outside config home"
		}
	}
	if path == "" {
		path = "configuration"
	}
	// Bound and quote paths because filenames may contain terminal controls.
	if len(path) > 256 {
		path = path[:256] + "..."
	}
	if strings.ContainsFunc(path, unicode.IsControl) {
		path = strconv.QuoteToASCII(path)
	}
	if err.Line > 0 {
		path += fmt.Sprintf(":%d", err.Line)
	}
	if err.Key != "" {
		path += ": " + err.Key
	}
	return path + " (" + role + "): " + err.Reason
}

func sourceError(path string, line int, key, role, reason string, cause error) error {
	return &SourceError{Path: path, Line: line, Key: key, Role: role, Reason: reason, cause: cause}
}

func sourceContext(path string, line int, key, role string, cause error) error {
	var source *SourceError
	if errors.As(cause, &source) {
		copy := *source
		if copy.Path == "" {
			copy.Path = path
			copy.Line = line
		}
		if copy.Key == "" {
			copy.Key = key
		}
		copy.Role = role
		copy.cause = cause
		return &copy
	}
	return sourceError(path, line, key, role, "invalid configuration assignment", cause)
}

func recognizedSettingKey(settings Catalog, name string) string {
	name = canonicalSettingName(name)
	if len(name) <= 128 && ValidVariable(name) {
		if _, ok := settings.LookupSetting(name); ok {
			return name
		}
	}
	return ""
}

func recordSettingKey(record string) string {
	name, _ := persistentDirectAssignment(record)
	return recognizedSettingKey(Catalog{}, name)
}

func (tracker *settingTracker) sourceError(name, reason string, cause error) error {
	path, line, role := "", 0, "configuration"
	if assignments := tracker.assignments[name]; len(assignments) > 0 {
		assignment := assignments[len(assignments)-1]
		path, line = assignment.Path, assignment.Line
		role = tracker.layers[assignment.Layer].Role
	}
	return sourceError(path, line, recognizedSettingKey(tracker.catalog, name), role, reason, cause)
}

func sourceAccessError(path, role string, err error) error {
	reason := "cannot read configuration source"
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
		reason = "configuration source has unsafe file type"
	}
	return sourceError(path, 0, "", role, reason, err)
}

func settingValueReason(definition SettingDefinition) string {
	switch definition.Type {
	case SettingPort:
		return fmt.Sprintf("invalid port value; must be in range %d..%d", definition.Minimum, definition.Maximum)
	case SettingInteger:
		if definition.Maximum != 0 {
			return fmt.Sprintf("invalid integer value; must be in range %d..%d", definition.Minimum, definition.Maximum)
		}
		return "invalid integer value"
	case SettingMountList:
		return "invalid mount list"
	case SettingLinkList:
		return "invalid persistent-link list"
	case SettingBoolean:
		return "invalid boolean value"
	case SettingSize:
		return "invalid size value"
	case SettingName, SettingNameList:
		return "invalid name value"
	case SettingAbsolutePath, SettingRelativePath, SettingRegularFilePath:
		return "invalid path value"
	case SettingIPv4:
		return "invalid IPv4 value"
	case SettingInterface:
		return "invalid network interface value"
	case SettingSHA256:
		return "invalid SHA-256 value"
	case SettingVersion:
		return "invalid version value"
	case SettingExecutable:
		return "invalid executable value"
	case SettingImageReference:
		return "invalid image-reference value"
	default:
		return "invalid setting value"
	}
}
