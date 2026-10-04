package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gera2ld/prism/internal/gateway"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	_ "github.com/pocketbase/pocketbase/migrations"
)

var testStartedAt = time.Date(2026, time.September, 20, 12, 0, 0, 0, time.UTC)

func gatewayRecord(keyID, providerID string, total *int64) gateway.Record {
	return gateway.Record{
		StartedAt:     testStartedAt,
		KeyID:         keyID,
		KeyName:       "smoke",
		Alias:         "alias",
		Endpoint:      gateway.EndpointChat,
		ProviderID:    providerID,
		ProviderName:  "prov",
		UpstreamModel: "gpt-x",
		Stream:        false,
		Usage:         gateway.Usage{TotalTokens: total},
		TotalMS:       12,
		Status:        200,
		Bodies:        &gateway.Bodies{Request: "req-body", Response: "resp-body"},
	}
}

func newTestApp(t *testing.T) *core.BaseApp {
	t.Helper()
	t.Setenv("GATEWAY_ENCRYPTION_KEY", "test-encryption-secret")
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: t.TempDir()})
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.ClearBootstrap() })
	if err := app.RunAllMigrations(); err != nil {
		t.Fatal(err)
	}
	return app
}

func seedConfig(t *testing.T, app *core.BaseApp, s *Store) (providerID string) {
	t.Helper()
	providers, err := app.FindCollectionByNameOrId("providers")
	if err != nil {
		t.Fatal(err)
	}
	p := core.NewRecord(providers)
	p.Set("name", "prov")
	p.Set("base_url", "http://127.0.0.1:9911")
	p.Set("api_key", "upstream-secret")
	p.Set("enabled", true)
	if err := app.Save(p); err != nil {
		t.Fatal(err)
	}

	routes, err := app.FindCollectionByNameOrId("routes")
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(routes)
	r.Set("alias", "alias")
	r.Set("provider", p.Id)
	r.Set("upstream_model", "gpt-x")
	r.Set("priority", 0)
	r.Set("enabled", true)
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
	return p.Id
}

func TestStoreEndToEnd(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	providerID := seedConfig(t, app, s)

	// api_key is encrypted at rest
	raw, err := app.FindAllRecords("providers")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(raw))
	}
	if stored := raw[0].GetString("api_key"); stored == "" || !strings.HasPrefix(stored, "enc:") {
		t.Fatalf("api_key not encrypted at rest: %q", stored)
	}

	// authenticate
	secret, err := s.CreateAPIKey(app, "smoke")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "sk-") {
		t.Fatalf("CLI secret missing sk- prefix: %q", secret)
	}
	key, err := s.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if key.ID == "" {
		t.Fatal("empty key id")
	}
	if _, err := s.Authenticate(context.Background(), "wrong"); err == nil {
		t.Fatal("expected unauthorized for wrong key")
	}

	// secret is encrypted at rest but revealable; auth still uses the hash.
	stored, err := app.FindFirstRecordByFilter("api_keys", "name = 'smoke'")
	if err != nil {
		t.Fatal(err)
	}
	if plain := stored.GetString("key_plain"); plain == "" || !strings.HasPrefix(plain, "enc:") {
		t.Fatalf("key_plain not encrypted at rest: %q", plain)
	}
	revealed, err := s.RevealAPIKey(app, "smoke")
	if err != nil {
		t.Fatal(err)
	}
	if revealed != secret {
		t.Fatal("RevealAPIKey did not return the issued secret")
	}
	if _, err := s.RevealAPIKey(app, "nope"); err == nil {
		t.Fatal("expected error for unknown key name")
	}

	// resolve alias
	targets, err := s.Resolve(context.Background(), gateway.EndpointChat, "alias")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(targets) != 1 || targets[0].Model != "gpt-x" || targets[0].ProviderID != providerID {
		t.Fatalf("unexpected targets: %+v", targets)
	}
	if targets[0].APIKey != "upstream-secret" {
		t.Fatalf("api key not decrypted: %q", targets[0].APIKey)
	}

	// Strict routing: no provider/model escape hatch. Anything unaliased,
	// slashes included, is an unknown model.
	for _, unaliased := range []string{"openai/anything", "prov/any-model", "qwen/qwen3:free"} {
		if _, err := s.Resolve(context.Background(), gateway.EndpointChat, unaliased); err == nil {
			t.Fatalf("expected unknown model error for %q", unaliased)
		}
	}

	// Aliases may contain slashes: exact table match wins.
	routes, _ := app.FindCollectionByNameOrId("routes")
	slash := core.NewRecord(routes)
	slash.Set("alias", "qwen/qwen3:free")
	slash.Set("provider", providerID)
	slash.Set("upstream_model", "qwen/qwen3-30b:free")
	slash.Set("priority", 0)
	slash.Set("enabled", true)
	if err := app.Save(slash); err != nil {
		t.Fatalf("slash alias rejected: %v", err)
	}
	slashed, err := s.Resolve(context.Background(), gateway.EndpointChat, "qwen/qwen3:free")
	if err != nil {
		t.Fatalf("slash alias: %v", err)
	}
	if len(slashed) != 1 || slashed[0].Model != "qwen/qwen3-30b:free" {
		t.Fatalf("unexpected slash targets: %+v", slashed)
	}

	// Same alias on two providers: priority-ordered fallback chain.
	providers, _ := app.FindCollectionByNameOrId("providers")
	prov2 := core.NewRecord(providers)
	prov2.Set("name", "prov2")
	prov2.Set("base_url", "http://127.0.0.1:9912")
	prov2.Set("api_key", "secret2")
	prov2.Set("enabled", true)
	if err := app.Save(prov2); err != nil {
		t.Fatal(err)
	}
	second := core.NewRecord(routes)
	second.Set("alias", "alias")
	second.Set("provider", prov2.Id)
	second.Set("upstream_model", "gpt-x-mirror")
	second.Set("priority", 10)
	second.Set("enabled", true)
	if err := app.Save(second); err != nil {
		t.Fatal(err)
	}
	chain, err := s.Resolve(context.Background(), gateway.EndpointChat, "alias")
	if err != nil {
		t.Fatalf("chain resolve: %v", err)
	}
	if len(chain) != 2 || chain[0].ProviderID != providerID || chain[1].ProviderID != prov2.Id {
		t.Fatalf("unexpected fallback chain: %+v", chain)
	}

	// models list
	names, err := s.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "alias" || names[1] != "qwen/qwen3:free" {
		t.Fatalf("unexpected models: %v", names)
	}

	// cache invalidation on route change
	routes, _ = app.FindCollectionByNameOrId("routes")
	r2 := core.NewRecord(routes)
	r2.Set("alias", "second")
	r2.Set("provider", providerID)
	r2.Set("upstream_model", "m2")
	r2.Set("priority", 0)
	r2.Set("enabled", true)
	if err := app.Save(r2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(context.Background(), gateway.EndpointChat, "second"); err != nil {
		t.Fatalf("expected cache invalidation to pick up new route: %v", err)
	}

	// unknown alias
	if _, err := s.Resolve(context.Background(), gateway.EndpointChat, "nope"); err == nil {
		t.Fatal("expected unknown model error")
	}

	// log write with bodies
	sink := NewLogSink(app)
	v := int64(9)
	err = sink.Write(context.Background(), gatewayRecord(key.ID, providerID, &v))
	if err != nil {
		t.Fatalf("write log: %v", err)
	}
	logs, err := app.FindAllRecords("request_logs")
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].GetString("alias") != "alias" {
		t.Fatalf("unexpected logs: %+v", logs)
	}
	if logs[0].GetString("provider") != providerID || logs[0].GetString("provider_name") != "prov" {
		t.Fatalf("unexpected provider log fields: provider=%q provider_name=%q", logs[0].GetString("provider"), logs[0].GetString("provider_name"))
	}
	if logs[0].GetString("api_key_name") != "smoke" {
		t.Fatalf("unexpected api key name snapshot: %q", logs[0].GetString("api_key_name"))
	}
	if got := logs[0].GetDateTime("started_at").Time(); !got.Equal(testStartedAt) {
		t.Fatalf("expected started_at %v, got %v", testStartedAt, got)
	}
	bodies, err := app.FindAllRecords("request_bodies")
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 || bodies[0].GetString("request") != "req-body" {
		t.Fatalf("unexpected bodies: %+v", bodies)
	}
}

func TestLogCachedTokensAndFinishReason(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	providerID := seedConfig(t, app, s)
	secret, err := s.CreateAPIKey(app, "logtest")
	if err != nil {
		t.Fatal(err)
	}
	key, err := s.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}

	sink := NewLogSink(app)
	total := int64(13)
	cached := int64(4)
	reason := "stop"
	base := gateway.Record{
		KeyID: key.ID, KeyName: "logtest", Alias: "alias",
		ProviderID: providerID, ProviderName: "prov", UpstreamModel: "gpt-x",
		TotalMS: 12, Status: 200, Outcome: gateway.OutcomeCompleted,
	}
	// Absent usage stays NULL rather than becoming a misleading zero.
	full := base
	full.Usage = gateway.Usage{TotalTokens: &total, CachedTokens: &cached}
	full.Outcome = gateway.OutcomeClientDisconnected
	full.Error = "response write failed: io: read/write on closed pipe"
	full.FinishReason = &reason
	if err := sink.Write(context.Background(), full); err != nil {
		t.Fatalf("write log: %v", err)
	}
	if err := sink.Write(context.Background(), base); err != nil {
		t.Fatalf("write bare log: %v", err)
	}

	logs, err := app.FindAllRecords("request_logs")
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 {
		t.Fatalf("expected 2 logs, got %d", len(logs))
	}
	// JSON number fields round-trip as raw JSON; compare textually.
	if got := logs[0].GetString("cached_tokens"); got != "4" {
		t.Fatalf("expected cached_tokens 4, got %q (%#v raw)", got, logs[0].Get("cached_tokens"))
	}
	if got := logs[0].GetString("finish_reason"); got != "stop" {
		t.Fatalf("expected finish_reason stop, got %q", got)
	}
	if got := logs[0].GetString("outcome"); got != "client_disconnected" {
		t.Fatalf("expected client_disconnected outcome, got %q", got)
	}
	if got := logs[0].GetString("error"); !strings.Contains(got, "closed pipe") {
		t.Fatalf("expected disconnect error, got %q", got)
	}
	if got := logs[1].GetString("outcome"); got != "completed" {
		t.Fatalf("expected completed outcome, got %q", got)
	}
	// Absent data persists as JSON null, not a misleading zero.
	if got := logs[1].GetString("cached_tokens"); got != "null" {
		t.Fatalf("expected NULL cached_tokens, got %q", got)
	}
	if got := logs[1].GetString("finish_reason"); got != "" {
		t.Fatalf("expected empty finish_reason, got %q", got)
	}
	// A zero start time stays NULL.
	if got := logs[1].GetDateTime("started_at"); !got.IsZero() {
		t.Fatalf("expected NULL started_at, got %v", got.Time())
	}
}

func TestUsageViewsSplitOutcomes(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	providerID := seedConfig(t, app, s)
	secret, err := s.CreateAPIKey(app, "split")
	if err != nil {
		t.Fatal(err)
	}
	key, err := s.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}

	// A second key that never makes a request, to pin the zero-count path.
	idle, err := s.CreateAPIKey(app, "idle")
	if err != nil {
		t.Fatal(err)
	}
	idleKey, err := s.Authenticate(context.Background(), idle)
	if err != nil {
		t.Fatal(err)
	}

	base := gateway.Record{
		KeyID: key.ID, KeyName: "split", Alias: "alias", Endpoint: gateway.EndpointChat,
		ProviderID: providerID, ProviderName: "prov", UpstreamModel: "gpt-x",
	}
	outcomes := []gateway.Outcome{
		gateway.OutcomeCompleted,
		gateway.OutcomeCompleted,
		gateway.OutcomeCompleted,
		gateway.OutcomeUpstreamError,
		gateway.OutcomeClientDisconnected,
		gateway.OutcomeRejected,
	}
	sink := NewLogSink(app)
	for i, outcome := range outcomes {
		rec := base
		rec.Outcome = outcome
		rec.Status = 200
		if outcome != gateway.OutcomeCompleted {
			rec.Status = 500
		}
		rec.StartedAt = testStartedAt.Add(time.Duration(i) * time.Second)
		if err := sink.Write(context.Background(), rec); err != nil {
			t.Fatalf("write log %d: %v", i, err)
		}
	}

	// A completed request on a different key must not leak into the split's
	// numbers, and last_used must come from the newest log.
	other := base
	other.KeyID = idleKey.ID
	other.KeyName = "idle"
	other.Outcome = gateway.OutcomeCompleted
	other.Status = 200
	other.StartedAt = testStartedAt.Add(time.Minute)
	if err := sink.Write(context.Background(), other); err != nil {
		t.Fatalf("write idle log: %v", err)
	}

	assertCounts := func(t *testing.T, collection, idField, id string, wantSuccess, wantFail int) {
		t.Helper()
		records, err := app.FindAllRecords(collection)
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range records {
			if rec.GetString(idField) != id {
				continue
			}
			if got := rec.GetInt("success_requests"); got != wantSuccess {
				t.Fatalf("%s: success_requests = %d, want %d", collection, got, wantSuccess)
			}
			if got := rec.GetInt("fail_requests"); got != wantFail {
				t.Fatalf("%s: fail_requests = %d, want %d", collection, got, wantFail)
			}
			return
		}
		t.Fatalf("%s: no row for %s", collection, id)
	}

	routes, err := app.FindAllRecords("routes")
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(routes))
	}

	// Providers and routes see every request regardless of key, so the idle
	// key's extra success is included on top of the six split ones.
	assertCounts(t, "api_keys_usage", "id", key.ID, 3, 3)
	assertCounts(t, "api_keys_usage", "id", idleKey.ID, 1, 0)
	assertCounts(t, "providers_usage", "id", providerID, 4, 3)
	assertCounts(t, "routes_usage", "id", routes[0].Id, 4, 3)
}

func TestUsageViewsZeroCountsForUnusedRows(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	providerID := seedConfig(t, app, s)

	records, err := app.FindAllRecords("providers_usage")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 provider usage row, got %d", len(records))
	}
	row := records[0]
	// Zeroed counts, not NULL, and no last_used.
	if row.GetString("id") != providerID {
		t.Fatalf("unexpected provider row %q", row.GetString("id"))
	}
	if got := row.GetInt("success_requests"); got != 0 {
		t.Fatalf("success_requests = %d, want 0", got)
	}
	if got := row.GetInt("fail_requests"); got != 0 {
		t.Fatalf("fail_requests = %d, want 0", got)
	}
	if got := row.GetString("last_used"); got != "null" {
		t.Fatalf("last_used = %q, want NULL", got)
	}
}

func TestSettingsLiveToggle(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}

	if s.CaptureEnabled() {
		t.Fatal("expected capture off by default")
	}
	if s.Retention() != 24*60*60*1e9 {
		t.Fatalf("expected 24h retention, got %v", s.Retention())
	}
	if s.RetentionCron() != "0 3 * * *" {
		t.Fatalf("expected default cron, got %q", s.RetentionCron())
	}

	settings, err := app.FindAllRecords("gateway_settings")
	if err != nil {
		t.Fatal(err)
	}
	if len(settings) != 1 {
		t.Fatalf("expected 1 settings row, got %d", len(settings))
	}

	// Flip on: hooks must invalidate the in-memory cache so the next
	// read reflects the edit with no restart and no manual Invalidate.
	settings[0].Set("capture_bodies", true)
	settings[0].Set("retention_hours", 48)
	settings[0].Set("retention_cron", "0 4 * * *")
	if err := app.Save(settings[0]); err != nil {
		t.Fatal(err)
	}
	if !s.CaptureEnabled() {
		t.Fatal("expected capture on after settings edit")
	}
	if s.Retention() != 48*60*60*1e9 {
		t.Fatalf("expected 48h retention, got %v", s.Retention())
	}
	if s.RetentionCron() != "0 4 * * *" {
		t.Fatalf("expected updated cron, got %q", s.RetentionCron())
	}

	settings[0].Set("capture_bodies", false)
	if err := app.Save(settings[0]); err != nil {
		t.Fatal(err)
	}
	if s.CaptureEnabled() {
		t.Fatal("expected capture off after settings edit")
	}
}

func TestRetentionScheduleUpdate(t *testing.T) {
	app := newTestApp(t)
	sink := NewLogSink(app)
	if err := sink.RegisterRetention(app, "0 3 * * *", nil); err != nil {
		t.Fatal(err)
	}

	if err := sink.UpdateSchedule("0 4 * * *"); err != nil {
		t.Fatalf("valid reschedule: %v", err)
	}

	// Invalid expression keeps the previous job, never kills cleanup.
	if err := sink.UpdateSchedule("not a cron"); err == nil {
		t.Fatal("expected error for invalid cron expression")
	}
	if total := sink.cron.Total(); total != 1 {
		t.Fatalf("expected 1 job after failed update, got %d", total)
	}
}

// The gateway_settings row is data, so it is seeded on start: backfilled with
// defaults when blank, and recreated outright if deleted. The collection's shape
// is the snapshot's business, covered in schema_test.go.
func TestSeedSettingsRow(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := app.FindAllRecords("gateway_settings")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 settings row, got %d", len(rows))
	}
	if got := rows[0].GetInt("retention_hours"); got != 24 {
		t.Fatalf("retention_hours = %d, want the default 24", got)
	}

	// Re-seeding leaves an operator's values alone. The numeric and cron fields
	// are required, so a blank cannot be represented here; the backfill in
	// readSettings only matters for a field newly added by the snapshot, and
	// PocketBase fills absent columns itself.
	rows[0].Set("retention_hours", 72)
	rows[0].Set("retention_cron", "5 4 * * *")
	if err := app.Save(rows[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.seedSettings(); err != nil {
		t.Fatal(err)
	}
	rows, err = app.FindAllRecords("gateway_settings")
	if err != nil {
		t.Fatal(err)
	}
	if got := rows[0].GetInt("retention_hours"); got != 72 {
		t.Fatalf("an existing value must be preserved, got %d", got)
	}
	if got := rows[0].GetString("retention_cron"); got != "5 4 * * *" {
		t.Fatalf("an existing value must be preserved, got %q", got)
	}

	// A deleted singleton row is recreated with defaults.
	if err := app.Delete(rows[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.seedSettings(); err != nil {
		t.Fatal(err)
	}
	rows, err = app.FindAllRecords("gateway_settings")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].GetInt("retention_hours") != 24 ||
		rows[0].GetString("retention_cron") != "0 3 * * *" ||
		rows[0].GetBool("capture_bodies") {
		t.Fatalf("expected a defaults-regenerated row, got %+v", rows[0])
	}
}

func seedTransformer(t *testing.T, app *core.BaseApp, name, providerID, pattern string, priority int, expression string, enabled bool) {
	t.Helper()
	collection, err := app.FindCollectionByNameOrId("transformers")
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(collection)
	r.Set("name", name)
	r.Set("provider", providerID)
	r.Set("model_pattern", pattern)
	r.Set("priority", priority)
	r.Set("expression", expression)
	r.Set("enabled", enabled)
	if err := app.Save(r); err != nil {
		t.Fatalf("save transformer %s: %v", name, err)
	}
}

func TestTransformersEndToEnd(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	providerID := seedConfig(t, app, s)
	providers, err := app.FindCollectionByNameOrId("providers")
	if err != nil {
		t.Fatal(err)
	}
	prov2 := core.NewRecord(providers)
	prov2.Set("name", "prov2")
	prov2.Set("base_url", "http://127.0.0.1:9912")
	prov2.Set("api_key", "secret2")
	prov2.Set("enabled", true)
	if err := app.Save(prov2); err != nil {
		t.Fatal(err)
	}
	target := gateway.Target{ProviderID: providerID, BaseURL: "http://127.0.0.1:9911", Model: "gpt-x"}
	body := []byte(`{"model":"gpt-x","messages":[]}`)

	seedTransformer(t, app, "broad", providerID, `gpt-`, 20, `{"shaped": "broad"}`, true)
	seedTransformer(t, app, "exact", providerID, `^gpt-x$`, 10, `{"shaped": "exact"}`, true)
	seedTransformer(t, app, "other-provider", prov2.Id, `.*`, 0, `{"shaped": "wrong"}`, true)
	seedTransformer(t, app, "disabled", providerID, `.*`, 0, `{"shaped": "wrong"}`, false)

	// First match by priority wins.
	out, name, err := s.Transform(context.Background(), target, body)
	if err != nil {
		t.Fatal(err)
	}
	if name != "exact" || string(out) != `{"shaped":"exact"}` {
		t.Fatalf("got name=%q out=%s", name, out)
	}

	// No match passes the body through untouched.
	out, name, err = s.Transform(context.Background(), gateway.Target{ProviderID: providerID, Model: "o1"}, body)
	if err != nil || name != "" || string(out) != string(body) {
		t.Fatalf("got name=%q out=%s err=%v", name, out, err)
	}

	// New rows are picked up without restart (hook invalidation).
	seedTransformer(t, app, "new-winner", providerID, `^gpt-x$`, 5, `{"shaped": "new"}`, true)
	if out, name, err = s.Transform(context.Background(), target, body); err != nil || name != "new-winner" {
		t.Fatalf("got name=%q out=%s err=%v", name, out, err)
	}

	// Invalid definitions are rejected at save time.
	collection, err := app.FindCollectionByNameOrId("transformers")
	if err != nil {
		t.Fatal(err)
	}
	bad := core.NewRecord(collection)
	bad.Set("name", "bad")
	bad.Set("provider", providerID)
	bad.Set("model_pattern", "([bad")
	bad.Set("priority", 0)
	bad.Set("expression", `$`)
	bad.Set("enabled", true)
	if err := app.Save(bad); err == nil {
		t.Fatal("expected save-time rejection of bad pattern")
	}
	bad.Set("model_pattern", `.*`)
	bad.Set("expression", `{{{`)
	if err := app.Save(bad); err == nil {
		t.Fatal("expected save-time rejection of bad expression")
	}
}

func TestAPIKeyDashboardFlow(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	collection, err := app.FindCollectionByNameOrId("api_keys")
	if err != nil {
		t.Fatal(err)
	}

	// Name only: secret is autogenerated, hashed and encrypted by the hook.
	dash := core.NewRecord(collection)
	dash.Set("name", "dashboard")
	dash.Set("enabled", true)
	if err := app.Save(dash); err != nil {
		t.Fatalf("name-only save: %v", err)
	}
	reloaded, err := app.FindRecordById(collection, dash.Id)
	if err != nil {
		t.Fatal(err)
	}
	if plain := reloaded.GetString("key_plain"); plain == "" || !strings.HasPrefix(plain, "enc:") {
		t.Fatalf("expected generated enc: secret, got %q", plain)
	}
	if hash := reloaded.GetString("key_hash"); hash == "" {
		t.Fatal("expected derived key_hash")
	}
	revealed, err := s.RevealAPIKey(app, "dashboard")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(revealed, "sk-") {
		t.Fatalf("generated secret missing sk- prefix: %q", revealed)
	}
	if _, err := s.Authenticate(context.Background(), revealed); err != nil {
		t.Fatalf("generated secret does not authenticate: %v", err)
	}

	// Typed secret: derived hash, encrypted at rest, authenticates.
	typed := core.NewRecord(collection)
	typed.Set("name", "typed")
	typed.Set("key_plain", "my-own-secret")
	typed.Set("enabled", true)
	if err := app.Save(typed); err != nil {
		t.Fatalf("typed save: %v", err)
	}
	if _, err := s.Authenticate(context.Background(), "my-own-secret"); err != nil {
		t.Fatalf("typed secret does not authenticate: %v", err)
	}
	// Typed input passes through byte-identical: no prefix added, no mutation.
	if revealed, err := s.RevealAPIKey(app, "typed"); err != nil || revealed != "my-own-secret" {
		t.Fatalf("typed secret was modified, got %q err=%v", revealed, err)
	}

	// Rotation: new plaintext re-derives the hash; old secret stops working.
	typed.Set("key_plain", "rotated-secret")
	if err := app.Save(typed); err != nil {
		t.Fatalf("rotation save: %v", err)
	}
	if _, err := s.Authenticate(context.Background(), "rotated-secret"); err != nil {
		t.Fatalf("rotated secret does not authenticate: %v", err)
	}

	// Clearing the secret on update regenerates it; old secret stops working.
	typed.Set("key_plain", "")
	if err := app.Save(typed); err != nil {
		t.Fatalf("regeneration save: %v", err)
	}
	regenerated, err := s.RevealAPIKey(app, "typed")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(regenerated, "sk-") {
		t.Fatalf("regenerated secret missing sk- prefix: %q", regenerated)
	}
	if _, err := s.Authenticate(context.Background(), regenerated); err != nil {
		t.Fatalf("regenerated secret does not authenticate: %v", err)
	}
	if _, err := s.Authenticate(context.Background(), "rotated-secret"); err == nil {
		t.Fatal("old secret still authenticates after regeneration")
	}
}

func TestListAPIKeys(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAPIKey(app, "beta"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAPIKey(app, "alpha"); err != nil {
		t.Fatal(err)
	}
	disabled, err := app.FindFirstRecordByFilter("api_keys", "name = {:name}", dbx.Params{"name": "beta"})
	if err != nil {
		t.Fatal(err)
	}
	disabled.Set("enabled", false)
	if err := app.Save(disabled); err != nil {
		t.Fatal(err)
	}

	keys, err := s.ListAPIKeys(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
	if keys[0].Name != "alpha" || !keys[0].Enabled {
		t.Fatalf("unexpected first key: %+v", keys[0])
	}
	if keys[1].Name != "beta" || keys[1].Enabled {
		t.Fatalf("unexpected second key: %+v", keys[1])
	}
}

func TestProviderCommands(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	seedConfig(t, app, s)

	providers, err := app.FindCollectionByNameOrId("providers")
	if err != nil {
		t.Fatal(err)
	}
	off := core.NewRecord(providers)
	off.Set("name", "aaa")
	off.Set("base_url", "http://127.0.0.1:9912")
	off.Set("api_key", "other-secret")
	off.Set("enabled", false)
	if err := app.Save(off); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListProviders(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(list))
	}
	if list[0].Name != "aaa" || list[0].BaseURL != "http://127.0.0.1:9912" || list[0].Enabled {
		t.Fatalf("unexpected first provider: %+v", list[0])
	}
	if list[1].Name != "prov" || !list[1].Enabled {
		t.Fatalf("unexpected second provider: %+v", list[1])
	}

	got, err := s.GetProvider(app, "prov")
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL != "http://127.0.0.1:9911" || !got.Enabled {
		t.Fatalf("unexpected provider info: %+v", got)
	}
	if _, err := s.GetProvider(app, "nope"); err == nil {
		t.Fatal("expected error for unknown provider")
	}

	token, err := s.RevealProviderToken(app, "prov")
	if err != nil {
		t.Fatal(err)
	}
	if token != "upstream-secret" {
		t.Fatalf("unexpected token: %q", token)
	}
	if _, err := s.RevealProviderToken(app, "nope"); err == nil {
		t.Fatal("expected error for unknown provider token")
	}
}

func TestRoutesCSVImportExport(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	providerID := seedConfig(t, app, s)
	file := t.TempDir() + "/routes.csv"
	if err := s.ExportRoutes(file); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "alias,prov,gpt-x,chat,0,true") {
		t.Fatalf("unexpected export: %s", data)
	}

	if err := os.WriteFile(file, []byte("alias,provider,upstream_model,endpoint_type,priority,enabled\nqwen/qwen3:free,prov,qwen-30b,chat,5,false\nalias,prov,gpt-x,chat,7,false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportRoutes(file, false); err != nil {
		t.Fatal(err)
	}
	routes, err := app.FindAllRecords("routes")
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes after merge, got %d", len(routes))
	}
	updated, err := app.FindFirstRecordByFilter("routes", "alias = 'alias'")
	if err != nil {
		t.Fatal(err)
	}
	if updated.GetInt("priority") != 7 || updated.GetBool("enabled") {
		t.Fatalf("route was not updated: %+v", updated)
	}
	newRoute, err := app.FindFirstRecordByFilter("routes", "alias = 'qwen/qwen3:free'")
	if err != nil || newRoute.GetString("provider") != providerID {
		t.Fatalf("route was not created: %v", err)
	}

	if err := os.WriteFile(file, []byte("alias,provider,upstream_model,priority,enabled\nnew,missing,m,0,true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportRoutes(file, false); err == nil {
		t.Fatal("expected unknown provider error")
	}
	if _, err := app.FindFirstRecordByFilter("routes", "alias = 'new'"); err == nil {
		t.Fatal("failed import partially wrote a route")
	}
}

func TestRoutesCSVBytesRoundTrip(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	seedConfig(t, app, s)

	// Bytes variant matches the file variant exactly.
	file := t.TempDir() + "/routes.csv"
	if err := s.ExportRoutes(file); err != nil {
		t.Fatal(err)
	}
	fromFile, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	fromBytes, err := s.ExportRoutesCSV()
	if err != nil {
		t.Fatal(err)
	}
	if string(fromBytes) != string(fromFile) {
		t.Fatalf("bytes export differs from file export:\n%s\n%s", fromBytes, fromFile)
	}

	// Re-importing the export is an idempotent no-op merge.
	if err := s.ImportRoutesReader(strings.NewReader(string(fromBytes)), false); err != nil {
		t.Fatalf("re-import of export: %v", err)
	}
	routes, err := app.FindAllRecords("routes")
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("expected 1 route after no-op re-import, got %d", len(routes))
	}

	// Reader variant surfaces the same validation errors as the file variant.
	if err := s.ImportRoutesReader(strings.NewReader("wrong,header\n"), false); err == nil {
		t.Fatal("expected header error")
	}
}

func TestRoutesImportPrune(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	seedConfig(t, app, s)
	collection, err := app.FindCollectionByNameOrId("routes")
	if err != nil {
		t.Fatal(err)
	}
	extra := core.NewRecord(collection)
	extra.Set("alias", "extra")
	extra.Set("provider", seedProviderID(t, app))
	extra.Set("upstream_model", "other")
	extra.Set("priority", 1)
	extra.Set("enabled", true)
	if err := app.Save(extra); err != nil {
		t.Fatal(err)
	}

	csv := "alias,provider,upstream_model,priority,enabled\nalias,prov,gpt-x,0,true\n"

	// Merge-only import keeps the absent route.
	if err := s.ImportRoutesReader(strings.NewReader(csv), false); err != nil {
		t.Fatal(err)
	}
	if n := countRoutes(t, app); n != 2 {
		t.Fatalf("expected 2 routes without prune, got %d", n)
	}

	// Prune import deletes it.
	if err := s.ImportRoutesReader(strings.NewReader(csv), true); err != nil {
		t.Fatal(err)
	}
	if n := countRoutes(t, app); n != 1 {
		t.Fatalf("expected 1 route with prune, got %d", n)
	}
	if _, err := app.FindFirstRecordByFilter("routes", "alias = 'extra'"); err == nil {
		t.Fatal("prune left the absent route behind")
	}

	// A failed prune import changes nothing.
	bad := "alias,provider,upstream_model,priority,enabled\nnew,missing,m,0,true\n"
	if err := s.ImportRoutesReader(strings.NewReader(bad), true); err == nil {
		t.Fatal("expected unknown provider error")
	}
	if n := countRoutes(t, app); n != 1 {
		t.Fatalf("expected 1 route after failed prune, got %d", n)
	}
}

func seedProviderID(t *testing.T, app *core.BaseApp) string {
	t.Helper()
	p, err := app.FindFirstRecordByFilter("providers", "name = 'prov'")
	if err != nil {
		t.Fatal(err)
	}
	return p.Id
}

func countRoutes(t *testing.T, app *core.BaseApp) int {
	t.Helper()
	routes, err := app.FindAllRecords("routes")
	if err != nil {
		t.Fatal(err)
	}
	return len(routes)
}

func setKeyPolicy(t *testing.T, app *core.BaseApp, name, alias, provider, model string) {
	t.Helper()
	rec, err := app.FindFirstRecordByFilter("api_keys", "name = '"+name+"'")
	if err != nil {
		t.Fatal(err)
	}
	rec.Set("alias_pattern", alias)
	rec.Set("provider_pattern", provider)
	rec.Set("model_pattern", model)
	if err := app.Save(rec); err != nil {
		t.Fatalf("save policy for %s: %v", name, err)
	}
}

func TestKeyPolicyEndToEnd(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := s.CreateAPIKey(app, "scoped")
	if err != nil {
		t.Fatal(err)
	}
	setKeyPolicy(t, app, "scoped", `^qwen/`, `^new-api$`, `qwen`)
	key, err := s.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	allowTarget := gateway.Target{ProviderID: "p1", ProviderName: "new-api", Model: "qwen/qwen3:free"}
	if !key.Allows("qwen/qwen3:free", allowTarget) {
		t.Fatal("expected policy to allow")
	}
	if key.Allows("gpt-4", allowTarget) {
		t.Fatal("alias pattern must deny")
	}
	if key.Allows("qwen/qwen3:free", gateway.Target{ProviderID: "p2", ProviderName: "other", Model: "qwen/qwen3:free"}) {
		t.Fatal("provider pattern must deny")
	}
	if key.Allows("qwen/qwen3:free", gateway.Target{ProviderID: "p1", ProviderName: "new-api", Model: "gpt-4"}) {
		t.Fatal("model pattern must deny")
	}

	// Invalid regexps are rejected at save time.
	rec, err := app.FindFirstRecordByFilter("api_keys", "name = 'scoped'")
	if err != nil {
		t.Fatal(err)
	}
	rec.Set("model_pattern", "([bad")
	if err := app.Save(rec); err == nil {
		t.Fatal("expected save-time rejection of bad model pattern")
	}

	// Disabling the key invalidates the cached authentication immediately.
	rec.Set("model_pattern", "")
	rec.Set("enabled", false)
	if err := app.Save(rec); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(context.Background(), secret); err == nil {
		t.Fatal("disabled key must not authenticate from cache")
	}

	// Tightening the policy applies without restart.
	rec.Set("enabled", true)
	rec.Set("alias_pattern", `^nothing-matches-this$`)
	if err := app.Save(rec); err != nil {
		t.Fatal(err)
	}
	key, err = s.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	if key.Allows("qwen/qwen3:free", allowTarget) {
		t.Fatal("tightened policy must deny")
	}
}

func seedRoute(t *testing.T, app *core.BaseApp, alias, providerID, upstream, endpoint string) {
	t.Helper()
	collection, err := app.FindCollectionByNameOrId("routes")
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(collection)
	r.Set("alias", alias)
	r.Set("provider", providerID)
	r.Set("upstream_model", upstream)
	if endpoint != "" {
		r.Set("endpoint_type", endpoint)
	}
	r.Set("priority", 0)
	r.Set("enabled", true)
	if err := app.Save(r); err != nil {
		t.Fatalf("save route %s/%s: %v", alias, endpoint, err)
	}
}

func TestRouteEndpointsIsolateChatAndImage(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	providerID := seedConfig(t, app, s)
	seedRoute(t, app, "alias", providerID, "img-x", "image")
	seedRoute(t, app, "pic", providerID, "img-y", "image")

	chat, err := s.Resolve(context.Background(), gateway.EndpointChat, "alias")
	if err != nil || len(chat) != 1 || chat[0].Model != "gpt-x" {
		t.Fatalf("chat targets = %+v, err = %v", chat, err)
	}
	img, err := s.Resolve(context.Background(), gateway.EndpointImage, "alias")
	if err != nil || len(img) != 1 || img[0].Model != "img-x" {
		t.Fatalf("image targets = %+v, err = %v", img, err)
	}
	// Same alias serves both kinds, listed once in the union.
	names, err := s.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "alias" || names[1] != "pic" {
		t.Fatalf("unexpected models: %v", names)
	}
	// A chat-only alias is unknown to images and vice versa.
	if _, err := s.Resolve(context.Background(), gateway.EndpointImage, "pic-alias-missing"); err == nil {
		t.Fatal("expected unknown model for missing image alias")
	}
	if _, err := s.Resolve(context.Background(), gateway.EndpointChat, "pic"); err == nil {
		t.Fatal("expected unknown model resolving an image-only alias as chat")
	}
	if _, err := s.Resolve(context.Background(), "video", "alias"); err == nil {
		t.Fatal("expected unknown model for an unknown endpoint kind")
	}
}

func TestRouteEndpointDefaultsAndValidation(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	providerID := seedConfig(t, app, s)

	// Empty defaults to chat via the save hook.
	seedRoute(t, app, "plain", providerID, "m", "")
	plain, err := app.FindFirstRecordByFilter("routes", "alias = 'plain'")
	if err != nil {
		t.Fatal(err)
	}
	if got := plain.GetString("endpoint_type"); got != "chat" {
		t.Fatalf("endpoint_type = %q, want chat", got)
	}

	// Unknown kinds are rejected at save time.
	collection, err := app.FindCollectionByNameOrId("routes")
	if err != nil {
		t.Fatal(err)
	}
	bad := core.NewRecord(collection)
	bad.Set("alias", "bad")
	bad.Set("provider", providerID)
	bad.Set("upstream_model", "m")
	bad.Set("endpoint_type", "video")
	bad.Set("enabled", true)
	if err := app.Save(bad); err == nil {
		t.Fatal("expected save-time rejection of unknown endpoint_type")
	}
}

func TestRoutesCSVLegacyImportDefaultsChat(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	seedConfig(t, app, s)

	legacy := "alias,provider,upstream_model,priority,enabled\nlegacy,prov,legacy-model,0,true\n"
	if err := s.ImportRoutesReader(strings.NewReader(legacy), false); err != nil {
		t.Fatalf("legacy import: %v", err)
	}
	chat, err := s.Resolve(context.Background(), gateway.EndpointChat, "legacy")
	if err != nil || len(chat) != 1 || chat[0].Model != "legacy-model" {
		t.Fatalf("chat targets = %+v, err = %v", chat, err)
	}
	if _, err := s.Resolve(context.Background(), gateway.EndpointImage, "legacy"); err == nil {
		t.Fatal("legacy rows must not resolve as image routes")
	}

	bad := "alias,provider,upstream_model,endpoint_type,priority,enabled\nbad,prov,m,video,0,true\n"
	if err := s.ImportRoutesReader(strings.NewReader(bad), false); err == nil {
		t.Fatal("expected rejection of unknown endpoint_type")
	}
	if _, err := app.FindFirstRecordByFilter("routes", "alias = 'bad'"); err == nil {
		t.Fatal("failed import partially wrote a route")
	}
}

func TestRoutesUsageSplitsByEndpoint(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	providerID := seedConfig(t, app, s)
	seedRoute(t, app, "alias", providerID, "img-x", "image")
	secret, err := s.CreateAPIKey(app, "split-endpoint")
	if err != nil {
		t.Fatal(err)
	}
	key, err := s.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}

	sink := NewLogSink(app)
	rec := gateway.Record{
		KeyID: key.ID, KeyName: "split-endpoint", Alias: "alias", Endpoint: gateway.EndpointImage,
		ProviderID: providerID, ProviderName: "prov", UpstreamModel: "img-x",
		Outcome: gateway.OutcomeCompleted, Status: 200, StartedAt: testStartedAt,
	}
	if err := sink.Write(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	rows, err := app.FindAllRecords("routes_usage")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string][2]int{}
	for _, row := range rows {
		if row.GetString("alias") != "alias" {
			continue
		}
		counts[row.GetString("endpoint_type")] = [2]int{row.GetInt("success_requests"), row.GetInt("fail_requests")}
	}
	if counts["image"] != [2]int{1, 0} {
		t.Fatalf("image row counts = %v, want one success", counts)
	}
	if counts["chat"] != [2]int{0, 0} {
		t.Fatalf("chat row counts = %v, want zeros", counts)
	}
}

func TestLogCostAndImageFiles(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	providerID := seedConfig(t, app, s)
	secret, err := s.CreateAPIKey(app, "imglog")
	if err != nil {
		t.Fatal(err)
	}
	key, err := s.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}

	sink := NewLogSink(app)
	cost := 0.04
	total := int64(4175)
	rec := gateway.Record{
		KeyID: key.ID, KeyName: "imglog", Alias: "pic", Endpoint: gateway.EndpointImage,
		ProviderID: providerID, ProviderName: "prov", UpstreamModel: "img-x",
		Usage:     gateway.Usage{TotalTokens: &total, Cost: &cost},
		TotalMS:   12,
		Status:    200,
		Outcome:   gateway.OutcomeCompleted,
		StartedAt: testStartedAt,
		Bodies:    &gateway.Bodies{Request: "req", Response: "resp"},
		Images: []gateway.GeneratedImage{
			{Data: []byte("first-bytes"), Name: "pic-0.png", MediaType: "image/png", Kind: gateway.ImageKindOutput},
			{Data: []byte("second-bytes"), Name: "pic-1.jpg", MediaType: "image/jpeg", Kind: gateway.ImageKindOutput},
			{Data: []byte("input-bytes"), Name: "pic-input-image-0.png", MediaType: "image/png", Kind: gateway.ImageKindInput},
		},
	}
	if err := sink.Write(context.Background(), rec); err != nil {
		t.Fatalf("write log: %v", err)
	}

	logs, err := app.FindAllRecords("request_logs")
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 log, got %d", len(logs))
	}
	if got := logs[0].GetFloat("cost"); got != 0.04 {
		t.Fatalf("cost = %v, want 0.04", got)
	}

	images, err := app.FindAllRecords("request_images")
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 3 {
		t.Fatalf("expected 3 image rows, got %d", len(images))
	}
	for i, row := range images {
		if row.GetString("log") != logs[0].Id {
			t.Fatalf("row %d links to %q, want log %q", i, row.GetString("log"), logs[0].Id)
		}
		if row.GetInt("position") != i {
			t.Fatalf("row %d position = %d", i, row.GetInt("position"))
		}
		name := row.GetString("image")
		if name == "" {
			t.Fatalf("row %d has no stored file", i)
		}
		if i == 0 && (!strings.HasSuffix(name, ".png") || row.GetString("media_type") != "image/png") {
			t.Fatalf("unexpected first image %q media %q", name, row.GetString("media_type"))
		}
	}
	kinds := map[string]int{}
	for _, row := range images {
		kinds[row.GetString("kind")]++
	}
	if kinds[gateway.ImageKindOutput] != 2 || kinds[gateway.ImageKindInput] != 1 {
		t.Fatalf("unexpected kinds: %v", kinds)
	}
}

func TestImageFilesExpireWithRetention(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	providerID := seedConfig(t, app, s)
	secret, err := s.CreateAPIKey(app, "imgexpire")
	if err != nil {
		t.Fatal(err)
	}
	key, err := s.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}

	sink := NewLogSink(app)
	rec := gateway.Record{
		KeyID: key.ID, KeyName: "imgexpire", Alias: "pic", Endpoint: gateway.EndpointImage,
		ProviderID: providerID, ProviderName: "prov", UpstreamModel: "img-x",
		TotalMS: 12, Status: 200, Outcome: gateway.OutcomeCompleted, StartedAt: testStartedAt,
		Images: []gateway.GeneratedImage{{Data: []byte("x"), Name: "pic-0.png", MediaType: "image/png"}},
	}
	if err := sink.Write(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	// Age the image row behind the autodate field, then run the job with a
	// retention that only catches it.
	old := time.Now().Add(-2 * time.Hour).UTC().Format("2006-01-02 15:04:05.000Z")
	if _, err := app.DB().NewQuery("UPDATE {{request_images}} SET [[created]] = {:old}").
		Bind(dbx.Params{"old": old}).Execute(); err != nil {
		t.Fatal(err)
	}
	sink.retentionFn = func() time.Duration { return time.Hour }
	sink.retentionJob(app)()

	remaining, err := app.FindAllRecords("request_images")
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected expired images deleted, got %d", len(remaining))
	}
	// The log row itself is history and stays.
	logs, err := app.FindAllRecords("request_logs")
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected log row kept, got %d", len(logs))
	}
}

func TestBackfillImageKinds(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	providerID := seedConfig(t, app, s)
	secret, err := s.CreateAPIKey(app, "kindfill")
	if err != nil {
		t.Fatal(err)
	}
	key, err := s.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	sink := NewLogSink(app)
	rec := gateway.Record{
		KeyID: key.ID, KeyName: "kindfill", Alias: "pic", Endpoint: gateway.EndpointImage,
		ProviderID: providerID, ProviderName: "prov", UpstreamModel: "img-x",
		TotalMS: 12, Status: 200, Outcome: gateway.OutcomeCompleted, StartedAt: testStartedAt,
		Images: []gateway.GeneratedImage{
			{Data: []byte("x"), Name: "pic-0.png", MediaType: "image/png", Kind: gateway.ImageKindOutput},
		},
	}
	if err := sink.Write(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	// A row predating the kind column, blanked behind the backfill.
	if _, err := app.DB().NewQuery("UPDATE {{request_images}} SET [[kind]] = ''").Execute(); err != nil {
		t.Fatal(err)
	}
	if err := backfillImageKinds(app); err != nil {
		t.Fatal(err)
	}
	images, err := app.FindAllRecords("request_images")
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 || images[0].GetString("kind") != "output" {
		t.Fatalf("unexpected kinds: %+v", images)
	}
}
