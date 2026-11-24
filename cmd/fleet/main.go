package main

import (
	"os"

	"github.com/bdatdo0601/fleet/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
