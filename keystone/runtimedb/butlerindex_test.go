package runtimedb

// P9's evidence (ADR-W030): retrieval quality and resource limits are
// recorded, permissions filter before model context, source deletion and
// revocation remove dependent entries and summaries, and changing app
// state is verified live rather than trusted from the index.

import (
	"fmt"
	"sort"
	"testing"
	"time"
)

func indexed(t *testing.T, db *DB, content, owner string, audience ButlerTaskAudience) int64 {
	t.Helper()
	seq, err := db.IndexDocumentInto(IndexDocument{
		SourceKind: "conversation", SourceID: "bs-1", Owner: owner, Audience: audience,
		PermissionLabel: owner, Content: content, ObservedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("IndexDocumentInto: %v", err)
	}
	return seq
}

func TestIndexNeedsPermissionMetadata(t *testing.T) {
	db := butlerTestDB(t)
	base := IndexDocument{
		SourceKind: "conversation", SourceID: "bs-1", Owner: "sam",
		Audience: ButlerAudienceShared, PermissionLabel: "sam", Content: "dinner is at seven",
	}
	for name, mutate := range map[string]func(*IndexDocument){
		"no owner":     func(d *IndexDocument) { d.Owner = "" },
		"no label":     func(d *IndexDocument) { d.PermissionLabel = "" },
		"no audience":  func(d *IndexDocument) { d.Audience = "" },
		"bad audience": func(d *IndexDocument) { d.Audience = "everyone" },
		"no content":   func(d *IndexDocument) { d.Content = "" },
		"no source":    func(d *IndexDocument) { d.SourceID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			d := base
			mutate(&d)
			if _, err := db.IndexDocumentInto(d); err == nil {
				t.Fatal("an indexed chunk without permission metadata was accepted")
			}
		})
	}
	if _, err := db.SearchIndex(IndexQuery{Text: "dinner"}); err == nil {
		t.Fatal("the index answered a question that was not on anyone's behalf")
	}
}

// Permissions filter in the candidate query, not over its results.
func TestRetrievalFiltersBeforeRanking(t *testing.T) {
	db := butlerTestDB(t)
	indexed(t, db, "dinner is at seven on Thursday", "household", ButlerAudienceShared)
	indexed(t, db, "sam has a hospital appointment on Thursday", "sam", ButlerAudiencePrivate)

	hits, err := db.SearchIndex(IndexQuery{Reader: "mika", Text: "Thursday"})
	if err != nil {
		t.Fatalf("SearchIndex: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("mika got %d hits, want the shared one only: %+v", len(hits), hits)
	}
	if hits[0].Content != "dinner is at seven on Thursday" {
		t.Fatalf("hit = %q", hits[0].Content)
	}
	// Sam sees their own.
	own, err := db.SearchIndex(IndexQuery{Reader: "sam", Text: "Thursday"})
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 2 {
		t.Fatalf("sam got %d hits, want both: %+v", len(own), own)
	}
	// An empty question matches nothing rather than everything.
	empty, err := db.SearchIndex(IndexQuery{Reader: "sam", Text: "   "})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("an empty question returned %d rows", len(empty))
	}
	// A question containing FTS5 syntax is a question, not an
	// expression.
	if _, err := db.SearchIndex(IndexQuery{Reader: "sam", Text: `what's on AND "Thursday"`}); err != nil {
		t.Fatalf("a question with punctuation failed to parse: %v", err)
	}
}

// A volatile source is never an answer: it comes back flagged for a
// live read, because the index knows what WAS true.
func TestVolatileSourcesDemandLiveVerification(t *testing.T) {
	db := butlerTestDB(t)
	if _, err := db.IndexDocumentInto(IndexDocument{
		SourceKind: "home-assistant", SourceID: "light.kitchen", Owner: "household",
		Audience: ButlerAudienceShared, PermissionLabel: "household",
		Content: "the kitchen light is on", ObservedAt: time.Now().Add(-time.Hour),
		Volatile: true,
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := db.SearchIndex(IndexQuery{Reader: "household", Text: "kitchen light"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v", hits)
	}
	if !hits[0].MustVerifyLive {
		t.Fatal("a volatile source came back as an answer rather than a hint to read live")
	}
	if hits[0].Age < 50*time.Minute {
		t.Fatalf("age = %s; a caller cannot say \"as of an hour ago\" without it", hits[0].Age)
	}
	// A freshness bound drops it entirely.
	fresh, err := db.SearchIndex(IndexQuery{Reader: "household", Text: "kitchen light", MaxAge: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 0 {
		t.Fatalf("a stale reading survived a freshness bound: %+v", fresh)
	}
}

// Deleting a source removes its entries AND the searchable text: a
// revocation that leaves a ghost to match on has revoked nothing.
func TestDeletionRemovesEntriesAndSearchableText(t *testing.T) {
	db := butlerTestDB(t)
	indexed(t, db, "the gate code is on the fridge", "household", ButlerAudienceShared)
	if hits, _ := db.SearchIndex(IndexQuery{Reader: "household", Text: "gate code"}); len(hits) != 1 {
		t.Fatalf("the chunk was not indexed: %+v", hits)
	}
	n, err := db.RemoveIndexForSource("conversation", "bs-1")
	if err != nil || n != 1 {
		t.Fatalf("RemoveIndexForSource = %d, %v", n, err)
	}
	hits, err := db.SearchIndex(IndexQuery{Reader: "household", Text: "gate code"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("a removed source is still searchable: %+v", hits)
	}
}

// Forgetting a memory record takes its index entries and its summaries
// with it.
func TestForgottenMemoryLeavesTheIndex(t *testing.T) {
	db := butlerTestDB(t)
	memSeq, err := db.RecordMemory(MemoryRecord{
		Kind: MemoryFact, Content: "the spare key is under the mat", Subject: "household",
		Audience: ButlerAudienceShared, PermissionLabel: "household", Owner: "household",
		Author: "mika", SourceKind: "conversation", SourceID: "bs-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.IndexDocumentInto(IndexDocument{
		SourceKind: "memory", SourceID: fmt.Sprintf("%d", memSeq), MemorySeq: memSeq,
		Owner: "household", Audience: ButlerAudienceShared, PermissionLabel: "household",
		Content: "the spare key is under the mat", ObservedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ForgetMemory(memSeq, "mika"); err != nil {
		t.Fatal(err)
	}
	// Immediately, with no sweep in between. "I asked you to forget
	// that" cannot mean "once a background job gets round to it".
	hits, err := db.SearchIndex(IndexQuery{Reader: "household", Text: "spare key"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("a forgotten record was still retrievable before any compaction: %+v", hits)
	}
	// The chunk is gone, not merely filtered — so there is nothing left
	// for a later compaction to find.
	pruned, _, err := db.CompactIndex(0)
	if err != nil {
		t.Fatalf("CompactIndex: %v", err)
	}
	if pruned != 0 {
		t.Fatalf("compaction still found %d chunks to prune, so forgetting left them behind", pruned)
	}

	// And the filter is a real backstop, not decoration: a chunk that
	// some other writer puts back for a forgotten record is still not
	// an answer, and the sweep is what finally removes it.
	if _, err := db.ExecForTest(
		`INSERT INTO butler_index (source_kind, source_id, memory, owner, audience,
		     permission_label, content, observed_at, indexed_at, volatile, tokens)
		 VALUES ('memory', 'stray', ?, 'household', 'shared', 'household',
		     'the spare key is under the mat', '', ?, 0, 7)`,
		memSeq, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if hits, _ := db.SearchIndex(IndexQuery{Reader: "household", Text: "spare key"}); len(hits) != 0 {
		t.Fatalf("a stray chunk for a forgotten record was returned: %+v", hits)
	}
	if pruned, _, err := db.CompactIndex(0); err != nil || pruned != 1 {
		t.Fatalf("compaction pruned %d stray chunks (err %v), want 1", pruned, err)
	}
}

// Background work is background: it starts behind anything a person is
// waiting on.
func TestIndexJobsAreLocalFirstAndOrdered(t *testing.T) {
	db := butlerTestDB(t)
	if _, err := db.QueueIndexJob(IndexJobIndex, `{"source":"bs-1"}`, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.QueueIndexJob(IndexJobCompact, "", 0); err != nil {
		t.Fatal(err)
	}
	// Something a person is waiting on goes in front without renumbering
	// anything.
	urgent, err := db.QueueIndexJob(IndexJobReindex, `{"why":"a correction"}`, 1)
	if err != nil {
		t.Fatal(err)
	}
	job, ok, err := db.ClaimIndexJob()
	if err != nil || !ok {
		t.Fatalf("ClaimIndexJob = %v, %t", err, ok)
	}
	if job.Seq != urgent {
		t.Fatalf("claimed job %d, want the interactive one (%d)", job.Seq, urgent)
	}
	if job.Priority >= BackgroundJobPriority {
		t.Fatalf("interactive priority = %d, not ahead of background (%d)", job.Priority, BackgroundJobPriority)
	}
	if err := db.FinishIndexJob(job.Seq, ""); err != nil {
		t.Fatal(err)
	}
	// The rest come out in queue order, and an empty queue is idle
	// rather than an error.
	for range 2 {
		j, ok, err := db.ClaimIndexJob()
		if err != nil || !ok {
			t.Fatalf("claiming background work = %v, %t", err, ok)
		}
		if err := db.FinishIndexJob(j.Seq, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, err := db.ClaimIndexJob(); err != nil || ok {
		t.Fatalf("an empty queue = %v, %t; want idle", err, ok)
	}
}

// The implementation choice is RECORDED, not asserted. This fixture run
// measures the keyword and hybrid strategies over the same corpus and
// stores what it found, which is what makes the choice reviewable.
func TestRetrievalQualityIsMeasuredAndRecorded(t *testing.T) {
	db := butlerTestDB(t)

	// A small corpus with a known right answer per query. Embeddings are
	// deterministic bag-of-words vectors, standing in for whatever local
	// model a deployment has — the point is the harness, not the model.
	corpus := []string{
		"bin day is Wednesday morning",
		"the spare key is under the mat by the back door",
		"sam is allergic to peanuts and shellfish",
		"dinner is usually at seven on weeknights",
		"the boiler pressure gauge should read one point five",
		"immich stores the family photos on the nas",
	}
	vocab := map[string]int{}
	for _, doc := range corpus {
		for _, w := range fields(doc) {
			if _, ok := vocab[w]; !ok {
				vocab[w] = len(vocab)
			}
		}
	}
	embed := func(text string) []float32 {
		v := make([]float32, len(vocab))
		for _, w := range fields(text) {
			if i, ok := vocab[w]; ok {
				v[i] = 1
			}
		}
		return v
	}
	for i, doc := range corpus {
		if _, err := db.IndexDocumentInto(IndexDocument{
			SourceKind: "fixture", SourceID: fmt.Sprintf("doc-%d", i), Owner: "household",
			Audience: ButlerAudienceShared, PermissionLabel: "household", Content: doc,
			ObservedAt: time.Now(), Embedding: embed(doc), Tokens: len(fields(doc)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	queries := []struct{ text, want string }{
		{"when is bin day", corpus[0]},
		{"where is the spare key", corpus[1]},
		{"what is sam allergic to", corpus[2]},
		{"what time is dinner", corpus[3]},
		{"boiler pressure", corpus[4]},
		{"where are the photos", corpus[5]},
	}

	for _, strategy := range []string{StrategyKeyword, StrategyHybrid} {
		hits, correct := 0, 0
		var latencies []time.Duration
		for _, q := range queries {
			query := IndexQuery{Reader: "household", Text: q.text, Limit: 3}
			if strategy == StrategyHybrid {
				query.Embedding = embed(q.text)
			}
			start := time.Now()
			got, err := db.SearchIndex(query)
			latencies = append(latencies, time.Since(start))
			if err != nil {
				t.Fatalf("%s search: %v", strategy, err)
			}
			hits += len(got)
			if len(got) > 0 && got[0].Content == q.want {
				correct++
			}
		}
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		p95 := latencies[len(latencies)*95/100]
		recall := correct * 100 / len(queries)

		if err := db.RecordIndexMeasurement(IndexMeasurement{
			Fixture: "household-6doc", Strategy: strategy, Queries: len(queries),
			Hits: hits, RecallPct: recall, P95Micros: int(p95.Microseconds()),
			Note: "top-1 exact-document recall over the household fixture corpus",
		}); err != nil {
			t.Fatalf("RecordIndexMeasurement: %v", err)
		}
		// The bar this fixture holds the chosen implementation to. It is
		// deliberately a floor rather than a target: a change that drops
		// below it should fail here rather than be discovered by someone
		// asking where the spare key is.
		if recall < 80 {
			t.Fatalf("%s top-1 recall = %d%%, below the 80%% floor this fixture holds", strategy, recall)
		}
	}

	measurements, err := db.ListIndexMeasurements()
	if err != nil {
		t.Fatal(err)
	}
	if len(measurements) != 2 {
		t.Fatalf("measurements = %+v", measurements)
	}
	for _, m := range measurements {
		if m.RecallPct == 0 || m.Queries == 0 {
			t.Fatalf("a measurement recorded nothing useful: %+v", m)
		}
		t.Logf("%s: recall %d%% over %d queries, p95 %dµs",
			m.Strategy, m.RecallPct, m.Queries, m.P95Micros)
	}
	// A measurement has to say what it measured.
	if err := db.RecordIndexMeasurement(IndexMeasurement{Strategy: StrategyHybrid}); err == nil {
		t.Fatal("a measurement with no fixture was recorded")
	}
	if err := db.RecordIndexMeasurement(IndexMeasurement{Fixture: "x", Strategy: "vibes"}); err == nil {
		t.Fatal("a measurement of an unknown strategy was recorded")
	}
}

// fields is the fixture's tokenizer: lowercase words, nothing clever.
func fields(s string) []string {
	var out []string
	word := make([]rune, 0, 16)
	flush := func() {
		if len(word) > 0 {
			out = append(out, string(word))
			word = word[:0]
		}
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			word = append(word, r)
		case r >= 'A' && r <= 'Z':
			word = append(word, r+32)
		case r >= '0' && r <= '9':
			word = append(word, r)
		default:
			flush()
		}
	}
	flush()
	return out
}
