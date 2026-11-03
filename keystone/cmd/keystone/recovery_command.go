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
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/recovery"
)

func recoverCommand(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "preflight-management":
			return preflightManagementCommand(args[1:])
		case "restore-management":
			return restoreManagementCommand(args[1:])
		}
	}
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

func restoreManagementCommand(args []string) error {
	fs := flag.NewFlagSet("keystone recover restore-management", flag.ContinueOnError)
	setDir := fs.String("set", "", "directory containing the extracted private management recovery set")
	keyDir := fs.String("keys", "", "separately held directory containing bootstrap/decryption keys")
	dataDir := fs.String("data-dir", "", "new chart 0.2 Keystone private data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *setDir == "" || *keyDir == "" || *dataDir == "" || fs.NArg() != 0 {
		return fmt.Errorf("usage: keystone recover restore-management -set <directory> -keys <directory> -data-dir <new-directory>")
	}
	report, err := recovery.RestoreManagementSet(*setDir, *keyDir, *dataDir)
	if err != nil {
		return err
	}
	fmt.Printf("restored the preserved management runtime into %s in quarantine\n", *dataDir)
	fmt.Printf("accepted Statefile: %s\n", report.ApprovedStatefileRevision)
	fmt.Printf("preserved coordinator: %s epoch %d (Owner release and promotion are still required)\n", report.CoordinatorHolder, report.CoordinatorEpoch)
	fmt.Println("do not start Keystone until the original writer is fenced; never start more than one writer")
	return nil
}

func preflightManagementCommand(args []string) error {
	fs := flag.NewFlagSet("keystone recover preflight-management", flag.ContinueOnError)
	setDir := fs.String("set", "", "directory containing the extracted private management recovery set")
	keyDir := fs.String("keys", "", "separately held directory containing bootstrap/decryption keys")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *setDir == "" || *keyDir == "" || fs.NArg() != 0 {
		return fmt.Errorf("usage: keystone recover preflight-management -set <directory> -keys <directory>")
	}
	report, err := recovery.VerifyManagementSet(*setDir, *keyDir)
	if err != nil {
		return err
	}
	fmt.Printf("management recovery preflight passed\n")
	fmt.Printf("snapshot: %s\n", report.SnapshotAt.UTC().Format(time.RFC3339))
	fmt.Printf("accepted Statefile: %s\n", report.ApprovedStatefileRevision)
	fmt.Printf("promoted deployment: %s\n", report.PromotedDeploymentRevision)
	fmt.Printf("preserved coordinator: %s epoch %d (promotion is still required before dispatch)\n", report.CoordinatorHolder, report.CoordinatorEpoch)
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
		"--no-cache", "restore", "latest", "--target", "/restore")
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
