// rollback-declaration-check refuses a release's own sealed rollback
// declaration when it claims a registry feature the packaged engine's compiled
// parser does not implement. It is a packaging tool, never part of the runtime.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: rollback-declaration-check DECLARATION")
		os.Exit(2)
	}
	file, err := os.Open(os.Args[1])
	if err == nil {
		defer file.Close()
		var payload []byte
		payload, err = io.ReadAll(io.LimitReader(file, releasetransition.MaxRollbackDeclarationBytes+1))
		if err == nil {
			err = releasetransition.CheckOwnRollbackDeclaration(payload)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "rollback-declaration-check: %v\n", err)
		os.Exit(1)
	}
}
