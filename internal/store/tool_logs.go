package store

import (
	"context"

	"github.com/pocketbase/pocketbase/core"

	"github.com/gera2ld/prism/internal/gateway"
)

// ToolLogStore writes invocation records. It is a separate type from LogStore
// because a tool call has no provider, model or token usage: request_logs is
// chat-shaped, and forcing tool calls into it would leave most of its columns
// meaningless on every row.
type ToolLogStore struct {
	app core.App
}

func NewToolLogSink(app core.App) *ToolLogStore { return &ToolLogStore{app: app} }

func (l *ToolLogStore) WriteTool(ctx context.Context, rec gateway.ToolRecord) error {
	collection, err := l.app.FindCollectionByNameOrId(toolLogsCollection)
	if err != nil {
		return err
	}
	record := core.NewRecord(collection)
	record.Set("api_key", rec.KeyID)
	record.Set("api_key_name", rec.KeyName)
	record.Set("tool", rec.Tool)
	record.Set("source", string(rec.Source))
	record.Set("server", rec.Server)
	record.Set("arguments", rec.Args)
	record.Set("result", rec.Result)
	record.Set("truncated", rec.Truncated)
	record.Set("duration_ms", rec.DurationMS)
	record.Set("status", rec.Status)
	record.Set("outcome", string(rec.Outcome))
	record.Set("error", rec.Error)
	if !rec.StartedAt.IsZero() {
		record.Set("started_at", rec.StartedAt.UTC())
	}
	return l.app.SaveWithContext(ctx, record)
}
