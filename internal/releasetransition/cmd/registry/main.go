// Command registry regenerates the release registry from compiled definitions.
package main

import (
	"fmt"
	"os"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: registry OUTPUT")
		os.Exit(2)
	}
	payload, err := releasetransition.BuiltinRegistryPayload()
	if err == nil {
		err = os.WriteFile(os.Args[1], payload, 0o644)
	}
	if err == nil {
		err = os.Chmod(os.Args[1], 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
