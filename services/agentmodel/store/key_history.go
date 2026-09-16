package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// KeyActor contains an opaque principal identifier, never email, token or token
// hash. HTTP gates supply it after authentication; direct store callers are
// explicitly identified as system operations, not invented human operators.
type KeyActor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}
type keyActorContext struct{}

func WithKeyActor(ctx context.Context, actor KeyActor) context.Context {
	return context.WithValue(ctx, keyActorContext{}, actor)
}

type KeyEvent struct {
	ID        int64     `json:"id"`
	KeyID     string    `json:"key_id"`
	Actor     KeyActor  `json:"actor"`
	Action    string    `json:"action"`
	CreatedAt time.Time `json:"created_at"`
}

func appendKeyEvent(ctx context.Context, tx *sql.Tx, keyID, action string) error {
	actor, ok := ctx.Value(keyActorContext{}).(KeyActor)
	if !ok {
		actor = KeyActor{Kind: "system", ID: "store"}
	}
	if actor.ID == "" || len(actor.ID) > 128 || (actor.Kind != "admin" && actor.Kind != "user" && actor.Kind != "master" && actor.Kind != "system") {
		return errors.New("invalid audit actor")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO key_events(key_id,actor_kind,actor_id,action,created_at) VALUES (?,?,?,?,?)`, keyID, actor.Kind, actor.ID, action, time.Now().UTC().Unix())
	return err
}

// keyMutation keeps audit failure and mutation failure in the same transaction.
// The write takes the SQLite writer lock before any subsequent reads.
func (s *SQLiteStore) keyMutation(ctx context.Context, keyID, action, query string, args ...any) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if err := affectedOrNotFound(res, action); err != nil {
		return err
	}
	if err := appendKeyEvent(ctx, tx, keyID, action); err != nil {
		return err
	}
	return tx.Commit()
}

// GetKeyHistory is key-scoped keyset pagination, newest first, at most 100
// events. Events are independent of the current secret and never pruned with
// request logs. before=0 starts from the latest event.
func (s *SQLiteStore) GetKeyHistory(ctx context.Context, keyID string, before int64) ([]KeyEvent, error) {
	if keyID == "" || len(keyID) > 256 || before < 0 {
		return nil, errors.New("invalid history filter")
	}
	rows, err := s.reporting.QueryContext(ctx, `SELECT id,key_id,actor_kind,actor_id,action,created_at FROM key_events WHERE key_id=? AND (?=0 OR id<?) ORDER BY id DESC LIMIT 100`, keyID, before, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []KeyEvent{}
	for rows.Next() {
		var e KeyEvent
		var ts int64
		if err := rows.Scan(&e.ID, &e.KeyID, &e.Actor.Kind, &e.Actor.ID, &e.Action, &ts); err != nil {
			return nil, err
		}
		e.CreatedAt = time.Unix(ts, 0).UTC()
		events = append(events, e)
	}
	return events, rows.Err()
}
