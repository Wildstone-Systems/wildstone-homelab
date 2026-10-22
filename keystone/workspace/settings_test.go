package workspace

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

func validSettings(root string) Settings {
	return Settings{Root: root, QuotaBytes: 1 << 30, MaxFileBytes: MaxFileBytes, ScratchRetentionDays: 7, TrashRetentionDays: 30}
}
func config(root string) resource.Resource {
	return resource.Resource{Kind: resource.KindConfigFile, Metadata: resource.Metadata{Node: "n1", Owner: "capability/workspace"}, ConfigFile: &resource.ConfigFileSpec{Filename: SettingsFile, Content: fmt.Sprintf("root: %q\nquotaBytes: 1073741824\nmaxFileBytes: 10485760\nscratchRetentionDays: 7\ntrashRetentionDays: 30\n", root)}}
}
func TestReadFailsClosedAndRevisionTracksConfiguration(t *testing.T) {
	r := config(DefaultRoot)
	good, err := Read([]resource.Resource{r}, "n1")
	if err != nil || good.Root != DefaultRoot || good.Revision == "" {
		t.Fatalf("%+v %v", good, err)
	}
	for name, change := range map[string]func(*resource.Resource){
		"other owner":       func(r *resource.Resource) { r.Metadata.Owner = "capability/other" },
		"unqualified owner": func(r *resource.Resource) { r.Metadata.Owner = "workspace" },
		"other filename":    func(r *resource.Resource) { r.ConfigFile.Filename = "other.yaml" },
		"other node":        func(r *resource.Resource) { r.Metadata.Node = "n2" },
		"malformed":         func(r *resource.Resource) { r.ConfigFile.Content = "root: [" },
		"unknown":           func(r *resource.Resource) { r.ConfigFile.Content += "unexpected: true\n" },
		"duplicate field":   func(r *resource.Resource) { r.ConfigFile.Content += "root: /tmp/other\n" },
		"extra document":    func(r *resource.Resource) { r.ConfigFile.Content += "---\nroot: /tmp/other\n" },
	} {
		t.Run(name, func(t *testing.T) {
			r := config(DefaultRoot)
			change(&r)
			if _, err := Read([]resource.Resource{r}, "n1"); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
	for _, node := range []string{"", "n2"} {
		if _, err := Read([]resource.Resource{r}, node); !errors.Is(err, ErrNode) {
			t.Fatal(err)
		}
	}
	if _, err := Read(nil, "n1"); !errors.Is(err, ErrDisabled) {
		t.Fatal(err)
	}
	if _, err := Read([]resource.Resource{r, r}, "n1"); !errors.Is(err, ErrSettings) {
		t.Fatal(err)
	}
	r.ConfigFile.Content = strings.ReplaceAll(r.ConfigFile.Content, "1073741824", "2147483648")
	changed, err := Read([]resource.Resource{r}, "n1")
	if err != nil || changed.Revision == good.Revision {
		t.Fatal("config revision did not change", err)
	}
}
func TestSettingsRefuseYAMLMergesAndAliases(t *testing.T) {
	for _, content := range []string{
		"root: /srv/workspace\n<<: {quotaBytes: 1073741824.5, maxFileBytes: 1024, scratchRetentionDays: 7, trashRetentionDays: 30}\n",
		"root: /srv/workspace\nquotaBytes: &quota 1073741824\nmaxFileBytes: 1024\nscratchRetentionDays: &days 7\ntrashRetentionDays: *days\n",
	} {
		if _, err := Parse(content); !errors.Is(err, ErrSettings) {
			t.Fatal("accepted indirect YAML settings", err)
		}
	}
}

func TestSettingsRequireStringPaths(t *testing.T) {
	for _, field := range []string{"root", "requiredMount"} {
		for _, value := range []string{"null", "~", "", "true", "42", "!!null anything"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				r := config(DefaultRoot)
				content := r.ConfigFile.Content
				if field == "root" {
					content = strings.Replace(content, fmt.Sprintf("root: %q", DefaultRoot), "root: "+value, 1)
				} else {
					content += "requiredMount: " + value + "\n"
				}
				if _, err := Parse(content); !errors.Is(err, ErrSettings) {
					t.Fatal("accepted non-string path", err)
				}
			})
		}
	}
	// Explicit empty strings remain the supported local-disk opt-out.
	for _, value := range []string{`""`, `''`} {
		settings, err := Parse(config(DefaultRoot).ConfigFile.Content + "requiredMount: " + value + "\n")
		if err != nil || settings.RequiredMount != "" {
			t.Fatalf("explicit local disk settings: %+v %v", settings, err)
		}
	}
}

func TestSettingsBounds(t *testing.T) {
	for name, change := range map[string]func(*Settings){
		"relative": func(s *Settings) { s.Root = "relative" }, "filesystem root": func(s *Settings) { s.Root = "/" },
		"unclean": func(s *Settings) { s.Root = "/srv/../tmp" }, "control": func(s *Settings) { s.Root = "/srv/\nx" },
		"zero quota": func(s *Settings) { s.QuotaBytes = 0 }, "huge quota": func(s *Settings) { s.QuotaBytes = MaxQuotaBytes + 1 },
		"large file": func(s *Settings) { s.MaxFileBytes = MaxFileBytes + 1 }, "zero file": func(s *Settings) { s.MaxFileBytes = 0 },
		"file exceeds quota": func(s *Settings) { s.QuotaBytes = 1 },
		"zero scratch":       func(s *Settings) { s.ScratchRetentionDays = 0 }, "long trash": func(s *Settings) { s.TrashRetentionDays = MaxRetentionDays + 1 },
		"outside mount": func(s *Settings) { s.RequiredMount = "/srv/other" }, "root mount": func(s *Settings) { s.RequiredMount = "/" },
	} {
		t.Run(name, func(t *testing.T) {
			s := validSettings(DefaultRoot)
			change(&s)
			if s.Validate() == nil {
				t.Fatal("accepted invalid settings")
			}
		})
	}
	for _, v := range []string{"1.5", "0", "-1", "18446744073709551616", "null", ".inf"} {
		r := config(DefaultRoot)
		r.ConfigFile.Content = strings.ReplaceAll(r.ConfigFile.Content, "1073741824", v)
		if _, err := Parse(r.ConfigFile.Content); err == nil {
			t.Errorf("accepted quota %s", v)
		}
	}
}
func TestReadinessNeverCreatesOrDeletesStorage(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	s := validSettings(root)
	if err := s.Ready(); err == nil {
		t.Fatal("missing root ready")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created missing root", err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "retained.txt")
	if err := os.WriteFile(file, []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(); err != nil {
		t.Fatal(err)
	}
	s.RequiredMount = base // Ordinary existing directory is not a mounted filesystem.
	if err := s.Ready(); !errors.Is(err, ErrMount) {
		t.Fatal("unmounted directory accepted", err)
	}
	if body, err := os.ReadFile(file); err != nil || string(body) != "retained" {
		t.Fatal("readiness modified bytes", err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(); !errors.Is(err, ErrMount) {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing mount created replacement", err)
	}
	s.RequiredMount = ""
	if err := os.Symlink(base, root); err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(); !errors.Is(err, ErrRoot) {
		t.Fatal("symlink root accepted", err)
	}
}
func TestMountIdentityAndEscapedPaths(t *testing.T) {
	line := `42 1 0:3 / /mnt/work\040space rw - ext4 /dev/example rw` + "\n"
	if !mountHostsRoot(strings.NewReader(line), "/mnt/work space", "/mnt/work space/workspace", "42") {
		t.Fatal("escaped mount not found")
	}
	if mountHostsRoot(strings.NewReader(line), "/mnt/work space", "/mnt/work space/workspace", "99") {
		t.Fatal("replacement mount ID accepted")
	}
	if mountHostsRoot(strings.NewReader(line), "/mnt/work", "/mnt/work/workspace", "42") {
		t.Fatal("prefix accepted")
	}
}

func TestPinnedMountRefusesReplacementAndLoss(t *testing.T) {
	for _, tc := range []struct {
		name, second, fd, rootFD string
		want                     bool
	}{
		{"stable", "42", "mnt_id: 42\n", "mnt_id: 42\n", true},
		{"disappeared", "", "mnt_id: 42\n", "mnt_id: 42\n", false},
		{"replaced", "43", "mnt_id: 42\n", "mnt_id: 42\n", false},
		{"underlying local directory", "42", "mnt_id: 1\n", "mnt_id: 1\n", false},
		{"missing fd identity", "42", "", "", false},
		{"nested mount added during open", "42", "mnt_id: 42\n", "mnt_id: 43\n", false},
		{"root fell back to underlying mount", "42", "mnt_id: 42\n", "mnt_id: 1\n", false},
		{"missing root identity", "42", "mnt_id: 42\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mount := t.TempDir()
			root := filepath.Join(mount, "workspace")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			s := validSettings(root)
			s.RequiredMount = mount
			calls := 0
			readMount := func() (io.ReadCloser, error) {
				calls++
				id := "42"
				if calls > 1 {
					id = tc.second
				}
				line := ""
				if id != "" {
					line = id + " 1 0:3 / " + mount + " rw - ext4 /dev/example rw\n"
				}
				return io.NopCloser(strings.NewReader(line)), nil
			}
			fdCalls := 0
			opened, err := s.openMountedRootWith(readMount, func(uintptr) ([]byte, error) {
				fdCalls++
				if fdCalls == 1 {
					return []byte(tc.fd), nil
				}
				return []byte(tc.rootFD), nil
			})
			if opened != nil {
				opened.Close()
			}
			if (err == nil) != tc.want {
				t.Fatalf("open = %v, want success %v", err, tc.want)
			}
			if !tc.want && !errors.Is(err, ErrMount) {
				t.Fatal(err)
			}
		})
	}
}

func TestRequiredMountMustDirectlyHostWorkspace(t *testing.T) {
	base := "42 1 0:3 / /mnt/storage rw - ext4 /dev/example rw\n"
	for _, tc := range []struct {
		name, nested string
		want         bool
	}{
		{"direct", "", true},
		{"intervening mount", "/mnt/storage/nas", false},
		{"root itself mounted", "/mnt/storage/nas/workspace", false},
		{"unrelated nested mount", "/mnt/storage/other", true},
		{"path prefix sibling", "/mnt/storage/na", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metadata := base
			if tc.nested != "" {
				metadata += "43 42 0:4 / " + tc.nested + " rw - nfs example:/export rw\n"
			}
			if got := mountHostsRoot(strings.NewReader(metadata), "/mnt/storage", "/mnt/storage/nas/workspace", "42"); got != tc.want {
				t.Fatalf("mount readiness = %v, want %v", got, tc.want)
			}
		})
	}
	// A truncated kernel snapshot must not hide an intervening mount.
	if mountHostsRoot(strings.NewReader(base+strings.Repeat("x", 100000)), "/mnt/storage", "/mnt/storage/nas/workspace", "42") {
		t.Fatal("accepted unreadable mount snapshot")
	}
}
