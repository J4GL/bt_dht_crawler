// Package crawler implements the main crawling logic
package crawler

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"dhtcrawler-go/internal/storage"
)

// HashCache provides in-memory caching and batching for discovered hashes
// This is a key optimization from dhtcrawler2 - it collapses duplicate
// hash discoveries into counts and batch-inserts to reduce DB load
type HashCache struct {
	mu      sync.RWMutex
	items   map[string]*cacheItem
	maxSize int
	storage *storage.Storage

	// Statistics
	totalAdded   atomic.Uint64
	totalFlushed atomic.Uint64
}

type cacheItem struct {
	count     int
	firstSeen time.Time
}

// NewHashCache creates a new hash cache
func NewHashCache(maxSize int, storage *storage.Storage) *HashCache {
	return &HashCache{
		items:   make(map[string]*cacheItem),
		maxSize: maxSize,
		storage: storage,
	}
}

// Add adds or increments a hash in the cache
func (c *HashCache) Add(infoHash string) {
	c.mu.Lock()

	if item, exists := c.items[infoHash]; exists {
		item.count++
	} else {
		c.items[infoHash] = &cacheItem{
			count:     1,
			firstSeen: time.Now(),
		}
	}

	shouldFlush := len(c.items) >= c.maxSize
	c.mu.Unlock()

	c.totalAdded.Add(1)

	if shouldFlush {
		c.Flush()
	}
}

// Flush writes all cached hashes to the database
func (c *HashCache) Flush() error {
	c.mu.Lock()
	if len(c.items) == 0 {
		c.mu.Unlock()
		return nil
	}

	// Take ownership of current items
	items := c.items
	c.items = make(map[string]*cacheItem)
	c.mu.Unlock()

	// Convert to map for storage
	hashes := make(map[string]int, len(items))
	for hash, item := range items {
		hashes[hash] = item.count
	}

	c.totalFlushed.Add(uint64(len(hashes)))

	if err := c.storage.InsertHashes(hashes); err != nil {
		log.Printf("Failed to flush hash cache: %v", err)
		// Put items back in cache on error
		c.mu.Lock()
		for hash, item := range items {
			if existing, exists := c.items[hash]; exists {
				existing.count += item.count
			} else {
				c.items[hash] = item
			}
		}
		c.mu.Unlock()
		return err
	}

	return nil
}

// Size returns the current number of cached hashes
func (c *HashCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// Stats returns cache statistics
func (c *HashCache) Stats() (added, flushed uint64, currentSize int) {
	c.mu.RLock()
	currentSize = len(c.items)
	c.mu.RUnlock()
	return c.totalAdded.Load(), c.totalFlushed.Load(), currentSize
}

// StartFlusher starts a background goroutine that periodically flushes the cache
func (c *HashCache) StartFlusher(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Final flush on shutdown
			c.Flush()
			return
		case <-ticker.C:
			if err := c.Flush(); err != nil {
				log.Printf("Periodic flush failed: %v", err)
			}
		}
	}
}

// DownloadQueue manages the queue of hashes waiting to be downloaded
type DownloadQueue struct {
	mu       sync.Mutex
	items    []downloadItem
	maxSize  int
	storage  *storage.Storage
	inFlight sync.Map // Track in-progress downloads
}

type downloadItem struct {
	infoHash string
	peerAddr string // Optional peer address from announce
	reqCount int
	addedAt  time.Time
}

// NewDownloadQueue creates a new download queue
func NewDownloadQueue(maxSize int, storage *storage.Storage) *DownloadQueue {
	return &DownloadQueue{
		items:   make([]downloadItem, 0, maxSize),
		maxSize: maxSize,
		storage: storage,
	}
}

// Add adds a hash to the download queue
func (q *DownloadQueue) Add(infoHash, peerAddr string) bool {
	// Check if already in flight
	if _, loaded := q.inFlight.LoadOrStore(infoHash, true); loaded {
		return false
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	// Check if queue is full
	if len(q.items) >= q.maxSize {
		q.inFlight.Delete(infoHash)
		return false
	}

	q.items = append(q.items, downloadItem{
		infoHash: infoHash,
		peerAddr: peerAddr,
		addedAt:  time.Now(),
	})

	return true
}

// Get retrieves the next item from the queue
func (q *DownloadQueue) Get() (string, string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.items) == 0 {
		return "", "", false
	}

	item := q.items[0]
	q.items = q.items[1:]

	return item.infoHash, item.peerAddr, true
}

// Complete marks a download as complete (success or failure)
func (q *DownloadQueue) Complete(infoHash string) {
	q.inFlight.Delete(infoHash)
}

// Size returns the current queue size
func (q *DownloadQueue) Size() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Refill loads more hashes from the database into the queue
func (q *DownloadQueue) Refill(count int) error {
	hashes, err := q.storage.GetPendingHashes(count)
	if err != nil {
		return err
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	for _, h := range hashes {
		// Skip if already in flight
		if _, loaded := q.inFlight.LoadOrStore(h.InfoHash, true); loaded {
			continue
		}

		// Skip if queue is full
		if len(q.items) >= q.maxSize {
			q.inFlight.Delete(h.InfoHash)
			break
		}

		q.items = append(q.items, downloadItem{
			infoHash: h.InfoHash,
			reqCount: h.ReqCount,
			addedAt:  time.Now(),
		})
	}

	return nil
}

// InFlightCount returns the number of in-progress downloads
func (q *DownloadQueue) InFlightCount() int {
	count := 0
	q.inFlight.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	return count
}
