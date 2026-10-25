package main

import (
	"fmt"
	"github.com/Subyard/Subyard/config/profiles/telemetry/collector"
	"github.com/Subyard/Subyard/dev/telemetry-compare/minimal"
	"os"
)

func main() {
	if err := collector.RunCLI(os.Args[1:], minimal.Serve); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
