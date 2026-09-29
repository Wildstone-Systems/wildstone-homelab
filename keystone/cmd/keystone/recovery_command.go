package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/recovery"
)

func recoverCommand(args []string) error {
	fs := flag.NewFlagSet("keystone recover", flag.ContinueOnError)
	setDir := fs.String("set", "", "directory containing the restored wildstone recovery set")
	kitPath := fs.String("kit", "", "recovery kit JSON; downloads the wildstone set from the bucket")
	dataDir := fs.String("data-dir", "/var/lib/wildstone/keystone", "fresh Keystone data directory")
	privateKeyPath := fs.String("private-key", "", "recovery private-key file from the offline kit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*setDir == "") == (*kitPath == "") {
		return fmt.Errorf("exactly one of -set or -kit is required")
	}
	var privateKey []byte
	var cleanup func()
	if *kitPath != "" {
		kit, err := recovery.LoadKit(*kitPath)
		if err != nil {
			return err
		}
		downloaded, remove, err := downloadRecoverySet(kit)
		if err != nil {
			return err
		}
		*setDir, privateKey, cleanup = downloaded, []byte(kit.RecoveryPrivateKey), remove
	} else {
		if *privateKeyPath == "" {
			return fmt.Errorf("-private-key is required with -set")
		}
		var err error
		privateKey, err = os.ReadFile(*privateKeyPath)
		if err != nil {
			return fmt.Errorf("reading recovery private key: %w", err)
		}
	}
	if cleanup != nil {
		defer cleanup()
	}
	defer clear(privateKey)
	if err := recovery.RestoreSet(*setDir, *dataDir, strings.TrimSpace(string(privateKey))); err != nil {
		return err
	}
	fmt.Printf("restored Statefile and quarantined runtime database into %s\n", *dataDir)
	fmt.Println("start Keystone, re-join each node, import its .sealed bundle, then run wild restore reconcile --release")
	return nil
}

func downloadRecoverySet(kit recovery.Kit) (string, func(), error) {
	target, err := os.MkdirTemp("", "wildstone-recovery-download-")
	if err != nil {
		return "", nil, err
	}
	remove := func() { _ = os.RemoveAll(target) }
	args := []string{"run", "--rm",
		"-e", "RESTIC_REPOSITORY", "-e", "RESTIC_PASSWORD",
		"-e", "AWS_ACCESS_KEY_ID", "-e", "AWS_SECRET_ACCESS_KEY"}
	// Restic preserves the source files' root ownership when its restore
	// container runs as root. Recovery artifacts staged by Wardstone are
	// intentionally mode 0600, so that would leave the invoking operator
	// unable to verify or install the downloaded set. Restore as the caller;
	// the temporary directory already belongs to that uid/gid.
	if runtime.GOOS != "windows" {
		current, err := user.Current()
		if err != nil {
			remove()
			return "", nil, fmt.Errorf("identifying recovery operator: %w", err)
		}
		args = append(args, "--user", current.Uid+":"+current.Gid)
	}
	if kit.Region != "" {
		args = append(args, "-e", "AWS_DEFAULT_REGION")
	}
	args = append(args, "-v", target+":/restore", defaultResticImage,
		"restore", "latest", "--target", "/restore")
	if kit.PathStyle {
		args = append(args, "-o", "s3.bucket-lookup=path")
	}
	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(),
		"RESTIC_REPOSITORY="+kit.Repository(recovery.PlanName),
		"RESTIC_PASSWORD="+kit.RepositoryPasswords[recovery.PlanName],
		"AWS_ACCESS_KEY_ID="+kit.ReadOnlyAccessKeyID,
		"AWS_SECRET_ACCESS_KEY="+kit.ReadOnlySecretAccessKey,
	)
	if kit.Region != "" {
		cmd.Env = append(cmd.Env, "AWS_DEFAULT_REGION="+kit.Region)
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		remove()
		return "", nil, fmt.Errorf("downloading recovery repository: %w", err)
	}
	setDir := filepath.Join(target, "backup", "srv", "wildstone", "recovery")
	if _, err := recovery.VerifySet(setDir); err != nil {
		remove()
		return "", nil, fmt.Errorf("downloaded recovery set: %w", err)
	}
	return setDir, remove, nil
}
