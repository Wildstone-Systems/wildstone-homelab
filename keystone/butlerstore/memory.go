package butlerstore

// The memory seams, bound to one session (ADR-W030 A3/A3.1, ADR-W031
// A4/A5).
//
// keystone/butler defines the seams and holds no database handle, for
// the reason this package exists at all: the Butler must not be able to
// reach the store by following a field. Everything that decides who may
// read a record — the reader, the owner, the audience, the permission
// label and Agent identity — is closed over HERE from the session.
// A model may narrow its Personal note to Agent-local, never change its audience.
//
// It lives beside the ledger rather than in cmd/keystone because the
// write path makes claims that have to be tested. A note is the
// SPEAKER'S even when it is said in a room; missing permission metadata
// is a refusal rather than a default; a note defaults to Personal and
// cannot become Household without the Person's approval; and a record somebody forgot does not come
// back by being said again. None of that is provable from a closure in
// a main function.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// preambleKinds are what a session is opened knowing.
//
// Preferences and procedures shape how a Butler behaves and only do so
// if they are present before the model speaks. Facts and inferences are
// answers to questions nobody has asked yet, and they are searched.
var preambleKinds = []string{runtimedb.MemoryPreference, runtimedb.MemoryProcedure}

// preambleLimit is how many records of each kind the injected block
// draws from. The block itself is capped in bytes by the butler package;
// this stops the query reading a thousand rows to render two.
const preambleLimit = 25

// MemorySeams binds memory to one reader, one Butler and one
// deployment's store and trusted destination. Canonical identities are bound
// once; their current admission and resource permissions are checked at read.
//
// Both names are captured here and neither is a parameter of anything
// the model can reach. runtimedb ANDs them into the SQL that finds
// candidates, so a widened scope is not something these closures could
// pass on even if they wanted to.
//
// A session that cannot name both gets NO seams, which is fail-closed
// by being absent: the gateway registers a tool only where its seam is
// wired, so such a session offers neither recall nor remember rather
// than offering them and refusing every call. "Memory is off here" and
// "I could not do that" are different answers, and a person is owed the
// first one.
//
// log may be nil. It is used for exactly one thing — a note that was
// stored but could not be made searchable — and a deployment without a
// logger is not a reason to refuse the write.
func MemorySeams(db *runtimedb.DB, butlerName, member string, destination runtimedb.MemoryDestination, log *slog.Logger) butler.MemorySeams {
	if db == nil {
		return butler.MemorySeams{}
	}
	return memorySeams(db, butlerName, member, db.BindMemoryRead(member, butlerName, destination), log)
}

func memorySeams(db *runtimedb.DB, butlerName, member string, read *runtimedb.MemoryReadContext, log *slog.Logger) butler.MemorySeams {
	if db == nil || butlerName == "" || member == "" {
		return butler.MemorySeams{}
	}
	return butler.MemorySeams{
		Preamble: func(ctx context.Context) ([]butler.MemoryHit, error) {
			var out []butler.MemoryHit
			for _, kind := range preambleKinds {
				records, err := db.RetrieveMemory(runtimedb.MemoryQuery{
					ReadContext: read, Reader: member, Butler: butlerName, Kind: kind, Limit: preambleLimit,
				})
				if err != nil {
					return nil, runtimedb.TrackMemoryPrivacy(ctx, 0, err)
				}
				for _, r := range records {
					runtimedb.TrackMemoryPrivacy(ctx, r.Seq, nil)
					out = append(out, butler.MemoryHit{
						Content: r.Content, Kind: r.Kind, Subject: r.Subject,
						Source: r.SourceKind, Age: time.Since(r.ObservedAt),
					})
				}
			}
			return out, nil
		},
		Recall: func(ctx context.Context, text string, limit int) ([]butler.MemoryHit, error) {
			hits, err := db.SearchIndex(runtimedb.IndexQuery{
				ReadContext: read, Reader: member, Butler: butlerName, Text: text, Limit: limit,
			})
			if err != nil {
				return nil, runtimedb.TrackMemoryPrivacy(ctx, 0, err)
			}
			out := make([]butler.MemoryHit, 0, len(hits))
			for _, h := range hits {
				runtimedb.TrackMemoryPrivacy(ctx, h.MemorySeq, nil)
				hit := butler.MemoryHit{
					Content: h.Content, Source: h.SourceKind, Age: h.Age,
					MustVerifyLive: h.MustVerifyLive,
				}
				// An indexed chunk knows its permissions and not what
				// sort of claim it is, so the record behind it supplies
				// the two things a session must be told: WHO it is
				// about, and whether anybody actually said it.
				//
				// Without the subject a hit is a free-floating sentence
				// — the bug the preamble hit on its first live run.
				// Without the kind an inference reads as a statement,
				// which is the certainty A3.1 says a Butler must not
				// invent.
				//
				// GetMemory does not filter by permission, and does not
				// need to: these seqs came back from a query whose
				// permission clause was in the candidate SQL, so the
				// reader is already entitled to every one of them.
				if h.MemorySeq != 0 {
					if r, err := db.GetMemory(h.MemorySeq); err == nil {
						hit.Kind, hit.Subject = r.Kind, r.Subject
					}
				}
				out = append(out, hit)
			}
			return out, nil
		},
		Remember: func(_ context.Context, n butler.MemoryNote) error {
			if n.SourceKind == "" || n.SourceID == "" {
				return fmt.Errorf("butlerstore: a remembered note needs the conversation it came from (ADR-W030 A3.1)")
			}
			scope := n.Scope
			if scope == "" {
				scope = runtimedb.MemoryScopePersonal
			}
			if scope != runtimedb.MemoryScopePersonal && scope != runtimedb.MemoryScopeAgentLocal {
				return fmt.Errorf("remember cannot create Household memory")
			}
			rec := runtimedb.MemoryRecord{
				Scope:   scope,
				Kind:    n.Kind,
				Subject: n.Subject,
				Content: n.Content,
				// The speaker owns it, always, a room included. A3.1:
				// "I dislike cilantro" said in the kitchen is that
				// person's preference and not everyone's — the room is
				// recorded as the source, which is what n.SourceKind
				// carries, and it does not touch who this belongs to.
				Owner: member,
				// Private, always. There is no field on the note
				// through which a model could ask for shared, and there
				// is no branch here that would grant it: household-
				// shared memory is an adult's deliberate act with a
				// digest behind it (A1), not something a sentence in a
				// conversation performs.
				Audience: runtimedb.ButlerAudiencePrivate,
				// The stamp is provenance for Personal memory; only Agent-local
				// records use it as an access boundary. Both remain Person-private.
				Butler:          butlerName,
				PermissionLabel: butlerName,
				SourceKind:      n.SourceKind,
				SourceID:        n.SourceID,
				ObservedAt:      time.Now(),
				// Stated, because that is what this path is: somebody
				// said it. The two lines below are the ones that differ
				// when inference capture lands — an inference is the
				// BUTLER's, drawn about the person, and filing the two
				// as the same claim is what A3.1 exists to stop.
				Confidence: runtimedb.ConfidenceStated,
				Author:     member,
			}
			// The store refuses a record with no owner, no audience, no
			// permission label or no provenance. Nothing above can
			// produce one — but the refusal is the guarantee, and it
			// belongs where every writer meets it rather than here.
			seq, err := db.RecordMemory(rec)
			if errors.Is(err, runtimedb.ErrSourceSuppressed) {
				return butler.ErrForgotten
			}
			if err != nil {
				return err
			}
			// Searchable now rather than at the next sweep.
			//
			// Indexing is a queued job for DOCUMENTS, where the slow
			// part is the embedding and a person waiting on an answer
			// should not wait on it. A note is one row and there is no
			// embedder yet; queueing it would mean telling somebody it
			// is remembered while recall cannot find it until a worker
			// runs.
			//
			// MemorySeq is what makes forgetting whole: correcting and
			// forgetting delete the chunks that point at the record, in
			// their own transaction, so a note taken back does not live
			// on as a search hit.
			if _, err := db.IndexDocumentInto(runtimedb.IndexDocument{
				SourceKind: n.SourceKind, SourceID: n.SourceID, MemorySeq: seq,
				Owner: member, Audience: runtimedb.ButlerAudiencePrivate,
				PermissionLabel: butlerName, Butler: butlerName,
				Content: n.Content, ObservedAt: rec.ObservedAt,
			}); err != nil {
				// The note IS kept, and saying otherwise would have the
				// person say it again — two records, one of them a lie.
				// What was lost is finding it by searching, which the
				// next reindex restores.
				if log != nil {
					log.Error("a remembered note could not be indexed; it is stored, and recall will not find it until it is reindexed",
						"memory", seq, "butler", butlerName, "err", err)
				}
			}
			return nil
		},
	}
}
