package statefile

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Bundle creates a self-contained git bundle containing every Statefile ref.
// The destination must be outside the repository and must not already exist;
// callers can therefore stage it directly for the whole-site backup without
// racing a partial overwrite of the previous artifact.
func (s *Store) Bundle(destination string) error {
	if destination == "" {
		return fmt.Errorf("statefile: bundle destination is required")
	}
	absDestination, err := filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("statefile: resolving bundle destination: %w", err)
	}
	absRoot, err := filepath.Abs(s.Root)
	if err != nil {
		return fmt.Errorf("statefile: resolving root: %w", err)
	}
	rel, err := filepath.Rel(absRoot, absDestination)
	if err == nil && filepath.IsLocal(rel) {
		return fmt.Errorf("statefile: bundle destination must be outside the Statefile repository")
	}
	if _, err := os.Stat(absDestination); err == nil {
		return fmt.Errorf("statefile: bundle destination already exists")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("statefile: checking bundle destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absDestination), 0o700); err != nil {
		return fmt.Errorf("statefile: creating bundle directory: %w", err)
	}
	if _, err := s.git(context.Background(), s.Root, "bundle", "create", absDestination, "--all"); err != nil {
		return fmt.Errorf("statefile: creating git bundle: %w", err)
	}
	return nil
}

// RestoreBundle clones a Statefile bundle into a fresh destination. The
// destination must not exist: whole-site recovery never merges recovered
// desired state into an install that may already contain newer intent.
func RestoreBundle(bundle, destination string) (*Store, error) {
	if bundle == "" || destination == "" {
		return nil, fmt.Errorf("statefile: bundle and restore destination are required")
	}
	absBundle, err := filepath.Abs(bundle)
	if err != nil {
		return nil, fmt.Errorf("statefile: resolving bundle: %w", err)
	}
	absDestination, err := filepath.Abs(destination)
	if err != nil {
		return nil, fmt.Errorf("statefile: resolving restore destination: %w", err)
	}
	if _, err := os.Stat(absDestination); err == nil {
		return nil, fmt.Errorf("statefile: restore destination already exists")
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("statefile: checking restore destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absDestination), 0o700); err != nil {
		return nil, fmt.Errorf("statefile: creating restore parent: %w", err)
	}
	if _, err := defaultGitRunner(context.Background(), filepath.Dir(absDestination),
		"clone", "--no-local", absBundle, absDestination); err != nil {
		return nil, fmt.Errorf("statefile: restoring git bundle: %w", err)
	}
	return &Store{Root: absDestination, git: defaultGitRunner, gitIn: defaultGitInputRunner}, nil
}
