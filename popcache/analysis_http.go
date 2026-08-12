package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

func (c *CacheServer) handleAnalysisStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	c.analysis.Start(time.Now(), analysisMaxRequestIds(r))
	w.WriteHeader(http.StatusNoContent)
}

func (c *CacheServer) handleAnalysisStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	c.analysis.Stop(time.Now())
	w.WriteHeader(http.StatusNoContent)
}

func (c *CacheServer) handleAnalysisSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	writeAnalysisJSON(w, c.analysis)
}

func writeAnalysisJSON(w http.ResponseWriter, a *AnalysisState) {
	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(a.Snapshot()); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func analysisMaxRequestIds(r *http.Request) uint64 {
	raw := r.URL.Query().Get("maxRequestIds")
	if raw == "" {
		return DefaultAnalysisMaxRequestIds
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return DefaultAnalysisMaxRequestIds
	}
	return n
}
