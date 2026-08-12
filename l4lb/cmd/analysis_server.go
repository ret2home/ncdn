package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yzp0n/ncdn/l4lb/l4lbdrv"
)

type analysisPop struct {
	ID  string
	URL string
}

type analysisPopResult struct {
	ID       string          `json:"id"`
	URL      string          `json:"url"`
	Snapshot json.RawMessage `json:"snapshot,omitempty"`
	Error    string          `json:"error,omitempty"`
}

type analysisAggregate struct {
	CapturedAt time.Time           `json:"capturedAt"`
	Pops       []analysisPopResult `json:"pops"`
}

func analysisPopsFromDests(dests []l4lbdrv.DestinationEntry, healthCheckDest string) []analysisPop {
	pops := make([]analysisPop, 0, len(dests))
	addr := analysisAddrFromHealthCheckDest(healthCheckDest)
	if len(dests) > 0 {
		dests = dests[1:]
	}
	for _, dest := range dests {
		u := "http://[" + dest.IPAddr.String() + "]" + addr
		pops = append(pops, analysisPop{
			ID:  fmt.Sprintf("pop%d", len(pops)),
			URL: u,
		})
	}
	return pops
}

func analysisAddrFromHealthCheckDest(healthCheckDest string) string {
	healthCheckDest = strings.TrimSpace(healthCheckDest)
	healthCheckDest = strings.TrimSuffix(healthCheckDest, "/statusz")
	if healthCheckDest == "" {
		return ":8889"
	}
	return healthCheckDest
}

func startAnalysisServer(addr string, pops []analysisPop, staticDir string) {
	mux := http.NewServeMux()
	client := &http.Client{Timeout: 2 * time.Second}

	if staticDir != "" {
		fileServer := http.StripPrefix("/analyzer/", http.FileServer(http.Dir(staticDir)))
		mux.Handle("/analyzer/", fileServer)
		mux.HandleFunc("/analyzer", func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, staticDir+"/index.html")
		})
	}

	mux.HandleFunc("/debug/analysis/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		writeAggregatorJSON(w, fanoutAnalysisPost(client, pops, analysisPathWithQuery("/debug/analysis/start", r)))
	})
	mux.HandleFunc("/debug/analysis/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		writeAggregatorJSON(w, fanoutAnalysisPost(client, pops, "/debug/analysis/stop"))
	})
	mux.HandleFunc("/debug/cache/flush", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		writeAggregatorJSON(w, fanoutAnalysisPost(client, pops, "/debug/cache/flush"))
	})
	mux.HandleFunc("/debug/analysis", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		writeAggregatorJSON(w, fetchAnalysisSnapshots(client, pops))
	})

	slog.Info("Analysis server started", slog.String("addr", addr))
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("Analysis server stopped", slog.String("err", err.Error()))
	}
}

func fanoutAnalysisPost(client *http.Client, pops []analysisPop, path string) analysisAggregate {
	return eachAnalysisPop(pops, func(pop analysisPop) analysisPopResult {
		req, err := http.NewRequest(http.MethodPost, pop.URL+path, bytes.NewReader(nil))
		if err != nil {
			return analysisPopResult{ID: pop.ID, URL: pop.URL, Error: err.Error()}
		}

		resp, err := client.Do(req)
		if err != nil {
			return analysisPopResult{ID: pop.ID, URL: pop.URL, Error: err.Error()}
		}
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return analysisPopResult{ID: pop.ID, URL: pop.URL, Error: resp.Status}
		}
		return analysisPopResult{ID: pop.ID, URL: pop.URL}
	})
}

func analysisPathWithQuery(path string, r *http.Request) string {
	if r.URL.RawQuery == "" {
		return path
	}
	return path + "?" + r.URL.RawQuery
}

func fetchAnalysisSnapshots(client *http.Client, pops []analysisPop) analysisAggregate {
	return eachAnalysisPop(pops, func(pop analysisPop) analysisPopResult {
		resp, err := client.Get(pop.URL + "/debug/analysis")
		if err != nil {
			return analysisPopResult{ID: pop.ID, URL: pop.URL, Error: err.Error()}
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return analysisPopResult{ID: pop.ID, URL: pop.URL, Error: resp.Status}
		}

		var body json.RawMessage
		err = json.NewDecoder(resp.Body).Decode(&body)
		if err != nil {
			return analysisPopResult{ID: pop.ID, URL: pop.URL, Error: err.Error()}
		}
		return analysisPopResult{ID: pop.ID, URL: pop.URL, Snapshot: body}
	})
}

func eachAnalysisPop(pops []analysisPop, fn func(analysisPop) analysisPopResult) analysisAggregate {
	results := make([]analysisPopResult, len(pops))
	var wg sync.WaitGroup

	for i, pop := range pops {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = fn(pop)
		}()
	}
	wg.Wait()

	return analysisAggregate{
		CapturedAt: time.Now(),
		Pops:       results,
	}
}

func writeAggregatorJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}
