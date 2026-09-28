package resource

import (
	"strings"
	"testing"
)

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
		{"literal access key", "spec.accessKeySecret", func(s *BackupTargetSpec) { s.AccessKeySecret = "AKIAIOSFODNN7EXAMPLE" }},
		{"literal secret key", "spec.secretKeySecret", func(s *BackupTargetSpec) { s.SecretKeySecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" }},
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
