package imapread

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/mail/imapread/mimenorm"
)

// keys are the per-connection secrets, derived from Config.Key with
// domain separation so a fingerprint can never be replayed as a
// reference or the other way round.
type keys struct {
	fpKey  []byte
	refKey []byte
}

func deriveKeys(master []byte) keys {
	derive := func(label string) []byte {
		m := hmac.New(sha256.New, master)
		m.Write([]byte(label))
		return m.Sum(nil)
	}
	return keys{
		fpKey:  derive("wildstone/mail/fingerprint/v1"),
		refKey: derive("wildstone/mail/ref/v1"),
	}
}

// fingerprint is the stable keyed identity of one message: an HMAC
// over its Message-ID, normalized identifying headers and size. It
// does not include the folder, UID or UIDVALIDITY, so the same message
// has the same fingerprint after a reconnect, a UIDVALIDITY reset or a
// duplicate delivery — which is what lets the caller deduplicate. It is
// keyed per connection, so two people who received the same message
// get unrelated fingerprints and cannot learn that from comparing them.
func (k keys) fingerprint(h mimenorm.Headers, size int64, rawHeader []byte) string {
	m := hmac.New(sha256.New, k.fpKey)
	field := func(s string) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(s)))
		m.Write(n[:])
		m.Write([]byte(s))
	}
	field("v1")
	field(h.MessageID)
	if !h.Date.IsZero() {
		field(h.Date.UTC().Format(time.RFC3339))
	} else {
		field(h.DateRaw)
	}
	field(joinAddrs(h.From))
	field(joinAddrs(h.To) + ";" + joinAddrs(h.Cc))
	field(strings.Join(strings.Fields(strings.ToLower(h.Subject)), " "))
	field(strconv.FormatInt(size, 10))
	if h.MessageID == "" && h.From == nil && h.Subject == "" {
		// Nothing identifying parsed: fall back to the raw header bytes
		// so unrelated broken messages do not collide.
		sum := sha256.Sum256(rawHeader)
		field(hex.EncodeToString(sum[:]))
	}
	return "mfp1_" + hex.EncodeToString(m.Sum(nil)[:16])
}

func joinAddrs(list []mimenorm.Address) string {
	parts := make([]string, 0, len(list))
	for _, a := range list {
		parts = append(parts, strings.ToLower(a.Address))
	}
	return strings.Join(parts, ",")
}

// ref is what an opaque message reference names.
type ref struct {
	Folder      string
	UIDValidity uint32
	UID         uint32
}

// ErrBadRef is returned for a reference this connection did not issue.
var ErrBadRef = errors.New("imapread: not a message reference issued for this connection")

const refPrefix = "mref1."

// encodeRef returns an opaque, MAC-protected reference. It is bound to
// this connection's key: a reference minted for one Person's
// connection is rejected by every other connection, even on the same
// provider, so a reference cannot be used to reach another mailbox.
func (k keys) encodeRef(r ref) string {
	payload := make([]byte, 8, 8+len(r.Folder))
	binary.BigEndian.PutUint32(payload[0:4], r.UIDValidity)
	binary.BigEndian.PutUint32(payload[4:8], r.UID)
	payload = append(payload, r.Folder...)
	m := hmac.New(sha256.New, k.refKey)
	m.Write(payload)
	tag := m.Sum(nil)[:16]
	enc := base64.RawURLEncoding
	return refPrefix + enc.EncodeToString(payload) + "." + enc.EncodeToString(tag)
}

func (k keys) decodeRef(s string) (ref, error) {
	if len(s) > 1024 || !strings.HasPrefix(s, refPrefix) {
		return ref{}, ErrBadRef
	}
	body, tagText, ok := strings.Cut(strings.TrimPrefix(s, refPrefix), ".")
	if !ok {
		return ref{}, ErrBadRef
	}
	enc := base64.RawURLEncoding
	payload, err1 := enc.DecodeString(body)
	tag, err2 := enc.DecodeString(tagText)
	if err1 != nil || err2 != nil || len(payload) < 9 {
		return ref{}, ErrBadRef
	}
	m := hmac.New(sha256.New, k.refKey)
	m.Write(payload)
	if !hmac.Equal(tag, m.Sum(nil)[:16]) {
		return ref{}, ErrBadRef
	}
	return ref{
		UIDValidity: binary.BigEndian.Uint32(payload[0:4]),
		UID:         binary.BigEndian.Uint32(payload[4:8]),
		Folder:      string(payload[8:]),
	}, nil
}
