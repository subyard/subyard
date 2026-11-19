package configmaterial

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
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

	// Write outcomes that leave the destination as the guest has it.
	OutcomeChanged        = "changed"
	OutcomeUnreadable     = "unreadable"
	OutcomeMissingConsent = "missing-consent"
	// OutcomeLeftover is a destination beside guest bytes that an earlier
	// refused write preserved: nothing is written until the operator
	// resolves them.
	OutcomeLeftover = "leftover"
)

var tokenPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ValidToken reports a token a writer may compare with.
func ValidToken(token string) bool { return token == TokenAbsent || tokenPattern.MatchString(token) }

// PayloadToken is the token of exact bytes.
func PayloadToken(payload []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(payload))
}

// FileToken is one observed destination and the files that writes left
// beside it under the writer's temporary names, with the token of each.
type FileToken struct {
	Destination    string   `json:"destination"`
	Token          string   `json:"token"`
	Leftovers      []string `json:"leftovers"`
	LeftoverTokens []string `json:"leftover_tokens,omitempty"`
	unlisted       int
}

// leftoversListed bounds the leftovers named per destination.
const leftoversListed = 64

// Leftovers are the files that writes left beside one destination. Staged
// hold an unpublished copy of the refreshed file, left by a write stopped
// before it exchanged; Kept hold guest bytes that a write displaced or could
// not verify. Unlisted counts those that could not be named.
type Leftovers struct {
	Kept, Staged []string
	Unlisted     int
}

// Any reports whether a leftover blocks the destination.
func (leftovers Leftovers) Any() bool {
	return len(leftovers.Kept)+len(leftovers.Staged)+leftovers.Unlisted != 0
}

// Recovery is the operator's step for the leftovers beside destination.
func (leftovers Leftovers) Recovery(destination string) string {
	var parts []string
	if len(leftovers.Kept) != 0 {
		parts = append(parts, "preserved guest bytes in "+strings.Join(leftovers.Kept, ", ")+": keep them by moving one over "+destination+", or delete them")
	}
	if len(leftovers.Staged) != 0 {
		parts = append(parts, "an interrupted write left its unpublished copy of the refreshed file in "+strings.Join(leftovers.Staged, ", ")+": delete it")
	}
	if leftovers.Unlisted != 0 {
		parts = append(parts, fmt.Sprintf("%d more files named %s beside it could not be listed: inspect and remove them", leftovers.Unlisted, path.Join(path.Dir(destination), "."+path.Base(destination)+".subyard-cas.*")))
	}
	return strings.Join(parts, "; ") + "; the destination is not refreshed or reported converged until none remain"
}

// LeftoversFor classifies the observed leftovers: one holding desired, the
// token of the refreshed payload when the caller knows it, is a staged copy.
func (file FileToken) LeftoversFor(desired string) Leftovers {
	leftovers := Leftovers{Unlisted: file.unlisted}
	for index, name := range file.Leftovers {
		if desired != "" && index < len(file.LeftoverTokens) && file.LeftoverTokens[index] == desired {
			leftovers.Staged = append(leftovers.Staged, name)
		} else {
			leftovers.Kept = append(leftovers.Kept, name)
		}
	}
	return leftovers
}

// listLeftovers keeps the leftovers named with the writer's own temporary
// names, which are safe to print, up to the bound; the rest are counted.
func listLeftovers(destination string, names, tokens []string) ([]string, []string, int) {
	if len(tokens) != len(names) {
		tokens = nil
	}
	prefix := path.Dir(destination) + "/." + path.Base(destination) + ".subyard-cas."
	var listed, listedTokens []string
	unlisted := 0
	for index, name := range names {
		suffix, found := strings.CutPrefix(name, prefix)
		if _, err := hex.DecodeString(suffix); !found || len(suffix) != 16 || err != nil || strings.ToLower(suffix) != suffix || len(listed) == leftoversListed {
			unlisted++
			continue
		}
		listed = append(listed, name)
		if tokens != nil {
			listedTokens = append(listedTokens, tokens[index])
		}
	}
	return listed, listedTokens, unlisted
}

func casProgram() string { return guestCASProgram + "\ncas_main()\n" }

// CompareWriteProgram is the exact guest program of token observation and raw writes.
func CompareWriteProgram() string { return casProgram() }

func validDestination(developer, destination string) error {
	home := "/home/" + developer
	if !domain.SafeName(developer) || path.Clean(destination) != destination || !strings.HasPrefix(destination, home+"/") {
		return errors.New("guest config destination leaves the developer home")
	}
	return nil
}

// ObserveTokensRequest reads the tokens of a yard's destinations in one exec.
func ObserveTokensRequest(developer string, destinations []string) (ports.InstanceExecRequest, error) {
	command := []string{"python3", "-B", "-c", casProgram(), "observe", "/home/" + developer}
	for _, destination := range destinations {
		if err := validDestination(developer, destination); err != nil {
			return ports.InstanceExecRequest{}, err
		}
		command = append(command, destination)
	}
	return ports.InstanceExecRequest{Command: command}, nil
}

// ParseTokens accepts exactly one valid observation per requested destination.
func ParseTokens(payload []byte, destinations []string) ([]FileToken, error) {
	var encoded struct {
		Files []FileToken `json:"files"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	invalid := errors.New("invalid guest config token observation")
	if err := decoder.Decode(&encoded); err != nil || decoder.Decode(&struct{}{}) != io.EOF || len(encoded.Files) != len(destinations) {
		return nil, invalid
	}
	for index, file := range encoded.Files {
		if file.Destination != destinations[index] || !ValidToken(file.Token) && file.Token != TokenUnreadable {
			return nil, invalid
		}
		// A leftover that cannot be named still blocks the destination.
		file.Leftovers, file.LeftoverTokens, file.unlisted = listLeftovers(file.Destination, file.Leftovers, file.LeftoverTokens)
		encoded.Files[index] = file
	}
	return encoded.Files, nil
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
		Command: []string{"python3", "-B", "-c", casProgram(), "write", "/home/" + developer, destination,
			fmt.Sprint(uid), expect, fmt.Sprintf("%x", sha256.Sum256(payload))},
		Stdin: payload,
	}, nil
}

// WriteRefusal is a compare-and-write that left its destination as the guest
// has it, with the files left beside it under temporary names.
type WriteRefusal struct {
	Destination, Outcome string
	Leftovers            Leftovers
}

func (refusal WriteRefusal) Error() string {
	text := map[string]string{
		OutcomeChanged:        "changed after its refresh was planned; the guest's file was kept",
		OutcomeUnreadable:     "is not a readable regular file; it was kept",
		OutcomeMissingConsent: "was not part of the confirmed refresh; it was kept",
		OutcomeLeftover:       "was kept: an earlier write left files beside it",
	}[refusal.Outcome]
	message := refusal.Destination + " " + text
	if refusal.Leftovers.Any() {
		message += "; " + refusal.Leftovers.Recovery(refusal.Destination)
	}
	return message
}

// ParseWriteResult maps a compare-and-write exit to nil, a WriteRefusal or a failure.
func ParseWriteResult(destination string, exitCode int, stdout []byte) error {
	outcome := map[int]string{3: OutcomeUnreadable, 4: OutcomeChanged, 5: OutcomeLeftover}[exitCode]
	if exitCode == 0 {
		return nil
	}
	var encoded struct {
		Outcome   string   `json:"outcome"`
		Leftovers []string `json:"leftovers"`
		Staged    []string `json:"staged"`
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	decoder.DisallowUnknownFields()
	if outcome == "" || decoder.Decode(&encoded) != nil || encoded.Outcome != outcome {
		return errors.New("guest config write failed")
	}
	// The writer knows its payload, so it names its own staged copies.
	tokens := make([]string, len(encoded.Leftovers))
	for index, name := range encoded.Leftovers {
		if slices.Contains(encoded.Staged, name) {
			tokens[index] = "staged"
		}
	}
	listed, listedTokens, unlisted := listLeftovers(destination, encoded.Leftovers, tokens)
	return WriteRefusal{Destination: destination, Outcome: outcome,
		Leftovers: FileToken{Leftovers: listed, LeftoverTokens: listedTokens, unlisted: unlisted}.LeftoversFor("staged")}
}

// ObserveTokens reads the tokens of one yard's destinations in one exec.
func ObserveTokens(ctx context.Context, executor ports.InstanceExecutor, project, instance, developer string, destinations []string) ([]FileToken, error) {
	if len(destinations) == 0 {
		return nil, nil
	}
	request, err := ObserveTokensRequest(developer, destinations)
	if err != nil {
		return nil, err
	}
	result, err := executor.Exec(ctx, project, instance, request)
	var completed *ports.GuestExitError
	if err != nil && !errors.As(err, &completed) {
		// A transport failure or cancellation stays itself.
		return nil, fmt.Errorf("guest config files could not be observed: %w", err)
	}
	if err != nil || result.ExitCode != 0 {
		return nil, errors.New("guest config files could not be observed")
	}
	return ParseTokens(result.Stdout, destinations)
}

// WriteResult classifies one compare-and-write exec; a transport failure stays itself.
func WriteResult(destination string, result ports.InstanceExecResult, err error) error {
	var completed *ports.GuestExitError
	if err != nil && (!errors.As(err, &completed) || completed.Code != result.ExitCode || result.ExitCode == 0) {
		return err
	}
	return ParseWriteResult(destination, result.ExitCode, result.Stdout)
}

// AssessFile is the one convergence check of a guest config file, shared by
// the runtime's init, config status and release readiness. A raw file
// converges when its token is the desired bytes' token, an owned file when
// its structured observation, owned, converges. Neither converges while it
// is unreadable or a leftover remains beside it.
func AssessFile(desired config.DesiredMaterializedArtifact, token FileToken, owned *Observation) (config.MaterializedArtifactAssessment, error) {
	observed := config.MaterializationObservation{ExitCode: 1}
	if digest, ok := strings.CutPrefix(token.Token, "sha256:"); ok {
		observed = config.MaterializationObservation{Output: []byte(digest + "  " + desired.Destination + "\n")}
	}
	if owned != nil {
		observed = config.MaterializationObservation{Fingerprint: owned.Fingerprint, OwnedConverged: owned.Converged}
	}
	assessed, err := config.AssessMaterializedArtifact(desired, observed)
	if err == nil && (token.Token == TokenUnreadable || len(token.Leftovers) != 0 || token.unlisted != 0) {
		assessed.Converged = false
	}
	return assessed, err
}
