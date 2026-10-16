package main

import (
	"os"

	"github.com/OcheOps/Infra-Monitor/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(cli.Run(os.Args[1:], version))
}
