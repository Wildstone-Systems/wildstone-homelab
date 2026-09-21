package domestic

// The filedrop executors: the household actions that make
// `packages/filedrop-wildstone` usable by an agent the moment it is
// installed.
//
// They are the calendar's siblings and follow the same three rules it
// records, with one of its own on top.
//
// THE SUMMARY IS THE WHOLE ANSWER. A family session calls these through
// butler.do_household_action, which hands the model `Result.Summary` and
// drops `Result.Detail` on the floor. So the summary is written to be
// read aloud — "Shared has 3 files: receipt.pdf (2.1 MB, today), ..." —
// rather than as a log line with the answer in a field nobody
// downstream can see.
//
// A CALLER NEVER NAMES A PATH, and this is keystone/portal's rule
// restated where a model can reach it. A call names a DESTINATION —
// "shared", "photos", "me" — and the closed table in that package maps
// it to a directory. There is no field here in which a model could
// spell /etc, ../, or somebody else's folder, and the one string a
// caller does supply (a filename) is re-normalised by the store before
// it touches the filesystem.
//
// "MINE" IS RESOLVED FROM THE SESSION, NEVER FROM A PARAMETER. The
// personal destination is whoever the call is FOR — Call.Subject, which
// the household gateway stamps from the session's member and a rule
// gets from its trigger — so there is no spelling of "somebody else's
// private folder" even for a model that wants one. A call with no
// subject gets the shared destinations and no personal one, which fails
// closed.
//
// NOTHING HERE HANDS OVER BYTES. There is no read-the-file tool and
// there is no upload tool, deliberately: a drop box's contents are the
// household's documents, and piping them through a model's context is a
// disclosure nobody asked for. An agent can say what is there, find it
// by name, tidy it up and — with the person's yes — remove it. Reading
// it is what the Files page is for.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// FiledropCapability is the capability these executors belong to. It is
// the `metadata.capability` of the package that declares the tools, and
// the two must agree or the gateway refuses every call —
// TestFiledropToolsAreDeclaredByThePackage pins them together.
const FiledropCapability = "filedrop"

// Filedrop tool names, without the capability prefix.
const (
	FileToolRecent = "list_recent"
	FileToolList   = "list_files"
	FileToolFind   = "find_file"
	FileToolRename = "rename_file"
	FileToolDelete = "delete_file"
)

// FiledropTools maps each tool to the tier it must be declared at.
//
// Nothing here is above what a household surface may carry (ADR-W024
// A5): none of these touches a container, a node or a plan.
//
// Deleting is at `confirm` for the reason the calendar's delete is: it
// is the one irreversible thing in this set, a household drop box holds
// the only copy of things people put in it, and "do it, but ask first"
// is exactly what ADR-W024 A5.1 added the tier for. Renaming is
// `domestic` rather than `confirm` because the file is still there
// under a name the same tool can put back.
var FiledropTools = map[string]string{
	FileToolRecent: TierRead,
	FileToolList:   TierRead,
	FileToolFind:   TierRead,
	FileToolRename: TierDomestic,
	FileToolDelete: TierConfirm,
}

// maxListedFiles bounds what one answer names individually. Past it the
// summary gives a count and the newest few, because a model handed two
// hundred lines will read two hundred lines back to somebody.
const maxListedFiles = 20

// defaultListedFiles is what a call that names no limit gets.
const defaultListedFiles = 10

// minFindQuery is the shortest search a find will run. One character
// matches most of a drop box, which is a listing with extra steps.
const minFindQuery = 2

// Filedrop is the household's drop box, as the action gateway sees it.
//
// One instance backs every tool; Executors returns the registrations.
// It holds a SEAM rather than a path, because the storage root lives in
// desired state — the capability installs no container, so the
// ConfigFile it renders is the only carrier a root has — and reading it
// once at startup would pin the answer to whatever was true before the
// household installed anything.
type Filedrop struct {
	beforeWrite func() error
	Authority   HouseholdExecutionAuthority
	// Settings reports where the household's drop box is, now.
	//
	// Root is the absolute host path the filedrop capability declares;
	// empty means the capability is installed with nowhere to put
	// anything, which every tool reports as the configuration fault it
	// is rather than guessing at a relative path. MaxUpload is the
	// household's per-file ceiling; zero takes the portal default.
	//
	// nil makes every tool answer "no storage configured", which is the
	// honest thing for a control plane that cannot look.
	Settings func() (root string, maxUpload int64, err error)
	// Enabled reports whether this deployment has the filedrop
	// capability INSTALLED, asked per call.
	//
	// Registered from the catalog, gated on the Statefile, exactly as
	// ChatSender is and for the same reason (ADR-W026 A8): the catalog
	// says a package exists, and only an install says this household
	// has it. Checking per call rather than filtering at registration
	// means enabling the drop box works without a restart, and
	// disabling it stops the tools at once.
	//
	// nil means "do not ask", which is the right posture for a test
	// and for a control plane with no catalog: the declaration check in
	// the gateway is still in front of every call.
	Enabled func() (bool, error)
	// Now is the clock, injectable for tests.
	Now func() time.Time
}

func (f *Filedrop) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// store builds the member-scoped view of the drop box for one call.
//
// The member comes from the CALL, not from a parameter: on a family
// session the household gateway stamps Subject with the session's
// member, and on a rule it is whoever the trigger was about. An empty
// subject yields a store whose personal destination is unavailable,
// which is portal's own fail-closed behaviour rather than a special
// case here.
func (f *Filedrop) store(c Call) (*portal.FileStore, error) {
	if f == nil || f.Settings == nil {
		return nil, errNoFiledropRoot
	}
	root, maxUpload, err := f.Settings()
	if err != nil {
		return nil, fmt.Errorf("reading where the household's drop box is: %w", err)
	}
	if strings.TrimSpace(root) == "" {
		return nil, errNoFiledropRoot
	}
	return &portal.FileStore{BeforeWrite: func() error {
		if err := checkExecution(f.Authority); err != nil {
			return err
		}
		if f.beforeWrite != nil {
			return f.beforeWrite()
		}
		return nil
	}, Root: root, MaxUpload: maxUpload, Member: strings.TrimSpace(c.Subject)}, nil
}

// errNoFiledropRoot is the one sentence every tool gives for "installed,
// but nobody pointed it at a disk". A CONFIGURATION fault rather than
// the caller's, and saying so is what stops a household concluding
// their files are gone.
var errNoFiledropRoot = errors.New("the household drop box has no storage configured on this control plane")

// Executors returns one registration per tool, in a stable order.
func (f *Filedrop) Executors() []Executor {
	names := make([]string, 0, len(FiledropTools))
	for name := range FiledropTools {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Executor, 0, len(names))
	for _, name := range names {
		out = append(out, &filedropTool{fd: f, tool: name})
	}
	return out
}

// filedropTool is one registered tool. It carries the name rather than
// being a type per tool, for the reason calendarTool does: they share
// every seam and differ only in which branch of Execute they take.
type filedropTool struct {
	fd   *Filedrop
	tool string
}

func (t *filedropTool) Name() string { return FiledropCapability + "." + t.tool }

// filedropUsage is each tool's params, in the words a model reads.
var filedropUsage = map[string]string{
	FileToolRecent: "What has landed in the household's drop box lately, across every destination. " +
		"params: limit (1-20, default 10).",
	FileToolList: "What is in one destination. params: destination (shared | photos | me, " +
		"default shared), limit (1-20, default 10). \"me\" is the asker's own folder, which " +
		"nobody else in the household can see.",
	FileToolFind: "Find a file by part of its name, across every destination. params: query " +
		"(required, at least 2 characters), limit (1-20, default 10).",
	FileToolRename: "Rename a file. params: destination, name, new_name (all required).",
	FileToolDelete: "Delete a file; the person is asked first. params: destination, name " +
		"(both required).",
}

// Usage implements UsageDescriber.
func (t *filedropTool) Usage() string { return filedropUsage[t.tool] }

func (t *filedropTool) Execute(ctx context.Context, c Call) (Result, error) {
	if FiledropTools[t.tool] != TierRead {
		if err := checkExecution(t.fd.Authority); err != nil {
			return Result{}, err
		}

		local := *t.fd
		local.beforeWrite = func() error { return runtimedb.CheckPrivacyDelivery(ctx, runtimedb.MemoryDestination{}) }
		t = &filedropTool{fd: &local, tool: t.tool}
	}
	if err := t.fd.installed(); err != nil {
		return Result{}, err
	}
	switch t.tool {
	case FileToolRecent:
		return t.fd.recent(c)
	case FileToolList:
		return t.fd.list(c)
	case FileToolFind:
		return t.fd.find(c)
	case FileToolRename:
		return t.fd.rename(c)
	case FileToolDelete:
		return t.fd.remove(c)
	default:
		return Result{}, fmt.Errorf("filedrop has no tool %q", t.tool)
	}
}

// installed is the per-call install gate. A refusal here is what stops
// a Butler acting on a drop box the household removed between one
// conversation and the next.
func (f *Filedrop) installed() error {
	if f == nil || f.Enabled == nil {
		return nil
	}
	on, err := f.Enabled()
	if err != nil {
		return fmt.Errorf("checking whether the household has a drop box: %w", err)
	}
	if !on {
		return fmt.Errorf("this household has no drop box installed")
	}
	return nil
}

// Describe implements Describer for the confirm-tier delete, so the
// person is asked about a file rather than about a tool call.
//
// It resolves the file FIRST, which moves the executor's own refusal
// ahead of the question: nobody is asked to approve deleting something
// that is not there.
func (t *filedropTool) Describe(_ context.Context, c Call) (string, error) {
	if t.tool != FileToolDelete {
		return "", nil
	}
	if err := t.fd.installed(); err != nil {
		return "", err
	}
	nsKey, name, err := t.fd.target(c)
	if err != nil {
		return "", err
	}
	item, err := t.fd.find1(c, nsKey, name)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Delete %q from %s (%s, added %s)?",
		item.Name, destinationTitle(nsKey), humanSize(item.Size),
		humanWhen(t.fd.now(), item.ModTime)), nil
}

// --- reads ---

// recent answers "what has landed lately".
//
// Params:
//
//	limit (optional) 1..20; default 10
func (f *Filedrop) recent(c Call) (Result, error) {
	limit, err := listLimit(c.Params["limit"])
	if err != nil {
		return Result{}, err
	}
	store, err := f.store(c)
	if err != nil {
		return Result{}, err
	}
	items, err := store.Recent(limit)
	if err != nil {
		return Result{}, filedropError(err)
	}
	if len(items) == 0 {
		return Result{
			Summary: "Nothing has been dropped in the household's files yet.",
			Detail:  map[string]string{"count": "0"},
		}, nil
	}
	return Result{
		Summary: "Most recent in the household's files: " + f.renderItems(items, true),
		Detail:  map[string]string{"count": strconv.Itoa(len(items))},
	}, nil
}

// list answers "what is in one destination".
//
// Params:
//
//	destination (optional) shared | photos | me; default shared
//	limit       (optional) 1..20; default 10
func (f *Filedrop) list(c Call) (Result, error) {
	nsKey, err := destination(c.Params["destination"])
	if err != nil {
		return Result{}, err
	}
	limit, err := listLimit(c.Params["limit"])
	if err != nil {
		return Result{}, err
	}
	store, err := f.store(c)
	if err != nil {
		return Result{}, err
	}
	items, err := store.List(nsKey)
	if err != nil {
		return Result{}, filedropError(err)
	}
	title := destinationTitle(nsKey)
	if len(items) == 0 {
		return Result{
			Summary: fmt.Sprintf("%s is empty.", title),
			Detail:  map[string]string{"destination": nsKey, "count": "0"},
		}, nil
	}
	shown := items
	if len(shown) > limit {
		shown = shown[:limit]
	}
	summary := fmt.Sprintf("%s holds %s: %s", title, countFiles(len(items)), f.renderItems(shown, false))
	if len(items) > len(shown) {
		summary += fmt.Sprintf(" (newest %d of %d)", len(shown), len(items))
	}
	return Result{
		Summary: summary,
		Detail:  map[string]string{"destination": nsKey, "count": strconv.Itoa(len(items))},
	}, nil
}

// find answers "where did that file go".
//
// It searches every destination the caller can see, which is the shared
// ones plus their own — never anybody else's, because the store resolves
// "me" from the call's subject and has no spelling for another person's.
//
// Params:
//
//	query (required) at least 2 characters, matched case-insensitively
//	limit (optional) 1..20; default 10
func (f *Filedrop) find(c Call) (Result, error) {
	query := strings.TrimSpace(c.Params["query"])
	if len([]rune(query)) < minFindQuery {
		return Result{}, fmt.Errorf("param %q must be at least %d characters: a shorter one matches most of the drop box",
			"query", minFindQuery)
	}
	limit, err := listLimit(c.Params["limit"])
	if err != nil {
		return Result{}, err
	}
	all, err := f.everything(c)
	if err != nil {
		return Result{}, err
	}
	needle := strings.ToLower(query)
	var hits []portal.Item
	for _, item := range all {
		if strings.Contains(strings.ToLower(item.Name), needle) {
			hits = append(hits, item)
		}
	}
	if len(hits) == 0 {
		return Result{
			Summary: fmt.Sprintf("Nothing in the household's files matches %q.", query),
			Detail:  map[string]string{"query": query, "count": "0"},
		}, nil
	}
	shown := hits
	if len(shown) > limit {
		shown = shown[:limit]
	}
	summary := fmt.Sprintf("%s matching %q: %s", countFiles(len(hits)), query, f.renderItems(shown, true))
	if len(hits) > len(shown) {
		summary += fmt.Sprintf(" (newest %d of %d)", len(shown), len(hits))
	}
	return Result{
		Summary: summary,
		Detail:  map[string]string{"query": query, "count": strconv.Itoa(len(hits))},
	}, nil
}

// --- writes ---

// rename changes what a file is called, in place.
//
// Params:
//
//	destination (required) shared | photos | me
//	name        (required) the file as it is now
//	new_name    (required) what to call it
func (f *Filedrop) rename(c Call) (Result, error) {
	nsKey, name, err := f.target(c)
	if err != nil {
		return Result{}, err
	}
	to := strings.TrimSpace(c.Params["new_name"])
	if to == "" {
		return Result{}, fmt.Errorf("param %q is required", "new_name")
	}
	// Resolved first, so "there is no such file" is the answer to a
	// rename of something absent rather than the store's own error
	// shape.
	if _, err := f.find1(c, nsKey, name); err != nil {
		return Result{}, err
	}
	store, err := f.store(c)
	if err != nil {
		return Result{}, err
	}
	settled, err := store.Rename(nsKey, name, to)
	if err != nil {
		return Result{}, filedropError(err)
	}
	summary := fmt.Sprintf("Renamed %q to %q in %s.", name, settled, destinationTitle(nsKey))
	if settled != to {
		// The store settles on a name that is safe and not already
		// taken. Saying so is the difference between a household
		// looking for "report.pdf" and finding "report-2.pdf".
		summary += fmt.Sprintf(" (%q was not usable as-is, so it was saved as %q.)", to, settled)
	}
	return Result{EffectConfirmed: true,
		Summary: summary,
		Detail:  map[string]string{"destination": nsKey, "from": name, "to": settled},
	}, nil
}

// remove deletes a file. Confirm-tier: the gateway has already put the
// question from Describe above to the member and had a yes for THIS
// call before this runs.
//
// Params:
//
//	destination (required) shared | photos | me
//	name        (required) the file to remove
func (f *Filedrop) remove(c Call) (Result, error) {
	nsKey, name, err := f.target(c)
	if err != nil {
		return Result{}, err
	}
	item, err := f.find1(c, nsKey, name)
	if err != nil {
		return Result{}, err
	}
	store, err := f.store(c)
	if err != nil {
		return Result{}, err
	}
	if err := store.Delete(nsKey, item.Name); err != nil {
		return Result{}, filedropError(err)
	}
	return Result{EffectConfirmed: true,
		Summary: fmt.Sprintf("Deleted %q from %s.", item.Name, destinationTitle(nsKey)),
		Detail:  map[string]string{"destination": nsKey, "name": item.Name},
	}, nil
}

// --- resolution and rendering ---

// target reads the destination and filename every write tool takes.
func (f *Filedrop) target(c Call) (string, string, error) {
	nsKey, err := destination(c.Params["destination"])
	if err != nil {
		return "", "", err
	}
	name := strings.TrimSpace(c.Params["name"])
	if name == "" {
		return "", "", fmt.Errorf("param %q is required", "name")
	}
	return nsKey, name, nil
}

// find1 resolves ONE file in one destination, by the name a person
// would say.
//
// An exact name wins outright. Failing that it is matched as a
// case-insensitive substring, and an ambiguous match is a refusal that
// NAMES the candidates rather than a silent pick — the same safety
// property the calendar's event resolution has, and for the same
// reason: a model that guesses which "report" was meant deletes the
// wrong one and the household finds out later.
func (f *Filedrop) find1(c Call, nsKey, name string) (portal.Item, error) {
	store, err := f.store(c)
	if err != nil {
		return portal.Item{}, err
	}
	items, err := store.List(nsKey)
	if err != nil {
		return portal.Item{}, filedropError(err)
	}
	for _, item := range items {
		if item.Name == name {
			return item, nil
		}
	}
	needle := strings.ToLower(name)
	var hits []portal.Item
	for _, item := range items {
		if strings.Contains(strings.ToLower(item.Name), needle) {
			hits = append(hits, item)
		}
	}
	switch len(hits) {
	case 0:
		return portal.Item{}, fmt.Errorf("there is no file called %q in %s", name, destinationTitle(nsKey))
	case 1:
		return hits[0], nil
	default:
		names := make([]string, 0, len(hits))
		for _, item := range hits {
			names = append(names, strconv.Quote(item.Name))
		}
		if len(names) > maxListedFiles {
			names = names[:maxListedFiles]
		}
		return portal.Item{}, fmt.Errorf("%d files in %s match %q: %s — say which one",
			len(hits), destinationTitle(nsKey), name, strings.Join(names, ", "))
	}
}

// everything is every file the caller can see, newest first.
func (f *Filedrop) everything(c Call) ([]portal.Item, error) {
	store, err := f.store(c)
	if err != nil {
		return nil, err
	}
	var all []portal.Item
	for _, ns := range portal.Namespaces {
		items, err := store.List(ns.Key)
		if err != nil {
			// A member with no usable name has no personal space; the
			// shared destinations are still theirs to see. portal.Recent
			// skips it for the same reason.
			if errors.Is(err, portal.ErrNamespace) {
				continue
			}
			return nil, filedropError(err)
		}
		all = append(all, items...)
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].ModTime.Equal(all[j].ModTime) {
			return all[i].ModTime.After(all[j].ModTime)
		}
		return all[i].Name < all[j].Name
	})
	return all, nil
}

// renderItems writes the list a person hears. withDestination is on for
// the cross-destination tools, where "which folder" is part of the
// answer, and off inside one destination, where repeating it is noise.
func (f *Filedrop) renderItems(items []portal.Item, withDestination bool) string {
	now := f.now()
	parts := make([]string, 0, len(items))
	for _, item := range items {
		line := fmt.Sprintf("%s (%s, %s", item.Name, humanSize(item.Size), humanWhen(now, item.ModTime))
		if withDestination {
			line += ", in " + destinationTitle(item.Namespace)
		}
		parts = append(parts, line+")")
	}
	return strings.Join(parts, "; ")
}

// destination resolves the one destination param against portal's closed
// table. An unrecognised one is refused and NEVER coerced to the
// default: a file listed from somewhere other than where the person was
// told is worse than a refusal.
func destination(raw string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(raw))
	if key == "" {
		return portal.DefaultNamespace, nil
	}
	if _, err := portal.NamespaceFor(key); err != nil {
		return "", fmt.Errorf("param %q must be one of %s, got %q",
			"destination", strings.Join(destinationKeys(), " | "), raw)
	}
	return key, nil
}

func destinationKeys() []string {
	out := make([]string, 0, len(portal.Namespaces))
	for _, ns := range portal.Namespaces {
		out = append(out, ns.Key)
	}
	return out
}

// destinationTitle is the name a household member sees on the Files
// page, so a spoken answer and the page agree about what a folder is
// called.
func destinationTitle(key string) string {
	for _, ns := range portal.Namespaces {
		if ns.Key == key {
			return ns.Title
		}
	}
	return key
}

func listLimit(raw string) (int, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return defaultListedFiles, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > maxListedFiles {
		return 0, fmt.Errorf("limit must be a whole number from 1 to %d, got %q", maxListedFiles, raw)
	}
	return n, nil
}

func countFiles(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

// humanSize is the size a person says out loud. Deliberately coarse:
// "2.1 MB" is what somebody asks about, and the exact byte count is in
// Detail for the audit record.
func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

// humanWhen is when a file landed, in the register a household uses.
// A date past the last week, because "23 days ago" is arithmetic nobody
// asked for.
func humanWhen(now, at time.Time) string {
	if at.IsZero() {
		return "at an unknown time"
	}
	d := now.Sub(at)
	switch {
	case d < 0:
		return at.Format("2 Jan")
	case d < time.Hour:
		return "just now"
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	default:
		return at.Format("2 Jan")
	}
}

// filedropError turns the store's sentinel errors into the sentence a
// household hears, and passes anything else through.
//
// ErrNoRoot is a CONFIGURATION fault rather than the caller's, and
// saying so is what stops a household concluding their files are gone
// when what actually happened is that nobody pointed the capability at
// a disk.
func filedropError(err error) error {
	switch {
	case errors.Is(err, portal.ErrNoRoot):
		return errNoFiledropRoot
	case errors.Is(err, portal.ErrNamespace):
		return fmt.Errorf("that destination does not exist here; the household's are %s",
			strings.Join(destinationKeys(), ", "))
	case errors.Is(err, portal.ErrNotFound):
		return fmt.Errorf("there is no such file in the household's drop box")
	}
	return err
}
