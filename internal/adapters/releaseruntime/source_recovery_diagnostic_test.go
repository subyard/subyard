package releaseruntime

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

func TestSourceRecoveryHistoryNamesCauseAndForwardRecovery(t *testing.T) {
	for _, eligible := range []bool{false, true} {
		cause := releasetransition.ErrSourceRecoveryHistory
		if !eligible {
			cause = errors.New(cause.Error())
		}
		err := sourceAdmissionError{"V2 history", fmt.Errorf("private native stderr: %w", cause)}
		message := err.Error()
		if strings.Contains(message, `next: yard update --version "$VERSION"`) != eligible || strings.Contains(message, "private native stderr") || strings.Contains(message, "<newer>") {
			t.Fatal(message)
		}
		if !eligible && !strings.Contains(message, "finish pending publications or recovery intent with the exact sealed original owner") {
			t.Fatal(message)
		}
	}
}
