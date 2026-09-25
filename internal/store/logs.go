package store

import (
	"context"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/cron"

	"github.com/gera2ld/prism/internal/gateway"
)

type LogStore struct {
	app core.App

	cron        *cron.Cron
	schedule    string
	retentionFn func() time.Duration
}

func NewLogSink(app core.App) *LogStore { return &LogStore{app: app} }

func nullable[T any](v *T) any {
	if v == nil {
		return nil
	}
	return *v
}

func (l *LogStore) Write(ctx context.Context, rec gateway.Record) error {
	collection, err := l.app.FindCollectionByNameOrId("request_logs")
	if err != nil {
		return err
	}
	record := core.NewRecord(collection)
	record.Set("api_key", rec.KeyID)
	record.Set("api_key_name", rec.KeyName)
	record.Set("alias", rec.Alias)
	record.Set("provider", rec.ProviderID)
	record.Set("provider_name", rec.ProviderName)
	record.Set("upstream_model", rec.UpstreamModel)
	record.Set("transformer", rec.Transformer)
	record.Set("stream", rec.Stream)
	record.Set("prompt_tokens", nullable(rec.Usage.PromptTokens))
	record.Set("completion_tokens", nullable(rec.Usage.CompletionTokens))
	record.Set("total_tokens", nullable(rec.Usage.TotalTokens))
	record.Set("cached_tokens", nullable(rec.Usage.CachedTokens))
	record.Set("ttft_ms", nullable(rec.TTFTMS))
	record.Set("total_ms", rec.TotalMS)
	// Gateway-side request start in UTC. Zero (handmade records only) stays
	// NULL; every gateway path populates Record.StartedAt.
	if !rec.StartedAt.IsZero() {
		record.Set("started_at", rec.StartedAt.UTC())
	}
	record.Set("status", rec.Status)
	record.Set("error", rec.Error)
	record.Set("outcome", string(rec.Outcome))
	record.Set("finish_reason", nullable(rec.FinishReason))
	if err := l.app.SaveWithContext(ctx, record); err != nil {
		return err
	}
	if rec.Bodies != nil {
		return l.saveBody(ctx, record.Id, rec.Bodies)
	}
	return nil
}

func (l *LogStore) saveBody(ctx context.Context, logID string, bodies *gateway.Bodies) error {
	collection, err := l.app.FindCollectionByNameOrId("request_bodies")
	if err != nil {
		return err
	}
	record := core.NewRecord(collection)
	record.Set("log", logID)
	record.Set("request", bodies.Request)
	record.Set("response", bodies.Response)
	record.Set("truncated", bodies.Truncated)
	return l.app.SaveWithContext(ctx, record)
}

func (l *LogStore) RegisterRetention(app core.App, schedule string, retentionFn func() time.Duration) error {
	if schedule == "" {
		schedule = defaultRetentionCron
	}
	if retentionFn == nil {
		retentionFn = defaultRetention
	}
	c := cron.New()
	l.cron = c
	l.schedule = schedule
	l.retentionFn = retentionFn
	if err := c.Add(retentionJobID, schedule, l.retentionJob(app)); err != nil {
		return err
	}
	c.Start()
	return nil
}

// UpdateSchedule swaps the retention job to a new cron expression, e.g.
// after a gateway_settings edit. An invalid expression keeps the previous
// job untouched so a typo never kills cleanup.
func (l *LogStore) UpdateSchedule(schedule string) error {
	if l.cron == nil || schedule == "" || schedule == l.schedule {
		return nil
	}
	l.cron.Remove(retentionJobID)
	if err := l.cron.Add(retentionJobID, schedule, l.retentionJob(l.app)); err != nil {
		_ = l.cron.Add(retentionJobID, l.schedule, l.retentionJob(l.app))
		return err
	}
	l.schedule = schedule
	return nil
}

const retentionJobID = "request_bodies_retention"

func (l *LogStore) retentionJob(app core.App) func() {
	retentionFn := l.retentionFn
	if retentionFn == nil {
		retentionFn = defaultRetention
	}
	return func() {
		cutoff := time.Now().Add(-retentionFn()).UTC().Format("2006-01-02 15:04:05.000Z")
		_, _ = app.DB().NewQuery("DELETE FROM {{request_bodies}} WHERE [[created]] < {:cutoff}").
			Bind(dbx.Params{"cutoff": cutoff}).
			Execute()
	}
}

func defaultRetention() time.Duration {
	return time.Duration(defaultRetentionHrs) * time.Hour
}
