package main

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type Entry struct {
	statusCode int
	header     http.Header
	data       []byte
	expire     time.Time
	finished   bool
	done       chan struct{}
}
type CacheServer struct {
	origin   *url.URL
	cachemap map[string]*Entry
	client   *http.Client
	nodeId   string
	mu       sync.RWMutex
}

func NewCacheServer(origin *url.URL, nodeId string) *CacheServer {
	cs := CacheServer{
		origin:   origin,
		cachemap: make(map[string]*Entry),
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
		nodeId: nodeId,
	}
	return &cs
}

func (c *CacheServer) internalNewRequest(w http.ResponseWriter, r *http.Request) {
	v := c.cachemap[r.URL.RequestURI()]

	defer func() {
		v.finished = true
		close(v.done)
	}()

	target := c.origin.ResolveReference(r.URL)
	req, err := http.NewRequest("GET", target.String(), nil)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	req.Header.Set("X-NCDN-PoPCache-NodeId", c.nodeId)
	resp, err := c.client.Do(req)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
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
		return
	}

	c.mu.Lock()
	if resp.StatusCode == http.StatusOK {
		v.statusCode = resp.StatusCode
		v.header = resp.Header
		v.data = buf.Bytes()
		v.expire = time.Now().Add(time.Second * 5)
		v.finished = true
	}
	c.mu.Unlock()
}

func internalServeCache(v *Entry, w http.ResponseWriter, XCache string) {
	for key, values := range v.header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Add("X-Cache", XCache)
	w.WriteHeader(v.statusCode)
	w.Write(v.data)
}
func (c *CacheServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var useExistCache bool
	var waitLeader bool

	c.mu.Lock()
	v, ok := c.cachemap[r.URL.RequestURI()]

	useExistCache = ok && v.finished && v.expire.After(time.Now())
	waitLeader = ok && !v.finished

	if !useExistCache && !waitLeader {
		c.cachemap[r.URL.RequestURI()] = &Entry{
			finished: false,
			done:     make(chan struct{}),
		}
	}
	c.mu.Unlock()

	if useExistCache {
		internalServeCache(v, w, "HIT")
		return
	}

	if waitLeader {
		for {
			<-v.done
			c.mu.RLock()
			if v.finished {
				c.mu.RUnlock()
				break
			}
			c.mu.RUnlock()
		}
		internalServeCache(v, w, "COLLAPSED")
		return
	}
	c.internalNewRequest(w, r)
}
