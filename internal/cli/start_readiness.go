package cli

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/domain"
)

func (cli *CLI) localAddresses() ([]netip.Addr, error) {
	if cli.options.LocalAddresses != nil {
		return cli.options.LocalAddresses()
	}
	return hostruntime.LocalAddresses()
}

func displayYardState(state string, startState domain.StartState) string {
	if startState == domain.StartWaitingForAddress {
		return "WAITING_FOR_ADDRESS"
	}
	return state
}

func (cli *CLI) printAddressWait(addresses []string) {
	if len(addresses) != 0 {
		fmt.Fprintf(cli.options.Stdout, "  waiting  local address: %s\n", strings.Join(addresses, ", "))
	}
}
