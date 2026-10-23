// Command agent is the getnabbed agent command-line tool.
package main

import (
	"os"

	"github.com/getnabbed/agent/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
