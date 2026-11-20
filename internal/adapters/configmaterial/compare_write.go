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
)

// Problem is the closed cause of a file a refresh cannot assess or write.
type Problem string

const (
	// ProblemChanged is a write that found its destination changed since the
	// token it compared with was observed.
	ProblemChanged Problem = "changed"
	// ProblemUnownedInvalid is a structured document that is not valid and
	// that Subyard does not own: the operator corrects or removes it.
	ProblemUnownedInvalid Problem = "unowned-invalid"
	// ProblemUnassessed is a completed guest observation that assessed
	// nothing, such as one over a broken Subyard baseline.
	ProblemUnassessed Problem = "unassessed"
)

// Guest exits of a compare-and-write that left the destination as it was.
const (
	exitUnreadable = 3
	exitChanged    = 4
)

var (
	tokenPattern    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
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
	// InitOnly is a file outside the config set that config status and
	// config apply cover, such as an instruction file: only init --configs
	// refreshes it.
	InitOnly bool
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
	// Problem is why a refresh cannot assess or write the file, if it cannot.
	Problem Problem
	// InitOnly is the desired file's: its refusals name init --configs.
	InitOnly bool
}

// State is the file's one state: converged, drift, unreadable, or its Problem.
func (file File) State() string {
	switch {
	case file.Problem != "":
		return string(file.Problem)
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
// guest refusal of a file is that file's problem; only a transport failure or
// an invalid request is an error.
func Observe(ctx context.Context, executor ports.InstanceExecutor, yard domain.Context, desired []Desired) ([]File, error) {
	files := make([]File, len(desired))
	command := []string{"python3", "-B", "-c", CompareWriteProgram(), "observe", "/home/" + yard.DevUser}
	var raw []int
	for index, item := range desired {
		files[index] = File{Label: item.Label, Destination: item.Destination, InitOnly: item.InitOnly}
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
			files[index].Problem = observationProblem(result.Stderr, item.Format)
		case err != nil || observation.Raw == "":
			files[index].Problem = ProblemUnassessed
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
			files[index].Problem = ProblemUnassessed
			continue
		}
		files[index].Token, files[index].Converged = tokens[position], tokens[position] == PayloadToken(desired[index].Payload)
	}
	return files, nil
}

// observationProblem is the closed cause of a guest refusal; guest text is
// matched, never rendered.
func observationProblem(stderr []byte, format string) Problem {
	if UnownedInvalidDestination(stderr, format) {
		return ProblemUnownedInvalid
	}
	return ProblemUnassessed
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
		if state := file.State(); state != "converged" && state != "drift" {
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

// describe renders only the yard, the destination, the state and InitOnly, so
// a refusal relayed from a child renders as the writer's own.
func (refusal FileRefusal) describe() (string, string) {
	file := refusal.File
	// The step is the command that refreshes this file: config apply for the
	// config set, init --configs for a file only the runtime refreshes.
	step := domain.RenderAction(domain.Action{Verb: domain.VerbConfigApply, Yard: refusal.Yard})
	if file.InitOnly {
		step = domain.RenderAction(domain.Action{Verb: domain.VerbInit, Yard: refusal.Yard}) + " --configs"
	}
	switch file.State() {
	case "unreadable":
		return file.Destination + " is not a readable regular file; it was kept",
			"replace it with a regular file or remove it, then run " + step
	case "changed":
		return file.Destination + " changed after its refresh was planned; the guest's file was kept", "run " + step
	case string(ProblemUnownedInvalid):
		return file.Destination + " is not a valid document and Subyard does not own it; it was kept",
			"correct or remove it in the yard, then run " + step
	}
	// Nothing the operator does to the file is known to help: only a retry.
	return file.Destination + " could not be assessed; it was kept", "run " + step
}

// MaxRefusalReport bounds the refusal report an init --configs child relays;
// a larger one is not a report.
const MaxRefusalReport = 64 << 10

type refusalReport struct {
	Files []reportedFile `json:"files"`
}

type reportedFile struct {
	Destination string `json:"destination"`
	State       string `json:"state"`
	InitOnly    bool   `json:"init_only"`
}

// EncodeRefusals is the report of a refresh that failed only with file
// refusals: each file's destination, state and InitOnly, and no text. Any
// other failure has no report.
func EncodeRefusals(err error) []byte {
	refusals, ok := FileRefusals(err)
	if !ok {
		return nil
	}
	report := refusalReport{Files: make([]reportedFile, len(refusals))}
	for index, refusal := range refusals {
		report.Files[index] = reportedFile{refusal.File.Destination, refusal.File.State(), refusal.File.InitOnly}
	}
	payload, err := json.Marshal(report)
	if err != nil || len(payload) > MaxRefusalReport {
		return nil
	}
	return payload
}

// FileRefusals are the file refusals of err, in order, and true only when err
// consists of nothing else.
func FileRefusals(err error) ([]FileRefusal, bool) {
	if refusal, ok := err.(FileRefusal); ok {
		return []FileRefusal{refusal}, true
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return nil, false
	}
	var refusals []FileRefusal
	for _, cause := range joined.Unwrap() {
		files, ok := FileRefusals(cause)
		if !ok {
			return nil, false
		}
		refusals = append(refusals, files...)
	}
	return refusals, len(refusals) != 0
}

// DecodeRefusals are the file refusals of yard that a child's report names.
// A report that is empty, too large, malformed, with a destination outside a
// developer home or a state outside the vocabulary relays nothing.
func DecodeRefusals(yard string, payload []byte) []error {
	if len(payload) == 0 || len(payload) > MaxRefusalReport {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var report refusalReport
	if decoder.Decode(&report) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return nil
	}
	refusals := make([]error, 0, len(report.Files))
	for _, reported := range report.Files {
		file := File{Destination: reported.Destination, InitOnly: reported.InitOnly}
		switch reported.State {
		case TokenUnreadable:
			file.Token = TokenUnreadable
		case string(ProblemChanged), string(ProblemUnownedInvalid), string(ProblemUnassessed):
			file.Problem = Problem(reported.State)
		default:
			return nil
		}
		developer, _, _ := strings.Cut(strings.TrimPrefix(file.Destination, "/home/"), "/")
		if validDestination(developer, file.Destination) != nil {
			return nil
		}
		refusals = append(refusals, FileRefusal{Yard: yard, File: file})
	}
	return refusals
}
