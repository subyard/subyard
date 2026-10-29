package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/Subyard/Subyard/internal/domain"
	"strconv"
	"strings"
	"syscall"
	"unicode"
)

// SourceError retains config provenance without rendering input values or the
// wrapped cause. Call SourceDiagnostic at the operation boundary to localize it.
type SourceError struct {
	Path         string
	Line         int
	Key          string
	Role         string
	Reason       string
	cause        error
	ownerLocal   bool
	localDetails string
}

func (err *SourceError) Error() string {
	if err.ownerLocal {
		return err.Role + ": " + err.Reason
	}
	return err.diagnostic("")
}
func (err *SourceError) Unwrap() error { return err.cause }

// SourceDiagnostic returns only trusted source context, never arbitrary error
// text. Paths within configHome are relative to that root; external sources are
// identified without exposing an absolute operator or repository path.
func SourceDiagnostic(configHome string, err error) (string, bool) {
	var source *SourceError
	if !errors.As(err, &source) {
		return "", false
	}
	if source.ownerLocal && configHome == "" {
		return source.Error(), true
	}
	return source.diagnostic(configHome), true
}

// OwnerMetadataError retains nonsecret owner-local metadata for the invoking
// CLI. Its default and public source diagnostics omit local details and paths.
// Callers must supply fixed reasons, never arbitrary wrapped error text.
func OwnerMetadataError(path, role, reason, localDetails string, cause error) error {
	return &SourceError{Path: path, Role: role, Reason: reason, cause: cause, ownerLocal: true, localDetails: localDetails}
}

// OwnerMetadataContext adds provenance without dropping typed or local facts.
func OwnerMetadataContext(path, role string, cause error) error {
	var source *SourceError
	if errors.As(cause, &source) && source.ownerLocal {
		copy := *source
		if copy.Path == "" {
			copy.Path = path
		} else if path != "" && copy.Path != path {
			if copy.localDetails != "" {
				copy.localDetails += "; "
			}
			copy.localDetails += role + " source=" + QuoteMetadataPath(path)
		}
		if role != "" && copy.Role != role {
			copy.Role = role + " / " + copy.Role
		}
		copy.cause = cause
		return &copy
	}
	return OwnerMetadataError(path, role, "invalid protected metadata", "", cause)
}

// OwnerSourceDiagnostic is only for the local operation's stderr boundary.
// RPC, activation and other remote consumers must use the default diagnostic.
func OwnerSourceDiagnostic(err error) (string, bool) {
	var source *SourceError
	if !errors.As(err, &source) || !source.ownerLocal {
		return "", false
	}
	message := source.Role + ": " + source.Reason
	if errors.Is(err, domain.ErrPlanStale) {
		message = domain.ErrPlanStale.Error() + ": " + message
	}
	if source.Path != "" {
		message += "; source=" + QuoteMetadataPath(source.Path)
	}
	if source.localDetails != "" {
		message += "; " + source.localDetails
	}
	return message, true
}

// QuoteMetadataPath bounds and escapes explicitly allowlisted nonsecret path
// metadata. Never use this for configuration contents or credential URLs.
func QuoteMetadataPath(path string) string {
	if strings.Contains(path, "://") {
		return "unavailable (URL-shaped metadata)"
	}
	if len(path) > 256 {
		path = path[:256] + "..."
	}
	return strconv.QuoteToASCII(path)
}

// MetadataAccessReason classifies an access failure without rendering a path,
// arbitrary wrapped stderr, or bytes from a protected source.
func MetadataAccessReason(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "source is missing (observed metadata unavailable)"
	case errors.Is(err, os.ErrPermission):
		return "source permission denied (observed metadata unavailable)"
	case errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR):
		return "source has unsafe file type (observed metadata unavailable)"
	default:
		return "cannot read source (observed metadata unavailable)"
	}
}

// FileMetadata reports only native type, permission and owner facts already
// observed by a safety check. It does not validate or read the source again.
func FileMetadata(info os.FileInfo) string {
	owner := "unavailable"
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		owner = strconv.FormatUint(uint64(stat.Uid), 10)
	}
	kind := info.Mode().Type().String()
	switch {
	case info.Mode().IsRegular():
		kind = "regular"
	case info.IsDir():
		kind = "directory"
	case info.Mode()&os.ModeSymlink != 0:
		kind = "symlink"
	}
	return fmt.Sprintf("observed type=%s, mode=%04o, uid=%s", kind, info.Mode().Perm(), owner)
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
	reason := "cannot read configuration source: " + MetadataAccessReason(err)
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
		reason = "configuration source has unsafe file type; observed metadata unavailable"
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

// SourceAliasRepairRetry identifies the bounded local repair boundary without rendering values.
func SourceAliasRepairRetry(home, yard string, cause error) string {
	var source *SourceError
	if !errors.As(cause, &source) || source.Reason != "conflicting legacy and canonical settings" || !slices.Contains([]string{"YARD_IMAGE", "YARD_IMAGE_FALLBACK", "ENVIRONMENT_PROFILES", "CODING_TOOL_INTEGRATIONS"}, source.Key) {
		return ""
	}
	if !domain.SafeName(yard) {
		return ""
	}
	scope := ""
	switch filepath.Clean(source.Path) {
	case filepath.Join(home, "config.env"):
		scope = "host"
	case filepath.Join(home, "overrides", "shared", "config.env"):
		scope = "shared"
	case filepath.Join(home, "yards", yard, "config.env"):
		scope = "yard"
	}
	if scope == "" {
		return "run config validate; restore the owning source or use its supported schema migration"
	}
	return fmt.Sprintf("choose the desired value explicitly, then run yard -Y %s config repair-alias %s CHOSEN_VALUE --scope %s --local --check", yard, source.Key, scope)
}
