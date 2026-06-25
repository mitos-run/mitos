package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/cli/sandboxtable"
	"mitos.run/mitos/internal/metering"
)

// meteringFetcher returns the CoW-aware metering report a forkd serves at
// GET <endpoint>/v1/metering. endpoint is a sandbox's Status.Endpoint (the
// forkd HTTP API host:port). The bool is false when the report could not be
// fetched (endpoint unset, unreachable, or a non-2xx); the caller then renders
// dashes for every sandbox on that node rather than inventing a number.
type meteringFetcher func(ctx context.Context, endpoint string) (metering.Report, bool)

// runTop lists the sandboxes in scope, fetches the per-node CoW-aware metering
// report from each node's forkd endpoint, and renders a per-sandbox table. A
// sandbox whose datum cannot be fetched (no endpoint, unreachable forkd, or no
// matching row) shows a dash, never a fabricated value.
func runTop(namespace string, allNamespaces bool) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var sandboxes v1.SandboxList
	if err := c.List(ctx, &sandboxes, listOpts(namespace, allNamespaces)...); err != nil {
		return fmt.Errorf("list sandboxes: %w", err)
	}

	rows := buildTopRows(ctx, sandboxes.Items, httpMeteringFetcher)
	fmt.Print(sandboxtable.FormatTop(rows))
	return nil
}

// buildTopRows resolves one TopRow per sandbox. It fetches each distinct node
// endpoint's metering report once (memoized), then matches each sandbox's
// Status.SandboxID against the report's per-sandbox rows. A sandbox with no
// endpoint, an unreachable forkd, or no matching sandbox row yields Found=false
// so every metered cell renders as a dash.
func buildTopRows(ctx context.Context, sandboxes []v1.Sandbox, fetch meteringFetcher) []sandboxtable.TopRow {
	reports := make(map[string]map[string]metering.SandboxMetering)
	fetched := make(map[string]bool)

	rows := make([]sandboxtable.TopRow, 0, len(sandboxes))
	for i := range sandboxes {
		s := &sandboxes[i]
		row := sandboxtable.TopRow{Name: s.Name, Node: s.Status.Node}

		ep := s.Status.Endpoint
		id := s.Status.SandboxID
		if ep != "" && id != "" {
			byID, ok := reports[ep]
			if !fetched[ep] {
				fetched[ep] = true
				if report, gotIt := fetch(ctx, ep); gotIt {
					byID = make(map[string]metering.SandboxMetering, len(report.Sandboxes))
					for _, sb := range report.Sandboxes {
						byID[sb.ID] = sb
					}
					reports[ep] = byID
				}
				ok = byID != nil
			}
			if ok {
				if datum, present := byID[id]; present {
					row.Datum = datum
					row.Found = true
				}
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// httpMeteringFetcher is the production meteringFetcher: it GETs
// http://<endpoint>/v1/metering and decodes the CoW-aware report. Any error
// (unset endpoint, dial failure, non-2xx, decode failure) returns ok=false so
// top degrades to dashes. The endpoint is operational data on the forkd HTTP
// mux, served unauthenticated alongside /metrics and /healthz, so no bearer
// token is sent here.
func httpMeteringFetcher(ctx context.Context, endpoint string) (metering.Report, bool) {
	if endpoint == "" {
		return metering.Report{}, false
	}
	url := fmt.Sprintf("http://%s/v1/metering", endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return metering.Report{}, false
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return metering.Report{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return metering.Report{}, false
	}
	var report metering.Report
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		return metering.Report{}, false
	}
	return report, true
}
