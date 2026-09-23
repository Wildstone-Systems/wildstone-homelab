package runtimedb

import "fmt"

func (db *DB) CalendarVisibility(member string) (map[string]bool, error) {
	rows, err := db.sql.Query(`SELECT calendar_id, visible FROM calendar_visibility WHERE member=?`, member)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		var v bool
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		out[id] = v
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
	_, err := db.sql.Exec(`INSERT INTO calendar_visibility(member,calendar_id,visible) VALUES(?,?,?) ON CONFLICT(member,calendar_id) DO UPDATE SET visible=excluded.visible`, member, id, visible)
	return err
}

func (db *DB) MoveBookingCalendar(from, href, to, newHref string) error {
	_, err := db.sql.Exec(`UPDATE commitment_bookings SET calendar_id=?, calendar_event=? WHERE calendar_id=? AND calendar_event=? AND cancelled_at=''`, to, newHref, from, href)
	return err
}
