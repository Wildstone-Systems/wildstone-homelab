package kubeapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/kubeapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/kubeapi/kubeapitest"
)

func TestMicroTimeRoundTrip(t *testing.T) {
	in := kubeapi.MicroTime{Time: time.Date(2026, 10, 25, 4, 5, 6, 123456000, time.UTC)}
	b, err := json.Marshal(in)
	if err != nil || string(b) != `"2026-10-25T04:05:06.123456Z"` {
		t.Fatalf("marshal = %s, %v", b, err)
	}
	var out kubeapi.MicroTime
	if err := json.Unmarshal(b, &out); err != nil || !out.Equal(in.Time) {
		t.Fatalf("unmarshal = %v, %v", out, err)
	}
	if b, _ := json.Marshal(kubeapi.MicroTime{}); string(b) != "null" {
		t.Fatalf("zero time = %s, want null", b)
	}
}

func TestRefusesAnythingButAnAPIServerURL(t *testing.T) {
	tok := func() (string, error) { return "t", nil }
	for _, u := range []string{"", "127.0.0.1:6443", "ftp://h", "https://h/api/v1", "https://"} {
		if _, err := kubeapi.NewWithHTTP(u, tok, http.DefaultClient); err == nil {
			t.Errorf("NewWithHTTP(%q) was accepted", u)
		}
	}
}

func TestStatusErrors(t *testing.T) {
	srv := kubeapitest.New()
	defer srv.Close()
	c := srv.Client("someone")
	ctx := context.Background()

	if _, err := c.GetLease(ctx, "ns", "missing"); !kubeapi.IsNotFound(err) {
		t.Fatalf("missing lease: %v", err)
	}
	l, err := c.CreateLease(ctx, &kubeapi.Lease{Metadata: kubeapi.ObjectMeta{Namespace: "ns", Name: "l"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateLease(ctx, &kubeapi.Lease{Metadata: kubeapi.ObjectMeta{Namespace: "ns", Name: "l"}}); !kubeapi.IsConflict(err) {
		t.Fatalf("second create: %v, want conflict", err)
	}
	stale := *l
	if _, err := c.UpdateLease(ctx, l); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateLease(ctx, &stale); !kubeapi.IsConflict(err) {
		t.Fatalf("stale update: %v, want conflict", err)
	}
	stale.Metadata.ResourceVersion = ""
	if _, err := c.UpdateLease(ctx, &stale); err == nil || kubeapi.IsConflict(err) {
		t.Fatalf("update without a resourceVersion: %v, want a local refusal", err)
	}
	if _, err := c.UpdateConfigMap(ctx, &kubeapi.ConfigMap{Metadata: kubeapi.ObjectMeta{Namespace: "ns", Name: "c"}}); err == nil {
		t.Fatal("a ConfigMap update without a resourceVersion was sent")
	}
}

func TestDryRunIsMarkedAndForbiddenIsReported(t *testing.T) {
	var gotQuery string
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","code":403,"reason":"Forbidden","message":"denied request: refused"}`))
	}))
	defer hs.Close()
	c, err := kubeapi.NewWithHTTP(hs.URL, func() (string, error) { return "t", nil }, hs.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = c.DryRunCreateConfigMap(context.Background(), &kubeapi.ConfigMap{Metadata: kubeapi.ObjectMeta{Namespace: "ns", GenerateName: "b-"}})
	if !kubeapi.IsForbidden(err) || gotQuery != "dryRun=All" {
		t.Fatalf("err = %v, query = %q", err, gotQuery)
	}
}
