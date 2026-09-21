package runtimedb

// LinkPlanSessionPrivacy runs after the session's source tracking is durable.
// Until then private-origin Plans fail closed on external inspection.
func (db *DB) LinkPlanSessionPrivacy(session string, plans []string) error {
	if len(plans) == 0 {
		return nil
	}
	s, err := db.GetButlerSession(session)
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range plans {
		seq, err := parsePlanID(id)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT OR IGNORE INTO plan_private_origins(plan) VALUES(?)", seq); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT OR IGNORE INTO plan_session_sources(plan,session) VALUES(?,?)", seq, s.Seq); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (db *DB) PlanPrivacySessions(id string) ([]string, bool, error) {
	seq, err := parsePlanID(id)
	if err != nil {
		return nil, false, err
	}
	var private bool
	if err = db.sql.QueryRow("SELECT EXISTS(SELECT 1 FROM plan_private_origins WHERE plan=?)", seq).Scan(&private); err != nil {
		return nil, false, err
	}
	rows, err := db.sql.Query("SELECT session FROM plan_session_sources WHERE plan=?", seq)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var sessions []string
	for rows.Next() {
		var session int64
		if err = rows.Scan(&session); err != nil {
			return nil, false, err
		}
		sessions = append(sessions, butlerSessionID(session))
	}
	return sessions, private, rows.Err()
}
func (db *DB) CheckPlanPrivacy(id, model string, destination MemoryDestination) error {
	sessions, private, err := db.PlanPrivacySessions(id)
	if err != nil {
		return err
	}
	if private && len(sessions) == 0 {
		return (PrivacyPolicy{TrustedOnly: true}).Check(model, destination)
	}
	for _, session := range sessions {
		if err = db.CheckSessionPrivacy(session, model, destination); err != nil {
			return err
		}
	}
	return nil
}
