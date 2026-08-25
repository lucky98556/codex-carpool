package quota

import (
	"strings"
	"time"
)

func (store *Store) ListDecisionLogs(keyID string, limit int) ([]DecisionLog, error) {
	items, _, err := store.ListDecisionLogsPage(keyID, "", "", limit, 0)
	return items, err
}

func (store *Store) ListDecisionLogsPage(keyID, decision, search string, limit, offset int) ([]DecisionLog, int, error) {
	keyID, decision, search = strings.TrimSpace(keyID), strings.ToLower(strings.TrimSpace(decision)), strings.TrimSpace(search)
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	where := []string{"1=1"}
	args := make([]any, 0)
	if keyID != "" {
		where = append(where, "l.key_id=?")
		args = append(args, keyID)
	}
	// Content-expression logs are a dedicated view of blocked requests. Filter
	// them by their stable reason while keeping the decision model consistent.
	if decision == "forbidden" {
		where = append(where, "l.reason=?")
		args = append(args, "content_forbidden")
	} else if decision != "" {
		where = append(where, "l.decision=?")
		args = append(args, decision)
	}
	if search != "" {
		where = append(where, "(l.model LIKE ? OR l.reason LIKE ? OR l.auth_id LIKE ? OR l.request_content LIKE ? OR p.name LIKE ? OR p.key_suffix LIKE ?)")
		pattern := "%" + search + "%"
		args = append(args, pattern, pattern, pattern, pattern, pattern, pattern)
	}
	clause := strings.Join(where, " AND ")
	store.mu.Lock()
	defer store.mu.Unlock()
	var total int
	from := ` FROM request_logs l LEFT JOIN key_policies p ON p.key_id=l.key_id WHERE `
	if err := store.db.QueryRow(`SELECT COUNT(*)`+from+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	queryArgs := append(append([]any(nil), args...), limit, offset)
	rows, err := store.db.Query(`SELECT l.id,l.key_id,l.key_suffix,l.auth_id,l.model,l.request_content,l.matched_term,l.matched_category,l.requested_at,l.decision,l.status_code,l.reason,
l.units,l.input_tokens,l.cached_tokens,l.output_tokens,l.input_cost_micros,l.cached_cost_micros,l.output_cost_micros,l.cost_micros`+
		from+clause+` ORDER BY l.requested_at DESC,l.id DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]DecisionLog, 0)
	for rows.Next() {
		var item DecisionLog
		var at int64
		if err := rows.Scan(&item.ID, &item.KeyID, &item.KeySuffix, &item.AuthID, &item.Model, &item.RequestContent, &item.MatchedTerm,
			&item.MatchedCategory, &at, &item.Decision, &item.StatusCode, &item.Reason, &item.Units, &item.InputTokens,
			&item.CachedTokens, &item.OutputTokens, &item.InputCostMicros, &item.CachedCostMicros, &item.OutputCostMicros, &item.CostMicros); err != nil {
			return nil, 0, err
		}
		item.RequestedAt = time.UnixMilli(at).UTC()
		items = append(items, item)
	}
	return items, total, rows.Err()
}

// ClearDecisionLogs clears one Key when supplied, or the global audit log when empty.
// Usage aggregation rows are stored separately and intentionally remain intact.
func (store *Store) ClearDecisionLogs(keyID string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if keyID = strings.TrimSpace(keyID); keyID != "" {
		_, err := store.db.Exec(`DELETE FROM request_logs WHERE key_id=?`, keyID)
		return err
	}
	_, err := store.db.Exec(`DELETE FROM request_logs`)
	return err
}

func (store *Store) ClearForbiddenDecisionLogs(keyID string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if keyID = strings.TrimSpace(keyID); keyID != "" {
		_, err := store.db.Exec(`DELETE FROM request_logs WHERE reason='content_forbidden' AND key_id=?`, keyID)
		return err
	}
	_, err := store.db.Exec(`DELETE FROM request_logs WHERE reason='content_forbidden'`)
	return err
}

func (store *Store) AppendOperationalLog(entry OperationalLog) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if entry.OccurredAt.IsZero() {
		entry.OccurredAt = time.Now().UTC()
	}
	_, err := store.db.Exec(`INSERT INTO operational_logs(occurred_at,level,event,message,auth_id,key_id) VALUES(?,?,?,?,?,?)`,
		entry.OccurredAt.UTC().UnixMilli(), strings.ToLower(strings.TrimSpace(entry.Level)), strings.TrimSpace(entry.Event), entry.Message, entry.AuthID, entry.KeyID)
	return err
}

func (store *Store) ListOperationalLogs(level, search string, limit int) ([]OperationalLog, error) {
	items, _, err := store.ListOperationalLogsPage(level, search, limit, 0)
	return items, err
}

func (store *Store) ListOperationalLogsPage(level, search string, limit, offset int) ([]OperationalLog, int, error) {
	level, search = strings.ToLower(strings.TrimSpace(level)), strings.TrimSpace(search)
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	where := []string{"1=1"}
	args := make([]any, 0)
	if level != "" {
		where = append(where, "level=?")
		args = append(args, level)
	}
	if search != "" {
		where = append(where, "(event LIKE ? OR message LIKE ? OR auth_id LIKE ? OR key_id LIKE ?)")
		pattern := "%" + search + "%"
		args = append(args, pattern, pattern, pattern, pattern)
	}
	clause := strings.Join(where, " AND ")
	store.mu.Lock()
	defer store.mu.Unlock()
	var total int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM operational_logs WHERE `+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	queryArgs := append(append([]any(nil), args...), limit, offset)
	rows, err := store.db.Query(`SELECT id,occurred_at,level,event,message,auth_id,key_id FROM operational_logs WHERE `+clause+` ORDER BY occurred_at DESC,id DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]OperationalLog, 0)
	for rows.Next() {
		var item OperationalLog
		var at int64
		if err := rows.Scan(&item.ID, &at, &item.Level, &item.Event, &item.Message, &item.AuthID, &item.KeyID); err != nil {
			return nil, 0, err
		}
		item.OccurredAt = time.UnixMilli(at).UTC()
		items = append(items, item)
	}
	return items, total, rows.Err()
}

func (store *Store) ClearOperationalLogs() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	_, err := store.db.Exec(`DELETE FROM operational_logs`)
	return err
}
