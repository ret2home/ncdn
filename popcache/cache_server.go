package main

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type WaiterEntry struct {
	cacheKey             string
	headerDone           chan struct{}
	complete             chan struct{}
	bodyReadFinish       bool
	bodyReadError        error
	mu                   sync.Mutex
	cond                 *sync.Cond
	produced             int64
	header               http.Header
	statusCode           int
	path                 string
	waiter               int
	isTmpFile            bool
	isErrorStale         bool
	errorStaleStatusCode int
	cacheEntry           *CacheEntry // for counter
}
type LoadingCounter struct {
	counter      int
	waiter_entry *WaiterEntry
}
type CacheServer struct {
	origin              *url.URL
	sievecache          SieveCache
	latestWaiterEntries map[string]*WaiterEntry
	client              *http.Client
	nodeId              string
	mu                  sync.Mutex
	maxFileSize         int64
}

func (c *CacheServer) createTargetURL(u *url.URL) *url.URL {
	reference := &url.URL{
		Path:     u.Path,
		RawPath:  u.RawPath,
		RawQuery: u.RawQuery,
	}

	return c.origin.ResolveReference(reference)
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
	close(waiter_entry.complete)
	if c.latestWaiterEntries[waiter_entry.cacheKey] == waiter_entry {
		delete(c.latestWaiterEntries, waiter_entry.cacheKey)
	}
}

func substituteHeaderEtc(waiterEntry *WaiterEntry, statusCode int, header http.Header, path string) {
	waiterEntry.statusCode = statusCode
	waiterEntry.header = header
	waiterEntry.path = path
}
func (c *CacheServer) internalNewRequest(
	waiter_entry *WaiterEntry,
	cacheKey string,
	noStore bool,
	targetURL *url.URL,
	httpMethod string,
	rangeSpec string,
) {
	slog.Info(fmt.Sprintf("New Request! %s", cacheKey))

	req, err := http.NewRequest(httpMethod, targetURL.String(), nil)
	if err != nil {
		substituteHeaderEtc(waiter_entry, http.StatusInternalServerError, make(http.Header), "")
		close(waiter_entry.headerDone)
		waiter_entry.mu.Lock()
		waiter_entry.bodyReadError = err
		waiter_entry.bodyReadFinish = true
		waiter_entry.cond.Broadcast()
		waiter_entry.mu.Unlock()
		c.mu.Lock()
		c.finishLoading(waiter_entry)
		c.mu.Unlock()

		slog.Error(fmt.Sprintf("Internal Server Error %s %v\n", cacheKey, err))
		return
	}

	req.Header.Set("X-NCDN-PoPCache-NodeId", c.nodeId)

	if rangeSpec != "" {
		req.Header.Set("Range", rangeSpec)
	}

	resp, err := c.client.Do(req)

	// STALE IF ERROR
	if err != nil || resp.StatusCode == 500 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504 {
		if err == nil {
			defer resp.Body.Close()
		}
		c.mu.Lock()

		cacheent, cachehit := c.sievecache.Get(cacheKey)

		staleFlag := false
		// for each requests, must check req.cc.Nocache!
		if cachehit && cacheent.cc.StaleIfError != -1 && !cacheent.cc.MustRevalidate && !cacheent.cc.NoCache && cacheent.cc.MaxAge != -1 {
			fresh := cacheent.saveTime.Add(time.Second * time.Duration(cacheent.cc.MaxAge))
			if fresh.Add(time.Second * time.Duration(cacheent.cc.StaleIfError)).After(time.Now()) {
				staleFlag = true
			}
		}

		returnStatusCode := http.StatusBadGateway
		if err == nil {
			returnStatusCode = resp.StatusCode
		}
		if staleFlag {
			waiter_entry.mu.Lock()
			waiter_entry.produced = cacheent.size
			waiter_entry.bodyReadError = nil
			waiter_entry.bodyReadFinish = true
			waiter_entry.cond.Broadcast()
			waiter_entry.cacheEntry = cacheent
			cacheent.counter++
			waiter_entry.mu.Unlock()
			substituteHeaderEtc(waiter_entry, cacheent.statusCode, cacheent.header, cacheent.path)
			waiter_entry.isErrorStale = true
			waiter_entry.errorStaleStatusCode = returnStatusCode
		} else {
			waiter_entry.statusCode = returnStatusCode
			substituteHeaderEtc(waiter_entry, returnStatusCode, make(http.Header), "")
		}
		close(waiter_entry.headerDone)

		waiter_entry.mu.Lock()
		waiter_entry.bodyReadFinish = true
		waiter_entry.cond.Broadcast()
		waiter_entry.mu.Unlock()
		c.finishLoading(waiter_entry)

		c.mu.Unlock()

		slog.Error(fmt.Sprintf("Origin Error %s %v\n", cacheKey, err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		slog.Error(fmt.Sprintf("status %s %d\n", cacheKey, resp.StatusCode))
	}

	resp_cc := ParseResponseCacheControl(resp.Header.Values("Cache-Control"))

	var (
		tmpfile   *os.File
		cachePath string
	)

	// リクエストとキャッシュに書き込む
	tmpfile, err = os.CreateTemp("/tmp/cache-"+c.nodeId, ".cache-*")
	if err == nil {
		cachePath = tmpfile.Name()
	} else {
		substituteHeaderEtc(waiter_entry, http.StatusInternalServerError, make(http.Header), "")
		close(waiter_entry.headerDone)
		waiter_entry.mu.Lock()
		waiter_entry.bodyReadError = err
		waiter_entry.bodyReadFinish = true
		waiter_entry.cond.Broadcast()
		waiter_entry.mu.Unlock()
		c.mu.Lock()
		c.finishLoading(waiter_entry)
		c.mu.Unlock()
		return
	}

	substituteHeaderEtc(waiter_entry, resp.StatusCode, resp.Header, cachePath)
	close(waiter_entry.headerDone)

	buf := make([]byte, 64*1024)
	var copyErr error
	var totalWritten int64

	for {
		rn, err := resp.Body.Read(buf)
		if rn > 0 {
			written := 0
			for {
				wn, werr := tmpfile.Write(buf[written:rn])
				if werr != nil {
					copyErr = werr
					break
				}
				written += wn
				if written == rn {
					break
				}
				if wn == 0 {
					copyErr = io.ErrNoProgress
					break
				}
			}
			totalWritten += int64(written)
			if copyErr != nil {
				break
			}
			waiter_entry.mu.Lock()
			waiter_entry.produced += int64(rn)
			waiter_entry.cond.Broadcast()
			waiter_entry.mu.Unlock()
		}
		if err != nil {
			if err != io.EOF {
				copyErr = err
			}
			break
		}
	}
	closeOK := false
	if tmpfile != nil {
		closeOK = tmpfile.Close() == nil
	}

	waiter_entry.mu.Lock()
	waiter_entry.bodyReadFinish = true
	waiter_entry.bodyReadError = copyErr
	waiter_entry.cond.Broadcast()
	waiter_entry.mu.Unlock()

	var (
		committed  bool
		removePath string
	)

	c.mu.Lock()

	if copyErr == nil && closeOK && cachePath != "" {

		// 事前に eviction しておく

		cacheable := (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent) && !resp_cc.NoStore && !noStore && resp_cc.MaxAge != -1 &&
			totalWritten < c.maxFileSize && c.sievecache.MakeRoom(cacheKey)

		if cacheable {
			result := &CacheEntry{
				statusCode: resp.StatusCode,
				header:     resp.Header.Clone(),
				path:       cachePath,
				size:       totalWritten,
				saveTime:   time.Now(),
				cc:         &resp_cc,
				retired:    false,
				counter:    0,
			}
			if c.sievecache.Set(cacheKey, result) { // shoule be always ok
				committed = true
				if waiter_entry.waiter > 0 {
					waiter_entry.cacheEntry = result
					result.counter++
				}
			}
		}

		if !committed {
			waiter_entry.isTmpFile = true
			if waiter_entry.waiter == 0 {
				removePath = cachePath
			}
		}
	} else {
		waiter_entry.isTmpFile = true
		if waiter_entry.waiter == 0 {
			removePath = cachePath
		}
	}

	switch {
	case copyErr != nil:
		{
			slog.Error(fmt.Sprintf("Copy Error: %s %v", cacheKey, copyErr))
		}

	case resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent:
		{
			slog.Error(fmt.Sprintf("Error: status %s %d", cacheKey, resp.StatusCode))
		}
	}

	c.finishLoading(waiter_entry)
	c.mu.Unlock()

	if removePath != "" {
		_ = os.Remove(removePath)
	}
}

func internalServeOnlyStatusCode(header http.Header, statusCode int, w http.ResponseWriter, XCache string) {
	for key, values := range header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-Cache", XCache)
	w.WriteHeader(statusCode)
	w.Write([]byte(http.StatusText(statusCode)))
}

type LoadTypeInfo struct {
	waiterEntry          *WaiterEntry
	wantNewLoad          bool
	returnCache          bool
	collapsed            bool
	staleWhileRevalidate bool
	inFlightLoading      bool
	cacheEntry           *CacheEntry
}

func (c *CacheServer) DecideTypeOfLoad(cacheKey string, cc *RequestCacheControl) LoadTypeInfo {
	cacheent, cachehit := c.sievecache.Get(cacheKey)
	waiter_entry, loading_flag := c.latestWaiterEntries[cacheKey]

	wantLoad := false
	background := false
	if !cachehit || cacheent.cc.NoCache || cc.NoCache || cacheent.cc.MaxAge == -1 { // Cache Miss, req/resp-no-cache, no resp-maxage
		wantLoad = true
	} else if cc.MaxAge != -1 && cacheent.saveTime.Before(time.Now().Add(-time.Second*time.Duration(cc.MaxAge))) { // expired by req-maxage
		wantLoad = true
	} else if cacheent.saveTime.Add(time.Second * time.Duration(cacheent.cc.MaxAge)).Before(time.Now()) { // expired by resp-maxage
		fresh := cacheent.saveTime.Add(time.Second * time.Duration(cacheent.cc.MaxAge))
		if cacheent.cc.MustRevalidate { // resp-must-revalidate
			wantLoad = true
		} else if cacheent.cc.StaleWhileRevalidate != -1 &&
			fresh.Add(time.Second*time.Duration(cacheent.cc.StaleWhileRevalidate)).After(time.Now()) { // resp-stale-while-revalidate
			background = true
		} else {
			wantLoad = true
		}
	}

	return LoadTypeInfo{
		waiterEntry:          waiter_entry,
		wantNewLoad:          wantLoad && !loading_flag,
		staleWhileRevalidate: background,
		collapsed:            wantLoad && loading_flag,
		returnCache:          !wantLoad,
		inFlightLoading:      loading_flag,
		cacheEntry:           cacheent,
	}
}

// Request ごとに Waiter Entry を作成し，Request Collapse する場合は entry の channel で待たせる
// Waiter Entry ごと，cache Entry ごとに counter がある
// Waiter Entry Counter: Cache に保存しない Collapse 側通知用の一時ファイルの寿命管理　Waiter で待っているリクエストごとにカウント
// Cache Entry Counter: Cache に保存したファイルの寿命管理　CacheEntry を見ている Waiter ごとにカウント，Waiter Counter が 0 になったら下げる
// SIEVE Cache, Waiter Count, cache file を操作する場合は Lock が必要

func newWaiterEntry(cacheKey string, waiter int) *WaiterEntry {
	w := &WaiterEntry{
		cacheKey:       cacheKey,
		headerDone:     make(chan struct{}),
		complete:       make(chan struct{}),
		waiter:         waiter,
		bodyReadFinish: false,
		bodyReadError:  nil,
	}
	w.cond = sync.NewCond(&w.mu)
	return w
}
func (c *CacheServer) createWaiter(cacheKey string, cc *RequestCacheControl, targetURL *url.URL, httpMethod string, rangeSpec string) (*WaiterEntry, string) {

	var waiterEntry *WaiterEntry
	var xCacheMessage string

	c.mu.Lock()
	loadInfo := c.DecideTypeOfLoad(cacheKey, cc)

	if loadInfo.wantNewLoad {
		xCacheMessage = "MISS"
		waiterEntry = newWaiterEntry(cacheKey, 1)

		c.latestWaiterEntries[cacheKey] = waiterEntry
		c.mu.Unlock()

		go c.internalNewRequest(waiterEntry, cacheKey, cc.NoStore, targetURL, httpMethod, rangeSpec)

	} else if loadInfo.returnCache {
		xCacheMessage = "HIT"

		waiterEntry = newWaiterEntry(cacheKey, 1)
		// pseudo-waiter

		loadInfo.cacheEntry.counter++
		if !loadInfo.staleWhileRevalidate {
			c.mu.Unlock()
		} else {
			xCacheMessage = "STALE-REVALIDATE"
			if !loadInfo.inFlightLoading {

				backgroundWaiterEntry := newWaiterEntry(cacheKey, 0)

				c.latestWaiterEntries[cacheKey] = backgroundWaiterEntry
				c.mu.Unlock()
				go c.internalNewRequest(backgroundWaiterEntry, cacheKey, cc.NoStore, targetURL, httpMethod, rangeSpec)

			} else {
				c.mu.Unlock()
				xCacheMessage = "STALE-REVALIDATE-COLLAPSED"
			}
		}
		waiterEntry.cacheEntry = loadInfo.cacheEntry
		substituteHeaderEtc(waiterEntry, loadInfo.cacheEntry.statusCode, loadInfo.cacheEntry.header, loadInfo.cacheEntry.path)
		waiterEntry.produced = loadInfo.cacheEntry.size
		waiterEntry.bodyReadFinish = true
		waiterEntry.bodyReadError = nil
		close(waiterEntry.headerDone)
		close(waiterEntry.complete)
	} else if loadInfo.collapsed {
		xCacheMessage = "COLLAPSED"
		waiterEntry = loadInfo.waiterEntry

		waiterEntry.waiter++
		c.mu.Unlock()
	}
	return waiterEntry, xCacheMessage
}

// [start,end)
func copyFlightRange(w http.ResponseWriter, file *os.File, we *WaiterEntry, start int64, end int64) error {
	pos := start

	for pos < end {
		we.mu.Lock()

		for we.produced <= pos && !we.bodyReadFinish {
			we.cond.Wait()
		}

		produced := we.produced
		finished := we.bodyReadFinish
		bodyErr := we.bodyReadError

		we.mu.Unlock()

		readEnd := min(produced, end)

		if pos < readEnd {
			section := io.NewSectionReader(
				file,
				pos,
				readEnd-pos,
			)

			if _, err := io.Copy(w, section); err != nil {
				return err
			}

			pos = readEnd
		}

		if pos >= end {
			return nil
		}

		if finished {
			if bodyErr != nil {
				return bodyErr
			}
			return io.ErrUnexpectedEOF
		}
	}

	return nil
}
func copyFlightToEOF(w http.ResponseWriter, file *os.File, we *WaiterEntry) error {
	var pos int64

	for {
		we.mu.Lock()

		for pos >= we.produced && !we.bodyReadFinish {
			we.cond.Wait()
		}

		produced := we.produced
		finished := we.bodyReadFinish
		bodyErr := we.bodyReadError

		we.mu.Unlock()

		if pos < produced {
			section := io.NewSectionReader(file, pos, produced-pos)

			if _, err := io.Copy(w, section); err != nil {
				return err
			}
			pos = produced
		}

		if finished {
			return bodyErr
		}
	}
}
func (c *CacheServer) handleNonRangeRequest(w http.ResponseWriter, r *http.Request) {
	cc := ParseRequestCacheControl(r.Header.Values("Cache-Control"))

	cacheKeyFromURI := r.Method + "\x00" + r.Host + "\x00" + r.URL.RequestURI()

	waiterEntry, xCacheMessage := c.createWaiter(cacheKeyFromURI, &cc, c.createTargetURL(r.URL), r.Method, "")

	<-waiterEntry.headerDone

	file, err := os.Open(waiterEntry.path)

	if err == nil {
		defer file.Close()
	}

	if waiterEntry.isErrorStale && cc.NoCache {
		http.Error(w, http.StatusText(waiterEntry.errorStaleStatusCode), waiterEntry.errorStaleStatusCode)
	} else {
		if waiterEntry.isErrorStale {
			xCacheMessage = xCacheMessage + "-ERROR-STALE" // MISS-ERROR-STALE, COLLAPSED-ERROR-STALE
		}
		if err == nil {
			for key, values := range waiterEntry.header {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			w.Header().Set("X-Cache", xCacheMessage)
			w.WriteHeader(waiterEntry.statusCode)
			copyFlightToEOF(w, file, waiterEntry)
		} else if waiterEntry.path == "" {
			internalServeOnlyStatusCode(waiterEntry.header, waiterEntry.statusCode, w, xCacheMessage)
		} else {
			http.Error(w, "Cache file unavailable", http.StatusInternalServerError)
		}
	}

	<-waiterEntry.complete

	c.mu.Lock()
	waiterEntry.waiter--
	if waiterEntry.waiter == 0 && waiterEntry.cacheEntry != nil {
		waiterEntry.cacheEntry.counter--
	}
	removeTmpFile := waiterEntry.waiter == 0 && waiterEntry.isTmpFile
	removeOldCacheFile := waiterEntry.cacheEntry != nil && waiterEntry.cacheEntry.retired && waiterEntry.cacheEntry.counter == 0
	c.mu.Unlock()
	if removeTmpFile {
		os.Remove(waiterEntry.path)
	}
	if removeOldCacheFile {
		os.Remove(waiterEntry.cacheEntry.path)
	}
}

func (c *CacheServer) handleSingleRangeRequest(w http.ResponseWriter, r *http.Request) {
	cc := ParseRequestCacheControl(r.Header.Values("Cache-Control"))

	cacheKeyFromURIAndHead := "HEAD" + "\x00" + r.Host + "\x00" + r.URL.RequestURI()

	var xTotalCacheMessage string
	headWaiterEntry, xHeadCacheMessage := c.createWaiter(cacheKeyFromURIAndHead, &cc, c.createTargetURL(r.URL), "HEAD", "")

	<-headWaiterEntry.complete

	c.mu.Lock()
	headWaiterEntry.waiter--
	if headWaiterEntry.waiter == 0 && headWaiterEntry.cacheEntry != nil {
		headWaiterEntry.cacheEntry.counter--
	}
	removeTmpFile := headWaiterEntry.waiter == 0 && headWaiterEntry.isTmpFile
	removeOldCacheFile := headWaiterEntry.cacheEntry != nil && headWaiterEntry.cacheEntry.retired && headWaiterEntry.cacheEntry.counter == 0

	c.mu.Unlock()

	if removeTmpFile {
		os.Remove(headWaiterEntry.path) // not required but...
	}
	if removeOldCacheFile {
		os.Remove(headWaiterEntry.cacheEntry.path)
	}

	if headWaiterEntry.isErrorStale && cc.NoCache {
		http.Error(w, http.StatusText(headWaiterEntry.errorStaleStatusCode), headWaiterEntry.errorStaleStatusCode)
		return
	} else {
		if headWaiterEntry.isErrorStale {
			xHeadCacheMessage = xHeadCacheMessage + "-ERROR-STALE" // MISS-ERROR-STALE, COLLAPSED-ERROR-STALE
		}
		if headWaiterEntry.statusCode != http.StatusOK {
			internalServeOnlyStatusCode(headWaiterEntry.header, headWaiterEntry.statusCode, w, "HEAD: "+xHeadCacheMessage)
			return
		}
	}

	xTotalCacheMessage = "HEAD: " + xHeadCacheMessage

	contentLength, _ := strconv.Atoi(headWaiterEntry.header.Get("Content-Length")) // ignore error fixme!
	rangeSpec := r.Header.Get("Range")
	byteRange, err := ParseSingleRange(rangeSpec, contentLength)
	if err != nil {
		slog.Error(fmt.Sprintf("BadRequest, %v %v", cacheKeyFromURIAndHead, err))
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	chunks := CalcChunks(byteRange, contentLength)

	waiterEntries := make([]*WaiterEntry, len(chunks))
	xCacheMessages := make([]string, len(chunks))
	cacheKeyFromURIAndChunkIDs := make([]string, len(chunks))

	for key, values := range headWaiterEntry.header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	w.Header().Set("Content-Length", strconv.Itoa(byteRange.end-byteRange.start+1))
	w.Header().Set("content-range", "bytes "+strconv.Itoa(byteRange.start)+"-"+strconv.Itoa(byteRange.end)+"/"+strconv.Itoa(contentLength))

	w.Header().Set("X-Cache", xTotalCacheMessage)
	w.WriteHeader(http.StatusPartialContent)

	issue := func(idx int) {
		cacheKeyFromURIAndChunkIDs[idx] = "GET" + "\x00" + r.Host + "\x00" + r.URL.RequestURI() + "\x00" + strconv.Itoa(chunks[idx].start)
		chunkSpecStr := "bytes=" + strconv.Itoa(chunks[idx].start) + "-" + strconv.Itoa(chunks[idx].end)
		waiterEntries[idx], xCacheMessages[idx] = c.createWaiter(cacheKeyFromURIAndChunkIDs[idx], &cc, c.createTargetURL(r.URL), "GET", chunkSpecStr)
	}

	const WINDOWSIZE = 10

	cleanedUp := make([]bool, len(chunks))

	cleaner := func(i int, file *os.File) {
		if cleanedUp[i] {
			return
		}
		cleanedUp[i] = true

		<-waiterEntries[i].complete

		c.mu.Lock()
		waiterEntries[i].waiter--
		if waiterEntries[i].waiter == 0 && waiterEntries[i].cacheEntry != nil {
			waiterEntries[i].cacheEntry.counter--
		}
		removeTmpFile := waiterEntries[i].waiter == 0 && waiterEntries[i].isTmpFile
		removeOldCacheFile := waiterEntries[i].cacheEntry != nil && waiterEntries[i].cacheEntry.retired && waiterEntries[i].cacheEntry.counter == 0

		if file != nil {
			file.Close()
		}
		if removeTmpFile {
			os.Remove(waiterEntries[i].path)
		}
		if removeOldCacheFile {
			os.Remove(waiterEntries[i].cacheEntry.path)
		}

		c.mu.Unlock()
	}

	nextIssue := 0
	for ; nextIssue < min(WINDOWSIZE, len(chunks)); nextIssue++ {
		issue(nextIssue)
	}

	defer func() {
		for i := 0; i < nextIssue; i++ {
			cleaner(i, nil)
		}
	}()
	for i := range chunks {
		<-waiterEntries[i].headerDone

		file, fileError := os.Open(waiterEntries[i].path)

		if waiterEntries[i].isErrorStale && cc.NoCache {
			cleaner(i, file)
			return
		} else {
			if waiterEntries[i].isErrorStale {
				xCacheMessages[i] = xCacheMessages[i] + "-ERROR-STALE" // MISS-ERROR-STALE, COLLAPSED-ERROR-STALE
			}

			if fileError != nil || waiterEntries[i].statusCode != http.StatusPartialContent {
				cleaner(i, file)
				return
			}
		}
		xTotalCacheMessage = xTotalCacheMessage + ";" + xCacheMessages[i]

		realStart := max(chunks[i].start, byteRange.start)
		realEnd := min(chunks[i].end, byteRange.end) + 1 // exclusive
		localStart := int64(realStart - chunks[i].start)
		localEnd := int64(realEnd - chunks[i].start)

		if err := copyFlightRange(w, file, waiterEntries[i], int64(localStart), int64(localEnd)); err != nil {
			cleaner(i, file)
			return
		}
		cleaner(i, file)

		if nextIssue < len(chunks) {
			issue(nextIssue)
			nextIssue++
		}
	}
}
func (c *CacheServer) handleMultiRangeRequest(w http.ResponseWriter, r *http.Request) {

	target := c.createTargetURL(r.URL)
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
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	rangeSpec := r.Header.Get("Range")
	if r.Method == http.MethodGet && rangeSpec != "" {
		if strings.Contains(rangeSpec, ",") {
			c.handleMultiRangeRequest(w, r)
		} else {
			c.handleSingleRangeRequest(w, r)
		}
	} else {
		c.handleNonRangeRequest(w, r)
	}
}
