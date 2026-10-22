package pkgtool

import (
	"slices"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// TestDBBackedPackagesUseConsistentExports pins the package half of I10: the
// known live PostgreSQL directories are absent from restic sources, and every
// migrated database has a typed hook whose output is actually covered.
func TestDBBackedPackagesUseConsistentExports(t *testing.T) {
	type wantHook struct {
		kind, container, database, output string
	}
	cases := []struct {
		pkg, plan, forbiddenVolume string
		hooks                      []wantHook
	}{
		{"photos-immich", "photos", "immich-pgdata", []wantHook{{resource.BackupHookPostgres, "immich-postgres", "immich", "immich-db-backup"}}},
		{"documents-paperless", "documents", "paperless-pgdata", []wantHook{{resource.BackupHookPostgres, "paperless-postgres", "paperless", "paperless-db-backup"}}},
		{"matrix-synapse", "matrix", "matrix-pgdata", []wantHook{{resource.BackupHookPostgres, "matrix-postgres", "synapse", "matrix-db-backup"}}},
		{"code-gitea", "code", "", []wantHook{{resource.BackupHookSQLite, "gitea", "/data/gitea/gitea.db", "gitea-db-backup"}}},
		{"recipes-mealie", "recipes", "", []wantHook{{resource.BackupHookSQLite, "mealie", "/app/data/mealie.db", "mealie-db-backup"}}},
		{"mail-roundcube", "mail", "", []wantHook{
			{resource.BackupHookSQLite, "roundcube", "/var/roundcube/db/sqlite.db", "mail-db-backup"},
			{resource.BackupHookSQLite, "davis", "/data/davis.db", "mail-db-backup"},
		}},
	}
	rendered := renderShippedPackages(t)
	for _, tc := range cases {
		t.Run(tc.pkg, func(t *testing.T) {
			var plan *resource.BackupPlanSpec
			for _, r := range rendered[tc.pkg] {
				if r.Kind == resource.KindBackupPlan && r.Metadata.Name == tc.plan {
					plan = r.BackupPlan
					break
				}
			}
			if plan == nil {
				t.Fatalf("BackupPlan/%s not rendered", tc.plan)
			}
			if tc.forbiddenVolume != "" && slices.Contains(plan.Volumes, tc.forbiddenVolume) {
				t.Fatalf("live database volume %q is still a restic source", tc.forbiddenVolume)
			}
			for _, want := range tc.hooks {
				found := false
				for _, got := range plan.PreBackup {
					if got.Kind == want.kind && got.Container == want.container && got.Database == want.database && got.OutputVolume == want.output {
						found = true
					}
				}
				if !found {
					t.Errorf("missing hook %+v; got %+v", want, plan.PreBackup)
				}
				if !slices.Contains(plan.Volumes, want.output) {
					t.Errorf("hook output volume %q is not backed up", want.output)
				}
			}
		})
	}
}
