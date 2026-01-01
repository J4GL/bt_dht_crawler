package crawler

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"dhtcrawler-go/internal/dht"
	"dhtcrawler-go/internal/storage"
)

// Rate limiter for search requests
var searchLimiter = make(chan struct{}, 10) // Max 10 concurrent searches

func init() {
	for i := 0; i < 10; i++ {
		searchLimiter <- struct{}{}
	}
}

// Config holds crawler configuration
type Config struct {
	// DHT settings
	NodeCount  int      `yaml:"node_count"`
	StartPort  int      `yaml:"start_port"`
	Bootstraps []string `yaml:"bootstrap"`

	// Cache settings
	CacheMaxSize    int           `yaml:"max_size"`
	CacheFlushInterval time.Duration `yaml:"flush_interval"`

	// Download settings
	DownloadWorkers   int           `yaml:"workers"`
	DownloadPerWorker int           `yaml:"per_worker"`
	DownloadTimeout   time.Duration `yaml:"timeout"`

	// Storage
	DBPath string `yaml:"path"`

	// Web
	WebPort    int  `yaml:"port"`
	WebEnabled bool `yaml:"enabled"`
}

// DefaultConfig returns the default configuration
func DefaultConfig() *Config {
	return &Config{
		NodeCount:         50,
		StartPort:         6881,
		Bootstraps:        []string{
			"router.bittorrent.com:6881",
			"dht.transmissionbt.com:6881",
			"router.utorrent.com:6881",
		},
		CacheMaxSize:      1000,
		CacheFlushInterval: 30 * time.Second,
		DownloadWorkers:   4,
		DownloadPerWorker: 100,
		DownloadTimeout:   15 * time.Second, // Shorter timeout to fail fast
		DBPath:            "crawler.db",
		WebPort:           8085,
		WebEnabled:        true,
	}
}

// Stats holds aggregated crawler statistics
type Stats struct {
	// DHT stats
	DHTNodesActive   int
	DHTQueriesRecv   uint64
	DHTQueriesSent   uint64
	DHTGetPeers      uint64
	DHTAnnounces     uint64
	DHTRoutingSize   int

	// Cache stats
	CacheHashesAdded   uint64
	CacheHashesFlushed uint64
	CacheCurrent       int

	// Download stats
	DownloadSuccess    uint64
	DownloadFail       uint64
	DownloadInProgress int
	DownloadQueued     int

	// Storage stats
	TotalHashes   int
	TotalTorrents int

	// Timing
	Uptime time.Duration
}

// Crawler is the main DHT crawler orchestrator
type Crawler struct {
	config     *Config
	dhtPool    *dht.Pool
	hashCache  *HashCache
	queue      *DownloadQueue
	workerPool *WorkerPool
	storage    *storage.Storage

	startTime time.Time

	// Event counters
	getPeersProcessed atomic.Uint64
	announcesProcessed atomic.Uint64
}

// New creates a new crawler
func New(config *Config) (*Crawler, error) {
	// Create storage
	store, err := storage.New(config.DBPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage: %w", err)
	}

	// Create DHT pool
	dhtPool, err := dht.NewPool(config.NodeCount, config.StartPort, config.Bootstraps)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("failed to create DHT pool: %w", err)
	}

	// Create hash cache
	hashCache := NewHashCache(config.CacheMaxSize, store)

	// Create download queue
	queue := NewDownloadQueue(config.DownloadWorkers*config.DownloadPerWorker*2, store)

	// Create worker pool
	workerPool := NewWorkerPool(
		config.DownloadWorkers,
		config.DownloadPerWorker,
		config.DownloadTimeout,
		store,
		dhtPool,
		queue,
	)

	return &Crawler{
		config:     config,
		dhtPool:    dhtPool,
		hashCache:  hashCache,
		queue:      queue,
		workerPool: workerPool,
		storage:    store,
		startTime:  time.Now(),
	}, nil
}

// Run starts the crawler
func (c *Crawler) Run(ctx context.Context) error {
	log.Println("Starting DHT crawler...")

	// Start DHT pool
	if err := c.dhtPool.Start(ctx); err != nil {
		return fmt.Errorf("failed to start DHT pool: %w", err)
	}

	// Start hash cache flusher
	go c.hashCache.StartFlusher(ctx, c.config.CacheFlushInterval)

	// Start worker pool
	go c.workerPool.Start(ctx)

	// Start stats printer
	go c.printStats(ctx)

	// Process DHT events
	log.Println("Processing DHT events...")
	for {
		select {
		case <-ctx.Done():
			log.Println("Shutting down crawler...")
			c.hashCache.Flush()
			c.storage.Close()
			c.dhtPool.Stop()
			return nil

		case event, ok := <-c.dhtPool.Events():
			if !ok {
				return nil
			}
			c.processEvent(event)
		}
	}
}

// processEvent handles a DHT event
func (c *Crawler) processEvent(event dht.Event) {
	infoHash := hex.EncodeToString(event.InfoHash[:])

	switch event.Type {
	case dht.EventGetPeers:
		// Someone is searching for this torrent - this means it's "hot"
		// Key insight: The peer asking might HAVE this torrent (since BT clients
		// search for torrents they're downloading/seeding)
		c.hashCache.Add(infoHash)
		c.getPeersProcessed.Add(1)

		// Try to download from the requester - they likely have it!
		// Submit multiple ports - BT clients often listen on different ports than DHT
		peerIP := event.PeerIP.String()
		// Try source port first, then common BT ports
		c.workerPool.Submit(infoHash, fmt.Sprintf("%s:%d", peerIP, event.PeerPort))
		// Also try common ports if different
		for _, port := range []int{6881, 6882, 51413, 16881} {
			if port != event.PeerPort {
				c.workerPool.Submit(infoHash, fmt.Sprintf("%s:%d", peerIP, port))
			}
		}

		// Also search DHT for more peers (async)
		go c.searchAndDownload(event.InfoHash, infoHash)

	case dht.EventAnnounce:
		// A peer announced they have this torrent - we can download from them!
		c.hashCache.Add(infoHash)
		c.announcesProcessed.Add(1)

		// Add to download queue with peer address
		peerAddr := fmt.Sprintf("%s:%d", event.PeerIP, event.PeerPort)
		c.workerPool.Submit(infoHash, peerAddr)
	}
}

// searchAndDownload actively searches DHT for peers and tries to download
func (c *Crawler) searchAndDownload(infoHash [20]byte, infoHashHex string) {
	// Rate limit searches
	select {
	case <-searchLimiter:
		defer func() { searchLimiter <- struct{}{} }()
	default:
		return // Too many searches in progress
	}

	// Check if we already have this torrent
	exists, _ := c.storage.TorrentExists(infoHashHex)
	if exists {
		return
	}

	// Search DHT for peers who have this torrent
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	peers := c.dhtPool.FindPeers(ctx, infoHash)
	if len(peers) > 0 {
		log.Printf("Found %d peers for %s via DHT search", len(peers), infoHashHex[:8])
	}

	// Submit download jobs for found peers
	submitted := 0
	for _, peer := range peers {
		peerAddr := fmt.Sprintf("%s:%d", peer.IP, peer.Port)
		if c.workerPool.Submit(infoHashHex, peerAddr) {
			submitted++
			if submitted >= 3 { // Try up to 3 peers
				break
			}
		}
	}
}

// Stats returns current crawler statistics
func (c *Crawler) Stats() Stats {
	dhtStats := c.dhtPool.Stats()
	cacheAdded, cacheFlushed, cacheSize := c.hashCache.Stats()
	dlSuccess, dlFail, dlActive, dlQueued := c.workerPool.Stats()
	hashCount, torrentCount, _ := c.storage.Stats()

	return Stats{
		DHTNodesActive:     dhtStats.TotalNodes,
		DHTQueriesRecv:     dhtStats.QueriesReceived,
		DHTQueriesSent:     dhtStats.QueriesSent,
		DHTGetPeers:        dhtStats.GetPeersCount,
		DHTAnnounces:       dhtStats.AnnounceCount,
		DHTRoutingSize:     dhtStats.RoutingSize,

		CacheHashesAdded:   cacheAdded,
		CacheHashesFlushed: cacheFlushed,
		CacheCurrent:       cacheSize,

		DownloadSuccess:    dlSuccess,
		DownloadFail:       dlFail,
		DownloadInProgress: dlActive,
		DownloadQueued:     dlQueued,

		TotalHashes:   hashCount,
		TotalTorrents: torrentCount,

		Uptime: time.Since(c.startTime),
	}
}

// Storage returns the storage instance (for web server)
func (c *Crawler) Storage() *storage.Storage {
	return c.storage
}

// printStats periodically prints statistics
func (c *Crawler) printStats(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stats := c.Stats()
			log.Printf(
				"Stats: DHT[nodes=%d, recv=%d, getPeers=%d, announces=%d] Cache[added=%d] Download[ok=%d, fail=%d, active=%d, queued=%d] DB[hashes=%d, torrents=%d]",
				stats.DHTNodesActive,
				stats.DHTQueriesRecv,
				stats.DHTGetPeers,
				stats.DHTAnnounces,
				stats.CacheHashesAdded,
				stats.DownloadSuccess,
				stats.DownloadFail,
				stats.DownloadInProgress,
				stats.DownloadQueued,
				stats.TotalHashes,
				stats.TotalTorrents,
			)
		}
	}
}
