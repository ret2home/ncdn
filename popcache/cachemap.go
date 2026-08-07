package main

import (
	"log/slog"
	"net/http"
	"os"
	"time"
)

type CacheEntry struct {
	statusCode int
	header     http.Header
	path       string
	size       int64
	saveTime   time.Time
	cc         *ResponseCacheControl
}
type cacheattr struct {
	accessed bool
	pin      bool
}

type SieveCache struct {
	cache           map[string]*CacheEntry
	attr            map[string]*cacheattr
	list            *LinkList
	size            uint32
	maxCacheEntries uint32
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
func (c *SieveCache) Get(key string) (*CacheEntry, bool) {
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
		pin:      false,
	}
	c.list.InsertFront(key)
	c.size++
}
func (c *SieveCache) evict(e *ListEntry) {
	os.Remove(c.cache[e.val].path)
	delete(c.cache, e.val)
	delete(c.attr, e.val)
	c.size--
	c.list.Remove(e)
}
func (c *SieveCache) evictOne() bool {
	for i := 0; i < int(c.size)*2; i++ {
		key := c.list.hand.val
		if !c.attr[key].accessed && !c.attr[key].pin {
			c.evict(c.list.hand)
			return true
		}
		if !c.attr[key].pin {
			c.attr[key].accessed = false
		}
		c.list.MoveHand()
	}
	slog.Info("Eviction failed...")
	return false
}
func (c *SieveCache) evictAndInsertInternal(key string, ent *CacheEntry) bool {
	if c.size < c.maxCacheEntries {
		c.insertInternal(key, ent)
		return true
	} else {
		if c.evictOne() {
			c.insertInternal(key, ent)
			return true
		}
		return false
	}
}
func (c *SieveCache) Set(key string, ent *CacheEntry) bool {
	_, ok := c.cache[key]
	if ok {
		c.cache[key] = ent
		return true
	} else {
		return c.evictAndInsertInternal(key, ent)
	}
}
func (c *SieveCache) MakeRoom(key string) bool {
	_, ok := c.cache[key]
	if ok || c.size < c.maxCacheEntries {
		return true
	} else {
		return c.evictOne()
	}
}

func (c *SieveCache) SetPin(key string, pin bool) {
	v, ok := c.attr[key]
	if ok {
		v.pin = pin
	}
}
