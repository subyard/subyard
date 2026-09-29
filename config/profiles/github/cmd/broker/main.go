package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Subyard/Subyard/config/profiles/github/broker"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) > 1 && os.Args[1] == "_github-client" {
		os.Exit(githubbroker.RunClient(ctx, os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) == 3 && os.Args[1] == "_github-broker" {
		if err := githubbroker.RunServer(ctx, os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	fmt.Fprintln(os.Stderr, "usage: broker _github-broker CONFIG | _github-client COMMAND [ARGS...]")
	os.Exit(2)
}
