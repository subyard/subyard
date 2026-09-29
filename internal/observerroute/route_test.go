package observerroute

import "testing"

func TestOwnedRouteReceipts(t *testing.T) {
	for _, test := range []struct {
		marker, host, port string
		owned, ready       bool
	}{
		{"v1:22222", "127.0.0.1", "22222", true, true},
		{"v1:pending:22222", "127.0.0.1", "22222", true, false},
		{"v2:100.101.102.103:22222", "100.101.102.103", "22222", true, true},
		{"v2:pending:100.101.102.103:22222", "100.101.102.103", "22222", true, false},
		{"v2:0.0.0.0:22222", "0.0.0.0", "22222", false, false},
		{"v2:192.168.1.1:22222", "192.168.1.1", "22222", false, false},
		{"v2:100.101.102.103:80", "100.101.102.103", "80", false, false},
		{"v2:100.101.102.103:02222", "100.101.102.103", "02222", false, false},
		{"v2:100.101.102.103:22222", "100.101.102.104", "22222", false, false},
		{"", "100.101.102.103", "22222", false, false},
	} {
		t.Run(test.marker+"/"+test.host, func(t *testing.T) {
			device := Device(test.host, test.port)
			host, port, ready := Owned(test.marker, device)
			if (host != "") != test.owned || ready != test.ready || (test.owned && (host != test.host || port != test.port)) {
				t.Fatalf("Owned = %q, %q, %v", host, port, ready)
			}
			device["nat"] = "true"
			if host, _, _ := Owned(test.marker, device); host != "" {
				t.Fatal("accepted extra device options")
			}
		})
	}
}
