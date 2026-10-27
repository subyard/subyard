package main

import (
	"fmt"
	"github.com/Subyard/Subyard/config/profiles/telemetry/collector"
	"os"
)

func main() {
	if err := collector.RunCLI(os.Args[1:], nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
