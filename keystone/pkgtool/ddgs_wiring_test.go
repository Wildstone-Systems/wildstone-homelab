package pkgtool

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/capability"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/research"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"path/filepath"
	"strings"
	"testing"
)

func TestDDGSPackageIsExplicitSingletonAndSigns(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "packages", research.ManagedPackage)
	p, err := pkgspec.Load(filepath.Join(dir, "package.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Metadata.Instanced {
		t.Fatal("worker must be shared")
	}
	engine := capability.NewEngine()
	catalog := capability.NewCatalog(p)
	if _, err := engine.Expand(p, map[string]string{"node": "n1"}, catalog); err == nil {
		t.Fatal("unverified default image installed")
	}
	image := "registry.example.internal/ddgs@sha256:" + strings.Repeat("1", 64)
	rs, err := engine.Expand(p, map[string]string{"node": "n1", "worker_image": image}, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Kind != resource.KindContainer {
		t.Fatalf("unexpected resources: %+v", rs)
	}
	c := rs[0].Container
	if len(c.Mounts) != 0 || len(c.Env) != 1 || c.Env["DDGS_LISTEN_HOST"] != "0.0.0.0" || !c.ReadOnlyRootfs || !c.NoNewPrivileges || len(c.CapDrop) != 1 || c.CapDrop[0] != "ALL" || c.Resources == nil || c.Resources.PidsLimit != 64 {
		t.Fatalf("worker isolation/resources: %+v", c)
	}
	if _, err := research.ManagedEndpoint(rs, "n1", research.ManagedEndpointRef); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"example/ddgs:latest", "example/ddgs@sha256:bad"} {
		if _, err := engine.Expand(p, map[string]string{"node": "n1", "worker_image": bad}, catalog); err == nil {
			t.Fatal("accepted unpinned image")
		}
	}
	var a, b bytes.Buffer
	sum, err := BuildArchive(dir, &a)
	if err != nil {
		t.Fatal(err)
	}
	sum2, err := BuildArchive(dir, &b)
	if err != nil || sum != sum2 || !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("non deterministic archive")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sig := SignArchive(priv, sum)
	if !VerifyArchive(pub, sum, sig) || VerifyArchive(pub, strings.Repeat("0", 64), sig) {
		t.Fatal("signature verification failed")
	}
}
