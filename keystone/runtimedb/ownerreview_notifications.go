package runtimedb

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type OwnerReviewPreference struct {
	Generation int64  `json:"-"`
	BindingID  int64  `json:"bindingId"`
	ConsoleURL string `json:"consoleUrl"`
}

func (db *DB) GetOwnerReviewPreference() (OwnerReviewPreference, error) {
	var p OwnerReviewPreference
	err := db.sql.QueryRow("SELECT COALESCE(binding_id,0),console_url,generation FROM owner_review_preference WHERE singleton=1").Scan(&p.BindingID, &p.ConsoleURL, &p.Generation)
	return p, err
}
func (db *DB) ownerReviewBinding(id int64) (ChatBinding, error) {
	bindings, err := db.ListChatBindings(false)
	if err != nil {
		return ChatBinding{}, err
	}
	for _, b := range bindings {
		if b.ID == id && b.Role == RoleOwner && b.Conversation != "" {
			member, err := db.GetButlerMember(b.Identity)
			if err != nil || !member.Active() {
				continue
			}
			at, err := db.ChatConnectionBySeq(b.Connection)
			if err != nil {
				continue
			}
			if _, err = db.LookupChatBinding(at, b.Account); err != nil {
				continue
			}
			return b, nil
		}
	}
	return ChatBinding{}, fmt.Errorf("choose an active Owner's connected account with a known conversation")
}
func (db *DB) SetOwnerReviewPreference(actor string, p OwnerReviewPreference) error {
	member, err := db.GetButlerMember(actor)
	if err != nil || !member.Active() || member.Role != RoleOwner {
		return fmt.Errorf("only an active Owner may choose review notifications")
	}
	var binding any
	if p.BindingID != 0 {
		if _, err = db.ownerReviewBinding(p.BindingID); err != nil {
			return err
		}
		u, err := url.Parse(p.ConsoleURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
			return fmt.Errorf("enter the household Console URL")
		}
		binding = p.BindingID
	} else {
		p.ConsoleURL = ""
	}
	_, err = db.sql.Exec("UPDATE owner_review_preference SET generation=generation+CASE WHEN COALESCE(binding_id,0)<>? OR console_url<>? THEN 1 ELSE 0 END,binding_id=?,console_url=? WHERE singleton=1", p.BindingID, p.ConsoleURL, binding, p.ConsoleURL)
	return err
}

// QueueOwnerReviewNotifications is retryable. It reads the same shared queue
// used by Console, never includes proposal content, and grants no approval.
func (db *DB) QueueOwnerReviewNotifications(at time.Time) error {
	if err := db.gate(GateDelivery); err != nil {
		return err
	}
	pref, err := db.GetOwnerReviewPreference()
	if err != nil {
		return err
	}
	if pref.BindingID == 0 {
		return nil
	}
	binding, err := db.ownerReviewBinding(pref.BindingID)
	if err != nil {
		return err
	}
	plans, err := db.OwnerReviewQueue(binding.Identity)
	if err != nil {
		return err
	}
	quiet, _, err := db.GetButlerSettings()
	if err != nil {
		return err
	}
	for _, p := range plans {
		key := fmt.Sprintf("owner-review:%s:%d", p.ID, binding.ID)
		body := "A Plan is waiting for Owner review. Open Console: " + strings.TrimSpace(pref.ConsoleURL)
		notice, err := db.RaiseNotice(NoticeRequest{Kind: "owner.review", Recipient: binding.Identity, DedupeKey: key, Body: body}, quiet, at)
		if err != nil {
			return err
		}
		if notice.Seq == 0 {
			continue
		}
		existing, err := db.GetNotice(notice.Seq)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if !existing.DeliveredAt.IsZero() {
			continue
		}
		// An expired, unacknowledged message needs a new sequence so adapters
		// whose cursor passed the old one can receive the retry.
		if _, err = db.sql.Exec("DELETE FROM chat_outbound WHERE dedupe_key=? AND delivered_at='' AND expires_at<=?", key, now()); err != nil {
			return err
		}
		prefs, err := db.GetMemberNoticePreferences(binding.Identity)
		if err != nil {
			return err
		}
		var hold time.Time
		if inQuiet, until := db.quietFor(prefs, quiet, at); inQuiet {
			hold = until
		}
		expires := at.Add(DefaultChatOutboundTTL)
		if hold.After(at) {
			expires = hold.Add(DefaultChatOutboundTTL)
		}
		seq, err := db.EnqueueChatOutbound(ChatOutbound{CorrelationID: fmt.Sprintf("owner-review-pref:%d", pref.Generation), Connection: binding.Connection, Target: binding.Conversation, BindingID: binding.ID, Kind: "push", Body: body, DedupeKey: key, HoldUntil: hold, ExpiresAt: expires})
		if err != nil && !errors.Is(err, ErrDuplicateOutbound) {
			return err
		}
		// Linking also catches an acknowledgement that raced the enqueue.
		if _, err = db.sql.Exec(`UPDATE notices SET outbound=?,hold_until=?,expires_at=?,undelivered_at=CASE WHEN (SELECT expires_at FROM chat_outbound WHERE seq=?)<=? THEN ? ELSE '' END,delivered_at=COALESCE((SELECT delivered_at FROM chat_outbound WHERE seq=?),'') WHERE seq=? AND delivered_at=''`, seq, timeText(hold), timeText(expires), seq, now(), now(), seq, notice.Seq); err != nil {
			return err
		}
	}
	return nil
}
