package api

// The Home page's readings, as seams a household session can reach.
//
// The four cards on the family dashboard — what needs you, what
// happened, what you missed, how the machines are — were answers a
// member could only get by opening a page. A member in chat, or on a
// phone, or asking out loud through a chat capability, got the Butler's
// description of a page they were not looking at.
//
// These are the same code paths the cards use, called from a tool
// instead of from a request. Deliberately the same: the design's
// most-repeated review finding is two surfaces telling different
// stories about one event, and a widget is a window onto a page rather
// than a second opinion about it.
//
// EVERY ONE IS A READ, and none of them takes a member from its caller
// except the one that is per-person — where the member is the SESSION's,
// stamped by keystone/butler's household gateway from an identity that
// was proven at the door. There is no field in any tool's schema in
// which a model could name somebody else.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/portal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// activityToolScan is how many raw events are read to fill one tool
// answer. More than the limit, because the audience test and the
// unmapped-verb drop both run after the query: a window that returned
// exactly the limit in raw events would return a fraction of it in
// lines. Bounded all the same — a household with a very chatty
// publisher gets a short answer rather than an unbounded scan.
const activityToolScan = 400

// maxMachinesRead bounds how many machines one call probes.
//
// Each is a live round trip to a node agent, and the per-node cache
// holds one reading, so a fleet would be read end to end on every ask.
// A household has one machine or a few; past this the answer is a
// Console screen, not a sentence.
const maxMachinesRead = 8

// HouseholdActivitySeam answers "what has been happening", as the
// sentences the Activity page renders.
//
// THE AUDIENCE TEST IS THE SAME ONE THE PAGE MAKES. A member-scoped
// event about somebody else is not this member's to read, and in a
// GROUP session it is nobody-in-the-room's to hear: the lookup carries
// GroupSafe and the same MemberScoped question decides, so the tool and
// the page cannot disagree about who may hear what.
func (s *Server) HouseholdActivitySeam() func(context.Context, butler.ActivityLookup) ([]butler.ActivityLine, error) {
	return func(_ context.Context, q butler.ActivityLookup) ([]butler.ActivityLine, error) {
		if s == nil || s.DB == nil {
			return nil, fmt.Errorf("this control plane cannot read the household's record")
		}
		limit := q.Limit
		if limit <= 0 || limit > butler.MaxActivityLines {
			limit = butler.MaxActivityLines
		}
		events, err := s.DB.HouseholdEventsBefore(0, activityToolScan, runtimedb.HouseholdEventFilter{})
		if err != nil {
			return nil, fmt.Errorf("reading the household's record: %w", err)
		}
		out := make([]butler.ActivityLine, 0, limit)
		for _, e := range events {
			if len(out) >= limit {
				break
			}
			if !q.Since.IsZero() && e.OccurredAt.Before(q.Since) {
				// SKIPPED, NOT A STOP. The stream is newest first by
				// SEQUENCE — the order things were recorded — and a
				// publisher that reports late carries an OccurredAt
				// older than the rows around it. Ending the walk on the
				// first out-of-window row would let one late delivery
				// hide everything behind it, so the whole bounded
				// window is read and the window filter is applied per
				// row.
				continue
			}
			if s.withholdFromMember(e.Type, e.Subject, q.Member, q.GroupSafe) {
				continue
			}
			sentence, tone, ok := portal.Sentence(e.Type, e.Subject)
			if !ok {
				// An unmapped verb is DROPPED, never printed raw. The
				// page's reasoning applies with more force here: a
				// model handed "automation.action_failed" will read it
				// aloud as though it were English.
				continue
			}
			out = append(out, butler.ActivityLine{
				At: e.OccurredAt, Sentence: sentence, Tone: string(tone),
			})
		}
		return out, nil
	}
}

// withholdFromMember is the one audience rule, asked once.
//
// An event with no subject is about the house and is everybody's. An
// event about the asker is theirs. An event about somebody ELSE is
// withheld when the deployment classifies its type as member-scoped —
// and, in a group session, whether or not it does, because "who is in
// the room" is what makes that answer different (ADR-W026 A7).
func (s *Server) withholdFromMember(eventType, subject, member string, groupSafe bool) bool {
	if subject == "" || strings.EqualFold(subject, member) {
		return false
	}
	if groupSafe {
		return true
	}
	return s.MemberScoped == nil || s.MemberScoped(eventType)
}

// HouseholdNotificationsSeam answers "did I miss anything", for one
// member and nobody else.
//
// ListNotices is already scoped to the recipient, which is what makes
// "another member's notifications are never read here" true at the
// query rather than in a filter somebody can forget.
//
// THE COUNTS ARE ASKED FOR SEPARATELY, and they have to be. A digest
// that said "3 unread" because three of the six rows it happened to
// read were unread goes quietly wrong in exactly the household that has
// a backlog, which is the one asking.
func (s *Server) HouseholdNotificationsSeam() func(context.Context, butler.NotificationLookup) (butler.NotificationDigest, error) {
	return func(_ context.Context, q butler.NotificationLookup) (butler.NotificationDigest, error) {
		if s == nil || s.DB == nil {
			return butler.NotificationDigest{}, fmt.Errorf("this control plane cannot read notifications")
		}
		member := strings.TrimSpace(q.Member)
		if member == "" {
			// Fail closed. A per-person read with no person is not a
			// per-person read, and defaulting to everybody's is the
			// disclosure this surface exists to prevent.
			return butler.NotificationDigest{}, fmt.Errorf("a notification lookup has to be for somebody")
		}
		limit := q.Limit
		if limit <= 0 || limit > butler.MaxNotificationLines {
			limit = butler.MaxNotificationLines
		}
		now := time.Now()

		unread, err := s.DB.CountNotices(runtimedb.NoticeFilter{
			Recipient: member, Deliverable: true, Unacknowledged: true,
		})
		if err != nil {
			return butler.NotificationDigest{}, fmt.Errorf("counting notifications: %w", err)
		}
		waiting, err := s.DB.CountNotices(runtimedb.NoticeFilter{
			Recipient: member, Deliverable: true, Pending: true, At: now,
		})
		if err != nil {
			return butler.NotificationDigest{}, fmt.Errorf("counting questions: %w", err)
		}

		filter := runtimedb.NoticeFilter{Recipient: member, Deliverable: true, UnreadFirst: true}
		if q.UnansweredOnly {
			filter.Pending, filter.At, filter.UnreadFirst = true, now, false
		}
		notices, err := s.DB.ListNotices(limit, filter)
		if err != nil {
			return butler.NotificationDigest{}, fmt.Errorf("reading notifications: %w", err)
		}
		digest := butler.NotificationDigest{Unread: unread, Waiting: waiting}
		for _, n := range notices {
			digest.Lines = append(digest.Lines, butler.NotificationLine{
				At:     n.CreatedAt,
				Title:  s.noticeTitle(n.Kind),
				Kind:   n.Kind,
				Unread: n.AcknowledgedAt.IsZero(),
				// An asking notice still waiting is one the person owes
				// an answer to. One already answered is history, and
				// saying it is waiting would send them to look for a
				// question that is not there.
				Asks:     n.Asks && n.AnsweredAt.IsZero(),
				Question: n.Question,
			})
		}
		return digest, nil
	}
}

// HouseholdMachinesSeam answers "are the machines all right".
//
// IT NAMES NO HOSTS, and that is the card's rule rather than an
// oversight: §7 gives the family surface no hostnames, and a spoken
// answer is not the place to start. Machines are numbered in the order
// the fleet lists them, which is stable, so "the second machine" means
// the same thing twice.
//
// A machine that cannot be read is reported as unreadable, never as
// idle. Every percentage carries its own known flag for the same
// reason: a kernel can publish processor load and not available memory,
// and reporting the missing one as zero would tell a household their
// server was resting while it was actually unreachable.
func (s *Server) HouseholdMachinesSeam() func(context.Context) ([]butler.MachineReading, error) {
	return func(ctx context.Context) ([]butler.MachineReading, error) {
		if s == nil || s.DB == nil {
			return nil, fmt.Errorf("this control plane cannot see the machines")
		}
		nodes, err := s.DB.ListNodes()
		if err != nil {
			return nil, fmt.Errorf("listing the machines: %w", err)
		}
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })
		if len(nodes) > maxMachinesRead {
			nodes = nodes[:maxMachinesRead]
		}
		out := make([]butler.MachineReading, 0, len(nodes))
		for i, n := range nodes {
			name := "The machine"
			if len(nodes) > 1 {
				name = fmt.Sprintf("Machine %d", i+1)
			}
			reading := butler.MachineReading{Name: name}
			stats, ok := s.machineReadings(ctx, n.NodeID)
			if !ok {
				reading.Problem = "it did not answer just now"
				out = append(out, reading)
				continue
			}
			reading.Connected = true
			reading.CPU, reading.CPUKnown = stats.CPU, stats.CPUKnown
			reading.Memory, reading.MemKnown = stats.Memory, stats.MemKnown
			reading.Disk, reading.DiskKnown = stats.Disk, stats.DiskKnown
			reading.Cores, reading.MemoryTotal, reading.DiskTotal = stats.Cores, stats.MemoryTotal, stats.DiskTotal
			out = append(out, reading)
		}
		return out, nil
	}
}
