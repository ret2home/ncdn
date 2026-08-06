package main

import (
	"crypto/sha256"
	"encoding/hex"
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
	uri_key     string
	ch          chan struct{}
	resultEntry *CacheEntry
	waiter      int
	isTmpFile   bool
}
type LoadingCounter struct {
	counter      int
	waiter_entry *WaiterEntry
}
type CacheServer struct {
	origin              *url.URL
	sievecache          SieveCache
	latestWaiterEntries map[string]*WaiterEntry
	waiterCount         map[string]int
	client              *http.Client
	nodeId              string
	mu                  sync.Mutex
	maxFileSize         int64
}

func NewCacheServer(origin *url.URL, nodeId string) *CacheServer {
	cacheDir := "/tmp/cache-" + nodeId
	if err := os.RemoveAll(cacheDir); err != nil {
		panic(err)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
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
		origin:              origin,
		sievecache:          *NewSieveCache(1 << 10),
		latestWaiterEntries: map[string]*WaiterEntry{},
		waiterCount:         map[string]int{},
		client: &http.Client{
			Transport: transport,
			Timeout:   0,
		},
		nodeId:      nodeId,
		maxFileSize: 16 * (1 << 20), // 16MB
	}
	return &cs
}

func (c *CacheServer) finishLoading(waiter_entry *WaiterEntry) {
	close(waiter_entry.ch)

	if c.latestWaiterEntries[waiter_entry.uri_key] == waiter_entry {
		delete(c.latestWaiterEntries, waiter_entry.uri_key)
	}
}

func (c *CacheServer) internalNewRequest(
	waiter_entry *WaiterEntry,
	uri_key string,
	w http.ResponseWriter,
	r *http.Request,
) {
	defer func() {
		c.mu.Lock()
		c.SubWaiterCount(uri_key)
		c.mu.Unlock()
	}()

	newErrorEntry := func(status int) *CacheEntry {
		return &CacheEntry{
			statusCode: status,
			header:     make(http.Header),
			path:       "",
			size:       0,
			saveTime:   time.Now(),
			cc:         nil,
		}
	}

	reference := &url.URL{
		Path:     r.URL.Path,
		RawPath:  r.URL.RawPath,
		RawQuery: r.URL.RawQuery,
	}

	target := c.origin.ResolveReference(reference)
	req, err := http.NewRequest(http.MethodGet, target.String(), nil)
	if err != nil {
		c.mu.Lock()
		waiter_entry.resultEntry =
			newErrorEntry(http.StatusInternalServerError)
		c.finishLoading(waiter_entry)
		c.mu.Unlock()

		slog.Error(fmt.Sprintf("Internal Server Error %s %v\n", uri_key, err))

		if w != nil {
			http.Error(
				w,
				http.StatusText(http.StatusInternalServerError),
				http.StatusInternalServerError,
			)
		}
		return
	}

	req.Header.Set("X-NCDN-PoPCache-NodeId", c.nodeId)

	// For Shield PoP
	if prevPopID := r.Header.Get("X-NCDN-PoPCache-NodeId"); prevPopID != "" {
		req.Header.Set("X-NCDN-PoPCache-NodeId", prevPopID)
	}

	resp, err := c.client.Do(req)

	req_cc := ParseRequestCacheControl(r.Header.Values("Cache-Control"))

	// STALE IF ERROR
	if err != nil || resp.StatusCode == 500 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504 {
		if err == nil {
			defer resp.Body.Close()
		}
		c.mu.Lock()

		cacheent, cachehit := c.sievecache.Get(uri_key)

		staleFlag := false
		if cachehit && cacheent.cc.StaleIfError != -1 && !cacheent.cc.MustRevalidate && !cacheent.cc.NoCache && !req_cc.NoCache && cacheent.cc.MaxAge != -1 {
			fresh := cacheent.saveTime.Add(time.Second * time.Duration(cacheent.cc.MaxAge))
			if fresh.Add(time.Second * time.Duration(cacheent.cc.StaleIfError)).After(time.Now()) {
				staleFlag = true
			}
		}

		var (
			cacheFile          *os.File
			cacheFileOpenError error
		)
		if w != nil && staleFlag {
			cacheFile, cacheFileOpenError = os.Open(cacheent.path)
		}

		returnStatusCode := http.StatusBadGateway
		if err == nil {
			returnStatusCode = resp.StatusCode
		}
		if staleFlag {
			waiter_entry.resultEntry = cacheent
		} else {
			waiter_entry.resultEntry = newErrorEntry(returnStatusCode)
		}
		c.finishLoading(waiter_entry)

		c.mu.Unlock()

		slog.Error(fmt.Sprintf("Origin Error %s %v\n", uri_key, err))
		if w != nil {
			if staleFlag {
				if cacheFileOpenError != nil {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				internalServeCache(cacheent, cacheFile, w, "STALE")
			} else {
				http.Error(w, http.StatusText(returnStatusCode), returnStatusCode)
			}
		}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Error(fmt.Sprintf("status %d\n", resp.StatusCode))
	}

	resp_cc := ParseResponseCacheControl(resp.Header.Values("Cache-Control"))

	if w != nil {
		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}

		w.Header().Set("X-Cache", "MISS")
		w.WriteHeader(resp.StatusCode)
	}

	sum := sha256.Sum256([]byte(uri_key))
	path := filepath.Join("/tmp/cache-"+c.nodeId, hex.EncodeToString(sum[:]))

	var (
		tmpfile *os.File
		tmpPath string
		dst     io.Writer = io.Discard
	)

	if w != nil {
		dst = w
	}

	// リクエストとキャッシュに書き込む
	tmpfile, err = os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err == nil {
		tmpPath = tmpfile.Name()
		if w != nil {
			dst = io.MultiWriter(w, tmpfile)
		} else {
			dst = tmpfile
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

	if copyErr == nil && closeOK && tmpPath != "" {

		// 事前に eviction しておく

		cacheable := resp.StatusCode == http.StatusOK && !resp_cc.NoStore && !req_cc.NoStore && resp_cc.MaxAge != -1 &&
			written < c.maxFileSize && c.sievecache.MakeRoom(uri_key)

		result = &CacheEntry{
			statusCode: resp.StatusCode,
			header:     resp.Header.Clone(),
			path:       path,
			size:       written,
			saveTime:   time.Now(),
			cc:         &resp_cc,
		}

		if cacheable {
			if renameErr := os.Rename(tmpPath, path); renameErr == nil {
				if c.sievecache.Set(uri_key, result) { // shoule be always ok
					c.sievecache.SetPin(uri_key, c.waiterCount[uri_key] > 1) // without own
					committed = true
				}
			}
		}

		if !committed {
			result.path = tmpPath
			waiter_entry.isTmpFile = true
			if waiter_entry.waiter == 0 {
				removePath = tmpPath
			}
		}
	} else {
		removePath = tmpPath
	}

	if result == nil {
		switch {
		case copyErr != nil:
			{
				result = newErrorEntry(http.StatusBadGateway)
				slog.Error(fmt.Sprintf("Copy Error: %v", copyErr))
			}

		case resp.StatusCode != http.StatusOK:
			{
				result = newErrorEntry(resp.StatusCode)
				slog.Error(fmt.Sprintf("Error: status %d", resp.StatusCode))
			}

		default:
			{
				result = newErrorEntry(http.StatusBadGateway)
				slog.Error("Unknown Bad Gateway")
			}
		}
	}

	waiter_entry.resultEntry = result

	c.finishLoading(waiter_entry)
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

func (c *CacheServer) AddWaiterCount(uri_key string) {
	waitersByURI, ok := c.waiterCount[uri_key]
	if ok {
		waitersByURI++
	} else {
		waitersByURI = 1
	}
	c.waiterCount[uri_key] = waitersByURI
	c.sievecache.SetPin(uri_key, true)
}
func (c *CacheServer) SubWaiterCount(uri_key string) {
	c.waiterCount[uri_key]--
	if c.waiterCount[uri_key] == 0 {
		delete(c.waiterCount, uri_key)
		c.sievecache.SetPin(uri_key, false)
	}
}

// Request ごとに Waiter Entry を作成し，Request Collapse する場合は entry の channel で待たせる
// Waiter Entry ごと，URI ごとに counter がある
// Waiter Entry Counter: Cache に保存しない Collapse 側通知用の一時ファイルの寿命管理　待ち collapsed requests を数える
// URI Counter: SIEVE Cache で eviction を避ける pin を付ける用　Cache Hit 以外の in-flight requests を数える
// SIEVE Cache, Waiter Count, cache file を操作する場合は Lock が必要

func (c *CacheServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	cc := ParseRequestCacheControl(r.Header.Values("Cache-Control"))

	uri_key := r.Host + "\x00" + r.URL.RequestURI()
	c.mu.Lock()
	cacheent, cachehit := c.sievecache.Get(uri_key)
	waiter_entry, loading_flag := c.latestWaiterEntries[uri_key]

	wantLoad := false
	background := false
	if !cachehit || cacheent.cc.NoCache || cc.NoCache || cacheent.cc.MaxAge == -1 {
		wantLoad = true
	} else if cc.MaxAge != -1 && cacheent.saveTime.Before(time.Now().Add(-time.Second*time.Duration(cc.MaxAge))) {
		wantLoad = true
	} else if cacheent.saveTime.Add(time.Second * time.Duration(cacheent.cc.MaxAge)).Before(time.Now()) {
		fresh := cacheent.saveTime.Add(time.Second * time.Duration(cacheent.cc.MaxAge))
		if cacheent.cc.MustRevalidate {
			wantLoad = true
		} else if cacheent.cc.StaleWhileRevalidate != -1 &&
			fresh.Add(time.Second*time.Duration(cacheent.cc.StaleWhileRevalidate)).After(time.Now()) {
			background = true
		} else {
			wantLoad = true
		}
	}

	new_load := wantLoad && !loading_flag
	collapsed := wantLoad && loading_flag
	returnCache := !wantLoad

	if new_load {
		waiter_entry = &WaiterEntry{
			uri_key:     uri_key,
			ch:          make(chan struct{}),
			resultEntry: nil,
			waiter:      0,
			isTmpFile:   false,
		}
		c.AddWaiterCount(uri_key)
		c.latestWaiterEntries[uri_key] = waiter_entry
	} else if returnCache {
		file, err := os.Open(cacheent.path)
		if err != nil {
			c.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if !background {
			c.mu.Unlock()
			internalServeCache(cacheent, file, w, "HIT")
		} else {
			// STALE WHILE REVALIDATE
			if !loading_flag {
				waiter_entry = &WaiterEntry{
					uri_key:     uri_key,
					ch:          make(chan struct{}),
					resultEntry: nil,
					waiter:      0,
					isTmpFile:   false,
				}
				c.AddWaiterCount(uri_key)
				c.latestWaiterEntries[uri_key] = waiter_entry
			}
			c.mu.Unlock()

			// COLLAPSE
			if !loading_flag {
				go c.internalNewRequest(waiter_entry, uri_key, nil, r)
			}
			internalServeCache(cacheent, file, w, "STALE")
		}
		return
	} else if collapsed {
		waiter_entry.waiter++
		c.AddWaiterCount(uri_key)
	}
	c.mu.Unlock()

	if collapsed {
		<-waiter_entry.ch

		c.mu.Lock()
		file, err := os.Open(waiter_entry.resultEntry.path)
		waiter_entry.waiter--
		c.SubWaiterCount(uri_key)
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
	c.internalNewRequest(waiter_entry, uri_key, w, r)
}
