package domain

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// MaxURLListItems bounds a url-list setting.
const MaxURLListItems = 8

var (
	urlListCharacters = regexp.MustCompile(`^[A-Za-z0-9._~:/@%+-]+$`)
	scpLikeURL        = regexp.MustCompile(`^([A-Za-z0-9._-]+)@([A-Za-z0-9.-]+):([^/:][^:]*|/[^:]+)$`)
	urlHost           = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)
)

// ValidateURLList checks a whitespace-separated list of remote Git URLs:
// https://host/path, ssh://[user@]host[:port]/path or user@host:path. Refusals
// name items by position only, so a refused value with credentials is never echoed.
func ValidateURLList(value string) error {
	items := strings.Fields(value)
	causes := []error{}
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
	return CollectOrderedRefusals(causes...)
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
	parsed, err := url.Parse(item)
	if err != nil || !strings.Contains(item, "://") {
		return "", errors.New("is not an https, ssh or user@host:path URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	defaultPort := map[string]string{"https": "443", "ssh": "22"}[scheme]
	if defaultPort == "" {
		return "", errors.New("uses an unsupported scheme")
	}
	if parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword || scheme == "https" || parsed.User.Username() == "" {
			return "", errors.New("contains credentials")
		}
	}
	host, port := strings.ToLower(parsed.Hostname()), parsed.Port()
	if !urlHost.MatchString(host) {
		return "", errors.New("has an invalid host")
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return "", errors.New("has an invalid port")
	}
	path := trimRepositoryPath(parsed.EscapedPath())
	if path == "" || path == "/" {
		return "", errors.New("has no repository path")
	}
	authority := host
	if port != "" && port != defaultPort {
		authority += ":" + port
	}
	if parsed.User != nil {
		authority = parsed.User.Username() + "@" + authority
	}
	return scheme + "://" + authority + path, nil
}

func trimRepositoryPath(path string) string {
	return strings.TrimSuffix(strings.TrimRight(path, "/"), ".git")
}
