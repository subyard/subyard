package yardnetwork

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/resource"
)

func TestApprovedIngressReadsLegacyFingerprintWithoutPersistingIt(t *testing.T) {
	approved := ApprovedIngress{Device: "sample-relay", Listen: "udp:10.20.30.40:42000", GuestPort: 41999}
	binding := Binding{Yard: Yard{Name: "private", Project: "subyard-private", Instance: "yard-private", Network: "incusbr0"},
		IPv4: "10.80.0.10", MAC: "00:16:3e:00:00:10", OriginalNIC: map[string]string{"type": "nic", "network": "incusbr0"},
		ApprovedIngress: []ApprovedIngress{approved}}
	policy := Policy{Schema: SchemaVersion, Bindings: []Binding{binding}}
	content, err := Encode(policy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "fingerprint") {
		t.Fatal("new policy persisted a derived fingerprint")
	}
	legacyHash := (resource.ProxyContract{}).OwnershipValue(approved.proxy(binding))
	for _, test := range []struct {
		name, extra string
		valid       bool
	}{
		{"current", "", true},
		{"legacy", `,"fingerprint":` + strconv.Quote(legacyHash), true},
		{"wrong legacy hash", `,"fingerprint":"v1:wrong"`, false},
		{"empty legacy hash", `,"fingerprint":""`, false},
		{"null legacy hash", `,"fingerprint":null`, false},
		{"non-string legacy hash", `,"fingerprint":42`, false},
		{"unknown ingress field", `,"unexpected":true`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := strings.Replace(string(content), `"guestPort":41999`, `"guestPort":41999`+test.extra, 1)
			decoded, err := Decode([]byte(input))
			if !test.valid {
				if err == nil {
					t.Fatal("malformed approval was accepted")
				}
				return
			}
			if err != nil || !reflect.DeepEqual(decoded, policy) {
				t.Fatalf("approval changed during decode: %+v, %v", decoded, err)
			}
			rewritten, err := Encode(decoded)
			if err != nil || string(rewritten) != string(content) {
				t.Fatalf("policy rewrite did not use canonical route parameters: %s, %v", rewritten, err)
			}
		})
	}
}

func TestLinksAreSymmetricExplicitAndSurviveIsolationToggle(t *testing.T) {
	policy, err := Decode(nil)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Isolation {
		t.Fatal("an unconfigured host must keep isolation disabled")
	}
	for _, pair := range [][2]string{{"alpha", "beta"}, {"beta", "gamma"}, {"beta", "alpha"}} {
		if err := policy.Link(pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}
	if peers := policy.Peers("alpha"); !reflect.DeepEqual(peers, []string{"beta"}) {
		t.Fatalf("alpha has implicit or missing peers: %v", peers)
	}
	if peers := policy.Peers("beta"); !reflect.DeepEqual(peers, []string{"alpha", "gamma"}) {
		t.Fatalf("links are not bidirectional: %v", peers)
	}
	if len(policy.Links) != 2 {
		t.Fatalf("reversed duplicate created another link: %+v", policy.Links)
	}
	for _, enabled := range []bool{true, false, true} {
		policy.Isolation = enabled
		content, err := Encode(policy)
		if err != nil {
			t.Fatal(err)
		}
		policy, err = Decode(content)
		if err != nil {
			t.Fatal(err)
		}
		if policy.Isolation != enabled || len(policy.Links) != 2 {
			t.Fatalf("mode change lost policy: %+v", policy)
		}
	}
	policy.Unlink("beta", "alpha")
	if len(policy.Peers("alpha")) != 0 || !reflect.DeepEqual(policy.Peers("beta"), []string{"gamma"}) {
		t.Fatalf("unlink removed the wrong pair: %+v", policy.Links)
	}
}

func TestMalformedPolicyFailsClosed(t *testing.T) {
	for _, content := range []string{
		`{}`, `null`, `{"schema":2}`, `{"schema":1,"unknown":true}`,
		`{"schema":1,"links":[{"a":"alpha","b":"alpha"}]}`,
		`{"schema":1,"links":[{"a":"../alpha","b":"beta"}]}`,
		`{"schema":1,"links":[{"a":"alpha","b":"beta"},{"a":"beta","b":"alpha"}]}`,
		`{"schema":1,"revision":1,"appliedRevision":2}`,
		`{"schema":1} {"schema":1}`,
		`{"schema":1,"pendingStart":["alpha"],"removing":["alpha"]}`,
	} {
		t.Run(content, func(t *testing.T) {
			if _, err := Decode([]byte(content)); err == nil {
				t.Fatalf("accepted malformed policy %s", content)
			}
		})
	}
}

func TestDeletingEndpointRemovesOnlyItsLinks(t *testing.T) {
	policy, _ := Decode(nil)
	for _, pair := range [][2]string{{"alpha", "beta"}, {"alpha", "gamma"}, {"beta", "gamma"}} {
		if err := policy.Link(pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}
	policy.Forget("alpha")
	if len(policy.Links) != 1 || !reflect.DeepEqual(policy.Peers("beta"), []string{"gamma"}) {
		t.Fatalf("endpoint removal damaged unrelated links: %+v", policy.Links)
	}
}

func TestStoredPolicyRejectsCollidingBindings(t *testing.T) {
	binding := func(name, project, instance, address, mac string) string {
		return `{"yard":{"name":"` + name + `","project":"` + project + `","instance":"` + instance + `","network":"incusbr0"},` +
			`"ipv4":"` + address + `","mac":"` + mac + `","originalNic":{"type":"nic","network":"incusbr0","name":"eth0"},"originalAccess":""}`
	}
	for _, test := range []struct {
		name  string
		first string
		other string
	}{
		{
			name:  "instance identity",
			first: binding("a", "project-a", "yard-a", "10.80.0.10", "00:16:3e:00:00:10"),
			other: binding("b", "project-a", "yard-a", "10.80.0.11", "00:16:3e:00:00:11"),
		},
		{
			name:  "IPv4 on same bridge",
			first: binding("a", "project-a", "yard-a", "10.80.0.10", "00:16:3e:00:00:10"),
			other: binding("b", "project-b", "yard-b", "10.80.0.10", "00:16:3e:00:00:11"),
		},
		{
			name:  "MAC on same bridge",
			first: binding("a", "project-a", "yard-a", "10.80.0.10", "00:16:3e:00:00:10"),
			other: binding("b", "project-b", "yard-b", "10.80.0.11", "00-16-3e-00-00-10"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := `{"schema":1,"isolation":true,"revision":1,"appliedRevision":1,"appliedIsolation":true,"bindings":[` +
				test.first + `,` + test.other + `]}`
			if _, err := Decode([]byte(content)); err == nil {
				t.Fatal("colliding stored bindings were accepted")
			}
		})
	}
}
