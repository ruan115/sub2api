package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var ErrPublishableRoute = errors.New("publishable execution route is invalid")

type PublishableRoute struct {
	SlotID     string
	NodeID     string
	Endpoint   string
	Epoch      uint64
	Generation uint64
}

func (r *Repository) ListPublishableRoutes(ctx context.Context, checkedAt time.Time) ([]PublishableRoute, error) {
	if r == nil || r.db == nil || ctx == nil || ctx.Err() != nil || checkedAt.IsZero() {
		return nil, ErrPublishableRoute
	}
	checkedAt = checkedAt.UTC().Truncate(time.Microsecond)
	rows, err := r.db.QueryContext(ctx, `
SELECT sa.slot_id, sa.node_id, n.labels_json, sa.execution_epoch, sa.desired_generation
FROM slot_assignments sa
JOIN slots s ON s.slot_id = sa.slot_id
JOIN nodes n ON n.node_id = sa.node_id
JOIN execution_leases el ON el.slot_id = sa.slot_id AND el.execution_epoch = sa.execution_epoch AND el.node_id = sa.node_id
WHERE sa.released_at IS NULL
  AND sa.healthy = 1
  AND sa.actual_state IN ('ready', 'running', 'busy')
  AND sa.desired_generation IS NOT NULL
  AND sa.desired_generation = s.desired_generation
  AND s.desired_state = 'ready'
  AND el.revoked_at IS NULL
  AND el.expires_at > ?
  AND el.created_at <= ?
  AND sa.assigned_at <= ?
ORDER BY sa.slot_id`, checkedAt, checkedAt, checkedAt)
	if err != nil {
		return nil, fmt.Errorf("list publishable execution routes: %w", err)
	}
	defer rows.Close()
	var routes []PublishableRoute
	for rows.Next() {
		var route PublishableRoute
		var labelsJSON []byte
		if err := rows.Scan(&route.SlotID, &route.NodeID, &labelsJSON, &route.Epoch, &route.Generation); err != nil {
			return nil, err
		}
		var labels map[string]string
		if err := json.Unmarshal(labelsJSON, &labels); err != nil {
			return nil, ErrPublishableRoute
		}
		endpoint, ok := labels["dataplane_endpoint"]
		if !ok || endpoint == "" {
			continue
		}
		route.Endpoint = endpoint
		if route.SlotID == "" || route.NodeID == "" || route.Epoch == 0 || route.Generation == 0 {
			return nil, ErrPublishableRoute
		}
		routes = append(routes, route)
	}
	return routes, rows.Err()
}

var _ interface {
	ListPublishableRoutes(context.Context, time.Time) ([]PublishableRoute, error)
} = (*Repository)(nil)
