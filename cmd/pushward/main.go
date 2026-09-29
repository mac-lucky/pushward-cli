// Command pushward is the PushWard command-line client.
package main

import (
	"os"
	"runtime/debug"
	"strings"

	"github.com/mac-lucky/pushward-cli/internal/cmd"
)

// Set by the release build with -ldflags -X.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	if version == "dev" {
		// go install builds carry the module version but no ldflags.
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = strings.TrimPrefix(info.Main.Version, "v")
		}
	}
	os.Exit(cmd.Execute(cmd.NewApp(version, commit, buildDate), os.Args[1:]))
}
