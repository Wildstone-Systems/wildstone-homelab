package portal

// Two phones, one filename, at the same moment.
//
// The collision policy is the only thing standing between a household
// and a lost photo, and it is the one policy a single-threaded test
// cannot exercise: everything it promises is about what happens when
// two uploads overlap, and a test that does them one after the other
// watches the promise hold for the wrong reason.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Every upload of the same name must survive, with its own contents,
// under a name of its own.
//
// Sequentially this passed while the commit was a look-then-rename: the
// gap it lost files through only opens when two uploads are inside it
// together.
func TestConcurrentUploadsOfOneNameAllSurvive(t *testing.T) {
	s := store(t)
	const uploaders = 12
	const name = "IMG_0001.jpg"

	var wg sync.WaitGroup
	results := make([]Stored, uploaders)
	errs := make([]error, uploaders)
	start := make(chan struct{})
	for i := range uploaders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Released together, so the uploads are actually overlapping
			// rather than merely unordered.
			<-start
			results[i], errs[i] = s.Save("shared", name, strings.NewReader(fmt.Sprintf("photo-from-phone-%d", i)))
		}()
	}
	close(start)
	wg.Wait()

	seen := map[string]bool{}
	for i := range uploaders {
		if errs[i] != nil {
			t.Fatalf("upload %d: %v", i, errs[i])
		}
		final := results[i].Item.Name
		if seen[final] {
			t.Fatalf("upload %d was told it stored %s, which another upload was also told", i, final)
		}
		seen[final] = true

		// The name it was given must hold the bytes it sent. A caller
		// told "stored as IMG_0001-3.jpg" and finding somebody else's
		// photo there is the same loss as an overwrite, wearing a
		// different name.
		body, err := os.ReadFile(filepath.Join(s.Root, "Shared", final))
		if err != nil {
			t.Fatalf("upload %d: reading back %s: %v", i, final, err)
		}
		if want := fmt.Sprintf("photo-from-phone-%d", i); string(body) != want {
			t.Fatalf("upload %d: %s holds %q, want %q", i, final, body, want)
		}
	}

	// And nothing else is in the directory: no empty reservation left
	// where a commit went wrong, no scratch file, no orphan.
	entries, err := os.ReadDir(filepath.Join(s.Root, "Shared"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != uploaders {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the namespace holds %d files, want %d: %v", len(entries), uploaders, names)
	}
}

// A rename must not eat a file that arrived while it was deciding.
// Same gap, same loss, a different pair of steps.
func TestConcurrentRenamesOntoOneNameKeepBothFiles(t *testing.T) {
	s := store(t)
	const target = "invoice.pdf"
	const renamers = 8
	for i := range renamers {
		save(t, s, "shared", fmt.Sprintf("scan-%d.pdf", i), fmt.Sprintf("scan-%d", i))
	}

	var wg sync.WaitGroup
	errs := make([]error, renamers)
	start := make(chan struct{})
	for i := range renamers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = s.Rename("shared", fmt.Sprintf("scan-%d.pdf", i), target)
		}()
	}
	close(start)
	wg.Wait()

	won := 0
	for i := range renamers {
		if errs[i] == nil {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d renames onto %s succeeded, want exactly 1", won, target)
	}
	// The losers keep their original files: a refused rename must not
	// have moved anything.
	entries, err := os.ReadDir(filepath.Join(s.Root, "Shared"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != renamers {
		t.Fatalf("the namespace holds %d files, want %d", len(entries), renamers)
	}
}
