package resource

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/proto/recoveryseal"
)

func TestReservedWholeSiteBackupPlan(t *testing.T) {
	publicKey, _, err := recoveryseal.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	plan := Resource{
		ApiVersion: ApiVersion, Kind: KindBackupPlan,
		Metadata: Metadata{Name: "wildstone", Owner: "system", Node: "n1"},
		BackupPlan: &BackupPlanSpec{
			Paths: []string{WholeSiteRecoveryPath}, Class: "critical", RecoveryPublicKey: publicKey,
			Offsite: &BackupOffsite{
				Target: "offsite", Route: BackupRouteOffsiteOnly,
				PasswordSecret: "secret/restic-wildstone-offsite", Schedule: "daily",
				Retention: BackupRetention{KeepDaily: 7},
			},
		},
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("valid reserved plan: %v", err)
	}
	for name, mutate := range map[string]func(*Resource){
		"missing key": func(r *Resource) { r.BackupPlan.RecoveryPublicKey = "" },
		"wrong name":  func(r *Resource) { r.Metadata.Name = "not-wildstone" },
		"wrong owner": func(r *Resource) { r.Metadata.Owner = "owner" },
		"local route": func(r *Resource) { r.BackupPlan.Offsite.Route = BackupRouteLocalAndOffsite },
		"wrong path":  func(r *Resource) { r.BackupPlan.Paths[0] = "/srv/wildstone/other" },
	} {
		copy := plan
		spec := *plan.BackupPlan
		offsite := *plan.BackupPlan.Offsite
		spec.Paths = append([]string(nil), plan.BackupPlan.Paths...)
		spec.Offsite = &offsite
		copy.BackupPlan = &spec
		mutate(&copy)
		if err := copy.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func backupTarget() Resource {
	return Resource{
		ApiVersion: ApiVersion,
		Kind:       KindBackupTarget,
		Metadata:   Metadata{Name: "offsite", Owner: "owner", Node: "n1"},
		BackupTarget: &BackupTargetSpec{
			Type:            "s3",
			Endpoint:        "https://s3.us-east-1.amazonaws.com",
			Bucket:          "wildstone-backups",
			Prefix:          "home",
			Region:          "us-east-1",
			AccessKeySecret: "secret/offsite-s3-access-key",
			SecretKeySecret: "secret/offsite-s3-secret-key",
		},
	}
}

// TestBackupTargetProviders: the endpoints of the providers ADR-W045
// names all validate, with no provider-specific field.
func TestBackupTargetProviders(t *testing.T) {
	for name, mut := range map[string]func(*BackupTargetSpec){
		"aws": func(s *BackupTargetSpec) {},
		"r2": func(s *BackupTargetSpec) {
			s.Endpoint = "https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com"
			s.Region = "auto"
		},
		"b2": func(s *BackupTargetSpec) {
			s.Endpoint = "https://s3.us-west-004.backblazeb2.com"
			s.Region = "us-west-004"
			s.Bucket = "Wildstone-Backups"
		},
		"minio-lan": func(s *BackupTargetSpec) {
			s.Endpoint = "http://nas.lan:9000"
			s.InsecureHTTP = true
			s.PathStyle = true
			s.Region = ""
		},
		"nested prefix":  func(s *BackupTargetSpec) { s.Prefix = "wildstone/home-1" },
		"no prefix":      func(s *BackupTargetSpec) { s.Prefix = "" },
		"trailing slash": func(s *BackupTargetSpec) { s.Endpoint = "https://s3.amazonaws.com/" },
	} {
		r := backupTarget()
		mut(r.BackupTarget)
		if err := r.Validate(); err != nil {
			t.Errorf("%s: valid target rejected: %v", name, err)
		}
	}
}

// TestBackupTargetRejects: each malformed field is refused and named.
func TestBackupTargetRejects(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		mut         func(*BackupTargetSpec)
	}{
		{"unknown type", "spec.type", func(s *BackupTargetSpec) { s.Type = "gcs" }},
		{"no endpoint", "spec.endpoint", func(s *BackupTargetSpec) { s.Endpoint = "" }},
		{"http without opt-in", "spec.endpoint", func(s *BackupTargetSpec) { s.Endpoint = "http://nas.lan:9000" }},
		{"other scheme", "spec.endpoint", func(s *BackupTargetSpec) { s.Endpoint = "ftp://host" }},
		{"credentials in url", "spec.endpoint", func(s *BackupTargetSpec) { s.Endpoint = "https://AKIA:secret@s3.amazonaws.com" }},
		{"path in url", "spec.endpoint", func(s *BackupTargetSpec) { s.Endpoint = "https://s3.amazonaws.com/bucket" }},
		{"query in url", "spec.endpoint", func(s *BackupTargetSpec) { s.Endpoint = "https://s3.amazonaws.com?x=1" }},
		{"no host", "spec.endpoint", func(s *BackupTargetSpec) { s.Endpoint = "https://" }},
		{"bucket too short", "spec.bucket", func(s *BackupTargetSpec) { s.Bucket = "ab" }},
		{"bucket bad char", "spec.bucket", func(s *BackupTargetSpec) { s.Bucket = "my_bucket" }},
		{"bucket double dot", "spec.bucket", func(s *BackupTargetSpec) { s.Bucket = "my..bucket" }},
		{"bucket edge hyphen", "spec.bucket", func(s *BackupTargetSpec) { s.Bucket = "-bucket" }},
		{"prefix leading slash", "spec.prefix", func(s *BackupTargetSpec) { s.Prefix = "/home" }},
		{"prefix trailing slash", "spec.prefix", func(s *BackupTargetSpec) { s.Prefix = "home/" }},
		{"prefix double slash", "spec.prefix", func(s *BackupTargetSpec) { s.Prefix = "a//b" }},
		{"prefix dotdot", "spec.prefix", func(s *BackupTargetSpec) { s.Prefix = "a/../b" }},
		{"prefix too long", "spec.prefix", func(s *BackupTargetSpec) { s.Prefix = strings.Repeat("a", 257) }},
		{"bad region", "spec.region", func(s *BackupTargetSpec) { s.Region = "US East" }},
		{"literal access key", "spec.accessKeySecret", func(s *BackupTargetSpec) { s.AccessKeySecret = "REPLACED-" + strings.Repeat("A", 16) }},
		{"literal secret key", "spec.secretKeySecret", func(s *BackupTargetSpec) { s.SecretKeySecret = "REPLACED-" + strings.Repeat("k7Qz", 8) }},
		{"same secret twice", "spec.secretKeySecret", func(s *BackupTargetSpec) { s.SecretKeySecret = s.AccessKeySecret }},
	} {
		r := backupTarget()
		tc.mut(r.BackupTarget)
		err := r.Validate()
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: error %q does not name %s", tc.name, err, tc.field)
		}
	}
}

// TestBackupTargetNodeScoped: credential references resolve in one
// node's secret store, so a target must name its node.
func TestBackupTargetNodeScoped(t *testing.T) {
	r := backupTarget()
	r.Metadata.Node = ""
	if err := r.Validate(); err == nil {
		t.Fatal("a BackupTarget with no node was accepted")
	}
}

// TestBackupTargetRoundTrip: the documented YAML decodes strictly and
// survives a round trip with every field.
func TestBackupTargetRoundTrip(t *testing.T) {
	doc := `apiVersion: wildstone.dev/v1
kind: BackupTarget
metadata:
  name: offsite
  owner: owner
  node: n1
spec:
  type: s3
  endpoint: https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com
  bucket: wildstone-backups
  prefix: home
  region: auto
  pathStyle: true
  appendOnly: true
  accessKeySecret: secret/offsite-s3-access-key
  secretKeySecret: secret/offsite-s3-secret-key
`
	r, err := Unmarshal([]byte(doc))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("documented target invalid: %v", err)
	}
	data, err := Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("round trip decode: %v", err)
	}
	if back.BackupTarget == nil || *back.BackupTarget != *r.BackupTarget {
		t.Fatalf("round trip changed the spec: %+v vs %+v", back.BackupTarget, r.BackupTarget)
	}

	// A typo is refused, not ignored: an unknown field is how a
	// credential ends up somewhere validation never looks.
	if _, err := Unmarshal([]byte(strings.Replace(doc, "accessKeySecret", "accessKey", 1))); err == nil {
		t.Fatal("unknown field accessKey was accepted")
	}
}

func offsitePlanRes(route string) Resource {
	r := Resource{
		ApiVersion: ApiVersion,
		Kind:       KindBackupPlan,
		Metadata:   Metadata{Name: "photos", Owner: "capability/photos", Node: "n1"},
		BackupPlan: &BackupPlanSpec{
			Volumes:        []string{"immich-library"},
			Repository:     "backup-repo",
			PasswordSecret: "secret/restic-photos",
			Schedule:       "daily",
			Offsite: &BackupOffsite{
				Target: "offsite", Route: route, PasswordSecret: "secret/restic-photos-offsite",
				Schedule: "weekly", Retention: BackupRetention{KeepWeekly: 8},
			},
		},
	}
	if route == BackupRouteOffsiteOnly {
		r.BackupPlan.Repository, r.BackupPlan.PasswordSecret, r.BackupPlan.Schedule = "", "", ""
	}
	return r
}

// TestBackupPlanOffsiteValidates: both routes, as documented.
func TestBackupPlanOffsiteValidates(t *testing.T) {
	for _, route := range []string{BackupRouteLocalAndOffsite, BackupRouteOffsiteOnly} {
		r := offsitePlanRes(route)
		if err := r.Validate(); err != nil {
			t.Errorf("%s: %v", route, err)
		}
		data, err := Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		back, err := Unmarshal(data)
		if err != nil {
			t.Fatalf("%s: round trip: %v", route, err)
		}
		if back.BackupPlan.Offsite == nil || *back.BackupPlan.Offsite != *r.BackupPlan.Offsite {
			t.Errorf("%s: offsite lost in round trip: %+v", route, back.BackupPlan.Offsite)
		}
	}
}

// TestBackupPlanOffsiteRejects: each bad off-site setting is refused and
// the field named.
func TestBackupPlanOffsiteRejects(t *testing.T) {
	for _, tc := range []struct {
		name, route, field string
		mut                func(*BackupPlanSpec)
	}{
		{"no target", BackupRouteLocalAndOffsite, "spec.offsite.target", func(p *BackupPlanSpec) { p.Offsite.Target = "" }},
		{"bad route", BackupRouteLocalAndOffsite, "spec.offsite.route", func(p *BackupPlanSpec) { p.Offsite.Route = "cloud" }},
		{"literal password", BackupRouteLocalAndOffsite, "spec.offsite.passwordSecret", func(p *BackupPlanSpec) { p.Offsite.PasswordSecret = "hunter2" }},
		{"shared password", BackupRouteLocalAndOffsite, "spec.offsite.passwordSecret", func(p *BackupPlanSpec) { p.Offsite.PasswordSecret = p.PasswordSecret }},
		{"bad schedule", BackupRouteLocalAndOffsite, "spec.offsite.schedule", func(p *BackupPlanSpec) { p.Offsite.Schedule = "hourly" }},
		{"negative retention", BackupRouteLocalAndOffsite, "spec.offsite.retention", func(p *BackupPlanSpec) { p.Offsite.Retention.KeepDaily = -1 }},
		{"local needs repository", BackupRouteLocalAndOffsite, "spec.repository", func(p *BackupPlanSpec) { p.Repository = "" }},
		{"offsite-only with repository", BackupRouteOffsiteOnly, "spec.repository", func(p *BackupPlanSpec) { p.Repository = "repo" }},
		{"offsite-only with local password", BackupRouteOffsiteOnly, "spec.passwordSecret", func(p *BackupPlanSpec) { p.PasswordSecret = "secret/x" }},
		{"offsite-only with local schedule", BackupRouteOffsiteOnly, "spec.schedule", func(p *BackupPlanSpec) { p.Schedule = "daily" }},
		{"offsite-only with local retention", BackupRouteOffsiteOnly, "spec.retention", func(p *BackupPlanSpec) { p.Retention.KeepDaily = 7 }},
	} {
		r := offsitePlanRes(tc.route)
		tc.mut(r.BackupPlan)
		err := r.Validate()
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: error %q does not name %s", tc.name, err, tc.field)
		}
	}
}

func nasPlanRes() Resource {
	return Resource{
		ApiVersion: ApiVersion, Kind: KindBackupPlan,
		Metadata: Metadata{Name: "movies", Owner: "owner", Node: "n1"},
		BackupPlan: &BackupPlanSpec{
			Paths:          []string{"/srv/wildstone/nas/media/My Movies"},
			Repository:     "/srv/wildstone/nas/backups/movies",
			PasswordSecret: "secret/restic-movies",
			Schedule:       "weekly",
			Reserve:        "50GiB",
		},
	}
}

// TestBackupPlanPathsAndReserve: NAS folders (spaces allowed) as sources,
// a path as the repository, and a reserve; bad ones are named.
func TestBackupPlanPathsAndReserve(t *testing.T) {
	if r := nasPlanRes(); r.Validate() != nil {
		err := r.Validate()
		t.Fatalf("valid NAS plan rejected: %v", err)
	}
	for _, tc := range []struct {
		name, field string
		mut         func(*BackupPlanSpec)
	}{
		{"no sources", "spec.volumes", func(p *BackupPlanSpec) { p.Paths = nil }},
		{"relative path", "spec.paths[0]", func(p *BackupPlanSpec) { p.Paths = []string{"nas/media"} }},
		{"unclean path", "spec.paths[0]", func(p *BackupPlanSpec) { p.Paths = []string{"/srv/wildstone/nas/../etc"} }},
		{"root", "spec.paths[0]", func(p *BackupPlanSpec) { p.Paths = []string{"/"} }},
		{"control char", "spec.paths[0]", func(p *BackupPlanSpec) { p.Paths = []string{"/srv/a\nb"} }},
		{"duplicate", "spec.paths[1]", func(p *BackupPlanSpec) { p.Paths = []string{"/srv/a", "/srv/a"} }},
		{"repo inside source", "spec.repository", func(p *BackupPlanSpec) { p.Repository = "/srv/wildstone/nas/media/My Movies/.repo" }},
		{"repo holds source", "spec.repository", func(p *BackupPlanSpec) { p.Repository = "/srv/wildstone/nas/media" }},
		{"zero reserve", "spec.reserve", func(p *BackupPlanSpec) { p.Reserve = "0GiB" }},
		{"bad reserve", "spec.reserve", func(p *BackupPlanSpec) { p.Reserve = "lots" }},
		{"reserve 100%", "spec.reserve", func(p *BackupPlanSpec) { p.Reserve = "100%" }},
	} {
		r := nasPlanRes()
		tc.mut(r.BackupPlan)
		err := r.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: err = %v, want %s named", tc.name, err, tc.field)
		}
	}
	for in, want := range map[string]uint64{"50GiB": 50 << 30, "2TiB": 2 << 40, "500MB": 500e6, "1.5GiB": 3 << 29} {
		if b, _, err := ParseReserve(in); err != nil || b != want {
			t.Errorf("ParseReserve(%q) = %d, %v; want %d", in, b, err, want)
		}
	}
	if _, pct, err := ParseReserve("15%"); err != nil || pct != 15 {
		t.Errorf("ParseReserve(15%%) = %v, %v", pct, err)
	}
}

func nfsMount() Resource {
	return Resource{
		ApiVersion: ApiVersion, Kind: KindNetworkMount,
		Metadata: Metadata{Name: "media", Owner: "owner", Node: "n1"},
		NetworkMount: &NetworkMountSpec{Type: "nfs4", Source: "nas.example.internal:/volume1/media",
			Target: "/srv/wildstone/nas/media", Options: []string{"ro", "vers=4.1", "soft"}},
	}
}

// TestNetworkMountValidates: NFS shares in, SMB and anything that could
// corrupt /etc/fstab or leak a password into it out.
func TestNetworkMountValidates(t *testing.T) {
	if r := nfsMount(); r.Validate() != nil {
		err := r.Validate()
		t.Fatalf("valid mount rejected: %v", err)
	}
	for _, tc := range []struct {
		name, field string
		mut         func(*NetworkMountSpec)
	}{
		{"smb", "spec.type", func(m *NetworkMountSpec) { m.Type = "cifs" }},
		{"no export", "spec.source", func(m *NetworkMountSpec) { m.Source = "nas.example.internal" }},
		{"source with space", "spec.source", func(m *NetworkMountSpec) { m.Source = "nas:/my share" }},
		{"source with comma", "spec.source", func(m *NetworkMountSpec) { m.Source = "nas:/a,rw" }},
		{"relative target", "spec.target", func(m *NetworkMountSpec) { m.Target = "nas/media" }},
		{"target with space", "spec.target", func(m *NetworkMountSpec) { m.Target = "/srv/wildstone/my media" }},
		{"option with comma", "spec.options[0]", func(m *NetworkMountSpec) { m.Options = []string{"ro,exec"} }},
		{"password option", "spec.options[0]", func(m *NetworkMountSpec) { m.Options = []string{"password=REPLACED-x"} }},
		{"credentials option", "spec.options[0]", func(m *NetworkMountSpec) { m.Options = []string{"credentials=/root/.smb"} }},
	} {
		r := nfsMount()
		tc.mut(r.NetworkMount)
		err := r.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: err = %v, want %s named", tc.name, err, tc.field)
		}
	}
}

// TestBackupPlanExcludeAndClass: excludes that restic can read, and the
// three classes; anything else named and refused.
func TestBackupPlanExcludeAndClass(t *testing.T) {
	r := nasPlanRes()
	r.BackupPlan.Exclude = []string{"*.nfo", "**/Samples", "/srv/wildstone/nas/media/My Movies/Extras"}
	r.BackupPlan.Class = "important"
	if err := r.Validate(); err != nil {
		t.Fatalf("valid excludes rejected: %v", err)
	}
	for _, tc := range []struct {
		name, field string
		mut         func(*BackupPlanSpec)
	}{
		{"empty", "spec.exclude[0]", func(p *BackupPlanSpec) { p.Exclude = []string{""} }},
		{"padded", "spec.exclude[0]", func(p *BackupPlanSpec) { p.Exclude = []string{" *.nfo"} }},
		{"negated", "spec.exclude[0]", func(p *BackupPlanSpec) { p.Exclude = []string{"!keep.txt"} }},
		{"control", "spec.exclude[0]", func(p *BackupPlanSpec) { p.Exclude = []string{"a\nb"} }},
		{"absolute outside paths", "spec.exclude[0]", func(p *BackupPlanSpec) { p.Exclude = []string{"/etc/passwd"} }},
		{"too many", "spec.exclude", func(p *BackupPlanSpec) { p.Exclude = make([]string, 101) }},
		{"bad class", "spec.class", func(p *BackupPlanSpec) { p.Class = "precious" }},
	} {
		r := nasPlanRes()
		tc.mut(r.BackupPlan)
		if err := r.Validate(); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: err = %v, want %s named", tc.name, err, tc.field)
		}
	}
}
