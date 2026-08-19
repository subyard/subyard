package systemdunit

import "testing"

func TestParseManagerState(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		wantLoad   string
		wantReload string
		wantError  bool
	}{
		{
			name:       "ordered properties",
			output:     "LoadState=loaded\nNeedDaemonReload=no\n",
			wantLoad:   "loaded",
			wantReload: "no",
		},
		{
			name:       "reverse property order",
			output:     "NeedDaemonReload=yes\nLoadState=bad-setting\n",
			wantLoad:   "bad-setting",
			wantReload: "yes",
		},
		{name: "missing property", output: "LoadState=loaded\n", wantError: true},
		{
			name:      "duplicate property",
			output:    "LoadState=loaded\nLoadState=loaded\n",
			wantError: true,
		},
		{
			name:      "unexpected property",
			output:    "LoadState=loaded\nActiveState=inactive\n",
			wantError: true,
		},
		{
			name:      "empty value",
			output:    "LoadState=\nNeedDaemonReload=no\n",
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			load, reload, err := parseManagerState(test.output)
			if (err != nil) != test.wantError {
				t.Fatalf("parse error = %v, want error %v", err, test.wantError)
			}
			if load != test.wantLoad || reload != test.wantReload {
				t.Fatalf(
					"manager state = %q, %q, want %q, %q",
					load, reload, test.wantLoad, test.wantReload,
				)
			}
		})
	}
}
