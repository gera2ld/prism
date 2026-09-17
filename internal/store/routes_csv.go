package store

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/pocketbase/pocketbase/core"
)

var routeCSVHeader = []string{"alias", "provider", "upstream_model", "priority", "enabled"}

type routeCSVRow struct {
	Alias         string
	Provider      string
	UpstreamModel string
	Priority      int
	Enabled       bool
}

func (s *Store) ExportRoutes(path string) error {
	data, err := s.ExportRoutesCSV()
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// ExportRoutesCSV renders the whole routing table as CSV bytes, shared by
// the CLI export command and the HTTP export endpoint.
func (s *Store) ExportRoutesCSV() ([]byte, error) {
	providers, err := s.app.FindAllRecords("providers")
	if err != nil {
		return nil, err
	}
	providerNames := make(map[string]string, len(providers))
	for _, provider := range providers {
		providerNames[provider.Id] = provider.GetString("name")
	}
	routes, err := s.app.FindAllRecords("routes")
	if err != nil {
		return nil, err
	}
	rows := make([]routeCSVRow, 0, len(routes))
	for _, route := range routes {
		provider, ok := providerNames[route.GetString("provider")]
		if !ok {
			return nil, fmt.Errorf("route %q references missing provider %q", route.Id, route.GetString("provider"))
		}
		rows = append(rows, routeCSVRow{
			Alias: route.GetString("alias"), Provider: provider,
			UpstreamModel: route.GetString("upstream_model"),
			Priority:      route.GetInt("priority"), Enabled: route.GetBool("enabled"),
		})
	}
	slices.SortFunc(rows, func(a, b routeCSVRow) int {
		if n := strings.Compare(a.Alias, b.Alias); n != 0 {
			return n
		}
		if n := strings.Compare(a.Provider, b.Provider); n != 0 {
			return n
		}
		if n := strings.Compare(a.UpstreamModel, b.UpstreamModel); n != 0 {
			return n
		}
		return a.Priority - b.Priority
	})

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(routeCSVHeader); err != nil {
		return nil, err
	}
	for _, row := range rows {
		if err := w.Write(routeCSVRecord(row)); err != nil {
			return nil, err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *Store) ImportRoutes(path string, prune bool) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return s.ImportRoutesReader(file, prune)
}

// ImportRoutesReader merges routes from CSV data, shared by the CLI import
// command and the HTTP import endpoint. With prune, every existing route
// absent from the CSV is deleted, making the file the full desired state.
// The file is fully validated before anything is written or deleted.
func (s *Store) ImportRoutesReader(r io.Reader, prune bool) error {
	reader := csv.NewReader(r)
	header, err := reader.Read()
	if err != nil {
		return fmt.Errorf("read CSV header: %w", err)
	}
	if !slices.Equal(header, routeCSVHeader) {
		return fmt.Errorf("invalid CSV header: got %v, want %v", header, routeCSVHeader)
	}
	providers, err := s.app.FindAllRecords("providers")
	if err != nil {
		return err
	}
	providerIDs := make(map[string]string, len(providers))
	for _, provider := range providers {
		providerIDs[provider.GetString("name")] = provider.Id
	}
	rows := make([]routeCSVRow, 0)
	seen := make(map[string]bool)
	for line := 2; ; line++ {
		record, readErr := reader.Read()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read CSV line %d: %w", line, readErr)
		}
		if len(record) != len(routeCSVHeader) {
			return fmt.Errorf("CSV line %d: expected %d fields", line, len(routeCSVHeader))
		}
		priority, parseErr := strconv.Atoi(record[3])
		if parseErr != nil {
			return fmt.Errorf("CSV line %d priority: %w", line, parseErr)
		}
		enabled, parseErr := strconv.ParseBool(record[4])
		if parseErr != nil {
			return fmt.Errorf("CSV line %d enabled: %w", line, parseErr)
		}
		if strings.TrimSpace(record[0]) == "" || strings.TrimSpace(record[1]) == "" || strings.TrimSpace(record[2]) == "" {
			return fmt.Errorf("CSV line %d has an empty required value", line)
		}
		providerID, ok := providerIDs[record[1]]
		if !ok {
			return fmt.Errorf("CSV line %d references unknown provider %q", line, record[1])
		}
		key := record[0] + "\x00" + providerID + "\x00" + record[2]
		if seen[key] {
			return fmt.Errorf("CSV line %d duplicates an earlier route", line)
		}
		seen[key] = true
		rows = append(rows, routeCSVRow{Alias: record[0], Provider: providerID, UpstreamModel: record[2], Priority: priority, Enabled: enabled})
	}

	routes, err := s.app.FindAllRecords("routes")
	if err != nil {
		return err
	}
	existing := make(map[string]*core.Record, len(routes))
	for _, route := range routes {
		key := route.GetString("alias") + "\x00" + route.GetString("provider") + "\x00" + route.GetString("upstream_model")
		existing[key] = route
	}
	collection, err := s.app.FindCollectionByNameOrId("routes")
	if err != nil {
		return err
	}
	for _, row := range rows {
		key := row.Alias + "\x00" + row.Provider + "\x00" + row.UpstreamModel
		route := existing[key]
		if route == nil {
			route = core.NewRecord(collection)
		}
		route.Set("alias", row.Alias)
		route.Set("provider", row.Provider)
		route.Set("upstream_model", row.UpstreamModel)
		route.Set("priority", row.Priority)
		route.Set("enabled", row.Enabled)
		if err := s.app.Save(route); err != nil {
			return err
		}
	}
	if prune {
		deleted := 0
		for key, route := range existing {
			if !seen[key] {
				if err := s.app.Delete(route); err != nil {
					return err
				}
				deleted++
			}
		}
		if s.logger != nil {
			s.logger.Info("pruned routes", "deleted", deleted)
		}
	}
	return nil
}

func routeCSVRecord(row routeCSVRow) []string {
	return []string{row.Alias, row.Provider, row.UpstreamModel, strconv.Itoa(row.Priority), strconv.FormatBool(row.Enabled)}
}
