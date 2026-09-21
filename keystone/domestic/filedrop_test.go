package domestic

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// filedropAt builds a drop box over a temporary root, with a fixed
// clock so the "added yesterday" wording is testable.
func filedropAt(t *testing.T, root string) *Filedrop {
	t.Helper()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	return &Filedrop{Authority: permittedExecution{},
		Settings: func() (string, int64, error) { return root, 0, nil },
		Now:      func() time.Time { return now },
	}
}

// drop writes one file into a destination's directory, ageing it so the
// newest-first ordering has something to order.
func drop(t *testing.T, root, dir, name string, size int, age time.Duration) {
	t.Helper()
	full := filepath.Join(root, dir)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(full, name)
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC).Add(-age)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func fileCall(tool string, params map[string]string) Call {
	return Call{
		Tool: "filedrop." + tool, Tier: FiledropTools[tool], Params: params,
		Rule: "butler-session", Subject: "mika",
		TriggerType: "butler.household_session", Attended: true,
	}
}

// exec runs one tool by name through its registered executor, which is
// the path the gateway takes — rather than calling the method, which
// would skip the install gate and the storage check.
func exec(t *testing.T, fd *Filedrop, tool string, params map[string]string) (Result, error) {
	t.Helper()
	for _, e := range fd.Executors() {
		if e.Name() == "filedrop."+tool {
			return e.Execute(context.Background(), fileCall(tool, params))
		}
	}
	t.Fatalf("no executor registered for filedrop.%s", tool)
	return Result{}, nil
}

func TestFiledropReadsNameTheFilesAndWhereTheyAre(t *testing.T) {
	root := t.TempDir()
	drop(t, root, "Shared", "receipt.pdf", 2<<20, time.Hour)
	drop(t, root, "Photos", "beach.jpg", 5<<20, 30*time.Hour)

	res, err := exec(t, filedropAt(t, root), FileToolRecent, nil)
	if err != nil {
		t.Fatalf("list_recent: %v", err)
	}
	// The SUMMARY is the whole channel to a family session, so the
	// answer has to be in it rather than in Detail.
	for _, want := range []string{"receipt.pdf", "2.0 MB", "Shared", "beach.jpg", "Photos"} {
		if !strings.Contains(res.Summary, want) {
			t.Errorf("list_recent summary does not mention %q: %s", want, res.Summary)
		}
	}
	// Newest first, so the thing somebody just dropped is the thing
	// they are asking about.
	if strings.Index(res.Summary, "receipt.pdf") > strings.Index(res.Summary, "beach.jpg") {
		t.Errorf("list_recent is not newest first: %s", res.Summary)
	}
}

func TestFiledropListIsOneDestination(t *testing.T) {
	root := t.TempDir()
	drop(t, root, "Shared", "receipt.pdf", 1<<10, time.Hour)
	drop(t, root, "Photos", "beach.jpg", 1<<10, time.Hour)
	fd := filedropAt(t, root)

	res, err := exec(t, fd, FileToolList, map[string]string{"destination": "photos"})
	if err != nil {
		t.Fatalf("list_files: %v", err)
	}
	if !strings.Contains(res.Summary, "beach.jpg") || strings.Contains(res.Summary, "receipt.pdf") {
		t.Fatalf("list_files leaked another destination: %s", res.Summary)
	}

	// An unrecognised destination is REFUSED, never coerced to the
	// default: a listing from somewhere other than where the person was
	// told is worse than a refusal.
	if _, err := exec(t, fd, FileToolList, map[string]string{"destination": "../etc"}); err == nil {
		t.Fatal("an unknown destination was accepted")
	}
}

// "Mine" is the SESSION's member and there is no spelling for anybody
// else's. The proof is that the same call, from two different people,
// reads two different folders — and that neither can name the other.
func TestMineIsResolvedFromTheSessionAndNotFromAParameter(t *testing.T) {
	root := t.TempDir()
	drop(t, root, "Members/mika", "notes.txt", 1<<10, time.Hour)
	drop(t, root, "Members/sam", "diary.txt", 1<<10, time.Hour)
	fd := filedropAt(t, root)

	mine, err := fd.list(fileCall(FileToolList, map[string]string{"destination": "me"}))
	if err != nil {
		t.Fatalf("list_files: %v", err)
	}
	if !strings.Contains(mine.Summary, "notes.txt") || strings.Contains(mine.Summary, "diary.txt") {
		t.Fatalf("mika's own folder is not his own: %s", mine.Summary)
	}

	// The same call for somebody else reads THEIR folder, because the
	// member comes from the call and not from a param a model writes.
	sams := fileCall(FileToolList, map[string]string{"destination": "me"})
	sams.Subject = "sam"
	theirs, err := fd.list(sams)
	if err != nil {
		t.Fatalf("list_files: %v", err)
	}
	if !strings.Contains(theirs.Summary, "diary.txt") || strings.Contains(theirs.Summary, "notes.txt") {
		t.Fatalf("sam's own folder is not their own: %s", theirs.Summary)
	}

	// And a param naming a person is not a param at all: "member" and
	// "path" are ignored, so the only folder reachable is the caller's.
	spoof := fileCall(FileToolList, map[string]string{"destination": "me", "member": "sam", "path": "Members/sam"})
	got, err := fd.list(spoof)
	if err != nil {
		t.Fatalf("list_files: %v", err)
	}
	if strings.Contains(got.Summary, "diary.txt") {
		t.Fatalf("a parameter reached another member's folder: %s", got.Summary)
	}
}

// An unbound call gets the shared destinations and no personal one,
// rather than the root or somebody's guess at one.
func TestAnUnboundCallHasNoPersonalSpace(t *testing.T) {
	root := t.TempDir()
	drop(t, root, "Shared", "receipt.pdf", 1<<10, time.Hour)
	drop(t, root, "Members/mika", "notes.txt", 1<<10, time.Hour)
	fd := filedropAt(t, root)

	unbound := fileCall(FileToolRecent, nil)
	unbound.Subject = ""
	res, err := fd.recent(unbound)
	if err != nil {
		t.Fatalf("list_recent: %v", err)
	}
	if strings.Contains(res.Summary, "notes.txt") {
		t.Fatalf("an unattributed call read somebody's personal folder: %s", res.Summary)
	}
	if !strings.Contains(res.Summary, "receipt.pdf") {
		t.Fatalf("an unattributed call lost the shared destinations: %s", res.Summary)
	}
}

// An ambiguous name is a refusal that NAMES the candidates, never a
// silent pick. A model that guesses which "report" was meant deletes
// the wrong one and the household finds out later.
func TestAnAmbiguousFileIsRefusedByName(t *testing.T) {
	root := t.TempDir()
	drop(t, root, "Shared", "report-jan.pdf", 1<<10, time.Hour)
	drop(t, root, "Shared", "report-feb.pdf", 1<<10, 2*time.Hour)
	fd := filedropAt(t, root)

	_, err := exec(t, fd, FileToolDelete, map[string]string{"destination": "shared", "name": "report"})
	if err == nil {
		t.Fatal("an ambiguous delete was performed")
	}
	for _, want := range []string{"report-jan.pdf", "report-feb.pdf"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	// Nothing was deleted by asking.
	for _, name := range []string{"report-jan.pdf", "report-feb.pdf"} {
		if _, serr := os.Stat(filepath.Join(root, "Shared", name)); serr != nil {
			t.Errorf("%s was removed by an ambiguous call", name)
		}
	}
}

// The confirm-tier question names the FILE, not the tool call: a person
// is answering about their own documents, not approving a parameter
// list. And the resolution runs first, so nobody is asked to confirm
// deleting something that is not there.
func TestDeleteAsksAboutTheFile(t *testing.T) {
	root := t.TempDir()
	drop(t, root, "Shared", "receipt.pdf", 2<<20, 25*time.Hour)
	fd := filedropAt(t, root)

	var del Executor
	for _, e := range fd.Executors() {
		if e.Name() == "filedrop."+FileToolDelete {
			del = e
		}
	}
	d, ok := del.(Describer)
	if !ok {
		t.Fatal("the delete executor is not a Describer, so a person would be asked about a tool call")
	}
	q, err := d.Describe(context.Background(), fileCall(FileToolDelete,
		map[string]string{"destination": "shared", "name": "receipt.pdf"}))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	for _, want := range []string{"receipt.pdf", "Shared", "2.0 MB", "yesterday"} {
		if !strings.Contains(q, want) {
			t.Errorf("the question does not mention %q: %s", want, q)
		}
	}

	// A file that is not there is the executor's own refusal, raised
	// BEFORE anybody is asked to approve something impossible.
	if _, err := d.Describe(context.Background(), fileCall(FileToolDelete,
		map[string]string{"destination": "shared", "name": "nothing.pdf"})); err == nil {
		t.Fatal("a delete of a missing file reached the confirmation step")
	}
}

func TestDeleteAndRenameActuallyDoIt(t *testing.T) {
	root := t.TempDir()
	drop(t, root, "Shared", "receipt.pdf", 1<<10, time.Hour)
	drop(t, root, "Shared", "old.txt", 1<<10, 2*time.Hour)
	fd := filedropAt(t, root)

	if _, err := exec(t, fd, FileToolRename, map[string]string{
		"destination": "shared", "name": "old.txt", "new_name": "new.txt",
	}); err != nil {
		t.Fatalf("rename_file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Shared", "new.txt")); err != nil {
		t.Fatalf("the file was not renamed: %v", err)
	}

	if _, err := exec(t, fd, FileToolDelete, map[string]string{
		"destination": "shared", "name": "receipt.pdf",
	}); err != nil {
		t.Fatalf("delete_file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Shared", "receipt.pdf")); err == nil {
		t.Fatal("the file is still there")
	}
}

// A drop box the household removed between one conversation and the
// next is refused at the next call, not at the next restart.
func TestAnUninstalledDropBoxIsRefused(t *testing.T) {
	fd := filedropAt(t, t.TempDir())
	fd.Enabled = func() (bool, error) { return false, nil }
	if _, err := exec(t, fd, FileToolRecent, nil); err == nil {
		t.Fatal("an uninstalled drop box answered a read")
	}
}

// "Installed with nowhere to put anything" is a CONFIGURATION fault and
// has to read as one. A household told their files are gone will go
// looking for them.
func TestNoStorageReadsAsAConfigurationFault(t *testing.T) {
	fd := &Filedrop{Authority: permittedExecution{}, Settings: func() (string, int64, error) { return "", 0, nil }}
	_, err := exec(t, fd, FileToolRecent, nil)
	if err == nil || !strings.Contains(err.Error(), "no storage configured") {
		t.Fatalf("err = %v, want a configuration fault", err)
	}
}

// Every declared tool has a usage line, because a family Butler is
// handed ONE tool that performs any household action and the params
// live nowhere else it can read.
func TestEveryFiledropToolSaysWhatItTakes(t *testing.T) {
	fd := filedropAt(t, t.TempDir())
	for _, e := range fd.Executors() {
		d, ok := e.(UsageDescriber)
		if !ok || strings.TrimSpace(d.Usage()) == "" {
			t.Errorf("%s describes no params, so a model has to guess them", e.Name())
		}
	}
}
