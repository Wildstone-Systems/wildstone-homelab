package backup

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/reconcile"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// DefaultSQLiteImage is a multi-architecture keinos/sqlite3 3.50.4 image.
// Its fixed sqlite3 entrypoint lets the engine pass only a database path and
// the .backup command. BackupPlan authors never choose an image or command.
const DefaultSQLiteImage = "keinos/sqlite3@sha256:7ea29f0c7e91a8c3f315e831459d07000f34e9e9b25fbc30be2e0481b3e0450f"

const hookOutputDir = "/wildstone-backup"

func (e *Engine) runPreBackup(ctx context.Context, plan resource.Resource, desired []resource.Resource) error {
	for i, hook := range plan.BackupPlan.PreBackup {
		spec, err := e.preBackupSpec(plan, hook, desired)
		if err != nil {
			return fmt.Errorf("pre-backup hook %d: %w", i+1, err)
		}
		name := fmt.Sprintf("backup-%s-hook-%d", plan.Metadata.Name, i+1)
		if err := e.Run.RunContainer(ctx, plan.Metadata.Node, name, spec); err != nil {
			return fmt.Errorf("pre-backup hook %d (%s): %w", i+1, hook.Kind, err)
		}
	}
	return nil
}

func (e *Engine) preBackupSpec(plan resource.Resource, hook resource.BackupHook, desired []resource.Resource) (resource.ContainerSpec, error) {
	container, ok := ownedContainer(desired, plan, hook.Container)
	if !ok {
		return resource.ContainerSpec{}, fmt.Errorf("container %q is not declared by %s on node %s", hook.Container, plan.Metadata.Owner, plan.Metadata.Node)
	}
	if !ownedVolume(desired, plan, hook.OutputVolume) {
		return resource.ContainerSpec{}, fmt.Errorf("output volume %q is not declared by %s on node %s", hook.OutputVolume, plan.Metadata.Owner, plan.Metadata.Node)
	}
	for _, mount := range container.Container.Mounts {
		if mount.Source == hook.OutputVolume {
			return resource.ContainerSpec{}, fmt.Errorf("output volume %q must be separate from container %q live data", hook.OutputVolume, hook.Container)
		}
	}
	output := path.Join(hookOutputDir, hook.Container+hookOutputExtension(hook.Kind))
	switch hook.Kind {
	case resource.BackupHookPostgres:
		user := container.Container.Env["POSTGRES_USER"]
		if user == "" {
			return resource.ContainerSpec{}, fmt.Errorf("container %q has no POSTGRES_USER", hook.Container)
		}
		env := map[string]string{"PGUSER": user, "PGDATABASE": hook.Database}
		spec := resource.ContainerSpec{
			Image: container.Container.Image,
			Env:   env,
			Mounts: []resource.Mount{
				{Source: hook.OutputVolume, Target: hookOutputDir},
			},
			Command: []string{"pg_dump", "--no-password", "--format=custom", "--file=" + output},
			Restart: "no",
		}
		if password := container.Container.Env["POSTGRES_PASSWORD"]; password != "" {
			env["PGPASSWORD"] = password
		}
		if socket, ok := mountAt(container.Container.Mounts, "/run/postgresql"); ok {
			env["PGHOST"] = socket.Target
			spec.Mounts = append(spec.Mounts, resource.Mount{Source: socket.Source, Target: socket.Target})
		} else {
			env["PGHOST"] = hook.Container
			spec.RunNetwork = reconcile.ContainerNetwork
		}
		return spec, nil

	case resource.BackupHookSQLite:
		mount, relative, ok := mountContaining(container.Container.Mounts, hook.Database)
		if !ok || strings.HasPrefix(mount.Source, "/") {
			return resource.ContainerSpec{}, fmt.Errorf("sqlite database %q is not inside a named volume mounted by container %q", hook.Database, hook.Container)
		}
		if !ownedVolume(desired, plan, mount.Source) {
			return resource.ContainerSpec{}, fmt.Errorf("sqlite source volume %q is not declared by %s on node %s", mount.Source, plan.Metadata.Owner, plan.Metadata.Node)
		}
		image := e.SQLiteImage
		if image == "" {
			image = DefaultSQLiteImage
		}
		return resource.ContainerSpec{
			Image: image,
			Mounts: []resource.Mount{
				{Source: mount.Source, Target: "/sqlite-source", ReadOnly: true},
				{Source: hook.OutputVolume, Target: hookOutputDir},
			},
			// DefaultSQLiteImage's entrypoint is sqlite3. Both arguments are
			// engine-derived and reach it directly, never through a shell.
			Command: []string{path.Join("/sqlite-source", relative), ".backup " + output},
			Restart: "no",
		}, nil
	default:
		return resource.ContainerSpec{}, fmt.Errorf("unsupported hook kind %q", hook.Kind)
	}
}

func hookOutputExtension(kind string) string {
	if kind == resource.BackupHookPostgres {
		return ".dump"
	}
	return ".sqlite3"
}

func ownedContainer(desired []resource.Resource, plan resource.Resource, name string) (resource.Resource, bool) {
	for _, r := range desired {
		if r.Kind == resource.KindContainer && r.Container != nil && r.Metadata.Name == name &&
			r.Metadata.Node == plan.Metadata.Node && r.Metadata.Owner == plan.Metadata.Owner {
			return r, true
		}
	}
	return resource.Resource{}, false
}

func ownedVolume(desired []resource.Resource, plan resource.Resource, name string) bool {
	for _, r := range desired {
		if r.Kind == resource.KindVolume && r.Volume != nil && r.Volume.Name == name &&
			r.Metadata.Node == plan.Metadata.Node && r.Metadata.Owner == plan.Metadata.Owner {
			return true
		}
	}
	return false
}

func mountAt(mounts []resource.Mount, target string) (resource.Mount, bool) {
	for _, mount := range mounts {
		if mount.Target == target {
			return mount, true
		}
	}
	return resource.Mount{}, false
}

func mountContaining(mounts []resource.Mount, database string) (resource.Mount, string, bool) {
	var best resource.Mount
	for _, mount := range mounts {
		if database == mount.Target || strings.HasPrefix(database, strings.TrimSuffix(mount.Target, "/")+"/") {
			if len(mount.Target) > len(best.Target) {
				best = mount
			}
		}
	}
	if best.Target == "" || database == best.Target {
		return resource.Mount{}, "", false
	}
	return best, strings.TrimPrefix(database, strings.TrimSuffix(best.Target, "/")+"/"), true
}
