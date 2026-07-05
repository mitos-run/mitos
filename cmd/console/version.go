package main

// version is the console binary's build version, surfaced through
// Capabilities.Version to the SPA (sidebar footer + feedback diagnostics).
// It is NOT yet wired to -ldflags: Dockerfile.console builds this binary
// with a plain `go build`, unlike cmd/mitos (see cmd/mitos/version.go and
// .goreleaser.yaml), so a build reports "dev" until a release pipeline
// injects a real value via `-ldflags -X main.version=...`. Keep this default
// honest: never claim a version this build cannot back up.
var version = "dev"
