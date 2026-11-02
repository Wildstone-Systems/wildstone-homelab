//go:build unix

package datadir

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSecureOwnedByKeystone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := Secure(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("mode = %04o, want 0700", got)
	}
}

func TestGroupOwnedAllowed(t *testing.T) {
	for _, test := range []struct {
		name string
		mode os.FileMode
		gid  int
		want bool
	}{
		{name: "kubernetes fsGroup mount", mode: 0o770, gid: 65532, want: true},
		{name: "wrong group", mode: 0o770, gid: 0},
		{name: "not writable", mode: 0o750, gid: 65532},
		{name: "world accessible", mode: 0o777, gid: 65532},
		{name: "setgid accepted", mode: os.ModeSetgid | 0o770, gid: 65532, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := groupOwnedAllowed(test.mode, test.gid, 65532); got != test.want {
				t.Fatalf("groupOwnedAllowed(%#o, %d) = %v, want %v", test.mode, test.gid, got, test.want)
			}
		})
	}
}
