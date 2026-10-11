package observation_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	producer "github.com/Subyard/Subyard/config/agents/aiobserver/observation"
)

func fixture() producer.Request {
	return producer.Request{Snapshot: `{"config":{},"devices":{}}`, Selected: true, OwnerHost: "127.0.0.1", Service: "owned-hash\nactive\nenabled\n", Installed: false, Provision: []byte("package-v1"),
		Environment: map[string]string{"AI_OBSERVER_HOST_PORT": "22222", "SUBYARD_INTEGRATION_SOURCE_BINDING": strings.Repeat("a", 64), "YARD_KIND": "container", "DEV_USER": "dev", "DEV_UID": "1000", "CODING_TOOL_INTEGRATIONS": "codex aiobserver"},
		Container:   `[{"Name":"/subyard-ai-observer","Id":"container-1","Image":"sha256:image","Config":{"Labels":{"org.subyard.managed":"ai-observer-v1"},"Cmd":["watch","all"],"Env":["A=1","B=2"]},"HostConfig":{"Binds":["/one:/data:rw"],"Future":9007199254740993},"Mounts":[{"Type":"bind","Source":"/one","Destination":"/data","RW":true,"Future":{"nested":[1,2]}},{"Type":"bind","Source":"/two","Destination":"/logs","RW":false}],"State":{"Running":true,"Status":"running","Health":{"Status":"starting"}}}]`}
}
func observe(t *testing.T, request producer.Request) producer.Result {
	t.Helper()
	result, err := producer.Observe(request)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func mutate(t *testing.T, request producer.Request, change func(map[string]any)) producer.Request {
	t.Helper()
	var containers []map[string]any
	decoder := json.NewDecoder(strings.NewReader(request.Container))
	decoder.UseNumber()
	if err := decoder.Decode(&containers); err != nil {
		t.Fatal(err)
	}
	change(containers[0])
	data, err := json.Marshal(containers)
	if err != nil {
		t.Fatal(err)
	}
	request.Container = string(data)
	return request
}
func TestMountsUnorderedFullRecordsAndOtherArraysOrdered(t *testing.T) {
	for _, selected := range []bool{true, false} {
		request := fixture()
		request.Selected = selected
		before := observe(t, request)
		permuted := mutate(t, request, func(c map[string]any) { mounts := c["Mounts"].([]any); mounts[0], mounts[1] = mounts[1], mounts[0] })
		if after := observe(t, permuted); before.Observation != after.Observation {
			t.Fatal("mount enumeration invalidated assessment")
		}
		for _, change := range []func(map[string]any){
			func(c map[string]any) { c["Mounts"] = append(c["Mounts"].([]any), c["Mounts"].([]any)[0]) },
			func(c map[string]any) {
				c["Mounts"].([]any)[0].(map[string]any)["Future"] = map[string]any{"nested": []int{2, 1}}
			},
			func(c map[string]any) { c["Mounts"].([]any)[0].(map[string]any)["Added"] = true },
			func(c map[string]any) { c["HostConfig"].(map[string]any)["Future"] = json.Number("9007199254740992") },
			func(c map[string]any) { c["HostConfig"].(map[string]any)["Additional"] = map[string]any{"x": 1} },
			func(c map[string]any) { c["Config"].(map[string]any)["Cmd"] = []string{"all", "watch"} },
			func(c map[string]any) { c["Config"].(map[string]any)["Env"] = []string{"B=2", "A=1"} },
			func(c map[string]any) { c["Config"].(map[string]any)["Entrypoint"] = []string{"first", "second"} },
			func(c map[string]any) { c["Id"] = "replacement" },
		} {
			after := observe(t, mutate(t, request, change))
			if after.Observation.Actual == before.Observation.Actual || after.Observation.Desired != before.Observation.Desired {
				t.Fatal("protected native change not distinguished from desired")
			}
		}
		health := mutate(t, request, func(c map[string]any) { c["State"].(map[string]any)["Health"] = map[string]any{"Status": "healthy"} })
		if observe(t, health).Observation != before.Observation {
			t.Fatal("advisory health entered stale binding")
		}
	}
}
func TestNativeValidationAndIdentityConstructor(t *testing.T) {
	request := fixture()
	original := []byte(request.Container)
	identity, err := producer.Container(original)
	if err != nil {
		t.Fatal(err)
	}
	original[0] = '!'
	if identity.ID != "container-1" || !identity.Running {
		t.Fatal("mutable native input changed identity")
	}
	for _, test := range []struct {
		change func(map[string]any)
		reason string
	}{
		{func(c map[string]any) { delete(c, "Id") }, "invalid-schema"},
		{func(c map[string]any) { c["State"].(map[string]any)["Running"] = "true" }, "invalid-schema"},
		{func(c map[string]any) { c["Mounts"].([]any)[0] = "not-record" }, "invalid-schema"},
		{func(c map[string]any) { c["Mounts"].([]any)[0].(map[string]any)["RW"] = "true" }, "invalid-schema"},
		{func(c map[string]any) { c["Config"].(map[string]any)["Cmd"] = []any{1} }, "invalid-schema"},
		{func(c map[string]any) {
			c["Config"].(map[string]any)["Labels"] = map[string]any{"org.subyard.managed": "foreign"}
		}, "foreign-ownership"},
	} {
		changed := mutate(t, request, test.change)
		if _, err := producer.Observe(changed); err == nil || producer.Reason(err) != test.reason {
			t.Fatalf("schema/ownership = %v, want %s", err, test.reason)
		}
	}
	for _, value := range []string{`[]`, `[{},{}]`} {
		if _, err := producer.Container([]byte(value)); producer.Reason(err) != "ambiguous-ownership" {
			t.Fatalf("ambiguous = %v", err)
		}
	}
	if _, err := producer.Container([]byte(strings.Replace(request.Container, `9007199254740993`, `1e1000001`, 1))); err == nil {
		t.Fatal("unbounded number accepted")
	}
}
func TestLegacySelectedConfigAndStateStatusMissingNullEquivalence(t *testing.T) {
	for _, selected := range []bool{true, false} {
		request := fixture()
		request.Selected = selected
		for _, field := range []struct{ object, name string }{{"Config", "Entrypoint"}, {"State", "Status"}} {
			missing := mutate(t, request, func(c map[string]any) { delete(c[field.object].(map[string]any), field.name) })
			null := mutate(t, missing, func(c map[string]any) { c[field.object].(map[string]any)[field.name] = nil })
			if observe(t, missing).Observation != observe(t, null).Observation {
				t.Fatalf("legacy %s.%s missing/null equivalence changed", field.object, field.name)
			}
		}
	}
}
func TestLegacyDesiredAndCurrentAbsentSemantics(t *testing.T) {
	request := fixture()
	result := observe(t, request)
	// Characterized against the previous Python v1 NUL recipe and sorted JSON.
	if result.Context != "0f43b81c4e4dc4bd6594466d6c36a81a57358649394d60f4d6d7bbbeb400caf4" || result.Observation.Desired != "49338ed5f1e19ca6c9f465b3f929532d602ad0d1071286af3e6e3e60fc2e2159" {
		t.Fatal("legacy desired/context encoding changed")
	}
	request.Installed = true
	request.Snapshot = `{"config":{"user.subyard.ai_observer_provision":"` + result.Context + `","user.subyard.ai_observer_proxy":"v1:22222"},"devices":{"ai-observer":{"type":"proxy","bind":"host","listen":"tcp:127.0.0.1:22222","connect":"tcp:127.0.0.1:8080"}}}`
	current := observe(t, request)
	if current.Observation.State != "current" || current.Observation.Actual != current.Observation.Desired {
		t.Fatal("current semantics changed")
	}
	// Preserve today's receipt/installation fixed point: unrelated native drift
	// is not promoted to stronger convergence criteria by this type cutover.
	if observe(t, mutate(t, request, func(c map[string]any) { c["Config"].(map[string]any)["Cmd"] = []string{"different"} })).Observation != current.Observation {
		t.Fatal("current convergence strengthened")
	}
	request.Selected = false
	request.Container = "null"
	request.Service = ""
	request.Snapshot = `{"config":{},"devices":{}}`
	absent := observe(t, request)
	if absent.Observation != (producer.RuntimeObservation{State: "absent"}) {
		t.Fatal("absence changed")
	}
	request.Service = "owned-hash\n"
	if observe(t, request).Observation.State != "current" {
		t.Fatal("retired controls should remain current")
	}
	request.Snapshot = `{"config":{},"expanded_config":{"user.subyard.ai_observer_provision":"inherited"},"devices":{}}`
	if _, err := producer.Observe(request); producer.Reason(err) != "ambiguous-ownership" {
		t.Fatalf("inherited ownership = %v", err)
	}
	request.Snapshot = `{"config":null}`
	if _, err := producer.Observe(request); producer.Reason(err) != "invalid-schema" {
		t.Fatalf("invalid instance schema = %v", err)
	}
	if bytes.Contains([]byte(result.Observation.Actual), []byte("9007199254740993")) {
		t.Fatal("native contents leaked")
	}
}
