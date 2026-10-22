package caldav

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMoveConfinesDestinationAndRefusesOverwrite(t *testing.T) {
	calls := 0
	var base string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "MOVE" || r.Header.Get("Overwrite") != "F" || r.Header.Get("Destination") != base+"/school/event.ics" {
			t.Errorf("unsafe move: %s %v", r.Method, r.Header)
		}
		w.WriteHeader(201)
	}))
	defer server.Close()
	base = server.URL
	u := &Upstream{CollectionURL: base + "/household/", Writable: true}
	href, err := u.MoveEvent(context.Background(), "/household/event.ics", base+"/school/")
	if err != nil || href != base+"/school/event.ics" {
		t.Fatal(href, err)
	}
	for _, dst := range []string{"https://other.test/school/", base + "/school/?redirect=elsewhere"} {
		if _, err := u.MoveEvent(context.Background(), "/household/event.ics", dst); err == nil {
			t.Fatal("accepted", dst)
		}
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}
func TestCalendarNeverForwardsCredentialsOnRedirect(t *testing.T) {
	leaked := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true; w.WriteHeader(204) }))
	defer other.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL+"/stolen", 307) }))
	defer source.Close()
	u := &Upstream{CollectionURL: source.URL + "/calendar/", Username: "service", Password: "fixture", Writable: true}
	_, err := u.DeleteEvent(context.Background(), EventWrite{Href: "/calendar/event.ics"})
	if err == nil || leaked {
		t.Fatal("followed redirect", err)
	}
	for _, href := range []string{other.URL + "/event.ics", "/somebody/personal/event.ics", "../private/event.ics"} {
		if _, err := u.resolveHref(EventWrite{Href: href}); err == nil {
			t.Fatal("accepted", href)
		}
	}
	if _, err := u.resolveHref(EventWrite{Href: "/calendar/event.ics"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(source.URL, "http://") {
		t.Fatal("fixture")
	}
}
