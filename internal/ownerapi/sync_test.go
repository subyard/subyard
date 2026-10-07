package ownerapi

import "testing"

func TestConfigRemoteOmitsCredentialsAndLocalPaths(t *testing.T) {
	for _, fixture := range []struct{ input, want string }{
		{"https://user:token@example.invalid/repo?token=x#private", "https://example.invalid/repo"},
		{"ssh://private-user@example.invalid/repo?token=x", "ssh://example.invalid/repo"},
		{"private-user@example.invalid:repo?token=x#private", "example.invalid:repo"},
		{"/private/checkout", "<local-source>"},
		{"file:///private/checkout", "<local-source>"},
		{"./private/checkout", "<redacted-remote>"},
		{"@example.invalid:repo", "<redacted-remote>"},
		{"https://example.invalid/repo?", "https://example.invalid/repo"},
		{"unsupported://example.invalid/repo", "<redacted-remote>"},
	} {
		if actual := publicConfigRemote(fixture.input); actual != fixture.want {
			t.Fatalf("sanitized remote=%q want=%q", actual, fixture.want)
		}
	}
}
