package configmaterial

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// A consent token names the exact guest bytes a refresh may replace:
// TokenAbsent, or "sha256:" and the hex digest of the destination's bytes. An
// unreadable destination has no token, so every write to it refuses.
const (
	TokenAbsent     = "absent"
	TokenUnreadable = "unreadable"
	// ProblemChanged is a write that found its destination changed since the
	// token it compared with was observed.
	ProblemChanged = "changed"
)

// Guest exits of a compare-and-write that left the destination as it was.
const (
	exitUnreadable = 3
	exitChanged    = 4
)

var (
	tokenPattern    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	problemPattern  = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z ._-]{0,79}$`)
	errUnobservable = errors.New("guest config files could not be observed")
)

// ValidToken reports a token a writer may compare with.
func ValidToken(token string) bool { return token == TokenAbsent || tokenPattern.MatchString(token) }

// PayloadToken is the token of exact bytes.
func PayloadToken(payload []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(payload))
}

// Desired is one guest config file a refresh writes; Format is "" for raw bytes.
type Desired struct {
	Label, Destination, Format string
	Payload                    []byte
}

// File is the one observation of a guest config file, and after a refused
// write the file as that write left it.
type File struct {
	Label, Destination string
	// Token is the consent token of the observed bytes, or TokenUnreadable.
	Token     string
	Converged bool
	// Fingerprint is a structured file's observed fingerprint.
	Fingerprint string
	// Problem is ProblemChanged, or why a completed guest observation could
	// not assess the file.
	Problem string
}

// State is the file's one state: converged, drift, unreadable, changed or unknown.
func (file File) State() string {
	switch {
	case file.Problem == ProblemChanged:
		return "changed"
	case file.Problem != "":
		return "unknown"
	case file.Token == TokenUnreadable:
		return "unreadable"
	case file.Converged:
		return "converged"
	}
	return "drift"
}

// Assess is the file's fact in the materialized fingerprint.
func (file File) Assess(desired config.DesiredMaterializedArtifact) (config.MaterializedArtifactAssessment, error) {
	observed := config.MaterializationObservation{ExitCode: 1}
	if digest, ok := strings.CutPrefix(file.Token, "sha256:"); ok && desired.OwnedFormat == "" {
		observed = config.MaterializationObservation{Output: []byte(digest + "  " + desired.Destination + "\n")}
	}
	if desired.OwnedFormat != "" {
		observed = config.MaterializationObservation{Fingerprint: file.Fingerprint, OwnedConverged: file.Converged}
	}
	assessed, err := config.AssessMaterializedArtifact(desired, observed)
	assessed.Converged = assessed.Converged && file.State() == "converged"
	return assessed, err
}

// CompareWriteProgram is the exact guest program of raw observation and writes.
func CompareWriteProgram() string { return guestCASProgram + "\ncas_main()\n" }

func validDestination(developer, destination string) error {
	home := "/home/" + developer
	if !domain.SafeName(developer) || path.Clean(destination) != destination || !strings.HasPrefix(destination, home+"/") {
		return errors.New("guest config destination leaves the developer home")
	}
	return nil
}

// transportFailure is an exec error that is not a completed guest exit.
func transportFailure(result ports.InstanceExecResult, err error) error {
	var completed *ports.GuestExitError
	if err != nil && (!errors.As(err, &completed) || completed.Code != result.ExitCode || result.ExitCode == 0) {
		return err
	}
	return nil
}

// Observe reads each desired file of one yard once: the raw files together in
// one exec, each structured file through its own observation. A completed
// guest refusal of a file is that file's unknown state; only a transport
// failure or an invalid request is an error.
func Observe(ctx context.Context, executor ports.InstanceExecutor, yard domain.Context, desired []Desired) ([]File, error) {
	files := make([]File, len(desired))
	command := []string{"python3", "-B", "-c", CompareWriteProgram(), "observe", "/home/" + yard.DevUser}
	var raw []int
	for index, item := range desired {
		files[index] = File{Label: item.Label, Destination: item.Destination}
		if err := validDestination(yard.DevUser, item.Destination); err != nil {
			return nil, err
		}
		if item.Format == "" {
			raw, command = append(raw, index), append(command, item.Destination)
			continue
		}
		request, err := Request(item.Format, ModeObserve, yard.DevUser, item.Destination, yard.DevUID, item.Payload)
		if err != nil {
			return nil, fmt.Errorf("observe %s: %w", item.Label, err)
		}
		result, err := executor.Exec(ctx, yard.IncusProject, yard.YardInstanceName, request)
		if err := transportFailure(result, err); err != nil {
			return nil, fmt.Errorf("observe %s: %w", item.Label, err)
		}
		observation, err := ParseObservation(result.Stdout)
		switch {
		case result.ExitCode != 0:
			files[index].Problem = observationProblem(result.Stderr)
		case err != nil || observation.Raw == "":
			files[index].Problem = "invalid structured configuration observation"
		default:
			files[index].Token, files[index].Converged, files[index].Fingerprint = observation.Raw, observation.Converged, observation.Fingerprint
		}
	}
	if len(raw) == 0 {
		return files, nil
	}
	result, err := executor.Exec(ctx, yard.IncusProject, yard.YardInstanceName, ports.InstanceExecRequest{Command: command})
	if err := transportFailure(result, err); err != nil {
		return nil, fmt.Errorf("%w: %w", errUnobservable, err)
	}
	tokens, err := parseTokens(result, command[6:])
	for position, index := range raw {
		if err != nil {
			files[index].Problem = err.Error()
			continue
		}
		files[index].Token, files[index].Converged = tokens[position], tokens[position] == PayloadToken(desired[index].Payload)
	}
	return files, nil
}

// observationProblem is the guest's refusal category, when it is safe to print.
func observationProblem(stderr []byte) string {
	category, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(string(stderr)), "config materialization: "), ";")
	if problemPattern.MatchString(category) && category != ProblemChanged {
		return category
	}
	return "structured configuration observation failed"
}

// parseTokens accepts exactly one valid token per requested destination.
func parseTokens(result ports.InstanceExecResult, destinations []string) ([]string, error) {
	var encoded struct {
		Files []struct {
			Destination string `json:"destination"`
			Token       string `json:"token"`
		} `json:"files"`
	}
	decoder := json.NewDecoder(bytes.NewReader(result.Stdout))
	decoder.DisallowUnknownFields()
	if result.ExitCode != 0 || decoder.Decode(&encoded) != nil || decoder.Decode(&struct{}{}) != io.EOF || len(encoded.Files) != len(destinations) {
		return nil, errUnobservable
	}
	tokens := make([]string, len(destinations))
	for index, file := range encoded.Files {
		if file.Destination != destinations[index] || !ValidToken(file.Token) && file.Token != TokenUnreadable {
			return nil, errUnobservable
		}
		tokens[index] = file.Token
	}
	return tokens, nil
}

// RawRequest replaces one unstructured destination only over the token.
func RawRequest(developer, destination string, uid int, payload []byte, expect string) (ports.InstanceExecRequest, error) {
	if err := validDestination(developer, destination); err != nil {
		return ports.InstanceExecRequest{}, err
	}
	if !ValidToken(expect) {
		return ports.InstanceExecRequest{}, errors.New("guest config write needs a consent token")
	}
	if uid <= 0 {
		uid = 1000
	}
	return ports.InstanceExecRequest{
		Command: []string{"python3", "-B", "-c", CompareWriteProgram(), "write", "/home/" + developer, destination,
			fmt.Sprint(uid), expect, fmt.Sprintf("%x", sha256.Sum256(payload))},
		Stdin: payload,
	}, nil
}

// Refused is file as a completed compare-and-write exec that kept the guest's
// bytes left it, and true; any other result is not a refusal.
func Refused(file File, result ports.InstanceExecResult, err error) (File, bool) {
	if transportFailure(result, err) != nil {
		return file, false
	}
	switch result.ExitCode {
	case exitUnreadable:
		file.Token, file.Problem = TokenUnreadable, ""
	case exitChanged:
		file.Problem = ProblemChanged
	default:
		return file, false
	}
	return file, true
}

// FileRefusal is a guest config file of one yard that a refresh does not
// replace, with the operator's next step.
type FileRefusal struct {
	Yard string
	File File
}

// Refusals are the files of one yard that a refresh does not replace, in order.
func Refusals(yard string, files []File) []error {
	var refusals []error
	for _, file := range files {
		if state := file.State(); state == "unreadable" || state == "changed" || state == "unknown" {
			refusals = append(refusals, FileRefusal{Yard: yard, File: file})
		}
	}
	return refusals
}

// Error names the file and its next step; ActivationDiagnostic also names the yard.
func (refusal FileRefusal) Error() string {
	message, next := refusal.describe()
	return message + "; next: " + next
}

func (refusal FileRefusal) ActivationDiagnostic() (string, string) {
	message, next := refusal.describe()
	return "yard " + refusal.Yard + ": " + message, next
}

func (refusal FileRefusal) describe() (string, string) {
	file, step := refusal.File, func(verb domain.Verb) string {
		return domain.RenderAction(domain.Action{Verb: verb, Yard: refusal.Yard})
	}
	switch file.State() {
	case "unreadable":
		return file.Destination + " is not a readable regular file; it was kept",
			"replace it with a regular file or remove it, then run " + step(domain.VerbConfigApply)
	case "changed":
		return file.Destination + " changed after its refresh was planned; the guest's file was kept", "run " + step(domain.VerbConfigApply)
	}
	return file.Destination + " could not be assessed: " + file.Problem, "run " + step(domain.VerbConfigStatus)
}
