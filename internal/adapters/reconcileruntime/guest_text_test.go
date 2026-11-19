package reconcileruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const guestCanary = "SUBYARD-CANARY-7f3a"

func decodedGuestText(t *testing.T, value string) guestText {
	t.Helper()
	var text guestText
	payload, _ := json.Marshal(value)
	if err := json.Unmarshal(payload, &text); err != nil {
		t.Fatal(err)
	}
	return text
}

func TestGuestTextNeverRendersItsValue(t *testing.T) {
	text := decodedGuestText(t, "/home/dev/"+guestCanary)
	type exported struct{ Path guestText }
	type unexported struct{ path guestText }
	values := []any{text, &text, exported{text}, &exported{text}, unexported{text}, []guestText{text},
		map[string]guestText{"key": text}, struct{ Items []exported }{[]exported{{text}}}}
	var rendered []string
	for _, value := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T"} {
			rendered = append(rendered, fmt.Sprintf(verb, value))
		}
		rendered = append(rendered, fmt.Sprint(value), fmt.Sprintln(value), errors.New(fmt.Sprint(value)).Error(), fmt.Errorf("%w: %v", errors.New("x"), value).Error())
		payload, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		rendered = append(rendered, string(payload))
	}
	for _, line := range rendered {
		if strings.Contains(line, guestCanary) {
			t.Fatalf("guest text rendered: %s", line)
		}
	}
	other := decodedGuestText(t, "/home/dev/other")
	first, _ := json.Marshal(text)
	second, _ := json.Marshal(other)
	if string(first) == string(second) || !strings.HasPrefix(string(first), `"guest-text sha256:`) {
		t.Fatalf("binding encodings %s and %s", first, second)
	}
	if !text.Matches("/home/dev/"+guestCanary) || text.Matches("/home/dev") || !(guestText{}).Matches("") {
		t.Fatal("exact matching changed")
	}
	if declared, ok := matchDeclared(text, "/home/dev/x", "/home/dev/"+guestCanary); !ok || declared != "/home/dev/"+guestCanary {
		t.Fatalf("matchDeclared = %q, %t", declared, ok)
	}
	if !text.below("/home/dev") || text.below("/home/de") || decodedGuestText(t, "/home/dev/../etc").below("/home/dev") {
		t.Fatal("below changed")
	}
}

func TestGuestTextRefusesNonStringsAndOversizedValues(t *testing.T) {
	for _, payload := range []string{`1`, `{}`, `["x"]`, `"` + strings.Repeat("x", guestTextLimit+1) + `"`} {
		var text guestText
		if json.Unmarshal([]byte(payload), &text) == nil {
			t.Fatalf("accepted %.40s", payload)
		}
	}
	var text guestText
	if err := json.Unmarshal([]byte(`null`), &text); err != nil || text.raw != nil {
		t.Fatalf("null = %v", err)
	}
}
