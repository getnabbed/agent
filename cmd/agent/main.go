// Command agent is the getnabbed agent command-line tool.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/getnabbed/agent/internal/cli"
)

func main() {
	// SIGTERM is what Docker, systemd and launchd send on stop; os.Interrupt
	// covers Ctrl-C and Ctrl-Break on Windows consoles.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
