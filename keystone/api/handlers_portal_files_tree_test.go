package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
)

func fileMutation(t *testing.T, h *harness, op, body string, want int) {
	t.Helper()
	res := h.member(http.MethodPost, "/api/v1/filedrop/"+op, "sam", "filedrop_users", strings.NewReader(body), "application/json")
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode != want {
		t.Fatalf("%s %s: %d %s, want %d", op, body, res.StatusCode, data, want)
	}
}

func TestFileManagerAPI(t *testing.T) {
	for _, ns := range []string{"shared", "me"} {
		t.Run(ns, func(t *testing.T) {
			h, root := portalHarness(t)
			fileMutation(t, h, "mkdir", `{"namespace":"`+ns+`","path":"folder"}`, 201)
			fileMutation(t, h, "mkdir", `{"namespace":"`+ns+`","path":"archive"}`, 201)
			res := h.member("POST", "/api/v1/filedrop/upload?namespace="+ns+"&path=folder&name=private.txt", "sam", "filedrop_users", strings.NewReader("hello"), "application/octet-stream")
			res.Body.Close()
			if res.StatusCode != 201 {
				t.Fatal("upload", res.StatusCode)
			}
			res = h.member("GET", "/api/v1/filedrop/list?namespace="+ns+"&path=folder", "sam", "filedrop_users", nil, "")
			var listing struct{ Items []portal.Item }
			json.NewDecoder(res.Body).Decode(&listing)
			res.Body.Close()
			if len(listing.Items) != 1 || listing.Items[0].Path != "folder/private.txt" || listing.Items[0].Type != "file" {
				t.Fatal(listing)
			}
			res = h.member("GET", "/api/v1/filedrop/file?namespace="+ns+"&path=folder/private.txt", "sam", "filedrop_users", nil, "")
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != 200 || string(b) != "hello" || res.Header.Get("Content-Type") != "application/octet-stream" || res.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment;") {
				t.Fatalf("download %d %q %v", res.StatusCode, b, res.Header)
			}
			fileMutation(t, h, "delete", `{"namespace":"`+ns+`","path":"folder"}`, 409)
			fileMutation(t, h, "rename", `{"namespace":"`+ns+`","path":"folder/private.txt","name":"renamed.txt"}`, 200)
			fileMutation(t, h, "move", `{"namespace":"`+ns+`","path":"folder/renamed.txt","to":"archive/renamed.txt"}`, 200)
			fileMutation(t, h, "move", `{"namespace":"`+ns+`","path":"folder","to":"archive"}`, 409)
			fileMutation(t, h, "delete", `{"namespace":"`+ns+`","path":"archive/renamed.txt"}`, 200)
			fileMutation(t, h, "delete", `{"namespace":"`+ns+`","path":"folder"}`, 200)
			namespaceRoot := "Shared"
			if ns == "me" {
				namespaceRoot = filepath.Join("Members", "sam")
			}
			if _, err := os.Stat(filepath.Join(root, namespaceRoot, "folder")); !os.IsNotExist(err) {
				t.Fatal(err)
			}
			audits, err := h.s.DB.ListAudit(50)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, a := range audits {
				if a.Path == "/api/v1/filedrop/move" && a.ActingFor == "sam" && a.Status == 200 {
					found = true
				}
			}
			if !found {
				t.Fatal("move not audited for actor")
			}
			events, err := h.s.DB.EventsSince(time.Time{}, 100)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(events)
			if !strings.Contains(string(encoded), "renamed.txt") {
				t.Fatal("operator events omit path")
			}
			household, err := h.s.DB.HouseholdEventsAfter(0, 50)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ = json.Marshal(household)
			for _, name := range []string{"private.txt", "renamed.txt"} {
				if strings.Contains(string(encoded), name) {
					t.Fatal("household event leaks filename", string(encoded))
				}
			}

		})
	}
}

func TestSharedFileEndpointsRefuseUntrustedPaths(t *testing.T) {
	h, _ := portalHarness(t)
	for _, raw := range []string{"../x", "/etc/passwd", "a//b", "a/./b", "a\\b", "a\x00b", strings.Repeat("a/", 32) + "b", strings.Repeat("x", 256)} {
		for _, endpoint := range []string{"list", "file", "upload"} {
			t.Run(endpoint+"/"+url.QueryEscape(raw), func(t *testing.T) {
				h.unthrottled()
				method := "GET"
				if endpoint == "upload" {
					method = "POST"
				}
				res := h.member(method, "/api/v1/filedrop/"+endpoint+"?namespace=shared&path="+url.QueryEscape(raw)+"&name=leaf", "sam", "filedrop_users", strings.NewReader("x"), "application/octet-stream")
				res.Body.Close()
				if res.StatusCode != 400 {
					t.Fatal(res.StatusCode)
				}
			})
		}
		for _, op := range []string{"mkdir", "rename", "move", "delete"} {
			h.unthrottled()
			body, _ := json.Marshal(map[string]string{"namespace": "shared", "path": raw, "name": "renamed", "to": "target"})
			fileMutation(t, h, op, string(body), 400)
		}
	}
	for _, ns := range []string{"photos"} {
		h.unthrottled()
		fileMutation(t, h, "mkdir", `{"namespace":"`+ns+`","path":"folder"}`, 400)
		h.unthrottled()
		fileMutation(t, h, "move", `{"namespace":"`+ns+`","path":"a","to":"b"}`, 400)
	}
	for _, body := range []string{`{"namespace":"shared","path":"a","unknown":true}`, `{"path":"a"} {"path":"b"}`, strings.Repeat(" ", 17000)} {
		h.unthrottled()
		fileMutation(t, h, "mkdir", body, 400)
	}
}

func TestNewFileMutationsUseThePortalDoor(t *testing.T) {
	h, _ := portalHarness(t)
	for _, op := range []string{"mkdir", "move", "rename", "delete"} {
		h.unthrottled()
		res := h.member("POST", "/api/v1/filedrop/"+op, "sam", "media_users", strings.NewReader(`{"namespace":"shared","path":"folder","to":"other"}`), "application/json")
		res.Body.Close()
		if res.StatusCode != 403 {
			t.Fatal(op, res.StatusCode)
		}
		req, err := http.NewRequest("POST", h.srv.URL+"/api/v1/filedrop/"+op, strings.NewReader(`{"path":"folder"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(TrustedIngressHeader, "sam")
		req.Header.Set(ConsoleGroupsHeader, "filedrop_users")
		req.Header.Set("Origin", "https://elsewhere.invalid")
		req.Header.Set("Content-Type", "application/json")
		res, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 403 {
			t.Fatal("cross-site", op, res.StatusCode)
		}
	}
}

func TestFiledropMountGuardReachesCapabilitiesAndBothUploadAPIs(t *testing.T) {
	h, root := portalHarness(t)
	desired, err := h.s.State.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range desired {
		if r.ConfigFile != nil && r.ConfigFile.Filename == filedropSettingsFile {
			r.ConfigFile.Content = "root: " + root + "\nrequiredMount: " + root + "\nmaxUploadMB: 1\n"
			if err := h.s.State.Put(r); err != nil {
				t.Fatal(err)
			}
		}
	}
	mount, err := h.s.FiledropRequiredMount()
	if err != nil || mount != root {
		t.Fatal(mount, err)
	}
	res := h.member("GET", "/api/v1/filedrop/capabilities", "sam", "filedrop_users", nil, "")
	var caps struct{ Ready bool }
	if err := json.NewDecoder(res.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if caps.Ready {
		t.Fatal("ordinary directory advertised as mounted storage")
	}
	for _, suffix := range []string{"", "&path="} {
		res := h.member("POST", "/api/v1/filedrop/upload?namespace=shared&name=file"+suffix, "sam", "filedrop_users", strings.NewReader("x"), "application/octet-stream")
		data, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 503 || strings.Contains(string(data), root) {
			t.Fatalf("%d %s", res.StatusCode, data)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("created local tree: %v %v", entries, err)
	}
}

func TestPersonalTreeIsolation(t *testing.T) {
	h, _ := portalHarness(t)
	request := func(member, method, endpoint, body string, want int) string {
		t.Helper()
		h.unthrottled()
		res := h.member(method, "/api/v1/filedrop/"+endpoint, member, "filedrop_users", strings.NewReader(body), "application/json")
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		if res.StatusCode != want {
			t.Fatalf("%s %s: %d %s, want %d", member, endpoint, res.StatusCode, data, want)
		}
		return string(data)
	}
	request("sam", "POST", "mkdir", `{"namespace":"me","path":"private"}`, 201)
	request("sam", "POST", "upload?namespace=me&path=private&name=secret.txt", "sam's private content", 201)
	request("alex", "POST", "mkdir", `{"namespace":"me","path":"private"}`, 201)
	if got := request("alex", "GET", "list?namespace=me&path=private", "", 200); strings.Contains(got, "secret.txt") {
		t.Fatal("leaked another member's filename")
	}
	request("alex", "GET", "file?namespace=me&path=private/secret.txt&member=sam", "", 404)
	for _, op := range []string{"rename", "move", "delete"} {
		request("alex", "POST", op, `{"namespace":"me","path":"private/secret.txt","name":"stolen.txt","to":"private/stolen.txt"}`, 404)
	}
	for _, p := range []string{"../sam/private/secret.txt", "../../Shared/file"} {
		request("alex", "GET", "file?namespace=me&path="+url.QueryEscape(p), "", 400)
	}
	request("alex", "POST", "upload?namespace=me&path=private&name=secret.txt", "alex's private content", 201)
	if got := request("sam", "GET", "file?namespace=me&path=private/secret.txt", "", 200); got != "sam's private content" {
		t.Fatal("changed another member's file", got)
	}
	if got := request("alex", "GET", "file?namespace=me&path=private/secret.txt", "", 200); got != "alex's private content" {
		t.Fatal("wrong private file", got)
	}
}
