package reconcileruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// IntegrationCleanupPlan binds a profile-owned cleanup to its source and guest
// observation. Cleanup never changes the configured integration selection.
type IntegrationCleanupPlan struct {
	Fingerprint string
	Changed     bool
	Steps       []string
	hookState   string
	script      []byte
}

func (runtime Runtime) IntegrationCleanupPlan(ctx context.Context, id string) (IntegrationCleanupPlan, error) {
	var plan IntegrationCleanupPlan
	if !domain.SafeName(id) {
		return plan, errors.New("invalid integration ID")
	}
	if slices.Contains(strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")), id) {
		return plan, fmt.Errorf("integration %s is still selected or required by another integration; disable it in persistent configuration before cleanup", id)
	}
	path := runtime.environmentValue("AGENT_" + id + "_CLEANUP")
	if path == "" {
		return plan, fmt.Errorf("integration %s does not declare a cleanup handler (AGENT_%s_CLEANUP); inspect its remaining installation manually", id, id)
	}
	if _, err := runtime.integrationPrecondition(ctx); err != nil {
		return plan, err
	}
	script, err := (guestConfigFile{source: path}).readSource()
	if err != nil {
		return plan, fmt.Errorf("read integration %s cleanup handler: %w", id, err)
	}
	result, err := runtime.executeIntegrationCleanup(ctx, id, script, "observe", "")
	if err != nil {
		return plan, err
	}
	var observed struct {
		Fingerprint string   `json:"fingerprint"`
		Changed     *bool    `json:"changed"`
		Steps       []string `json:"steps"`
	}
	if err := decodeCleanupReport(result.Stdout, &observed); err != nil || observed.Changed == nil ||
		len(observed.Fingerprint) != 64 || strings.Trim(observed.Fingerprint, "0123456789abcdef") != "" ||
		len(observed.Steps) > 64 {
		return plan, fmt.Errorf("integration %s cleanup handler returned an invalid observation", id)
	}
	for _, step := range observed.Steps {
		if !cleanupPublicText(step) {
			return plan, fmt.Errorf("integration %s cleanup handler returned invalid consequences", id)
		}
	}
	if *observed.Changed && len(observed.Steps) == 0 {
		return plan, fmt.Errorf("integration %s cleanup handler omitted its consequences", id)
	}
	identity, _ := json.Marshal([]string{id, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName,
		runtime.devUser(), strconv.Itoa(runtime.Yard.DevUID), fmt.Sprintf("%x", sha256.Sum256(script)),
		observed.Fingerprint, strconv.FormatBool(*observed.Changed), strings.Join(observed.Steps, "\n")})
	return IntegrationCleanupPlan{Fingerprint: fmt.Sprintf("%x", sha256.Sum256(identity)), Changed: *observed.Changed,
		Steps: observed.Steps, hookState: observed.Fingerprint, script: script}, nil
}

func (runtime Runtime) ApplyIntegrationCleanup(ctx context.Context, id string, plan IntegrationCleanupPlan) error {
	fresh, err := runtime.IntegrationCleanupPlan(ctx, id)
	if err != nil {
		return err
	}
	if fresh.Fingerprint != plan.Fingerprint || fresh.Changed != plan.Changed {
		return fmt.Errorf("%w: integration %s cleanup changed; assess it again", domain.ErrPlanStale, id)
	}
	if !fresh.Changed {
		return nil
	}
	if _, err := runtime.executeIntegrationCleanup(ctx, id, fresh.script, "apply", fresh.hookState); err != nil {
		return err
	}
	verified, err := runtime.IntegrationCleanupPlan(ctx, id)
	if err != nil {
		return err
	}
	if verified.Changed {
		return fmt.Errorf("integration %s cleanup remains pending; inspect its cleanup plan and retry", id)
	}
	return nil
}

func (runtime Runtime) executeIntegrationCleanup(ctx context.Context, id string, script []byte, mode, expected string) (ports.InstanceExecResult, error) {
	uid := runtime.Yard.DevUID
	if uid <= 0 {
		uid = 1000
	}
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName,
		ports.InstanceExecRequest{Command: []string{"sh", "-eu", "-s", "--", mode, runtime.devUser(), strconv.Itoa(uid), expected}, Stdin: script})
	if result.ExitCode != 0 {
		var report struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if decodeCleanupReport(result.Stdout, &report) == nil && domain.SafeName(report.Code) && cleanupPublicText(report.Message) {
			return result, fmt.Errorf("integration %s cleanup: %s: %s", id, report.Code, report.Message)
		}
		return result, fmt.Errorf("integration %s cleanup %s failed without a valid diagnostic; artifacts may require inspection before retrying", id, mode)
	}
	if err != nil {
		return result, fmt.Errorf("integration %s cleanup %s could not be observed or completed", id, mode)
	}
	return result, nil
}

func decodeCleanupReport(payload []byte, target any) error {
	if len(payload) > 64*1024 {
		return errors.New("cleanup report exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("cleanup report contains trailing data")
	}
	return nil
}

func cleanupPublicText(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 1024 && strings.IndexFunc(value, unicode.IsControl) < 0
}
