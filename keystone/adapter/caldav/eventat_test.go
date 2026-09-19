package caldav

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// An event created here reads back at the address the create returned,
// with the time it was written for. That address is what a booking keeps
// so it can follow the appointment afterwards.
func TestAnEventReadsBackAtTheAddressItWasCreatedAt(t *testing.T) {
	r := newRecorder(t)
	u := writable(r)
	summary := "Kai: dentist"
	ans, err := u.CreateEvent(context.Background(), EventWrite{
		Summary: &summary, Start: "2026-10-14T15:30:00Z", End: "2026-10-14T16:30:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	href, _ := ans.Data.(map[string]any)["href"].(string)
	ev, err := u.EventAt(context.Background(), href)
	if err != nil || ev == nil {
		t.Fatalf("EventAt(%q) = %v, %v", href, ev, err)
	}
	if !ev.Start.Equal(time.Date(2026, 10, 14, 15, 30, 0, 0, time.UTC)) || ev.Summary != summary || ev.Href != href {
		t.Errorf("read back %+v", ev)
	}
}

// A deleted event is an answer, not a failure: (nil, nil). An
// unreachable calendar is a failure, so the caller never mistakes an
// outage for a cancellation.
func TestADeletedEventIsNilAndAnOutageIsAnError(t *testing.T) {
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer gone.Close()
	u := &Upstream{CollectionURL: gone.URL + "/c/", Username: "sam", Password: "x"}
	if ev, err := u.EventAt(context.Background(), gone.URL+"/c/x.ics"); ev != nil || err != nil {
		t.Errorf("a deleted event read as %v, %v; want nil, nil", ev, err)
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer down.Close()
	u.CollectionURL = down.URL + "/c/"
	if _, err := u.EventAt(context.Background(), down.URL+"/c/x.ics"); err == nil {
		t.Error("an unreachable calendar read as a deleted event")
	}
}
