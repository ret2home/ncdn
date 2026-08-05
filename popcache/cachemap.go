package main

import (
	"net/http"
	"os"
	"time"
)

const MAXSIZE = 1024

type CacheEntry struct {
	statusCode int
	header     http.Header
	path       string
	size       uint64
	expire     time.Time
}
type cacheattr struct {
	deleted  bool
	accessed bool
}

type SieveCache struct {
	cache map[string]*CacheEntry
	attr  map[string]*cacheattr
	list  *LinkList
	size  uint32
}

func NewSieveCache() *SieveCache {
	return &SieveCache{
		cache: map[string]*CacheEntry{},
		attr:  map[string]*cacheattr{},
		list:  NewList(),
	}
}
func (c *SieveCache) Get(key string) (*CacheEntry, bool) {
	v, ok := c.cache[key]
	if ok && !c.attr[key].deleted {
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
		deleted:  false,
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
func (c *SieveCache) evictOne() {
	for {
		key := c.list.hand.val
		if c.attr[key].deleted || !c.attr[key].accessed {
			c.evict(c.list.hand)
			return
		}
		c.attr[key].accessed = false
		c.list.MoveHand()
	}
}
func (c *SieveCache) evictAndInsertInternal(key string, ent *CacheEntry) {
	if c.size < MAXSIZE {
		c.insertInternal(key, ent)
	} else {
		c.evictOne()
		c.insertInternal(key, ent)
	}
}
func (c *SieveCache) Set(key string, ent *CacheEntry) {
	_, ok := c.cache[key]
	if ok {
		c.cache[key] = ent
		if c.attr[key].deleted {
			c.attr[key] = &cacheattr{
				accessed: false,
				deleted:  false,
			}
		}
	} else {
		c.evictAndInsertInternal(key, ent)
	}
}

func (c *SieveCache) Delete(key string) {
	c.attr[key].deleted = true
}
