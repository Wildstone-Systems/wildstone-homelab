package runtimedb

import (
	"strings"
	"testing"
	"time"
)

func reading(meter string, value float64, at time.Time) MeterReading {
	return MeterReading{Meter: meter, Value: value, At: at,
		RecordedBy: "mika", Source: ReadingByPerson}
}

func mustAppend(t *testing.T, db *DB, r MeterReading) int64 {
	t.Helper()
	seq, dup, err := db.AppendMeterReading(r)
	if err != nil {
		t.Fatalf("AppendMeterReading: %v", err)
	}
	if dup {
		t.Fatalf("AppendMeterReading reported a duplicate for %+v", r)
	}
	return seq
}

func TestReadingsComeBackInTheOrderTheyWereTaken(t *testing.T) {
	db := open(t)
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	// Written out of order on purpose: the row order and the reading
	// order are different things, and the second is the one that matters.
	mustAppend(t, db, reading("odo", 82000, base.AddDate(0, 2, 0)))
	mustAppend(t, db, reading("odo", 79300, base))
	mustAppend(t, db, reading("odo", 80500, base.AddDate(0, 1, 0)))

	got, err := db.ListMeterReadings("odo", 0)
	if err != nil {
		t.Fatalf("ListMeterReadings: %v", err)
	}
	var values []float64
	for _, r := range got {
		values = append(values, r.Value)
	}
	if len(values) != 3 || values[0] != 79300 || values[1] != 80500 || values[2] != 82000 {
		t.Errorf("values = %v, want oldest first", values)
	}
}

func TestLatestReadingIsTheNewestTakenNotTheLastWritten(t *testing.T) {
	db := open(t)
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	mustAppend(t, db, reading("odo", 82000, base.AddDate(0, 2, 0)))
	// Recorded afterwards, but taken two months earlier — somebody
	// catching up on a number they wrote on a scrap of paper.
	mustAppend(t, db, reading("odo", 79300, base))

	got, err := db.LatestMeterReading("odo")
	if err != nil {
		t.Fatalf("LatestMeterReading: %v", err)
	}
	if got.Value != 82000 {
		t.Errorf("latest = %g, want 82000: the later row was an older reading", got.Value)
	}
}

func TestAtAndRecordedAtAreDifferentFacts(t *testing.T) {
	db := open(t)
	morning := time.Date(2026, 6, 1, 7, 30, 0, 0, time.UTC)
	mustAppend(t, db, reading("odo", 79300, morning))

	got, err := db.LatestMeterReading("odo")
	if err != nil {
		t.Fatalf("LatestMeterReading: %v", err)
	}
	if !got.At.Equal(morning) {
		t.Errorf("At = %s, want the moment it was read", got.At)
	}
	if got.RecordedAt.Before(morning) || got.RecordedAt.Equal(morning) {
		t.Errorf("RecordedAt = %s, want when Keystone heard about it", got.RecordedAt)
	}
}

// A publisher retrying a lost delivery must not record the number twice.
func TestARetriedPublisherDeliveryIsANoOp(t *testing.T) {
	db := open(t)
	at := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	r := MeterReading{Meter: "odo", Value: 79300, At: at,
		RecordedBy: "publisher-home-automation", Source: ReadingByPublisher,
		ExternalID: "ha-0001"}

	first := mustAppend(t, db, r)
	seq, dup, err := db.AppendMeterReading(r)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !dup || seq != first {
		t.Errorf("retry gave seq %d dup %v, want %d and true", seq, dup, first)
	}
	got, _ := db.ListMeterReadings("odo", 0)
	if len(got) != 1 {
		t.Errorf("%d readings after a retry, want 1", len(got))
	}
}

// A person is not retrying anything. Two identical numbers an hour apart
// are two real observations of a truck that did not move, and collapsing
// them would quietly lose one.
func TestAPersonMayRecordTheSameNumberTwice(t *testing.T) {
	db := open(t)
	at := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	mustAppend(t, db, reading("odo", 79300, at))
	mustAppend(t, db, reading("odo", 79300, at.Add(time.Hour)))

	got, _ := db.ListMeterReadings("odo", 0)
	if len(got) != 2 {
		t.Errorf("%d readings, want 2: neither was a retry", len(got))
	}
}

func TestARebaseCarriesItsPriorValue(t *testing.T) {
	db := open(t)
	at := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
	mustAppend(t, db, MeterReading{Meter: "odo", Value: 0, At: at,
		RecordedBy: "mika", Source: ReadingByPerson,
		Rebase: true, Prior: 84210, Reason: "cluster replaced"})

	got, err := db.LatestMeterReading("odo")
	if err != nil {
		t.Fatalf("LatestMeterReading: %v", err)
	}
	if !got.Rebase || got.Prior != 84210 || got.Reason != "cluster replaced" {
		t.Errorf("rebase lost its detail: %+v", got)
	}
}

func TestAppendRefusesAnIncoherentReading(t *testing.T) {
	db := open(t)
	at := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		r    MeterReading
		want string
	}{
		{"no meter", MeterReading{Value: 1, At: at, RecordedBy: "m", Source: ReadingByPerson}, "the meter it is of"},
		{"nobody recorded it", MeterReading{Meter: "odo", Value: 1, At: at, Source: ReadingByPerson}, "who recorded it"},
		{"no time", MeterReading{Meter: "odo", Value: 1, RecordedBy: "m", Source: ReadingByPerson}, "when it was read"},
		{"no source", MeterReading{Meter: "odo", Value: 1, At: at, RecordedBy: "m"}, "person|publisher"},
		{"invented source", MeterReading{Meter: "odo", Value: 1, At: at, RecordedBy: "m", Source: "smtp"}, "person|publisher"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := db.AppendMeterReading(tc.r); err == nil {
				t.Fatalf("accepted %s", tc.name)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestMeterNamesListsOnlyMetersThatHaveBeenRead(t *testing.T) {
	db := open(t)
	at := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	mustAppend(t, db, reading("odo", 1, at))
	mustAppend(t, db, reading("furnace-hours", 2, at))
	mustAppend(t, db, reading("odo", 3, at.Add(time.Hour)))

	got, err := db.MeterNames()
	if err != nil {
		t.Fatalf("MeterNames: %v", err)
	}
	if len(got) != 2 || got[0] != "furnace-hours" || got[1] != "odo" {
		t.Errorf("names = %v, want [furnace-hours odo]", got)
	}
}

func TestAnUnreadMeterIsNotFoundRatherThanZero(t *testing.T) {
	db := open(t)
	if _, err := db.LatestMeterReading("never-read"); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound: a meter nobody has read does not read zero", err)
	}
	got, err := db.ListMeterReadings("never-read", 0)
	if err != nil {
		t.Fatalf("ListMeterReadings: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d readings for a meter nobody has read", len(got))
	}
}
