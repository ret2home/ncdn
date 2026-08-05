package main

import (
	"net/http"
	"time"
)

const MAXSIZE = 1024

type CacheEntry struct {
	statusCode int
	header     http.Header
	data       []byte
	expire     time.Time
}
type cacheattr struct {
	pin      bool
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

func (c *SieveCache) insertInternal(key string, val *CacheEntry) {
	c.cache[key] = val
	c.attr[key] = &cacheattr{
		pin:      false,
		accessed: false,
		deleted:  false,
	}
	c.list.InsertFront(key)
	c.size++
}
func (c *SieveCache) evict(e *ListEntry) {
	delete(c.cache, e.val)
	delete(c.attr, e.val)
	c.size--
	c.list.Remove(e)
}
func (c *SieveCache) evictOne() {
	for {
		key := c.list.hand.val
		if (c.attr[key].deleted || !c.attr[key].accessed) && !c.attr[key].pin {
			c.evict(c.list.hand)
			return
		}
		c.attr[key].accessed = false
		c.list.MoveHand()
	}
}
func (c *SieveCache) evictAndInsertInternal(key string, val *CacheEntry) {
	if c.size < MAXSIZE {
		c.insertInternal(key, val)
	} else {
		c.evictOne()
		c.insertInternal(key, val)
	}
}
func (c *SieveCache) Set(key string, val *CacheEntry) {
	_, ok := c.cache[key]
	if ok {
		c.cache[key] = val
		if c.attr[key].deleted {
			c.attr[key] = &cacheattr{
				pin:      false,
				accessed: false,
				deleted:  false,
			}
		}
	} else {
		c.evictAndInsertInternal(key, val)
	}
}
func (c *SieveCache) SetPin(key string, pin bool) {
	_, ok := c.cache[key]
	if ok {
		c.attr[key].pin = pin
	}
}
func (c *SieveCache) Delete(key string) {
	c.attr[key].deleted = true
	c.attr[key].pin = false
}
