package main

import (
	"fmt"
	"io"
	"runtime"
)

// Build metadata. These are overridden at release time via -ldflags -X by
// goreleaser (see .goreleaser.yaml). The defaults keep `go install` and local
// `go build` honest: a binary built without ldflags reports "dev", so a number
// is never claimed that the build cannot back up.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// printVersion writes the CLI version line to w. The format is stable so
// scripts can parse it: "mitos <version> (commit <commit>, built <date>, <goos>/<goarch>)".
func printVersion(w io.Writer) {
	fmt.Fprintf(w, "mitos %s (commit %s, built %s, %s/%s)\n",
		version, commit, date, runtime.GOOS, runtime.GOARCH)
}
