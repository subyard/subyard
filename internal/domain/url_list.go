package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// MaxURLListItems bounds a url-list setting.
const MaxURLListItems = 8

var (
	urlListCharacters = regexp.MustCompile(`^[A-Za-z0-9._~:/@%+-]+$`)
	scpLikeURL        = regexp.MustCompile(`^([A-Za-z0-9._-]+)@([A-Za-z0-9.-]+):([^/:][^:]*|/[^:]+)$`)
	urlHost           = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)
	// Go regexp has no lookahead: a '%' followed by fewer than two hex digits.
	invalidPercentEscape = regexp.MustCompile(`%([^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)`)
)

// ValidateURLList checks a space-, tab- or newline-separated list of remote Git URLs:
// https://host/path, ssh://[user@]host[:port]/path or user@host:path. Refusals
// name items by position only, so a refused value with credentials is never echoed.
func ValidateURLList(value string) error {
	items := strings.Fields(value)
	causes := []error{}
	// The yard splits the list on space, tab and newline only; any other
	// separator Fields would accept is refused, so both sides see the same items.
	if strings.IndexFunc(value, func(r rune) bool {
		return (unicode.IsSpace(r) || unicode.IsControl(r)) && !strings.ContainsRune(" \t\n", r)
	}) >= 0 {
		causes = append(causes, errors.New("value contains a control or space character other than space, tab or newline"))
	}
	if len(items) > MaxURLListItems {
		causes = append(causes, fmt.Errorf("has more than %d URLs", MaxURLListItems))
	}
	seen := map[string]int{}
	for index, item := range items {
		normalized, err := NormalizeRemoteURL(item)
		if err != nil {
			causes = append(causes, fmt.Errorf("URL %d %w", index+1, err))
			continue
		}
		if first, ok := seen[normalized]; ok {
			causes = append(causes, fmt.Errorf("URL %d repeats URL %d", index+1, first))
			continue
		}
		seen[normalized] = index + 1
	}
	return CollectRefusals(causes...)
}

// NormalizeRemoteURL returns the comparison form of one remote Git URL: lowercase
// scheme and host, no default port, no trailing slash and no single ".git" suffix.
func NormalizeRemoteURL(item string) (string, error) {
	if strings.HasPrefix(item, "-") {
		return "", errors.New("starts with '-'")
	}
	if strings.ContainsAny(item, "?#") {
		return "", errors.New("has a query or fragment")
	}
	if !urlListCharacters.MatchString(item) {
		return "", errors.New("contains unsupported characters")
	}
	if match := scpLikeURL.FindStringSubmatch(item); match != nil && !strings.Contains(item, "://") {
		if !urlHost.MatchString(match[2]) {
			return "", errors.New("has an invalid host")
		}
		path := trimRepositoryPath(match[3])
		if path == "" || path == "/" {
			return "", errors.New("has no repository path")
		}
		return match[1] + "@" + strings.ToLower(match[2]) + ":" + path, nil
	}
	scheme, rest, found := strings.Cut(item, "://")
	if !found {
		return "", errors.New("is not an https, ssh or user@host:path URL")
	}
	scheme = strings.ToLower(scheme)
	defaultPort, supported := map[string]int{"https": 443, "ssh": 22}[scheme]
	if !supported {
		return "", errors.New("uses an unsupported scheme")
	}
	// The authority is parsed by hand, exactly as the guest sync helper does:
	// no unescaping, so a percent-encoded user or host is never reinterpreted.
	authority, path := rest, ""
	if slash := strings.Index(rest, "/"); slash >= 0 {
		authority, path = rest[:slash], rest[slash:]
	}
	user, hostPort, hasUser := "", authority, false
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		user, hostPort, hasUser = authority[:at], authority[at+1:], true
		if user == "" || strings.Contains(user, ":") || scheme == "https" {
			return "", errors.New("contains credentials")
		}
	}
	host, rawPort, hasPort := strings.Cut(hostPort, ":")
	host = strings.ToLower(host)
	if !urlHost.MatchString(host) {
		return "", errors.New("has an invalid host")
	}
	port := defaultPort
	if hasPort {
		number, err := strconv.Atoi(rawPort)
		if rawPort == "" || strings.Trim(rawPort, "0123456789") != "" || err != nil || number < 1 || number > 65535 {
			return "", errors.New("has an invalid port")
		}
		port = number
	}
	if invalidPercentEscape.MatchString(path) {
		return "", errors.New("has an invalid percent escape")
	}
	path = trimRepositoryPath(path)
	if path == "" || path == "/" {
		return "", errors.New("has no repository path")
	}
	if port != defaultPort {
		host += ":" + strconv.Itoa(port)
	}
	if hasUser {
		host = user + "@" + host
	}
	return scheme + "://" + host + path, nil
}

func trimRepositoryPath(path string) string {
	return strings.TrimSuffix(strings.TrimRight(path, "/"), ".git")
}
