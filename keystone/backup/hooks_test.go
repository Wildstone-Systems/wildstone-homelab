package backup

import (
	"context"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/reconcile"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

const hookTestImage = "postgres@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func hookFixtures(kind string) (resource.Resource, []resource.Resource) {
	owner := "capability/photos"
	plan := backupPlanResource("photos")
	plan.Metadata.Owner = owner
	plan.BackupPlan.Volumes = []string{"app-data", "db-export"}
	hook := resource.BackupHook{Kind: kind, Container: "app-db", Database: "app", OutputVolume: "db-export"}
	container := resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindContainer,
		Metadata: resource.Metadata{Name: "app-db", Owner: owner, Node: "n1"},
		Container: &resource.ContainerSpec{
			Image: hookTestImage,
			Env: map[string]string{
				"POSTGRES_USER": "app", "POSTGRES_PASSWORD": "secret/app-db-password",
			},
			Mounts:  []resource.Mount{{Source: "app-data", Target: "/data"}},
			Restart: "unless-stopped",
		},
	}
	if kind == resource.BackupHookSQLite {
		hook.Database = "/data/app.sqlite3"
	}
	plan.BackupPlan.PreBackup = []resource.BackupHook{hook}
	desired := []resource.Resource{
		container,
		{ApiVersion: resource.ApiVersion, Kind: resource.KindVolume, Metadata: resource.Metadata{Name: "app-data", Owner: owner, Node: "n1"}, Volume: &resource.VolumeSpec{Name: "app-data"}},
		{ApiVersion: resource.ApiVersion, Kind: resource.KindVolume, Metadata: resource.Metadata{Name: "db-export", Owner: owner, Node: "n1"}, Volume: &resource.VolumeSpec{Name: "db-export"}},
		{ApiVersion: resource.ApiVersion, Kind: resource.KindVolume, Metadata: resource.Metadata{Name: "backup-repo", Owner: owner, Node: "n1"}, Volume: &resource.VolumeSpec{Name: "backup-repo"}},
		plan,
	}
	return plan, desired
}

func TestPostgresHookIsClosedAndUsesDeclaredContainer(t *testing.T) {
	plan, desired := hookFixtures(resource.BackupHookPostgres)
	e := &Engine{}
	spec, err := e.preBackupSpec(plan, plan.BackupPlan.PreBackup[0], desired)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Image != hookTestImage || spec.RunNetwork != reconcile.ContainerNetwork {
		t.Fatalf("image/network = %q/%q", spec.Image, spec.RunNetwork)
	}
	if got := ensureOp("hook", spec).GetEnsureContainer().GetNetwork(); got != reconcile.ContainerNetwork {
		t.Fatalf("wire network = %q", got)
	}
	if spec.Env["PGPASSWORD"] != "secret/app-db-password" || spec.Env["PGHOST"] != "app-db" || spec.Env["PGUSER"] != "app" {
		t.Fatalf("derived postgres env = %#v", spec.Env)
	}
	if got := strings.Join(spec.Command, " "); got != "pg_dump --no-password --format=custom --file=/wildstone-backup/app-db.dump" {
		t.Fatalf("command = %q", got)
	}
}

func TestSQLiteHookMountsLiveDatabaseReadOnly(t *testing.T) {
	plan, desired := hookFixtures(resource.BackupHookSQLite)
	e := &Engine{}
	spec, err := e.preBackupSpec(plan, plan.BackupPlan.PreBackup[0], desired)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Image != DefaultSQLiteImage || len(spec.Command) != 2 || spec.Command[0] != "/sqlite-source/app.sqlite3" || spec.Command[1] != ".backup /wildstone-backup/app-db.sqlite3" {
		t.Fatalf("sqlite spec = %+v", spec)
	}
	if len(spec.Mounts) != 2 || !spec.Mounts[0].ReadOnly || spec.Mounts[1].ReadOnly {
		t.Fatalf("sqlite mounts = %+v", spec.Mounts)
	}
}

func TestPreBackupCompletesBeforeResticReadsSources(t *testing.T) {
	plan, desired := hookFixtures(resource.BackupHookPostgres)
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	seedAll(t, state, desired...)
	if err := e.RunNow(context.Background(), plan.Metadata.Name); err != nil {
		t.Fatal(err)
	}
	names := run.names()
	want := []string{"backup-photos-probe", "backup-photos-hook-1", "backup-photos", "backup-photos-forget"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("runs = %v, want %v", names, want)
	}
}

func TestFailedPreBackupPreventsSnapshot(t *testing.T) {
	_, desired := hookFixtures(resource.BackupHookPostgres)
	run := &fakeRunner{fail: map[string]bool{"backup-photos-hook-1": true}}
	e, _, state := newEngine(t, run)
	seedAll(t, state, desired...)
	if err := e.RunNow(context.Background(), "photos"); err == nil || !strings.Contains(err.Error(), "pre-backup hook") {
		t.Fatalf("err = %v", err)
	}
	for _, name := range run.names() {
		if name == "backup-photos" {
			t.Fatal("restic snapshot ran after the database hook failed")
		}
	}
}

func TestOffsiteOnlyRunsPreBackupBeforeRepositoryPasses(t *testing.T) {
	plan, desired := hookFixtures(resource.BackupHookPostgres)
	plan.BackupPlan.Repository, plan.BackupPlan.PasswordSecret, plan.BackupPlan.Schedule = "", "", ""
	plan.BackupPlan.Retention = resource.BackupRetention{}
	plan.BackupPlan.Offsite = &resource.BackupOffsite{
		Target: "offsite", Route: resource.BackupRouteOffsiteOnly,
		PasswordSecret: "secret/offsite-password", Schedule: "daily",
	}
	desired[len(desired)-1] = plan
	desired = append(desired, targetResource(false))
	run := &fakeRunner{}
	e, _, state := newEngine(t, run)
	seedAll(t, state, desired...)
	if err := e.RunNow(context.Background(), "photos"); err != nil {
		t.Fatal(err)
	}
	names := run.names()
	if len(names) < 3 || names[0] != "backup-photos-hook-1" || names[1] != "offsite-photos-probe" || names[2] != "offsite-photos" {
		t.Fatalf("off-site runs = %v", names)
	}
}
