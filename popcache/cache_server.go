package main

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type WaiterEntry struct {
	ch          chan struct{}
	resultEntry *CacheEntry
	waiter      int
}
type CacheServer struct {
	origin     *url.URL
	sievecache SieveCache
	loading    map[string]*WaiterEntry
	client     *http.Client
	nodeId     string
	mu         sync.RWMutex
}

func NewCacheServer(origin *url.URL, nodeId string) *CacheServer {
	if err := os.MkdirAll("/tmp/cache-"+nodeId, 0o755); err != nil {
		panic(err)
	}
	cs := CacheServer{
		origin:     origin,
		sievecache: *NewSieveCache(),
		loading:    map[string]*WaiterEntry{},
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
		nodeId: nodeId,
	}
	return &cs
}

func (c *CacheServer) finishLoading(uri string) {
	close(c.loading[uri].ch)
	delete(c.loading, uri)
}

func (c *CacheServer) internalNewRequest(
	w http.ResponseWriter,
	r *http.Request,
) {
	newErrorEntry := func(status int) *CacheEntry {
		return &CacheEntry{
			statusCode: status,
			header:     make(http.Header),
			path:       "",
			size:       0,
			expire:     time.Now(),
		}
	}

	uri := r.URL.RequestURI()

	target := c.origin.ResolveReference(r.URL)
	req, err := http.NewRequest(http.MethodGet, target.String(), nil)
	if err != nil {
		c.mu.Lock()
		c.loading[uri].resultEntry =
			newErrorEntry(http.StatusInternalServerError)
		c.finishLoading(uri)
		c.mu.Unlock()

		http.Error(
			w,
			http.StatusText(http.StatusInternalServerError),
			http.StatusInternalServerError,
		)
		return
	}

	req.Header.Set("X-NCDN-PoPCache-NodeId", c.nodeId)

	if prevPopID := r.Header.Get("X-NCDN-PoPCache-NodeId"); prevPopID != "" {
		req.Header.Set("X-NCDN-PoPCache-NodeId", prevPopID)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		c.mu.Lock()
		c.loading[uri].resultEntry = newErrorEntry(http.StatusBadGateway)
		c.finishLoading(uri)
		c.mu.Unlock()

		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	w.Header().Set("X-Cache", "MISS")
	w.WriteHeader(resp.StatusCode)

	path := c.URIToFilePath(uri)
	cacheable := resp.StatusCode == http.StatusOK

	var (
		tmpfile *os.File
		tmpPath string
		dst     io.Writer = w
	)

	if cacheable {
		tmpfile, err = os.CreateTemp(filepath.Dir(path), ".tmp-*")
		if err == nil {
			tmpPath = tmpfile.Name()
			dst = io.MultiWriter(w, tmpfile)
		}
	}

	written, copyErr := io.CopyBuffer(dst, resp.Body, make([]byte, 64*1024))

	closeOK := false
	if tmpfile != nil {
		closeOK = tmpfile.Close() == nil
	}

	var (
		result    *CacheEntry
		committed bool
	)

	c.mu.Lock()

	flight := c.loading[uri]
	if copyErr == nil && closeOK && tmpPath != "" {
		if renameErr := os.Rename(tmpPath, path); renameErr == nil {
			committed = true

			result = &CacheEntry{
				statusCode: resp.StatusCode,
				header:     resp.Header.Clone(),
				path:       path,
				size:       written,
				expire:     time.Now().Add(5 * time.Second),
			}

			c.sievecache.Set(uri, result)
			c.sievecache.SetPin(uri, flight.waiter > 0)
		}
	}

	if result == nil {
		switch {
		case copyErr != nil:
			result = newErrorEntry(http.StatusBadGateway)

		case resp.StatusCode != http.StatusOK:
			result = newErrorEntry(resp.StatusCode)

		default:
			result = newErrorEntry(http.StatusBadGateway)
		}
	}

	flight.resultEntry = result

	c.finishLoading(uri)
	c.mu.Unlock()

	if !committed && tmpPath != "" {
		_ = os.Remove(tmpPath)
	}
}

func internalServeCache(vent *CacheEntry, file *os.File, w http.ResponseWriter, XCache string) {
	defer file.Close()
	for key, values := range vent.header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-Cache", XCache)
	w.WriteHeader(vent.statusCode)
	io.Copy(w, file)
}
func internalServeData(vent *CacheEntry, w http.ResponseWriter, XCache string) {
	for key, values := range vent.header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-Cache", XCache)
	w.WriteHeader(vent.statusCode)
}

// NOT_LOADED : !cache && !loading
// WAITING    : !cache && loading
// HIT_CLEAN  :  cache && !loading

// NOT_LOADED -> WAITING -> OK_RETURNING -> HIT_CLEAN -> NOT_LOADED
//                       -> NG_RETURNING -> NOT_LOADED
// FIXME: OK_RETURNING が長引くと expire 判定が遅くなる可能性がある

func (c *CacheServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uri := r.URL.RequestURI()
	c.mu.Lock()
	cacheent, cachehit := c.sievecache.Get(uri)
	waiter_entry, loading_flag := c.loading[uri]

	not_loaded := !cachehit && !loading_flag
	waiting := !cachehit && loading_flag
	hit_clean := cachehit

	// HIT_CLEAN -> NOT_LOADED
	if hit_clean && time.Now().After(cacheent.expire) {
		c.sievecache.Delete(uri)
		hit_clean = false
		not_loaded = true
	}

	if not_loaded {
		// NOT_LOADED -> WAITING
		c.loading[uri] = &WaiterEntry{
			ch:          make(chan struct{}),
			resultEntry: nil,
			waiter:      0,
		}
	} else if hit_clean {
		file, err := os.Open(cacheent.path)
		if err != nil {
			c.sievecache.Delete(uri)
			c.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		c.mu.Unlock()
		internalServeCache(cacheent, file, w, "HIT")
		return
	} else if waiting {
		waiter_entry.waiter++
	}
	c.mu.Unlock()

	if waiting {
		<-waiter_entry.ch

		c.mu.Lock()
		file, err := os.Open(waiter_entry.resultEntry.path)
		waiter_entry.waiter--
		if waiter_entry.waiter == 0 {
			c.sievecache.SetPin(uri, false)
		}
		c.mu.Unlock()

		if err == nil {
			internalServeCache(waiter_entry.resultEntry, file, w, "COLLAPSED")
		} else {
			internalServeData(waiter_entry.resultEntry, w, "COLLAPSED")
		}
		return
	}
	c.internalNewRequest(w, r)
}
