package main

import (
	"os"

	"memorize/internal/app/localdaemon"
	"memorize/internal/transport/cli"
)

var version = "1.1.0"

func main() {
	os.Exit(cli.Execute(version, localdaemon.Hooks(version)))
}
