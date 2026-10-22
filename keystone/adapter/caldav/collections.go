package caldav

// A person's own calendars (ADR-W034 C2): listing the calendars under
// their principal, and making them a new one.
//
// The principal is chosen by the caller from the person's proven sign-in,
// never from a request, and every href the server reports is confined to
// a direct child of that principal before it is used.

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Collection is one calendar under a principal.
type Collection struct {
	// Segment is the collection's own path segment under the principal,
	// unescaped, e.g. "personal".
	Segment string
	// URL is the collection's full address, ending in a slash.
	URL   string
	Name  string
	Color string
}

// ErrNoPrincipal is a create under a principal that does not exist yet:
// the person has never signed in to the calendar server.
var ErrNoPrincipal = errors.New("caldav: this person has no calendar account yet")

// segmentPattern is the collection names this adapter will address. Every
// common client names calendars within it (Radicale's own page and Apple
// use UUIDs, DAVx5 and Thunderbird letters and dashes). A collection named
// outside it is not shown rather than escaped.
var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidSegment reports whether a collection segment is one this adapter
// addresses.
func ValidSegment(s string) bool { return segmentPattern.MatchString(s) && strings.Trim(s, ".") != "" }

var colorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}([0-9a-fA-F]{2})?$`)

const maxCollections = 64

type collectionStatus struct {
	XMLName  xml.Name `xml:"multistatus"`
	Response []struct {
		Href     string `xml:"href"`
		Propstat []struct {
			Status string `xml:"status"`
			Prop   struct {
				DisplayName  string `xml:"displayname"`
				Color        string `xml:"calendar-color"`
				ResourceType struct {
					Inner []struct{ XMLName xml.Name } `xml:",any"`
				} `xml:"resourcetype"`
				Components struct {
					Comp []struct {
						Name string `xml:"name,attr"`
					} `xml:"comp"`
				} `xml:"supported-calendar-component-set"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

// Calendars lists the event calendars directly under principalURL. A
// principal that does not exist yet has no calendars, and is not an error.
func (u *Upstream) Calendars(ctx context.Context, principalURL string) ([]Collection, error) {
	base, err := url.Parse(principalURL)
	if err != nil || base.Host == "" || !strings.HasSuffix(base.Path, "/") {
		return nil, fmt.Errorf("caldav: invalid principal address")
	}
	body := `<?xml version="1.0" encoding="utf-8" ?>
<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:I="http://apple.com/ns/ical/">
  <D:prop><D:displayname/><D:resourcetype/><C:supported-calendar-component-set/><I:calendar-color/></D:prop>
</D:propfind>`
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", base.String(), strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", "1")
	res, err := u.do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if res.StatusCode != http.StatusMultiStatus {
		return nil, writeStatus(res, "listing calendars")
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var ms collectionStatus
	if err := xml.Unmarshal(raw, &ms); err != nil {
		return nil, fmt.Errorf("caldav: reading the calendar list: %w", err)
	}
	var out []Collection
	for _, r := range ms.Response {
		ref, err := url.Parse(strings.TrimSpace(r.Href))
		if err != nil || ref.User != nil || ref.RawQuery != "" || ref.Fragment != "" {
			continue
		}
		target := base.ResolveReference(ref)
		if target.Scheme != base.Scheme || target.Host != base.Host {
			continue
		}
		rest, ok := strings.CutPrefix(target.Path, base.Path)
		rest = strings.TrimSuffix(rest, "/")
		if !ok || rest == "" || strings.Contains(rest, "/") || !ValidSegment(rest) {
			continue
		}
		c := Collection{Segment: rest, URL: base.String() + url.PathEscape(rest) + "/"}
		calendar, events := false, true
		for _, ps := range r.Propstat {
			if !strings.Contains(ps.Status, " 200") {
				continue
			}
			for _, t := range ps.Prop.ResourceType.Inner {
				if t.XMLName.Local == "calendar" && t.XMLName.Space == "urn:ietf:params:xml:ns:caldav" {
					calendar = true
				}
			}
			if comps := ps.Prop.Components.Comp; len(comps) > 0 {
				events = false
				for _, comp := range comps {
					if strings.EqualFold(comp.Name, "VEVENT") {
						events = true
					}
				}
			}
			if name := strings.TrimSpace(ps.Prop.DisplayName); name != "" {
				c.Name = name
			}
			if color := strings.TrimSpace(ps.Prop.Color); colorPattern.MatchString(color) {
				c.Color = strings.ToLower(color[:7])
			}
		}
		if !calendar || !events {
			continue
		}
		if c.Name == "" {
			c.Name = rest
		}
		out = append(out, c)
		if len(out) == maxCollections {
			break
		}
	}
	return out, nil
}

// MakeCalendar creates an event calendar at collectionURL. The name and
// colour are written as properties; the caller has already bounded them.
func (u *Upstream) MakeCalendar(ctx context.Context, collectionURL, name, color string) error {
	if !u.Writable {
		return fmt.Errorf("calendar is read-only")
	}
	var escaped bytes.Buffer
	if err := xml.EscapeText(&escaped, []byte(name)); err != nil {
		return err
	}
	if !colorPattern.MatchString(color) {
		return fmt.Errorf("caldav: a colour is #rrggbb")
	}
	body := `<?xml version="1.0" encoding="utf-8" ?>
<C:mkcalendar xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:I="http://apple.com/ns/ical/">
  <D:set><D:prop>
    <D:displayname>` + escaped.String() + `</D:displayname>
    <I:calendar-color>` + color + `</I:calendar-color>
    <C:supported-calendar-component-set><C:comp name="VEVENT"/></C:supported-calendar-component-set>
  </D:prop></D:set>
</C:mkcalendar>`
	req, err := http.NewRequestWithContext(ctx, "MKCALENDAR", collectionURL, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	res, err := u.do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusConflict {
		return ErrNoPrincipal
	}
	return writeStatus(res, "creating the calendar")
}

// MakePrincipal creates the plain collection a person's calendars live
// in, for somebody who has never signed in to the calendar server.
func (u *Upstream) MakePrincipal(ctx context.Context, principalURL string) error {
	if !u.Writable {
		return fmt.Errorf("calendar is read-only")
	}
	req, err := http.NewRequestWithContext(ctx, "MKCOL", principalURL, nil)
	if err != nil {
		return err
	}
	res, err := u.do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	// 405 is "already exists", which is what was wanted.
	if res.StatusCode == http.StatusMethodNotAllowed {
		return nil
	}
	return writeStatus(res, "setting up the calendar account")
}
