package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/rpc"
)

// Validate the serialized production projection against this flat response
// schema, including its closed object, required fields and declared bounds.
func TestReleaseReadinessMatchesClosedSchema(t *testing.T) {
	var schema struct {
		Type                 string
		AdditionalProperties *bool
		Required             []string
		Properties           map[string]struct {
			Type      string
			Enum      []string
			MaxLength int
			MaxItems  int
			Items     struct {
				Type                 string
				MinLength, MaxLength int
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), "api/yard-rpc/v1/release-readiness.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Type != "object" || schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		t.Fatal("readiness schema must remain a closed object")
	}
	accepts := func(payload []byte) bool {
		var object map[string]any
		if json.Unmarshal(payload, &object) != nil || object == nil {
			return false
		}
		for _, key := range schema.Required {
			if _, ok := object[key]; !ok {
				return false
			}
		}
		for key, value := range object {
			property, ok := schema.Properties[key]
			if !ok {
				return false
			}
			if property.Type == "array" {
				values, ok := value.([]any)
				if !ok || len(values) > property.MaxItems || property.Items.Type != "string" {
					return false
				}
				for _, value := range values {
					str, ok := value.(string)
					if !ok || utf8.RuneCountInString(str) < property.Items.MinLength || utf8.RuneCountInString(str) > property.Items.MaxLength {
						return false
					}
				}
			} else {
				str, ok := value.(string)
				if !ok {
					return false
				}
				if len(property.Enum) > 0 {
					if !slices.Contains(property.Enum, str) {
						return false
					}
				} else if property.Type != "string" || utf8.RuneCountInString(str) > property.MaxLength {
					return false
				}
			}
		}
		return true
	}
	for _, count := range []int{0, 1, 33, 34, 64} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			warnings := make([]string, count)
			for i := range warnings {
				warnings[i] = fmt.Sprintf("%03d%s", i, strings.Repeat("x", 509))
			}
			source := releasetransition.Outcome{Status: releasetransition.StatusReady, ReachedGoal: true, Active: "release-a", Target: "release-a", Code: releasetransition.CodeReady, Message: "verified", Warnings: warnings}
			if err := source.ValidateInspection(releasetransition.Goal{Target: "release-a", Direction: releasetransition.DirectionActivateTarget}); err != nil {
				t.Fatal("invalid native readiness fixture", err)
			}
			result := releaseReadinessResult(source)
			payload, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if !accepts(payload) {
				t.Fatalf("schema rejects production readiness with %d source warnings", count)
			}
			if len(payload) > rpc.MaxFrameSize-4096 {
				t.Fatal("bounded readiness exceeds owner response budget")
			}
			if count > 33 && (len(result.Warnings) != 33 || !strings.Contains(result.Warnings[32], fmt.Sprintf("%d additional release warnings", count-32))) {
				t.Fatal("overflow warnings lost their inspection guidance", result.Warnings)
			}
			for i, warning := range source.Warnings {
				if warning != fmt.Sprintf("%03d%s", i, strings.Repeat("x", 509)) {
					t.Fatal("status projection mutated a process warning")
				}
			}
			if len(source.Warnings) != count {
				t.Fatal("status projection changed the process outcome")
			}
			// Released Go readers ignore the optional member, preserving core readiness.
			var older struct{ Status, Code, Message, Next string }
			if err := json.Unmarshal(payload, &older); err != nil || older.Status != "ready" || older.Message != "verified" {
				t.Fatal("optional warnings broke an older reader", err)
			}
		})
	}
	rule := schema.Properties["warnings"]
	if rule.Type != "array" || rule.MaxItems != 33 || rule.Items.MinLength != 1 || rule.Items.MaxLength != 512 || slices.Contains(schema.Required, "warnings") {
		t.Fatal("warnings must be optional and bounded to 32 plus one summary")
	}
	for _, payload := range []string{`{"status":"ready","extra":true}`, `{"status":"ready","warnings":[7]}`, `{"warnings":[]}`, `{"status":"invalid"}`,
		`{"status":"ready","warnings":["` + strings.Repeat("x", 513) + `"]}`,
		`{"status":"ready","warnings":[` + strings.TrimSuffix(strings.Repeat(`"warning",`, 34), ",") + `]}`} {
		if accepts([]byte(payload)) {
			t.Fatal("schema admitted an invalid readiness payload")
		}
	}
}
