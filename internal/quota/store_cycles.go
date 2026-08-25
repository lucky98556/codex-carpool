package quota

import (
	"database/sql"
	"time"
)

func (store *Store) LoadBudgetCycles() (map[string]budgetCycleState, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	rows, err := store.db.Query(`SELECT key_id,five_hour_started_at,seven_day_started_at FROM key_budget_cycles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]budgetCycleState)
	for rows.Next() {
		var keyID string
		var five, seven sql.NullInt64
		if err := rows.Scan(&keyID, &five, &seven); err != nil {
			return nil, err
		}
		cycle := budgetCycleState{}
		if five.Valid {
			cycle.FiveHourStartedAt = time.UnixMilli(five.Int64).UTC()
		}
		if seven.Valid {
			cycle.SevenDayStartedAt = time.UnixMilli(seven.Int64).UTC()
		}
		result[keyID] = cycle
	}
	return result, rows.Err()
}

func (store *Store) UpsertBudgetCycles(keyID string, cycle budgetCycleState) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	var five, seven any
	if !cycle.FiveHourStartedAt.IsZero() {
		five = cycle.FiveHourStartedAt.UTC().UnixMilli()
	}
	if !cycle.SevenDayStartedAt.IsZero() {
		seven = cycle.SevenDayStartedAt.UTC().UnixMilli()
	}
	_, err := store.db.Exec(`INSERT INTO key_budget_cycles(key_id,five_hour_started_at,seven_day_started_at,updated_at)
VALUES(?,?,?,?) ON CONFLICT(key_id) DO UPDATE SET five_hour_started_at=excluded.five_hour_started_at,
seven_day_started_at=excluded.seven_day_started_at,updated_at=excluded.updated_at`, keyID, five, seven, time.Now().UTC().UnixMilli())
	return err
}
