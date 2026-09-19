package api

// The Activity page: the household record, in household language.
//
// It is the same projection the front page's "Today at home" card
// renders, over a longer window and with filters — deliberately the
// same code path, because the design's most-repeated review finding was
// two surfaces telling different stories about one event. A widget is a
// window onto a page, so the page must not have its own idea of what an
// event says.
//
// DAY GROUPS COME FROM THE HOUSEHOLD'S CLOCK. "Today" means midnight
// where the household is, not where the control plane's process thinks
// it is: a family in Chicago reading an evening's events must not see
// them filed under tomorrow because the server keeps UTC.

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// activityPage is how many rows one request answers. The page asks for
// more with a cursor rather than getting a longer list, because a
// household's stream is unbounded and a single response that tried to
// carry a month of it would be slow in exactly the households that have
// the most to show.
const activityPage = 40

// activityRowOut is one row as the page renders it.
type activityRowOut struct {
	Seq      int64       `json:"seq"`
	Sentence string      `json:"sentence"`
	At       time.Time   `json:"at"`
	Tone     portal.Tone `json:"tone"`
	// Subject is which filter this row lives under, so the client can
	// re-filter without another request — and so a row can never appear
	// under two.
	Subject portal.Filter `json:"subject,omitempty"`
	// Day is the household-local day key the row groups under, computed
	// server-side for the reason the whole file exists: the browser's
	// idea of midnight is not the household's.
	Day string `json:"day"`
}

// activityHistory answers one page of the household record.
func (s *Server) activityHistory(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("this page is not ready yet")
	}
	filter := portal.Filter(r.URL.Query().Get("filter"))
	switch filter {
	case portal.FilterEverything, portal.FilterWaiting, portal.FilterHome,
		portal.FilterFiles, portal.FilterAssistant:
	default:
		// Refused rather than silently treated as Everything: a client
		// asking for a filter this server does not have should be told,
		// not handed the unfiltered list to render under the wrong tab.
		return nil, http.StatusBadRequest, fmt.Errorf("no such filter")
	}

	// The cursor is a sequence number, exclusive. Sequence rather than a
	// timestamp because two events can share a second and a timestamp
	// cursor would either skip one or repeat it forever.
	before := int64(0)
	if raw := r.URL.Query().Get("before"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return nil, http.StatusBadRequest, fmt.Errorf("that is not a position in the list")
		}
		before = n
	}

	loc := s.householdLoc()
	if loc == nil {
		loc = time.Local
	}

	// Read more than a page, because the filters, the audience test and
	// the unmapped-verb drop all run after the query: a window that
	// returned exactly a page of raw events would return half a page of
	// rows, and a client would think it had reached the end.
	//
	// Bounded all the same. A household with a very chatty publisher
	// could have a page's worth of droppable rows several times over,
	// and the honest answer there is a short page with a cursor rather
	// than an unbounded scan.
	const scan = activityPage * 12
	rows := make([]activityRowOut, 0, activityPage)
	dropped := 0
	cursor := before
	for len(rows) < activityPage {
		events, err := s.DB.HouseholdEventsBefore(cursor, scan, runtimedb.HouseholdEventFilter{})
		if err != nil {
			s.Log.Error("activity history", "err", err)
			return nil, http.StatusInternalServerError, fmt.Errorf("can't load the household's record")
		}
		if len(events) == 0 {
			cursor = 0 // the end
			break
		}
		for _, e := range events {
			cursor = e.Seq
			// The audience test, identical to the front page's: a
			// member-scoped event about somebody else is not this
			// member's to read.
			if e.Subject != "" && !strings.EqualFold(e.Subject, member) {
				if s.MemberScoped == nil || s.MemberScoped(e.Type) {
					continue
				}
			}
			sentence, tone, ok := portal.Sentence(e.Type, e.Subject)
			if !ok {
				dropped++
				continue
			}
			if !portal.Matches(filter, e.Type, tone) {
				continue
			}
			subject, _ := portal.Subject(e.Type)
			rows = append(rows, activityRowOut{
				Seq:      e.Seq,
				Sentence: sentence,
				At:       e.OccurredAt,
				Tone:     tone,
				Subject:  subject,
				Day:      e.OccurredAt.In(loc).Format("2006-01-02"),
			})
			if len(rows) >= activityPage {
				break
			}
		}
		if len(events) < scan {
			cursor = 0 // nothing older to ask for
			break
		}
	}

	// Strictly newest first, worst first when times tie.
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].At.Equal(rows[j].At) {
			return rows[i].At.After(rows[j].At)
		}
		return toneRank(rows[i].Tone) < toneRank(rows[j].Tone)
	})

	return map[string]any{
		"rows":    rows,
		"dropped": dropped,
		// next is the cursor for "load earlier", or 0 at the end. The
		// client shows the control only when there is one, so a member
		// never presses a button that does nothing.
		"next":  cursor,
		"today": time.Now().In(loc).Format("2006-01-02"),
	}, http.StatusOK, nil
}
