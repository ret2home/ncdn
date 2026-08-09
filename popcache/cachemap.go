package main

import (
	"net/http"
	"os"
	"sync"
	"time"
)

type CacheEntry struct {
	statusCode int
	header     http.Header
	path       string
	size       int64
	saveTime   time.Time
	retired    bool
	counter    int
	cc         *ResponseCacheControl
}
type cacheattr struct {
	accessed bool
}

type SieveCache struct {
	cache           map[string]*CacheEntry
	attr            map[string]*cacheattr
	list            *LinkList
	size            uint32
	maxCacheEntries uint32
	mu              sync.Mutex
}

func NewSieveCache(maxEntries uint32) *SieveCache {
	return &SieveCache{
		cache:           map[string]*CacheEntry{},
		attr:            map[string]*cacheattr{},
		list:            NewList(),
		size:            0,
		maxCacheEntries: maxEntries,
	}
}
func (c *SieveCache) internalGet(key string) (*CacheEntry, bool) {
	v, ok := c.cache[key]
	if ok {
		c.attr[key].accessed = true
		return v, true
	} else {
		return nil, false
	}
}

func (c *SieveCache) insertInternal(key string, ent *CacheEntry) {
	c.cache[key] = ent
	c.attr[key] = &cacheattr{
		accessed: false,
	}
	c.list.InsertFront(key)
	c.size++
}
func (c *SieveCache) evict(e *ListEntry) {
	c.cache[e.val].retired = true
	if c.cache[e.val].counter == 0 {
		os.Remove(c.cache[e.val].path)
	}
	delete(c.cache, e.val)
	delete(c.attr, e.val)
	c.size--
	c.list.Remove(e)
}
func (c *SieveCache) evictOne() {
	for i := 0; i < int(c.size)*2; i++ {
		key := c.list.hand.val
		if !c.attr[key].accessed {
			c.evict(c.list.hand)
			return
		}
		c.attr[key].accessed = false
		c.list.MoveHand()
	}
}
func (c *SieveCache) evictAndInsertInternal(key string, ent *CacheEntry) {
	if c.size < c.maxCacheEntries {
		c.insertInternal(key, ent)
	} else {
		c.evictOne()
		c.insertInternal(key, ent)
	}
}
func (c *SieveCache) Set(key string, ent *CacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, ok := c.cache[key]
	if ok {
		prev.retired = true
		if prev.counter == 0 {
			os.Remove(prev.path)
		}
		c.cache[key] = ent
	} else {
		c.evictAndInsertInternal(key, ent)
	}
}
func (c *SieveCache) Acquire(key string) (*CacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.internalGet(key)
	if ok {
		v.counter++
	}
	return v, ok
}

func (c *SieveCache) Release(entry *CacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry != nil {
		entry.counter--
		if entry.retired && entry.counter == 0 {
			os.Remove(entry.path)
		}
	}
}
