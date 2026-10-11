package state

// The audit log (SPEC.md, Records): rows are only ever appended.

// Audit appends who did what to which subject.
func (s *Store) Audit(actor, action, subject, detail string) error {
	_, err := s.db.Exec(`INSERT INTO audit_events (actor, action, subject, detail, created_at) VALUES (?, ?, ?, ?, ?)`,
		actor, action, subject, detail, now())
	return err
}

// ListAuditEvents returns the newest n rows, newest first (100 when n is
// not positive).
func (s *Store) ListAuditEvents(n int) ([]AuditEvent, error) {
	if n < 1 {
		n = 100
	}
	rows, err := s.db.Query(`SELECT id, actor, action, subject, detail, created_at FROM audit_events ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var e AuditEvent
		var at string
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.Subject, &e.Detail, &at); err != nil {
			return nil, err
		}
		e.CreatedAt = parseTime(at)
		out = append(out, e)
	}
	return out, rows.Err()
}
