package portal

import (
	"errors"
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
