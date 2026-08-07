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
	cacheKey    string
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

	if c.latestWaiterEntries[waiter_entry.cacheKey] == waiter_entry {
		delete(c.latestWaiterEntries, waiter_entry.cacheKey)
	}
}

func (c *CacheServer) internalNewRequest(
	waiter_entry *WaiterEntry,
	cacheKey string,
	w http.ResponseWriter,
	r *http.Request,
) {
	defer func() {
		c.mu.Lock()
		c.SubWaiterCount(cacheKey)
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

		slog.Error(fmt.Sprintf("Internal Server Error %s %v\n", cacheKey, err))

		if w != nil {
			http.Error(
				w,
				http.StatusText(http.StatusInternalServerError),
				http.StatusInternalServerError,
			)
		}
		return
	}

	/* FIXME!
	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Set(key, value)
		}
	}*/
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

		cacheent, cachehit := c.sievecache.Get(cacheKey)

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

		slog.Error(fmt.Sprintf("Origin Error %s %v\n", cacheKey, err))
		if w != nil {
			if staleFlag {
				if cacheFileOpenError != nil {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				internalServeCache(cacheent, cacheFile, w, "STALE-ERROR")
			} else {
				http.Error(w, http.StatusText(returnStatusCode), returnStatusCode)
			}
		}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Error(fmt.Sprintf("status %s %d\n", cacheKey, resp.StatusCode))
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

	sum := sha256.Sum256([]byte(cacheKey))
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
			written < c.maxFileSize && c.sievecache.MakeRoom(cacheKey)

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
				if c.sievecache.Set(cacheKey, result) { // shoule be always ok
					c.sievecache.SetPin(cacheKey, c.waiterCount[cacheKey] > 1) // without own
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
				slog.Error(fmt.Sprintf("Copy Error: %s %v", cacheKey, copyErr))
			}

		case resp.StatusCode != http.StatusOK:
			{
				result = newErrorEntry(resp.StatusCode)
				slog.Error(fmt.Sprintf("Error: status %s %d", cacheKey, resp.StatusCode))
			}

		default:
			{
				result = newErrorEntry(http.StatusBadGateway)
				slog.Error(fmt.Sprintf("Unknown Bad Gateway %s", cacheKey))
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

func (c *CacheServer) AddWaiterCount(cacheKey string) {
	waitersByURI, ok := c.waiterCount[cacheKey]
	if ok {
		waitersByURI++
	} else {
		waitersByURI = 1
	}
	c.waiterCount[cacheKey] = waitersByURI
	c.sievecache.SetPin(cacheKey, true)
}
func (c *CacheServer) SubWaiterCount(cacheKey string) {
	c.waiterCount[cacheKey]--
	if c.waiterCount[cacheKey] == 0 {
		delete(c.waiterCount, cacheKey)
		c.sievecache.SetPin(cacheKey, false)
	}
}

func (c *CacheServer) internalNewLoad(cacheKey string, background bool,
	w http.ResponseWriter,
	r *http.Request) {

	waiter_entry := &WaiterEntry{
		cacheKey:    cacheKey,
		ch:          make(chan struct{}),
		resultEntry: nil,
		waiter:      0,
		isTmpFile:   false,
	}
	c.mu.Lock()
	c.AddWaiterCount(cacheKey)
	c.latestWaiterEntries[cacheKey] = waiter_entry
	c.mu.Unlock()

	if !background {
		c.internalNewRequest(waiter_entry, cacheKey, w, r)
	} else {
		go c.internalNewRequest(waiter_entry, cacheKey, nil, r)
	}
}

type LoadTypeInfo struct {
	waiterEntry          *WaiterEntry
	wantNewLoad          bool
	returnCache          bool
	collapsed            bool
	staleWhileRevalidate bool
	inFlightLoading      bool
	cacheEntry           *CacheEntry
	cacheFile            *os.File
	cacheFileError       error
}

func (c *CacheServer) DecideTypeOfLoad(cacheKey string, cc *RequestCacheControl) LoadTypeInfo {
	cacheent, cachehit := c.sievecache.Get(cacheKey)
	waiter_entry, loading_flag := c.latestWaiterEntries[cacheKey]

	wantLoad := false
	background := false
	if !cachehit || cacheent.cc.NoCache || cc.NoCache || cacheent.cc.MaxAge == -1 { // Cache Miss, No-Cache from req/resp, no cache maxage
		wantLoad = true
	} else if cc.MaxAge != -1 && cacheent.saveTime.Before(time.Now().Add(-time.Second*time.Duration(cc.MaxAge))) { // expired by req-maxage
		wantLoad = true
	} else if cacheent.saveTime.Add(time.Second * time.Duration(cacheent.cc.MaxAge)).Before(time.Now()) { // expired by resp-maxage
		fresh := cacheent.saveTime.Add(time.Second * time.Duration(cacheent.cc.MaxAge))
		if cacheent.cc.MustRevalidate { // must-revalidate
			wantLoad = true
		} else if cacheent.cc.StaleWhileRevalidate != -1 &&
			fresh.Add(time.Second*time.Duration(cacheent.cc.StaleWhileRevalidate)).After(time.Now()) { // stale-while-revalidate
			background = true
		} else {
			wantLoad = true
		}
	}

	var (
		file      *os.File
		openError error
	)
	if !wantLoad {
		file, openError = os.Open(cacheent.path)
	}
	return LoadTypeInfo{
		waiterEntry:          waiter_entry,
		wantNewLoad:          wantLoad && !loading_flag,
		staleWhileRevalidate: background,
		collapsed:            wantLoad && loading_flag,
		returnCache:          !wantLoad,
		cacheEntry:           cacheent,
		cacheFile:            file,
		cacheFileError:       openError,
	}
}

// Request ごとに Waiter Entry を作成し，Request Collapse する場合は entry の channel で待たせる
// Waiter Entry ごと，URI ごとに counter がある
// Waiter Entry Counter: Cache に保存しない Collapse 側通知用の一時ファイルの寿命管理　待ち collapsed requests を数える
// URI Counter: SIEVE Cache で eviction を避ける pin を付ける用　Cache Hit 以外の in-flight requests を数える
// SIEVE Cache, Waiter Count, cache file を操作する場合は Lock が必要

func (c *CacheServer) handleHeadRequest(w http.ResponseWriter, r *http.Request) {

	reference := &url.URL{
		Path:     r.URL.Path,
		RawPath:  r.URL.RawPath,
		RawQuery: r.URL.RawQuery,
	}

	target := c.origin.ResolveReference(reference)
	req, err := http.NewRequest(http.MethodHead, target.String(), nil)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	req.Header.Set("X-NCDN-PoPCache-NodeId", c.nodeId)
	resp, err := c.client.Do(req)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)

}

func (c *CacheServer) handleRangeRequest(w http.ResponseWriter, r *http.Request) {

	reference := &url.URL{
		Path:     r.URL.Path,
		RawPath:  r.URL.RawPath,
		RawQuery: r.URL.RawQuery,
	}

	target := c.origin.ResolveReference(reference)
	req, err := http.NewRequest(http.MethodGet, target.String(), nil)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	req.Header.Set("X-NCDN-PoPCache-NodeId", c.nodeId)
	req.Header.Set("Range", r.Header.Get("Range"))
	resp, err := c.client.Do(req)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (c *CacheServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		slog.Info("Head request")
		c.handleHeadRequest(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Range") != "" {
		slog.Info("range request")
		c.handleRangeRequest(w, r)
		return
	}

	cc := ParseRequestCacheControl(r.Header.Values("Cache-Control"))

	cacheKeyFromURI := r.Host + "\x00" + r.URL.RequestURI()
	c.mu.Lock()
	loadInfo := c.DecideTypeOfLoad(cacheKeyFromURI, &cc)
	c.mu.Unlock()

	if loadInfo.wantNewLoad {
		c.internalNewLoad(cacheKeyFromURI, false, w, r)
	} else if loadInfo.returnCache {
		if loadInfo.cacheFileError != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if !loadInfo.staleWhileRevalidate {
			internalServeCache(loadInfo.cacheEntry, loadInfo.cacheFile, w, "HIT")
		} else {
			message := "STALE-REVALIDATE"
			if !loadInfo.inFlightLoading {
				c.internalNewLoad(cacheKeyFromURI, true, nil, r)
			} else {
				message = "STALE-REVALIDATE-COLLAPSED"
			}
			internalServeCache(loadInfo.cacheEntry, loadInfo.cacheFile, w, message)
		}
		return
	} else if loadInfo.collapsed {
		waiterEntry := loadInfo.waiterEntry

		c.mu.Lock()
		waiterEntry.waiter++
		c.AddWaiterCount(cacheKeyFromURI)
		c.mu.Unlock()

		<-waiterEntry.ch

		c.mu.Lock()
		file, err := os.Open(waiterEntry.resultEntry.path)
		waiterEntry.waiter--
		c.SubWaiterCount(cacheKeyFromURI)
		removeFile := waiterEntry.waiter == 0 && waiterEntry.isTmpFile
		c.mu.Unlock()

		if err == nil {
			internalServeCache(waiterEntry.resultEntry, file, w, "COLLAPSED")
		} else {
			internalServeData(waiterEntry.resultEntry, w, "COLLAPSED")
		}

		if removeFile {
			os.Remove(waiterEntry.resultEntry.path)
		}
	}
}
