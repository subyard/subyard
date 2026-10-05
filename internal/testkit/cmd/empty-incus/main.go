// empty-incus serves the native empty-instance Incus fixture for release tests.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Subyard/Subyard/internal/testkit"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "empty-incus: expected a private fixture root")
		os.Exit(2)
	}
	server, err := testkit.NewIncusServer(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "empty-incus: cannot start fixture server")
		os.Exit(1)
	}
	defer server.Close()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	fmt.Println("ready")
	<-stop
}
