package backup

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestPackagePostgresDumpRestore proves the exact pinned database images used
// by Immich and Paperless accept the closed hook command and produce a custom
// archive that restores into a fresh PostgreSQL data directory. It is opt-in
// because it pulls large application images and needs a Docker daemon:
//
//	WILDSTONE_DB_BACKUP_E2E=1 go test ./backup -run TestPackagePostgresDumpRestore -timeout 20m
func TestPackagePostgresDumpRestore(t *testing.T) {
	if os.Getenv("WILDSTONE_DB_BACKUP_E2E") != "1" {
		t.Skip("set WILDSTONE_DB_BACKUP_E2E=1 to run package database restore tests")
	}
	cases := []struct {
		name, image, hookContainer string
	}{
		{"immich", "ghcr.io/immich-app/postgres@sha256:6f3e9d2c2177af16c2988ff71425d79d89ca630ec2f9c8db03209ab716542338", "immich-postgres"},
		{"paperless", "postgres@sha256:c1b3783309b6499c795eed7c20135a1a4d25cae1b575c3d52c6f536129a1b109", "paperless-postgres"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { postgresDumpRestoreE2E(t, tc.image, tc.hookContainer) })
	}
}

func postgresDumpRestoreE2E(t *testing.T, image, hookContainer string) {
	t.Helper()
	prefix := fmt.Sprintf("ws-db-e2e-%d-%s", time.Now().UnixNano(), strings.ReplaceAll(hookContainer, "_", "-"))
	network, source, restored := prefix+"-net", prefix+"-source", prefix+"-restored"
	sourceVolume, dumpVolume, restoredVolume := prefix+"-source-data", prefix+"-dump", prefix+"-restored-data"
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", source, restored).Run()
		_ = exec.Command("docker", "volume", "rm", "-f", sourceVolume, dumpVolume, restoredVolume).Run()
		_ = exec.Command("docker", "network", "rm", network).Run()
	})
	for _, args := range [][]string{{"network", "create", network}, {"volume", "create", sourceVolume}, {"volume", "create", dumpVolume}, {"volume", "create", restoredVolume}} {
		dockerE2E(t, args...)
	}

	start := func(name, volume string) {
		dockerE2E(t, "run", "-d", "--name", name, "--network", network,
			"-e", "POSTGRES_USER=app", "-e", "POSTGRES_PASSWORD=recovery-test", "-e", "POSTGRES_DB=app",
			"-v", volume+":/var/lib/postgresql/data", image)
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			if exec.Command("docker", "exec", name, "pg_isready", "-U", "app", "-d", "app").Run() == nil {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("PostgreSQL container %s did not become ready", name)
	}

	start(source, sourceVolume)
	dockerE2E(t, "exec", "-e", "PGPASSWORD=recovery-test", source, "psql", "-U", "app", "-d", "app", "-v", "ON_ERROR_STOP=1",
		"-c", "CREATE TABLE recovery_probe(value text NOT NULL); INSERT INTO recovery_probe VALUES ('survived');")
	dumpPath := hookOutputDir + "/" + hookContainer + ".dump"
	dockerE2E(t, "run", "--rm", "--network", network,
		"-e", "PGHOST="+source, "-e", "PGUSER=app", "-e", "PGPASSWORD=recovery-test", "-e", "PGDATABASE=app",
		"-v", dumpVolume+":"+hookOutputDir, image,
		"pg_dump", "--no-password", "--format=custom", "--file="+dumpPath)

	start(restored, restoredVolume)
	dockerE2E(t, "run", "--rm", "--network", network,
		"-e", "PGHOST="+restored, "-e", "PGUSER=app", "-e", "PGPASSWORD=recovery-test", "-e", "PGDATABASE=app",
		"-v", dumpVolume+":"+hookOutputDir+":ro", image,
		"pg_restore", "--clean", "--if-exists", "--no-owner", "--dbname=app", dumpPath)
	out := dockerE2E(t, "exec", "-e", "PGPASSWORD=recovery-test", restored,
		"psql", "-U", "app", "-d", "app", "-Atc", "SELECT value FROM recovery_probe;")
	if strings.TrimSpace(out) != "survived" {
		t.Fatalf("restored probe = %q", out)
	}
}

func dockerE2E(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
