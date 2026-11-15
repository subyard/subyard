package releaseruntime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

const latestReleaseURL = "https://github.com/Subyard/Subyard/releases/latest"
const stableReleaseURL = "https://github.com/Subyard/Subyard/releases/tag/v1.2.3"

// Both discovery paths must accept the same stable tags and name HTTP refusals.
var releaseLookupCases = []struct {
	name, location string
	status         int
	headers        http.Header
	wantTag        string
	wantError      string
	rateLimit      bool
	priorHeaders   string
	transportFail  bool
	repository     string
	via            []string
	wantRequests   int
}{
	{name: "stable", location: stableReleaseURL, status: 200, wantTag: "v1.2.3"},
	{name: "configured repository casing", repository: "subyard/subyard", location: stableReleaseURL, status: 200, wantTag: "v1.2.3"},
	{name: "redirect repository casing", location: "https://github.com/subyard/subyard/releases/tag/v1.2.3", status: 200, wantTag: "v1.2.3"},
	{name: "renamed repository", via: []string{"https://github.com/Renamed/Project/releases/latest"}, location: "https://github.com/Renamed/Project/releases/tag/v1.2.3", status: 200, wantTag: "v1.2.3"},
	{name: "no redirect", status: 200, wantError: "valid stable release tag"},
	{name: "missing v", location: "https://github.com/Subyard/Subyard/releases/tag/1.2.3", status: 200, wantError: "valid stable release tag"},
	{name: "prerelease", location: stableReleaseURL + "-rc.1", status: 200, wantError: "valid stable release tag"},
	{name: "build metadata", location: stableReleaseURL + "+build", status: 200, wantError: "valid stable release tag"},
	{name: "leading zero", location: "https://github.com/Subyard/Subyard/releases/tag/v01.2.3", status: 200, wantError: "valid stable release tag"},
	{name: "wrong host", location: "https://example.invalid/Subyard/Subyard/releases/tag/v1.2.3", status: 200, wantError: "stay on github.com", wantRequests: 1},
	{name: "off-host chain back to GitHub", via: []string{"https://example.invalid/bounce"}, location: stableReleaseURL, status: 200, wantError: "stay on github.com", wantRequests: 1},
	{name: "off-host intermediate", via: []string{"https://github.com/bounce", "https://example.invalid/bounce"}, location: stableReleaseURL, status: 200, wantError: "stay on github.com", wantRequests: 2},
	{name: "host suffix", location: "https://github.com.example.invalid/tag", status: 200, wantError: "stay on github.com", wantRequests: 1},
	{name: "host port", location: "https://github.com:443/tag", status: 200, wantError: "stay on github.com", wantRequests: 1},
	{name: "host casing remains strict", location: "https://GitHub.com/tag", status: 200, wantError: "stay on github.com", wantRequests: 1},
	{name: "userinfo", location: "https://operator@github.com/tag", status: 200, wantError: "stay on github.com", wantRequests: 1},
	{name: "insecure redirect", location: "http://github.com/Subyard/Subyard/releases/tag/v1.2.3", status: 200, wantError: "redirects must use https", wantRequests: 1},
	{name: "wrong repository", location: "https://github.com/Other/Project/releases/tag/v1.2.3", status: 200, wantError: "valid stable release tag"},
	{name: "release path casing remains strict", location: "https://github.com/Subyard/Subyard/Releases/tag/v1.2.3", status: 200, wantError: "valid stable release tag"},
	{name: "tag casing remains strict", location: "https://github.com/Subyard/Subyard/releases/tag/V1.2.3", status: 200, wantError: "valid stable release tag"},
	{name: "extra path", location: stableReleaseURL + "/other", status: 200, wantError: "valid stable release tag"},
	{name: "encoded tag", location: "https://github.com/Subyard/Subyard/releases/tag/%761.2.3", status: 200, wantError: "valid stable release tag"},
	{name: "query", location: stableReleaseURL + "?other=1", status: 200, wantError: "valid stable release tag"},
	{name: "forbidden", status: 403, wantError: "403"},
	{name: "unexhausted quota", status: 403, headers: http.Header{"X-Ratelimit-Remaining": {"1"}}, wantError: "403"},
	{name: "earlier quota headers", location: stableReleaseURL, status: 403, headers: http.Header{"X-Ratelimit-Remaining": {"1"}}, priorHeaders: "HTTP/2 302\r\nX-RateLimit-Remaining: 0\r\nX-RateLimit-Reset: 1\r\n\r\n", wantError: "403"},
	{name: "server failure", status: 503, wantError: "503"},
	{name: "transport failure after redirect", status: 302, headers: http.Header{"X-Ratelimit-Remaining": {"0"}}, transportFail: true, wantError: "could not resolve the stable release"},
	{name: "primary rate limit", status: 403, headers: http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"1794700000"}}, wantError: "reset at Unix time 1794700000", rateLimit: true},
	{name: "secondary rate limit", status: 403, headers: http.Header{"Retry-After": {"60"}}, wantError: "retry after 60", rateLimit: true},
	{name: "429 with reset", status: 429, headers: http.Header{"X-Ratelimit-Reset": {"1794700000"}}, wantError: "reset at Unix time 1794700000", rateLimit: true},
	{name: "429 without reset", status: 429, wantError: "reset time unavailable", rateLimit: true},
}

func TestLatestTagRedirectAndHTTPFailures(t *testing.T) {
	for _, test := range releaseLookupCases {
		t.Run(test.name, func(t *testing.T) {
			repository := test.repository
			if repository == "" {
				repository = "Subyard/Subyard"
			}
			redirects := append([]string{}, test.via...)
			if test.location != "" {
				redirects = append(redirects, test.location)
			}
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if test.transportFail {
					return nil, errors.New("connection failed")
				}
				if request.Method != http.MethodHead || request.URL.Host != "github.com" || request.URL.User != nil {
					t.Fatalf("unsafe lookup: %s %s", request.Method, request.URL)
				}
				if calls == 1 {
					if request.URL.String() != "https://github.com/"+repository+"/releases/latest" {
						t.Fatalf("lookup URL = %s", request.URL)
					}
				} else if calls > len(redirects)+1 || request.URL.String() != redirects[calls-2] {
					t.Fatalf("unexpected request %d: %s", calls, request.URL)
				}
				if calls <= len(redirects) {
					return &http.Response{StatusCode: 302, Header: http.Header{"Location": {redirects[calls-1]}}, Body: http.NoBody}, nil
				}
				return &http.Response{StatusCode: test.status, Status: fmt.Sprintf("%d %s", test.status, http.StatusText(test.status)), Header: test.headers, Body: http.NoBody, Request: request}, nil
			})}
			tag, err := New(Config{HTTPClient: client}).latestTag(context.Background(), repository)
			var limited *releaseRateLimitError
			if tag != test.wantTag || errors.As(err, &limited) != test.rateLimit ||
				(test.wantError == "" && err != nil) || (test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError))) {
				t.Fatalf("latestTag() = %q, %v (rate limit=%v)", tag, err, limited)
			}
			wantCalls := len(redirects) + 1
			if test.wantRequests != 0 {
				wantCalls = test.wantRequests
			}
			if calls != wantCalls {
				t.Fatalf("made %d requests, want %d", calls, wantCalls)
			}
		})
	}
}

func TestLatestTagRejectsInsecureRedirectHops(t *testing.T) {
	for _, intermediate := range []bool{false, true} {
		t.Run(fmt.Sprint(intermediate), func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Scheme != "https" || calls > 2 {
					t.Fatalf("followed unsafe redirect: %s", request.URL)
				}
				location := "http://github.com/Subyard/Subyard/releases/tag/v1.2.3"
				if intermediate && calls == 1 {
					location = "https://github.com/intermediate"
				}
				return &http.Response{StatusCode: 302, Header: http.Header{"Location": {location}}, Body: http.NoBody}, nil
			})}
			_, err := New(Config{HTTPClient: client}).latestTag(context.Background(), "Subyard/Subyard")
			if err == nil || !strings.Contains(err.Error(), "redirects must use https") {
				t.Fatalf("insecure redirect error = %v", err)
			}
		})
	}
}

func TestLatestTagPreservesClientPolicyAndTransportErrors(t *testing.T) {
	refused := errors.New("caller refused redirect")
	client := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": {stableReleaseURL}}, Body: http.NoBody}, nil
		}),
		CheckRedirect: func(*http.Request, []*http.Request) error { return refused },
	}
	_, err := New(Config{HTTPClient: client}).latestTag(context.Background(), "Subyard/Subyard")
	if !errors.Is(err, refused) || client.CheckRedirect(nil, nil) != refused {
		t.Fatalf("client redirect policy changed: %v", err)
	}
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded })
	_, err = New(Config{HTTPClient: client}).latestTag(context.Background(), "Subyard/Subyard")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("transport error = %v", err)
	}
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": {stableReleaseURL}}, Body: http.NoBody}, nil
	})
	client.CheckRedirect = func(request *http.Request, _ []*http.Request) error {
		request.URL.Scheme = "http"
		return nil
	}
	_, err = New(Config{HTTPClient: client}).latestTag(context.Background(), "Subyard/Subyard")
	if err == nil || !strings.Contains(err.Error(), "redirects must use https") {
		t.Fatalf("redirect policy bypassed https requirement: %v", err)
	}
}

func TestBootstrapLatestReleaseDiscovery(t *testing.T) {
	script, err := filepath.Abs("../../../dev/bootstrap-runtime.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range releaseLookupCases {
		t.Run(test.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			curl := filepath.Join(root, "curl")
			testkit.WriteFile(t, curl, []byte(fakeLatestReleaseCurl), 0o700)
			repository := test.repository
			if repository == "" {
				repository = "Subyard/Subyard"
			}
			redirects := append([]string{}, test.via...)
			if test.location != "" {
				redirects = append(redirects, test.location)
			}
			for index, location := range redirects {
				testkit.WriteFile(t, filepath.Join(root, fmt.Sprintf("headers-%d", index+1)),
					[]byte("HTTP/2 302\r\nLocation: "+location+"\r\n\r\n"), 0o600)
				testkit.WriteFile(t, filepath.Join(root, fmt.Sprintf("status-%d", index+1)), []byte("302"), 0o600)
			}
			headers := test.priorHeaders + fmt.Sprintf("HTTP/2 %d\r\n", test.status)
			for key, values := range test.headers {
				for _, value := range values {
					headers += key + ": " + value + "\r\n"
				}
			}
			testkit.WriteFile(t, filepath.Join(root, fmt.Sprintf("headers-%d", len(redirects)+1)), []byte(headers), 0o600)
			testkit.WriteFile(t, filepath.Join(root, fmt.Sprintf("status-%d", len(redirects)+1)), []byte(fmt.Sprint(test.status)), 0o600)
			command := exec.Command("bash", script, "--yes")
			command.Env = []string{
				"PATH=" + root + ":" + os.Getenv("PATH"), "HOME=" + root, "TMPDIR=/tmp",
				"FAKE_LOG=" + filepath.Join(root, "requests"), "FAKE_RESPONSES=" + root,
				"YARD_RELEASE_REPOSITORY=" + repository,
			}
			if test.transportFail {
				command.Env = append(command.Env, "FAKE_TRANSPORT_FAILURE=1")
			}
			output, err := command.CombinedOutput()
			// Successful discovery downloads deliberately invalid fixtures and stops
			// at checksum validation, before any installer can run.
			wantError := test.wantError
			if test.wantTag != "" {
				wantError = "installer checksum mismatch"
			}
			if err == nil || !strings.Contains(string(output), wantError) ||
				strings.Contains(string(output), "GitHub rate limit") != test.rateLimit {
				t.Fatalf("bootstrap = %v: %s", err, output)
			}
			requests, err := os.ReadFile(filepath.Join(root, "requests"))
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(requests)), "\n")
			wantLookups := len(redirects) + 1
			if test.wantRequests != 0 {
				wantLookups = test.wantRequests
			}
			if len(lines) < wantLookups || lines[0] != "https://github.com/"+repository+"/releases/latest" || (test.wantTag == "" && len(lines) != wantLookups) {
				t.Fatalf("unexpected requests: %s", requests)
			}
			for _, url := range lines[:wantLookups] {
				if !strings.HasPrefix(url, "https://github.com/") {
					t.Fatalf("contacted unsafe redirect: %s", url)
				}
			}
			if test.wantTag != "" {
				if len(lines) != wantLookups+6 {
					t.Fatalf("expected tag lookup and six pinned assets: %s", requests)
				}
				for _, url := range lines[wantLookups:] {
					if !strings.HasPrefix(url, "https://github.com/"+repository+"/releases/download/"+test.wantTag+"/") {
						t.Fatalf("unpinned asset: %s", url)
					}
				}
			}
		})
	}
}

const fakeLatestReleaseCurl = `#!/usr/bin/env bash
set -euo pipefail
headers='' output='' url='' head=0 proto='' retry='' follow=0 disabled=0 max_redirs=''
while [ $# -gt 0 ]; do
  case "$1" in
    --head) head=1; shift ;;
    --disable) disabled=1; shift ;;
    --location|-L|-fsSL) follow=1; shift ;;
    --dump-header) headers="$2"; shift 2 ;;
    --output|-o) output="$2"; shift 2 ;;
    --proto) proto="$2"; shift 2 ;;
    --retry) retry="$2"; shift 2 ;;
    --max-redirs) max_redirs="$2"; shift 2 ;;
    --write-out|--connect-timeout|--max-time) shift 2 ;;
    *://*) url="$1"; shift ;;
    *) shift ;;
  esac
done
printf '%s\n' "$url" >> "$FAKE_LOG"
if [ "$head" = 1 ]; then
  [ "$proto/$retry/$follow/$disabled/$max_redirs" = '=https/0/0/1/0' ]
  index="$(wc -l < "$FAKE_LOG")"
  cat "$FAKE_RESPONSES/headers-$index" > "$headers"
  status="$(cat "$FAKE_RESPONSES/status-$index")"
  printf '%s' "$status"
  [ "${FAKE_TRANSPORT_FAILURE:-0}" != 1 ] || exit 7
  [ "$status" -lt 400 ] || exit 22
else
  # A real download never runs in this fixture.
  printf 'invalid fixture bytes\n' > "$output"
fi
`
