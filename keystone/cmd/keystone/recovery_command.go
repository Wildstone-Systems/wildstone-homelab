package main

import (
	"crypto/ed25519"
	"crypto/rand"
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
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func recoverCommand(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "preflight-management":
			return preflightManagementCommand(args[1:])
		case "restore-management":
			return restoreManagementCommand(args[1:])
		case "journal-evidence":
			return journalEvidenceCommand(args[1:])
		case "journal-keygen":
			return journalKeygenCommand(args[1:])
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
	fmt.Println("start Keystone, re-join each node, import its .sealed bundle, then present signed journal evidence from a fresh active Owner Console session; quarantine release is separate")
	return nil
}

func journalKeygenCommand(args []string) error {
	fs := flag.NewFlagSet("keystone recover journal-keygen", flag.ContinueOnError)
	privatePath := fs.String("private", "", "new independently held recovery-journal private-key file")
	publicPath := fs.String("public", "", "new recovery-journal public-key file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *privatePath == "" || *publicPath == "" || fs.NArg() != 0 {
		return fmt.Errorf("usage: keystone recover journal-keygen -private <new-file> -public <new-file>")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	defer clear(priv)
	if err := writeExclusiveRecoveryFile(*privatePath, priv, 0o600); err != nil {
		return fmt.Errorf("writing recovery journal private key: %w", err)
	}
	if err := writeExclusiveRecoveryFile(*publicPath, pub, 0o644); err != nil {
		_ = os.Remove(*privatePath)
		return fmt.Errorf("writing recovery journal public key: %w", err)
	}
	fmt.Printf("created journal custodian key pair; keep %s outside every runtime claim and snapshot, and place %s in the separately held management key directory\n", *privatePath, *publicPath)
	return nil
}

func writeExclusiveRecoveryFile(path string, body []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func journalEvidenceCommand(args []string) error {
	fs := flag.NewFlagSet("keystone recover journal-evidence", flag.ContinueOnError)
	sourceDB := fs.String("source-db", "", "offline final source Keystone database")
	snapshot := fs.String("snapshot", "", "exact SQLite snapshot artifact being restored")
	snapshotAtText := fs.String("snapshot-at", "", "snapshot capture time (RFC3339)")
	fencedAtText := fs.String("source-fenced-at", "", "time the original writer was fenced (RFC3339)")
	source := fs.String("source", "", "stable journal source identifier")
	custody := fs.String("custody", "", "independent evidence custody description")
	signingKey := fs.String("signing-key", "", "independently held recovery-journal custodian private key")
	outPath := fs.String("out", "", "new evidence JSON file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sourceDB == "" || *snapshot == "" || *snapshotAtText == "" || *fencedAtText == "" || *source == "" || *custody == "" || *signingKey == "" || *outPath == "" || fs.NArg() != 0 {
		return fmt.Errorf("usage: keystone recover journal-evidence -source-db <offline-db> -snapshot <snapshot-db> -snapshot-at <RFC3339> -source-fenced-at <RFC3339> -source <id> -custody <description> -signing-key <recovery-journal.key> -out <new-file>")
	}
	snapshotAt, err := time.Parse(time.RFC3339, *snapshotAtText)
	if err != nil {
		return fmt.Errorf("snapshot-at: %w", err)
	}
	fencedAt, err := time.Parse(time.RFC3339, *fencedAtText)
	if err != nil {
		return fmt.Errorf("source-fenced-at: %w", err)
	}
	key, err := os.ReadFile(*signingKey)
	if err != nil {
		return fmt.Errorf("reading signing key: %w", err)
	}
	defer clear(key)
	if len(key) != ed25519.PrivateKeySize {
		return fmt.Errorf("signing key has invalid length")
	}
	db, err := runtimedb.Open(*sourceDB)
	if err != nil {
		return fmt.Errorf("opening offline source database: %w", err)
	}
	defer db.Close()
	evidence, err := db.BuildRecoveryJournalEvidence(*snapshot, *source, *custody, snapshotAt, fencedAt, time.Now().UTC(), ed25519.PrivateKey(key))
	if err != nil {
		return err
	}
	if err := writeExclusiveRecoveryFile(*outPath, evidence, 0o600); err != nil {
		return fmt.Errorf("creating evidence file: %w", err)
	}
	fmt.Printf("wrote signed terminal recovery journal evidence to %s; retain it independently from the snapshot\n", *outPath)
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
