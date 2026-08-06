package main

import (
	"fmt"
	"io"
	"log/slog"
	"net"
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
	isTmpFile   bool
}
type CacheServer struct {
	origin      *url.URL
	sievecache  SieveCache
	loading     map[string]*WaiterEntry
	client      *http.Client
	nodeId      string
	mu          sync.RWMutex
	maxFileSize int64
}

func NewCacheServer(origin *url.URL, nodeId string) *CacheServer {
	if err := os.MkdirAll("/tmp/cache-"+nodeId, 0o755); err != nil {
		panic(err)
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,

		MaxIdleConns:        1024,
		MaxIdleConnsPerHost: 256,

		MaxConnsPerHost: 256,

		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,

		DialContext: (&net.Dialer{
			Timeout:   3 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	cs := CacheServer{
		origin:     origin,
		sievecache: *NewSieveCache(1 << 10),
		loading:    map[string]*WaiterEntry{},
		client: &http.Client{
			Transport: transport,
			Timeout:   0,
		},
		nodeId:      nodeId,
		maxFileSize: 16 * (1 << 20), // 16MB
	}
	return &cs
}

func (c *CacheServer) finishLoading(uri string) {
	close(c.loading[uri].ch)
	delete(c.loading, uri)
}

func (c *CacheServer) internalNewRequest(
	uri_key string,
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

	target := c.origin.ResolveReference(r.URL)
	req, err := http.NewRequest(http.MethodGet, target.String(), nil)
	if err != nil {
		c.mu.Lock()
		c.loading[uri_key].resultEntry =
			newErrorEntry(http.StatusInternalServerError)
		c.finishLoading(uri_key)
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
		c.loading[uri_key].resultEntry = newErrorEntry(http.StatusBadGateway)
		c.finishLoading(uri_key)
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

	path := c.URIToFilePath(uri_key)
	savable := resp.StatusCode == http.StatusOK

	var (
		tmpfile *os.File
		tmpPath string
		dst     io.Writer = w
	)

	if savable {
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
		result     *CacheEntry
		committed  bool
		removePath string
	)

	c.mu.Lock()

	flight := c.loading[uri_key]

	if copyErr == nil && closeOK && tmpPath != "" {

		cacheable := written < c.maxFileSize && c.sievecache.MakeRoom(uri_key)

		result = &CacheEntry{
			statusCode: resp.StatusCode,
			header:     resp.Header.Clone(),
			path:       path,
			size:       written,
			expire:     time.Now().Add(5 * time.Second),
		}

		if cacheable {
			if renameErr := os.Rename(tmpPath, path); renameErr == nil {
				if c.sievecache.Set(uri_key, result) { // shoule be always ok
					c.sievecache.SetPin(uri_key, flight.waiter > 0)
					committed = true
				}
			}
		}

		if !committed {
			result.path = tmpPath
			flight.isTmpFile = true
			if flight.waiter == 0 {
				removePath = tmpPath
			}
		} else {
			removePath = tmpPath
		}
	}

	if result == nil {
		switch {
		case copyErr != nil:
			{
				result = newErrorEntry(http.StatusBadGateway)
				slog.Error(fmt.Sprintf("copy error: %v", copyErr))
			}

		case resp.StatusCode != http.StatusOK:
			{
				result = newErrorEntry(resp.StatusCode)
				slog.Error(fmt.Sprintf("status code: %d", resp.StatusCode))
			}

		default:
			{
				result = newErrorEntry(http.StatusBadGateway)
				slog.Error("unknown bad gateway")
			}
		}
	}

	flight.resultEntry = result

	c.finishLoading(uri_key)
	c.mu.Unlock()

	if removePath != "" {
		_ = os.Remove(removePath)
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
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	uri_key := r.Host + "\x00" + r.URL.RequestURI()
	c.mu.Lock()
	cacheent, cachehit := c.sievecache.Get(uri_key)
	waiter_entry, loading_flag := c.loading[uri_key]

	not_loaded := !cachehit && !loading_flag
	waiting := !cachehit && loading_flag
	hit_clean := cachehit

	// HIT_CLEAN -> NOT_LOADED
	if hit_clean && time.Now().After(cacheent.expire) {
		c.sievecache.Delete(uri_key)
		hit_clean = false
		not_loaded = true
	}

	if not_loaded {
		// NOT_LOADED -> WAITING
		c.loading[uri_key] = &WaiterEntry{
			ch:          make(chan struct{}),
			resultEntry: nil,
			waiter:      0,
			isTmpFile:   false,
		}
	} else if hit_clean {
		file, err := os.Open(cacheent.path)
		if err != nil {
			c.sievecache.Delete(uri_key)
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
			c.sievecache.SetPinIfSame(uri_key, waiter_entry.resultEntry, false)
		}
		removeFile := waiter_entry.waiter == 0 && waiter_entry.isTmpFile
		c.mu.Unlock()

		if err == nil {
			internalServeCache(waiter_entry.resultEntry, file, w, "COLLAPSED")
		} else {
			internalServeData(waiter_entry.resultEntry, w, "COLLAPSED")
		}

		if removeFile {
			os.Remove(waiter_entry.resultEntry.path)
		}
		return
	}
	c.internalNewRequest(uri_key, w, r)
}
