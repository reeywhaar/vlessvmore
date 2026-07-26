// Command vlessvmore runs a sing-box VLESS/Reality server and manages its users.
//
// It is both the container entrypoint (`vlessvmore serve`) and the CLI used to drive a
// running instance. See the README for installation and usage.
package main

import (
	"os"

	"vlessvmore/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
