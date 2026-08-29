package testvmsruntime

import (
	"errors"
	"strings"
	"testing"
)

func TestObserveRouteDistinguishesUnavailableAndAmbiguousDefaults(t *testing.T) {
	for _, test := range []struct {
		name, routes, message string
	}{
		{name: "unavailable", message: "default route is unavailable"},
		{
			name: "ambiguous",
			routes: "default via 192.0.2.1 dev eth0\n" +
				"default via 198.51.100.1 dev eth1\n",
			message: "route device is ambiguous",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ObserveRoute(func(arguments ...string) (string, error) {
				if strings.Join(arguments, " ") != "ip -4 -o route show default" {
					return "", errors.New("unexpected command after invalid default routes")
				}
				return test.routes, nil
			})
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("ObserveRoute() error = %v, want %q", err, test.message)
			}
			var conflict interface{ ActivationConflict() bool }
			if errors.As(err, &conflict) != (test.name == "ambiguous") {
				t.Fatalf("ObserveRoute() conflict classification = %v, err=%v", conflict, err)
			}
		})
	}
}
