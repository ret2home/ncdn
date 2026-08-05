package main

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

type WaiterEntry struct {
	ch          chan struct{}
	resultEntry *CacheEntry
	resultData  []byte
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

func (c *CacheServer) internalNewRequest(w http.ResponseWriter, r *http.Request) {
	newErrorEntry := func(status int) (*CacheEntry, []byte) {
		return &CacheEntry{
			statusCode: status,
			header:     make(http.Header),
		}, []byte(http.StatusText(status) + "\n")
	}
	uri := r.URL.RequestURI()

	target := c.origin.ResolveReference(r.URL)
	req, err := http.NewRequest("GET", target.String(), nil)
	if err != nil {
		c.mu.Lock()
		c.loading[uri].resultEntry, c.loading[uri].resultData = newErrorEntry(http.StatusInternalServerError)
		c.finishLoading(uri)
		c.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	req.Header.Set("X-NCDN-PoPCache-NodeId", c.nodeId)

	prevPopID := r.Header.Get("X-NCDN-PoPCache-NodeId")
	if prevPopID != "" {
		req.Header.Set("X-NCDN-PoPCache-NodeId", prevPopID)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		c.mu.Lock()
		c.loading[uri].resultEntry, c.loading[uri].resultData = newErrorEntry(http.StatusBadGateway)
		c.finishLoading(uri)
		c.mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	var buf bytes.Buffer
	reader := io.TeeReader(resp.Body, &buf)
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-Cache", "MISS")
	w.WriteHeader(resp.StatusCode)
	if _, err = io.Copy(w, reader); err != nil {
		c.mu.Lock()
		c.loading[uri].resultEntry, c.loading[uri].resultData = newErrorEntry(http.StatusBadGateway)
		c.finishLoading(uri)
		c.mu.Unlock()
		return
	}

	path := c.URIToFilePath(uri)
	if resp.StatusCode == http.StatusOK {
		err = c.SetFile(path, buf.Bytes())
	}

	c.mu.Lock()
	c.loading[uri].resultEntry = &CacheEntry{
		statusCode: resp.StatusCode,
		header:     resp.Header,
		expire:     time.Now().Add(time.Second * 5),
		size:       uint64(buf.Len()),
		path:       path,
	}
	c.loading[uri].resultData = buf.Bytes()
	if resp.StatusCode == http.StatusOK {
		if err == nil {
			c.sievecache.Set(uri, c.loading[uri].resultEntry)
		}
	}
	c.finishLoading(uri)
	c.mu.Unlock()
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
func internalServeData(vent *CacheEntry, data []byte, w http.ResponseWriter, XCache string) {
	for key, values := range vent.header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-Cache", XCache)
	w.WriteHeader(vent.statusCode)
	w.Write(data)
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
		}
	} else if hit_clean {
		file, err := os.Open(cacheent.path)
		c.mu.Unlock()

		if err != nil {
			c.sievecache.Delete(uri)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		internalServeCache(cacheent, file, w, "HIT")
		return
	}
	c.mu.Unlock()

	if waiting {
		<-waiter_entry.ch

		internalServeData(waiter_entry.resultEntry, waiter_entry.resultData, w, "COLLAPSED")
		return
	}
	c.internalNewRequest(w, r)
}
