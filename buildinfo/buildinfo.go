// Package buildinfo carries build metadata injected at compile time via
// -ldflags "-X copilot-go/buildinfo.Commit=... -X copilot-go/buildinfo.BuildTime=..."
// (see the makefile and Dockerfile). Values default to "dev"/"unknown" for
// plain `go run`/`go build` invocations without the flags.
package buildinfo

var (
	// Commit is the short git hash the binary was built from.
	Commit = "dev"
	// BuildTime is the compile timestamp (format YYYYMMDD-HHMM).
	BuildTime = "unknown"
)

// String renders the version shown next to the web console title:
// "{short commit}-{build time}", e.g. "ab2d0d1-20260904-2136".
func String() string {
	return Commit + "-" + BuildTime
}
