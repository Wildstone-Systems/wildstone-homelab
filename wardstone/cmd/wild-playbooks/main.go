// Command wild-playbooks signs and verifies a host playbook set
// (wardstone/internal/playbook).
//
//	wild-playbooks keygen -out signing.key       # prints the public key (hex)
//	wild-playbooks sign   -key signing.key DIR   # writes the manifest and its signature into DIR
//	wild-playbooks verify -pub <hex>  DIR        # exit 0 only if the whole set verifies
//
// A release signs with its own key in CI; a person testing on a
// throwaway machine can sign with a key made by keygen. The private key
// is written 0600 and never printed.
package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/wardstone/internal/playbook"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild-playbooks:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: wild-playbooks keygen -out FILE | sign -key FILE DIR | verify -pub HEX DIR")
	os.Exit(2)
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "", "where to write the private key (0600)")
	fs.Parse(args)
	if *out == "" {
		return fmt.Errorf("keygen: -out is required")
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(hex.EncodeToString(priv.Seed()) + "\n"); err != nil {
		return err
	}
	fmt.Println(hex.EncodeToString(pub))
	return nil
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyPath := fs.String("key", "", "private key from keygen")
	fs.Parse(args)
	if *keyPath == "" || fs.NArg() != 1 {
		return fmt.Errorf("sign: -key and one DIR are required")
	}
	b, err := os.ReadFile(*keyPath)
	if err != nil {
		return err
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return fmt.Errorf("sign: %s is not a key from keygen", *keyPath)
	}
	dir := fs.Arg(0)
	manifest, err := playbook.Build(dir)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, playbook.ManifestFile), manifest, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, playbook.SignatureFile), playbook.Sign(ed25519.NewKeyFromSeed(seed), manifest), 0o644)
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	pubHex := fs.String("pub", "", "public key (hex) printed by keygen")
	fs.Parse(args)
	if *pubHex == "" || fs.NArg() != 1 {
		return fmt.Errorf("verify: -pub and one DIR are required")
	}
	pub, err := hex.DecodeString(strings.TrimSpace(*pubHex))
	if err != nil {
		return fmt.Errorf("verify: -pub is not hex")
	}
	if err := playbook.Verify(fs.Arg(0), ed25519.PublicKey(pub)); err != nil {
		return err
	}
	fmt.Println("ok: playbook set verifies")
	return nil
}
