package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"apifactory/factory/system"
)

// factory system <dir> [--check]: the system made of the projects under <dir> (each */factory.yaml, one shared contracts
// directory): the cross-project message contract check, then the system-level topology manifest
// <contracts>/topology.json (the platform applies it before the services start; GAPS G23). --check: fail when the
// manifest on disk is not what the projects give (CI / scripts/projects.sh) instead of writing it.
func cmdSystem(args []string) error {
	fs := flag.NewFlagSet("system", flag.ExitOnError)
	check := fs.Bool("check", false, "do not write: fail if topology.json is not up to date")
	var dir string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		dir, args = args[0], args[1:]
	}
	fs.Parse(args)
	if dir == "" {
		return errors.New("usage: factory system <dir> [--check]   (dir = the projects' parent, e.g. examples/storefront-v2)")
	}
	s, err := system.Load(dir)
	if err != nil {
		return err
	}
	if problems := s.Check(); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "  ✗", p)
		}
		return fmt.Errorf("system %s: %d contract problem(s) across the projects", dir, len(problems))
	}
	b, err := s.ManifestJSON()
	if err != nil {
		return err
	}
	path := filepath.Join(s.Contracts, system.ManifestFile)
	m, _ := s.Manifest()
	if *check {
		old, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(old, b) {
			return fmt.Errorf("%s is not up to date (factory system %s)", path, dir)
		}
	} else if err := os.WriteFile(path, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("✓ system %s: %d projects, %d message types, contracts agree; %s: %d queues\n", dir, len(s.Projects), len(m.Messages), path, len(m.Queues))
	return nil
}
