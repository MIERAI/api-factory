package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/debug"

	"apifactory/factory/upgrade"
	"apifactory/factory/version"
)

// factory version: the factory's version and the runtime it pairs with (one release = tags vX.Y.Z + runtime/vX.Y.Z).
func cmdVersion(args []string) error {
	commit := ""
	if bi, ok := debug.ReadBuildInfo(); ok {
		var rev, dirty string
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					dirty = ", modified"
				}
			}
		}
		if len(rev) >= 7 {
			commit = fmt.Sprintf(" (commit %s%s)", rev[:7], dirty)
		}
	}
	fmt.Printf("factory %s%s\n", version.Factory, commit)
	fmt.Printf("runtime %s %s (tag %s)\n", version.RuntimeModule, version.Runtime, version.RuntimeTag())
	return nil
}

// factory upgrade [--project DIR] [--check] [--no-build] [--force]: the project's runtime dependency → the runtime
// paired with this factory (module path and version in go.mod and the Go files' imports), then go mod tidy + go build.
// The global form `factory --project DIR upgrade` works too. Regenerate afterwards (factory --project DIR prepare).
func cmdUpgrade(args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	dir := fs.String("project", ".", "the project's root (its go.mod)")
	check := fs.Bool("check", false, "change nothing; fail unless the project is at the paired runtime")
	noBuild := fs.Bool("no-build", false, "skip go mod tidy and go build ./...")
	force := fs.Bool("force", false, "allow moving to an older runtime than the project requires")
	fs.Parse(args)
	res, err := upgrade.Run(*dir, upgrade.Options{Check: *check, NoBuild: *noBuild, Force: *force, Out: os.Stdout})
	if err != nil {
		return err
	}
	switch {
	case *check:
		fmt.Printf("✓ %s: runtime %s %s (paired with factory %s)\n", *dir, version.RuntimeModule, version.Runtime, version.Factory)
	case !res.Changed:
		fmt.Printf("✓ %s: already at runtime %s\n", *dir, version.Runtime)
	default:
		fmt.Printf("✓ %s: runtime %s %s → %s %s\n", *dir, res.FromModule, res.FromVersion, version.RuntimeModule, version.Runtime)
	}
	for _, m := range res.Mentions {
		fmt.Printf("  note: %s still mentions %s (generated? regenerate with factory --project %s prepare)\n", m, version.OldRuntimeModule, *dir)
	}
	return nil
}
