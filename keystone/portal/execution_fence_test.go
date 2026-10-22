package portal

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

type fenceDuringUpload struct {
	reader io.Reader
	stop   func()
}

func (r fenceDuringUpload) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.stop()
	return n, err
}
func TestFileMutationsRequireExecutionAuthority(t *testing.T) {
	s := &FileStore{Root: t.TempDir()}
	if _, err := s.Save("shared", "photo.txt", strings.NewReader("fixture")); err == nil {
		t.Fatal("new file writer bypassed execution authority")
	}
	if err := s.Delete("shared", "photo.txt"); err == nil {
		t.Fatal("delete had no authority")
	}
	if _, err := s.Rename("shared", "photo.txt", "other.txt"); err == nil {
		t.Fatal("rename had no authority")
	}
	if _, err := s.List("shared"); err != nil {
		t.Fatal("read required execution authority", err)
	}
}
func TestAnUploadStoppedDuringStreamingIsNotPublished(t *testing.T) {
	stopped := false
	fenced := errors.New("coordinator fenced")
	s := &FileStore{Root: t.TempDir(), BeforeWrite: func() error {
		if stopped {
			return fenced
		}
		return nil
	}}
	_, err := s.Save("shared", "photo.txt", fenceDuringUpload{reader: strings.NewReader("fixture"), stop: func() { stopped = true }})
	if !errors.Is(err, fenced) {
		t.Fatal(err)
	}
	files, err := s.List("shared")
	if err != nil || len(files) != 0 {
		t.Fatalf("stopped upload published: %v %v", files, err)
	}
}

func TestFileMutationsRecheckAuthorityAtCommit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		denyAt int
	}{{"delete", 2}, {"rename", 2}, {"rename", 3}, {"save", 3}} {
		t.Run(fmt.Sprintf("%s-%d", tc.name, tc.denyAt), func(t *testing.T) {
			s := store(t)
			save(t, s, "shared", "original.txt", "keep")
			calls := 0
			denied := errors.New("privacy changed")
			s.BeforeWrite = func() error {
				calls++
				if calls >= tc.denyAt {
					return denied
				}
				return nil
			}
			var err error
			switch tc.name {
			case "delete":
				err = s.Delete("shared", "original.txt")
			case "rename":
				_, err = s.Rename("shared", "original.txt", "changed.txt")
			case "save":
				_, err = s.Save("shared", "changed.txt", strings.NewReader("private"))
			}
			if !errors.Is(err, denied) {
				t.Fatalf("late denial ignored: %v", err)
			}
			files, err := s.List("shared")
			if err != nil || len(files) != 1 || files[0].Name != "original.txt" {
				t.Fatalf("files changed: %v %v", files, err)
			}
		})
	}
}
