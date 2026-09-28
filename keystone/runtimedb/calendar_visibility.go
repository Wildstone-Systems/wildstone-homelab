package runtimedb

import "fmt"

// CalendarVisibility returns a member's per-calendar display choices,
// keyed by the Person so a rename keeps them (0093). A member with no
// Person on record has made no choices.
func (db *DB) CalendarVisibility(member string) (map[string]bool, error) {
	out := map[string]bool{}
	id, err := resolvePersonRef(db.sql, member)
	if err != nil || id == "" {
		return out, err
	}
	rows, err := db.sql.Query(`SELECT calendar_id, visible FROM calendar_visibility WHERE person_id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var cal string
		var v bool
		if err := rows.Scan(&cal, &v); err != nil {
			return nil, err
		}
		out[cal] = v
	}
	return out, rows.Err()
}

func (db *DB) SetCalendarVisibility(member, id string, visible bool) error {
	if member == "" || id == "" {
		return fmt.Errorf("calendar visibility needs a member and calendar")
	}
	if err := db.gate(GateIngestion); err != nil {
		return err
	}
	person, err := resolvePersonRef(db.sql, member)
	if err != nil {
		return err
	}
	if person == "" {
		return fmt.Errorf("%w: %q", ErrNoSuchPerson, member)
	}
	_, err = db.sql.Exec(`INSERT INTO calendar_visibility(person_id,calendar_id,visible) VALUES(?,?,?) ON CONFLICT(person_id,calendar_id) DO UPDATE SET visible=excluded.visible`, person, id, visible)
	return err
}

func (db *DB) MoveBookingCalendar(from, href, to, newHref string) error {
	_, err := db.sql.Exec(`UPDATE commitment_bookings SET calendar_id=?, calendar_event=? WHERE calendar_id=? AND calendar_event=? AND cancelled_at=''`, to, newHref, from, href)
	return err
}
