package localgit

import (
	"fmt"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
	"os"
	"path/filepath"
)

// CheckLocalPath refuses source-checkout ancestry and symlinks. Use this on
// deployment/bootstrap paths before generating or installing local artifacts.
func CheckLocalPath(p string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
		return fmt.Errorf("local source: absolute non-root path required")
	}
	for dir := p; ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return fmt.Errorf("local source: source checkout paths are forbidden")
		} else if !os.IsNotExist(err) {
			return err
		}
		info, err := os.Lstat(dir)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("local source: symlinked installation path refused")
			}
			if !info.IsDir() {
				return fmt.Errorf("local source: directory required")
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if dir == filepath.Dir(dir) {
			return nil
		}
	}
}

// WriteBundle creates a private, new review directory only. It never overwrites
// a previous bundle and does not install or start the rendered service.
func WriteBundle(c Config, out string) error {
	files, err := Render(c)
	if err != nil {
		return err
	}
	for _, p := range []string{c.Repository, c.MirrorRepository, c.InstallDirectory, out} {
		if err := CheckLocalPath(p); err != nil {
			return err
		}
	}
	if _, err := statefile.InspectLocalDeployment(c.Repository); err != nil {
		return fmt.Errorf("local source: initialized isolated deployment repository required: %w", err)
	}
	if info, err := os.Stat(c.MirrorRepository); err == nil {
		marker, e := os.ReadFile(filepath.Join(c.MirrorRepository, mirrorMarker))
		if !info.IsDir() || info.Mode().Perm()&0077 != 0 || e != nil || string(marker) != "1\n" {
			return fmt.Errorf("local source: existing directory is not a private dedicated mirror")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	// Staging cannot replace either active repository or serving configuration.
	if within(out, c.Repository) || within(c.Repository, out) || within(out, c.MirrorRepository) || within(c.MirrorRepository, out) || within(out, c.InstallDirectory) || within(c.InstallDirectory, out) {
		return fmt.Errorf("local source: review bundle must be separate from runtime paths")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			os.RemoveAll(out)
		}
	}()
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(out, name), data, 0600); err != nil {
			return err
		}
	}
	complete = true
	return nil
}
