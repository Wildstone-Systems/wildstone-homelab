package caldav

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/sharedcalendar"
)

// MoveEvent uses CalDAV MOVE to retain the complete event, including alarms,
// attendees and recurrence. Destination comes only from the declared registry.
func (u *Upstream) MoveEvent(ctx context.Context, href, destination string) (string, error) {
	if !u.Writable {
		return "", fmt.Errorf("calendar is read-only")
	}
	source, err := sharedcalendar.EventURL(u.CollectionURL, href)
	if err != nil {
		return "", err
	}
	src, _ := url.Parse(source)
	dst, err := url.Parse(destination)
	if err != nil || dst.Scheme != src.Scheme || dst.Host != src.Host || dst.User != nil || dst.RawQuery != "" || dst.Fragment != "" {
		return "", fmt.Errorf("cannot move between calendar origins")
	}
	target, err := sharedcalendar.EventURL(destination, strings.TrimRight(destination, "/")+"/"+url.PathEscape(path.Base(src.Path)))
	if err != nil {
		return "", err
	}
	if u.BeforeWrite != nil {
		if err := u.BeforeWrite(); err != nil {
			return "", err
		}
	}
	req, err := http.NewRequestWithContext(ctx, "MOVE", source, nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(u.Username, u.Password)
	req.Header.Set("Destination", target)
	req.Header.Set("Overwrite", "F")
	res, err := u.client().Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusNoContent {
		return "", fmt.Errorf("calendar refused move (HTTP %d)", res.StatusCode)
	}
	return target, nil
}
