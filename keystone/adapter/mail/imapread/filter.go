package imapread

import (
	"fmt"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/mail/imapread/mimenorm"
)

// Filters is a connection's source scope: which folders are read, and
// which of their messages are in scope. A message outside the filters
// is never fetched past its header, never returned, and never
// retrievable by reference — the reader advances its cursor past it
// and counts it, nothing more.
type Filters struct {
	// Folders to monitor, in the order they are read. Empty means
	// INBOX only.
	Folders []string `json:"folders,omitempty"`
	// Senders restricts to messages whose From address matches one
	// entry: a full address ("office@school.example.internal"), or a
	// domain ("school.example.internal" or "@school.example.internal"),
	// which also matches its subdomains. Empty means any sender.
	Senders []string `json:"senders,omitempty"`
	// SubjectContains restricts to messages whose decoded subject
	// contains one entry, case-insensitively. Empty means any subject.
	SubjectContains []string `json:"subjectContains,omitempty"`
}

// Filter bounds, so a scope cannot itself become a cost.
const (
	maxFolders       = 20
	maxFilterEntries = 100
	maxFilterEntry   = 256
)

func (f Filters) folders() []string {
	if len(f.Folders) == 0 {
		return []string{"INBOX"}
	}
	return f.Folders
}

// Validate rejects a scope the reader will not run.
func (f Filters) Validate() error {
	if len(f.Folders) > maxFolders {
		return fmt.Errorf("imapread: at most %d folders", maxFolders)
	}
	seen := map[string]bool{}
	for _, name := range f.Folders {
		if strings.TrimSpace(name) == "" || len(name) > maxFilterEntry || strings.ContainsAny(name, "\r\n\x00*%") {
			return fmt.Errorf("imapread: invalid folder name %q", name)
		}
		if seen[name] {
			return fmt.Errorf("imapread: folder %q listed twice", name)
		}
		seen[name] = true
	}
	for _, list := range [][]string{f.Senders, f.SubjectContains} {
		if len(list) > maxFilterEntries {
			return fmt.Errorf("imapread: at most %d filter entries", maxFilterEntries)
		}
		for _, e := range list {
			if strings.TrimSpace(e) == "" || len(e) > maxFilterEntry {
				return fmt.Errorf("imapread: invalid filter entry %q", e)
			}
		}
	}
	return nil
}

func (f Filters) hasFolder(name string) bool {
	for _, x := range f.folders() {
		if x == name {
			return true
		}
	}
	return false
}

// narrowing reports whether any content filter is set.
func (f Filters) narrowing() bool {
	return len(f.Senders) > 0 || len(f.SubjectContains) > 0
}

// Match reports whether a message's headers are in scope.
func (f Filters) Match(h mimenorm.Headers) bool {
	if len(f.Senders) > 0 && !f.senderMatch(h.From) {
		return false
	}
	if len(f.SubjectContains) > 0 {
		subject := strings.ToLower(h.Subject)
		ok := false
		for _, s := range f.SubjectContains {
			if strings.Contains(subject, strings.ToLower(strings.TrimSpace(s))) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func (f Filters) senderMatch(from []mimenorm.Address) bool {
	for _, a := range from {
		addr := strings.ToLower(a.Address)
		at := strings.LastIndexByte(addr, '@')
		if at < 0 {
			continue
		}
		domain := addr[at+1:]
		for _, e := range f.Senders {
			e = strings.ToLower(strings.TrimSpace(e))
			switch {
			case strings.HasPrefix(e, "@"):
				if domainMatch(domain, e[1:]) {
					return true
				}
			case strings.Contains(e, "@"):
				if addr == e {
					return true
				}
			default:
				if domainMatch(domain, e) {
					return true
				}
			}
		}
	}
	return false
}

func domainMatch(domain, want string) bool {
	return domain == want || strings.HasSuffix(domain, "."+want)
}
