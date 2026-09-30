package previewroute

import "testing"

func TestOwnedPreviewRoute(t *testing.T) {
	for _, test := range []struct {
		name, marker, host, port string
		owned, ready             bool
	}{
		{"ready", "v1:100.101.102.103:32222", "100.101.102.103", "32222", true, true},
		{"pending", "v1:pending:100.101.102.103:32222", "100.101.102.103", "32222", true, false},
		{"foreign", "", "100.101.102.103", "32222", false, false},
		{"unknown version", "v2:100.101.102.103:32222", "100.101.102.103", "32222", false, false},
		{"loopback", "v1:127.0.0.1:32222", "127.0.0.1", "32222", false, false},
		{"wildcard", "v1:0.0.0.0:32222", "0.0.0.0", "32222", false, false},
		{"LAN", "v1:192.168.1.2:32222", "192.168.1.2", "32222", false, false},
		{"privileged port", "v1:100.101.102.103:80", "100.101.102.103", "80", false, false},
		{"noncanonical port", "v1:100.101.102.103:032222", "100.101.102.103", "032222", false, false},
		{"different address", "v1:100.101.102.103:32222", "100.101.102.104", "32222", false, false},
		{"different port", "v1:100.101.102.103:32222", "100.101.102.103", "32223", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			device := map[string]string{"type": "proxy", "bind": "host",
				"listen": "tcp:" + test.host + ":" + test.port, "connect": "tcp:127.0.0.1:8765"}
			host, port, ready := Owned(test.marker, device)
			if (host != "") != test.owned || ready != test.ready || (test.owned && (host != test.host || port != test.port)) {
				t.Fatalf("Owned = %q, %q, %v", host, port, ready)
			}
		})
	}
	for _, mutation := range []string{"target", "extra option"} {
		t.Run(mutation, func(t *testing.T) {
			device := Device("100.101.102.103", "32222")
			if mutation == "target" {
				device["connect"] = "tcp:127.0.0.1:8080"
			} else {
				device["nat"] = "true"
			}
			if host, _, _ := Owned("v1:100.101.102.103:32222", device); host != "" {
				t.Fatal("divergent device accepted")
			}
		})
	}
}

func TestPreviewMetadata(t *testing.T) {
	for _, test := range []struct{ host, port, want string }{
		{"100.101.102.103", "32222", "{\"version\":1,\"host\":\"100.101.102.103\",\"port\":32222}\n"},
		{"127.0.0.1", "32222", "{\"version\":1,\"host\":\"127.0.0.1\",\"port\":8765}\n"},
	} {
		if got := string(Metadata(test.host, test.port)); got != test.want {
			t.Fatalf("metadata = %q, want %q", got, test.want)
		}
	}
}
