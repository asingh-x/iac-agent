package server

import (
	"context"
	"encoding/json"

	"github.com/tf-agent/tf-agent/internal/db"
)

// dbEventStore adapts db.Store's generic (task_id, event_type, payload)
// row shape to Hub's EventStore interface, which speaks ServerEvent
// directly. This keeps internal/server/stream.go decoupled from
// internal/db, matching this package's existing EventRelay/ControlRelay
// decoupling pattern.
type dbEventStore struct {
	store db.Store
}

func NewDBEventStore(store db.Store) EventStore {
	return &dbEventStore{store: store}
}

func (d *dbEventStore) AppendRunEvent(ctx context.Context, taskID string, ev ServerEvent) (int64, error) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return 0, err
	}
	return d.store.AppendRunEvent(ctx, taskID, ev.Type, payload)
}

func (d *dbEventStore) GetRunEventsSince(ctx context.Context, taskID string, sinceSeq int64) ([]ServerEvent, error) {
	rows, err := d.store.GetRunEventsSince(ctx, taskID, sinceSeq)
	if err != nil {
		return nil, err
	}
	events := make([]ServerEvent, 0, len(rows))
	for _, row := range rows {
		var ev ServerEvent
		if err := json.Unmarshal(row.Payload, &ev); err != nil {
			continue // skip a corrupt row rather than fail the whole replay
		}
		ev.Seq = row.Seq
		events = append(events, ev)
	}
	return events, nil
}
