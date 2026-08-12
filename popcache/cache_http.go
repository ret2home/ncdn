package main

import "net/http"

func (c *CacheServer) handleCacheFlush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	c.sievecache.FlushAll()
	w.WriteHeader(http.StatusNoContent)
}
