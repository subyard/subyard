package resource

import (
	"strings"
	"testing"
)

func TestStructuredPrepareContract(t *testing.T) {
	registry, actions := testActionRegistry(t)
	valid := `{"schema":"yard.resource-action-assessment.v2","action":"destroy","changed":true,"consequences":["remove runtime"],"binding":"` + strings.Repeat("a", 64) + `","steps":[{"id":"stop","target":"sample/service","observed":"running","desired":"stopped","decision":"apply","preconditions":["owned runtime"],"dependsOn":[],"verify":"runtime stopped"},{"id":"remove","target":"sample/service","observed":"unknown","desired":"absent","decision":"conditional","preconditions":["owned runtime only"],"dependsOn":["stop"],"verify":"owned runtime absent"}]}`
	result, err := registry.PrepareResult(actions, "svc", "destroy", []byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Steps) != 2 || result.Assessment.Action != "resource.sample.service.destroy" || result.Binding != strings.Repeat("a", 64) {
		t.Fatalf("unexpected result: %#v", result)
	}
	cases := map[string]string{
		"duplicate nested":      strings.Replace(valid, `"target":"sample/service"`, `"target":"sample/service","target":"other"`, 1),
		"unknown nested":        strings.Replace(valid, `"desired":"stopped"`, `"desired":"stopped","command":"rm"`, 1),
		"forward dependency":    strings.Replace(valid, `"dependsOn":[]`, `"dependsOn":["remove"]`, 1),
		"conditional unbounded": strings.Replace(valid, `"preconditions":["owned runtime only"]`, `"preconditions":[]`, 1),
		"empty binding":         strings.Replace(valid, strings.Repeat("a", 64), "", 1),
		"null binding":          strings.Replace(valid, `"binding":"`+strings.Repeat("a", 64)+`"`, `"binding":null`, 1),
		"uppercase binding":     strings.Replace(valid, strings.Repeat("a", 64), strings.Repeat("A", 64), 1),
		"control fact":          strings.Replace(valid, `"observed":"running"`, `"observed":"run\nning"`, 1),
		"duplicate ID":          strings.Replace(valid, `"id":"remove"`, `"id":"stop"`, 1),
		"bad binding":           strings.Replace(valid, strings.Repeat("a", 64), "secret", 1),
		"protected target":      strings.Replace(valid, "sample/service", "/run/secrets/token", 1),
		"changed contradiction": strings.Replace(valid, `"changed":true`, `"changed":false`, 1),
		"v1 extension":          strings.Replace(valid, "assessment.v2", "assessment.v1", 1),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := registry.PrepareResult(actions, "svc", "destroy", []byte(payload)); err == nil {
				t.Fatal("invalid assessment accepted")
			}
		})
	}
	for _, field := range []string{`"binding":""`, `"steps":null`} {
		payload := `{"schema":"yard.resource-action-assessment.v1","action":"destroy","changed":true,"consequences":["remove runtime"],` + field + `}`
		if _, err := registry.AssessPrepareResult(actions, "svc", "destroy", []byte(payload)); err == nil {
			t.Fatal("v1 extension accepted")
		}
	}
}

func TestStructuredPrepareLimitsAndExternalConditional(t *testing.T) {
	registry, actions := testActionRegistry(t)
	step := `{"id":"effect","target":"sample/service","observed":"unknown","desired":"stopped","decision":"conditional","preconditions":["owned target becomes observable"],"dependsOn":[],"verify":"runtime stopped"}`
	payload := `{"schema":"yard.resource-action-assessment.v2","action":"destroy","changed":true,"consequences":["remove runtime"],"steps":[` + step + `]}`
	if _, err := registry.PrepareResult(actions, "svc", "destroy", []byte(payload)); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.PrepareResult(actions, "svc", "destroy", []byte(strings.Replace(payload, "sample/service", strings.Repeat("s", 513), 1))); err == nil {
		t.Fatal("oversized target accepted")
	}
	steps := make([]string, 65)
	for i := range steps {
		steps[i] = step
	}
	if _, err := registry.PrepareResult(actions, "svc", "destroy", []byte(strings.Replace(payload, step, strings.Join(steps, ","), 1))); err == nil {
		t.Fatal("oversized step set accepted")
	}
}
