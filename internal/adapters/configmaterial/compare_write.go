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
	"strings"

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

// FileToken is one observed destination and the files a refused write
// preserved beside it.
type FileToken struct {
	Destination string   `json:"destination"`
	Token       string   `json:"token"`
	Leftovers   []string `json:"leftovers"`
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
		if file.Destination != destinations[index] || !ValidToken(file.Token) && file.Token != TokenUnreadable || !validLeftovers(file.Destination, file.Leftovers) {
			return nil, invalid
		}
	}
	return encoded.Files, nil
}

// Preserved files carry the writer's own temporary names, so they are safe to print.
func validLeftovers(destination string, leftovers []string) bool {
	prefix := path.Dir(destination) + "/." + path.Base(destination) + ".subyard-cas."
	for _, leftover := range leftovers {
		suffix, found := strings.CutPrefix(leftover, prefix)
		if _, err := hex.DecodeString(suffix); !found || len(suffix) != 16 || err != nil || strings.ToLower(suffix) != suffix {
			return false
		}
	}
	return len(leftovers) <= 64
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
// has it. Leftovers are guest files it preserved under temporary names.
type WriteRefusal struct {
	Destination, Outcome string
	Leftovers            []string
}

func (refusal WriteRefusal) Error() string {
	text := map[string]string{
		OutcomeChanged:        "changed after its refresh was planned; the guest's file was kept",
		OutcomeUnreadable:     "is not a readable regular file; it was kept",
		OutcomeMissingConsent: "was not part of the confirmed refresh; it was kept",
		OutcomeLeftover:       "was kept: a refused write preserved guest bytes beside it",
	}[refusal.Outcome]
	message := refusal.Destination + " " + text
	if len(refusal.Leftovers) != 0 {
		message += "; " + LeftoverRecovery(refusal.Destination, refusal.Leftovers)
	}
	return message
}

// LeftoverRecovery is the operator's recovery for preserved guest files. Each
// holds bytes a guest tool wrote while a refresh replaced the destination.
func LeftoverRecovery(destination string, leftovers []string) string {
	return "preserved guest bytes in " + strings.Join(leftovers, ", ") + ": keep them by moving one over " + destination +
		", or delete them; the destination is not refreshed or reported converged until none remain"
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
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	decoder.DisallowUnknownFields()
	if outcome == "" || decoder.Decode(&encoded) != nil || encoded.Outcome != outcome || !validLeftovers(destination, encoded.Leftovers) {
		return errors.New("guest config write failed")
	}
	return WriteRefusal{Destination: destination, Outcome: outcome, Leftovers: encoded.Leftovers}
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
