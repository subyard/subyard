package collector

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// A value can be sensitive even when it arrived under an allowed field name.
// Raw identifiers remain usable when they do not match a sensitive value or
// credential format.
var sensitiveValue = regexp.MustCompile(`(?i)[^\s<>@]+@[^\s<>@]+\.[a-z]{2,}|\b(?:bearer|basic)[[:space:]]+[a-z0-9+/_.=-]+|(?:password|passwd|passphrase|api[-_ ]?key|access[-_ ]?token|secret)[[:space:]]*[:=]|-----BEGIN [A-Z ]*PRIVATE KEY-----|\b(?:sk-[a-z0-9_-]+|gh[pousr]_[a-z0-9_]+|github_pat_[a-z0-9_]+|xox[baprs]-[a-z0-9-]+|akia[a-z0-9]{16}|eyJ[a-z0-9_-]+\.[a-z0-9_-]+\.[a-z0-9_-]+)\b`)

var embeddedCredential = regexp.MustCompile(`(?i)\b(?:bearer|basic)[[:space:]]+([a-z0-9+/_.=-]+)|(?:password|passwd|passphrase|api[-_ ]?key|access[-_ ]?token|secret)[[:space:]]*[:=][[:space:]]*([^,[:space:]]+)`)

type valueFilter map[string]struct{}

func (f valueFilter) safe(value string) bool {
	_, sensitive := f[value]
	return !sensitive && len(value) <= 512 && strings.IndexFunc(value, unicode.IsControl) < 0 && !sensitiveValue.MatchString(value)
}

func (f valueFilter) id(value string) bool {
	return legacyID.MatchString(value) && f.safe(value)
}

func isSensitiveField(key string) bool {
	key = strings.ToLower(key)
	fields := strings.FieldsFunc(key, func(r rune) bool { return r == '.' || r == '_' || r == '-' })
	joined := strings.Join(fields, "")
	for _, suffix := range []string{"accountid", "accountuuid", "userid", "useruuid", "organizationid", "organizationuuid",
		"apikey", "accesstoken", "refreshtoken", "idtoken", "authtoken", "privatetoken", "privatekey", "clientsecret"} {
		if strings.HasSuffix(joined, suffix) {
			return true
		}
	}
	switch joined {
	case "account", "useraccount", "proxyauthorization", "prompt", "userprompt", "arguments", "toolinput", "toolparameters", "tooloutput", "output", "response", "responsetext", "errormessage":
		return true
	}
	return slices.ContainsFunc(fields, func(field string) bool {
		return slices.Contains([]string{"email", "password", "passwd", "passphrase", "credential", "credentials", "secret",
			"authorization", "header", "headers", "cookie", "capability", "bearer", "token"}, field)
	}) && !strings.HasSuffix(joined, "tokencount") && !strings.Contains(joined, "tokenusage") && joined != "tokentype"
}

func (f valueFilter) discover(value any, sensitive bool) {
	switch node := value.(type) {
	case map[string]any:
		attr := isSensitiveField(stringValue(node, "key"))
		for key, child := range node {
			if key == "command_line" {
				f.discoverArguments(child)
			}
			if key == "body" {
				if wire, ok := child.(map[string]any); ok {
					f.discover(wire["stringValue"], true)
				}
			}
			f.discover(child, sensitive || key == "_sensitive" || isSensitiveField(key) || (attr && key == "value"))
		}
	case []any:
		for _, child := range node {
			f.discover(child, sensitive)
		}
	case string:
		if sensitive && node != "" {
			f[node] = struct{}{}
			for _, form := range []string{node, unescapeHeader(node)} {
				for _, match := range embeddedCredential.FindAllStringSubmatch(form, -1) {
					f[firstNonempty(match[1], match[2])] = struct{}{}
				}
			}
		}
	case json.Number:
		if sensitive {
			f[string(node)] = struct{}{}
		}
	}
}

func unescapeHeader(value string) string {
	decoded, _ := url.PathUnescape(value)
	return decoded
}

func (f valueFilter) discoverArguments(value any) {
	args, _ := value.([]any)
	for i, raw := range args {
		arg, ok := raw.(string)
		if !ok {
			continue
		}
		key, inline, hasValue := strings.Cut(arg, "=")
		if !strings.HasPrefix(key, "-") || !isSensitiveField(strings.TrimLeft(key, "-")) {
			continue
		}
		if hasValue {
			f.discover(inline, true)
		} else if i+1 < len(args) {
			f.discover(args[i+1], true)
		}
	}
}

func sanitizeRecord(r Record, policy Policy, f valueFilter) (Record, error) {
	if f == nil {
		f = valueFilter{}
	}
	f.discover(r.Body, false)
	clean, removed, err := f.cleanRecord(r)
	if err != nil {
		return Record{}, err
	}
	clean.Redactions = min(max(r.Redactions, 0), 1<<31) + removed
	if clean.hasSessionMetadata() {
		keep, removed := filterSessionRelation(&clean, policy)
		clean.Redactions += removed
		if !keep {
			clean = Record{Schema: RecordSchema, OriginID: clean.OriginID, EventID: clean.EventID, SourceID: clean.SourceID,
				Kind: clean.Kind, Filtered: true, Body: map[string]any{}}
		}
	}
	return clean, nil
}

func sanitizeState(family string, input map[string]any) (map[string]any, error) {
	if stateFields[family] == nil || input == nil {
		return nil, errors.New("invalid telemetry state family")
	}
	f := valueFilter{}
	f.discover(input, false)
	p := wirePolicy{filter: f}
	clean, ok := p.clean(family, input)
	if !ok {
		return nil, errors.New("invalid telemetry state")
	}
	return clean.(map[string]any), nil
}

var coreStateMessages = map[string]string{
	"producer":        "source_id:hex32 origin_id:hex32",
	"producer_policy": "origin_id:hex32 policy_version:capture_version attributes:*attribute_name span_names:*known_span_name event_names:*known_event_name",
	"producer_state":  "producer:producer policy:producer_policy",
	"loss_slot":       "revision:uint count:uint id:hex32 run:id first:time last:time",
	"cutover":         "device:uint inode:uint archive:cutover_archive nonce:hex32 started:time",
	"drain":           "drain:bool",
}

var coreStateRequired = map[string][]string{
	"producer":        {"source_id", "origin_id"},
	"producer_policy": {"origin_id", "policy_version", "attributes", "span_names", "event_names"},
	"producer_state":  {"producer", "policy"},
	"loss_slot":       {"revision"},
	"cutover":         {"device", "inode", "archive", "nonce", "started"},
	"drain":           {"drain"},
}
