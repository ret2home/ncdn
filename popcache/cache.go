package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type CacheEntry struct {
	statusCode int
	header     http.Header
	data       []byte
	expire     time.Time
}
type WaiterEntry struct {
	ch       chan struct{}
	waiters  int
	finished bool
	result   *CacheEntry
}
type CacheServer struct {
	origin   *url.URL
	cachemap map[string]*CacheEntry
	loading  map[string]*WaiterEntry
	client   *http.Client
	nodeId   string
	mu       sync.RWMutex
}

func NewCacheServer(origin *url.URL, nodeId string) *CacheServer {
	cs := CacheServer{
		origin:   origin,
		cachemap: make(map[string]*CacheEntry),
		loading:  map[string]*WaiterEntry{},
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
		nodeId: nodeId,
	}
	return &cs
}

func (c *CacheServer) finishLoading(uri string) {
	// WAITING -> OK/NG RETURNING
	close(c.loading[uri].ch)
	c.loading[uri].finished = true

	// OK_RETURNING -> HIT_CLEAN
	// NG_RETURNING -> NOT_LOADED
	if c.loading[uri].waiters == 0 {
		delete(c.loading, uri)
	}
}

func (c *CacheServer) internalNewRequest(w http.ResponseWriter, r *http.Request) {
	newErrorEntry := func(status int) *CacheEntry {
		return &CacheEntry{
			statusCode: status,
			header:     make(http.Header),
			data:       []byte(http.StatusText(status) + "\n"),
		}
	}
	uri := r.URL.RequestURI()

	target := c.origin.ResolveReference(r.URL)
	req, err := http.NewRequest("GET", target.String(), nil)
	if err != nil {
		c.mu.Lock()
		c.loading[uri].result = newErrorEntry(http.StatusInternalServerError)
		c.finishLoading(uri)
		c.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	req.Header.Set("X-NCDN-PoPCache-NodeId", c.nodeId)
	resp, err := c.client.Do(req)
	if err != nil {
		c.mu.Lock()
		c.loading[uri].result = newErrorEntry(http.StatusBadGateway)
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
	w.Header().Add("X-Cache", "MISS")
	w.WriteHeader(resp.StatusCode)
	if _, err = io.Copy(w, reader); err != nil {
		c.mu.Lock()
		c.loading[uri].result = newErrorEntry(http.StatusInternalServerError)
		c.finishLoading(uri)
		c.mu.Unlock()
		return
	}

	c.mu.Lock()
	c.loading[uri].result = &CacheEntry{
		statusCode: resp.StatusCode,
		header:     resp.Header,
		data:       buf.Bytes(),
		expire:     time.Now().Add(time.Second * 5),
	}
	if resp.StatusCode == http.StatusOK {
		c.cachemap[uri] = c.loading[uri].result
	}
	c.finishLoading(uri)
	c.mu.Unlock()
}

func internalServeCache(v *CacheEntry, w http.ResponseWriter, XCache string) {
	for key, values := range v.header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-Cache", XCache)
	w.WriteHeader(v.statusCode)
	w.Write(v.data)
}

// NOT_LOADED : !cache && !loading
// WAITING    : !cache && !loading.finished && loading.chan is open
// OK_RETURNING  : cache && loading.finished && loading.chan is closed && loading.waiters > 0
// NG_RETURNING  :!cache && loading.finished && loading.chan is closed && loading.waiters > 0
// HIT_CLEAN  :  cache && !loading

// NOT_LOADED -> WAITING -> OK_RETURNING -> HIT_CLEAN -> NOT_LOADED
//                       -> NG_RETURNING -> NOT_LOADED
// FIXME: OK_RETURNING が長引くと expire 判定が遅くなる可能性がある

func (c *CacheServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uri := r.URL.RequestURI()
	c.mu.Lock()
	cachev, cachehit := c.cachemap[uri]
	waiter_entry, loading_flag := c.loading[uri]

	not_loaded := !cachehit && !loading_flag
	waiting := !cachehit && loading_flag && !waiter_entry.finished
	ok_returning := cachehit && loading_flag && waiter_entry.finished
	ng_returning := !cachehit && loading_flag && waiter_entry.finished
	hit_clean := cachehit && !loading_flag

	// HIT_CLEAN -> NOT_LOADED
	if hit_clean && time.Now().After(cachev.expire) {
		delete(c.cachemap, uri)
		hit_clean = false
		not_loaded = true
	}

	if not_loaded {
		// NOT_LOADED -> WAITING
		c.loading[uri] = &WaiterEntry{
			waiters:  0,
			finished: false,
			ch:       make(chan struct{}),
			result:   nil,
		}
	} else if waiting {
		waiter_entry.waiters++
	} else if ok_returning || ng_returning {
		res := c.loading[uri].result
		c.mu.Unlock()
		internalServeCache(res, w, "COLLAPSED")
		return
	} else {
		c.mu.Unlock()
		internalServeCache(cachev, w, "HIT")
		return
	}
	c.mu.Unlock()

	if waiting {
		<-waiter_entry.ch

		c.mu.Lock()
		waiter_entry.waiters--

		fmt.Printf("waiters: %d\n", waiter_entry.waiters)

		res := c.loading[uri].result
		// OK_RETURNING -> HIT_CLEAN
		// NG_RETURNING -> NOT_LOADED
		if waiter_entry.waiters == 0 {
			delete(c.loading, uri)
		}
		c.mu.Unlock()

		internalServeCache(res, w, "COLLAPSED")
		return
	}
	c.internalNewRequest(w, r)
}
