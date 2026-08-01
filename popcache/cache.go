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
}
type CacheServer struct {
	origin   *url.URL
	cachemap map[string]Entry
	client   *http.Client
	nodeId   string
	mu       sync.Mutex
}

func NewCacheServer(origin *url.URL, nodeId string) *CacheServer {
	cs := CacheServer{
		origin:   origin,
		cachemap: make(map[string]Entry),
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
		nodeId: nodeId,
	}
	return &cs
}
func (c *CacheServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	v, ok := c.cachemap[r.URL.RequestURI()]
	c.mu.Unlock()
	if ok && v.expire.After(time.Now()) {
		for key, values := range v.header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.Header().Add("X-Cache", "HIT")
		w.WriteHeader(v.statusCode)
		w.Write(v.data)
		return
	}

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

	if resp.StatusCode == http.StatusOK {
		c.mu.Lock()
		c.cachemap[r.URL.RequestURI()] = Entry{
			statusCode: resp.StatusCode,
			header:     resp.Header,
			data:       buf.Bytes(),
			expire:     time.Now().Add(time.Second * 60),
		}
		c.mu.Unlock()
	}
}
