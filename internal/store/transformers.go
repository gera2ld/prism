package store

import (
	"cmp"
	"context"
	"slices"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/gera2ld/prism/internal/gateway"
)

// loadTransformers returns enabled transformers ordered by priority ascending.
// Expressions compile at load; a bad row logs and is skipped so one bad
// definition cannot break all traffic (invalid expressions are normally
// rejected at save time by validateTransformer).
func (s *Store) loadTransformers() ([]gateway.Transformer, error) {
	s.mu.Lock()
	if s.transformersLoaded {
		defer s.mu.Unlock()
		return slices.Clone(s.transformers), nil
	}
	s.mu.Unlock()

	rows, err := s.app.FindAllRecords("transformers", dbx.HashExp{"enabled": true})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(rows, func(a, b *core.Record) int {
		return cmp.Compare(a.GetFloat("priority"), b.GetFloat("priority"))
	})
	list := make([]gateway.Transformer, 0, len(rows))
	for _, r := range rows {
		t, err := gateway.CompileTransformer(
			r.GetString("name"),
			r.GetString("provider"),
			r.GetString("model_pattern"),
			r.GetFloat("priority"),
			r.GetString("expression"),
		)
		if err != nil {
			if s.logger != nil {
				s.logger.Error("skipping invalid transformer", "name", r.GetString("name"), "error", err)
			}
			continue
		}
		list = append(list, t)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.transformers = list
	s.transformersLoaded = true
	return slices.Clone(list), nil
}

func (s *Store) InvalidateTransformers() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transformers = nil
	s.transformersLoaded = false
}

// Transform applies the winning transformer for target, if any. Matching is
// provider-exact plus model-pattern regexp, first by priority wins.
func (s *Store) Transform(ctx context.Context, target gateway.Target, body []byte) ([]byte, string, error) {
	list, err := s.loadTransformers()
	if err != nil {
		return nil, "", err
	}
	for _, t := range list {
		if t.Matches(target.ProviderID, target.Model) {
			out, err := t.Apply(ctx, body)
			if err != nil {
				return nil, t.Name, err
			}
			return out, t.Name, nil
		}
	}
	return body, "", nil
}

// validateTransformer rejects uncompilable definitions at save time so bad
// JSONata or bad regexps never persist.
func validateTransformer(e *core.RecordEvent) error {
	_, err := gateway.CompileTransformer(
		e.Record.GetString("name"),
		e.Record.GetString("provider"),
		e.Record.GetString("model_pattern"),
		e.Record.GetFloat("priority"),
		e.Record.GetString("expression"),
	)
	return err
}
