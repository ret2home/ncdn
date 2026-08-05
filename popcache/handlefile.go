package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
)

func (c *CacheServer) SetFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	if _, err := io.Writer.Write(tmp, data); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return nil
}
func (c *CacheServer) URIToFilePath(uri string) string {
	sum := sha256.Sum256([]byte(uri))
	return filepath.Join("/tmp/cache-"+c.nodeId, hex.EncodeToString(sum[:]))
}
