// local-git-source renders a private review bundle; it installs nothing.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/localgit"
)

func main() {
	input := flag.String("config", "", "local JSON configuration path")
	out := flag.String("out", "", "new private review directory outside the source checkout")
	flag.Parse()
	if err := run(*input, *out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(input, out string) error {
	if input == "" || out == "" {
		return fmt.Errorf("both -config and -out are required; this command only renders a review bundle")
	}
	if err := localgit.CheckLocalPath(filepath.Dir(input)); err != nil {
		return err
	}
	info, err := os.Lstat(input)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 65536 {
		return fmt.Errorf("local source configuration must be a private regular file")
	}
	f, err := os.Open(input)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 65537))
	d.DisallowUnknownFields()
	var cfg localgit.Config
	if err := d.Decode(&cfg); err != nil {
		return fmt.Errorf("invalid local source configuration")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("one local source configuration document required")
	}
	return localgit.WriteBundle(cfg, out)
}
