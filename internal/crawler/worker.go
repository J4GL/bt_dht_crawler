package crawler

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"dhtcrawler-go/internal/bt"
	"dhtcrawler-go/internal/dht"
	"dhtcrawler-go/internal/storage"
)

// WorkerPool manages metadata download workers
type WorkerPool struct {
	numWorkers   int
	perWorker    int // Max concurrent downloads per worker
	timeout      time.Duration
	storage      *storage.Storage
	dhtPool      *dht.Pool
	queue        *DownloadQueue
	downloader   *bt.MetadataDownloader

	// Job channel
	jobs chan downloadJob

	// Statistics
	successCount atomic.Uint64
	failCount    atomic.Uint64
	downloading  atomic.Int32
}

type downloadJob struct {
	infoHash string
	peerAddr string // Optional - if empty, use DHT to find peers
}

// NewWorkerPool creates a new worker pool
func NewWorkerPool(numWorkers, perWorker int, timeout time.Duration, storage *storage.Storage, dhtPool *dht.Pool, queue *DownloadQueue) *WorkerPool {
	return &WorkerPool{
		numWorkers: numWorkers,
		perWorker:  perWorker,
		timeout:    timeout,
		storage:    storage,
		dhtPool:    dhtPool,
		queue:      queue,
		downloader: bt.NewMetadataDownloader(timeout),
		jobs:       make(chan downloadJob, numWorkers*perWorker),
	}
}

// Start starts the worker pool
func (p *WorkerPool) Start(ctx context.Context) {
	log.Printf("Starting %d download workers (%d concurrent each)", p.numWorkers, p.perWorker)

	var wg sync.WaitGroup

	// Start workers
	for i := 0; i < p.numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			p.worker(ctx, workerID)
		}(i)
	}

	// Start queue feeder
	go p.feedQueue(ctx)

	// Wait for all workers to finish
	<-ctx.Done()
	close(p.jobs)
	wg.Wait()
}

// feedQueue feeds jobs from the queue to workers
func (p *WorkerPool) feedQueue(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return

		default:
			// Try to get next job from queue
			infoHash, peerAddr, ok := p.queue.Get()
			if !ok {
				time.Sleep(100 * time.Millisecond)
				continue
			}

			// Skip jobs without peer address - we can't download without knowing a peer
			if peerAddr == "" {
				p.queue.Complete(infoHash)
				continue
			}

			// Check if torrent already exists
			exists, err := p.storage.TorrentExists(infoHash)
			if err != nil {
				log.Printf("DB check error: %v", err)
				p.queue.Complete(infoHash)
				continue
			}
			if exists {
				p.queue.Complete(infoHash)
				continue
			}

			// Submit job
			select {
			case p.jobs <- downloadJob{infoHash: infoHash, peerAddr: peerAddr}:
			case <-ctx.Done():
				return
			}
		}
	}
}

// worker processes download jobs
func (p *WorkerPool) worker(ctx context.Context, workerID int) {
	// Each worker can handle multiple concurrent downloads
	sem := make(chan struct{}, p.perWorker)

	for job := range p.jobs {
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}

		go func(j downloadJob) {
			defer func() { <-sem }()
			p.processJob(ctx, j)
		}(job)
	}
}

// processJob downloads metadata for a single hash
func (p *WorkerPool) processJob(ctx context.Context, job downloadJob) {
	defer p.queue.Complete(job.infoHash)

	p.downloading.Add(1)
	defer p.downloading.Add(-1)

	// Convert hex hash to bytes
	hashBytes, err := hex.DecodeString(job.infoHash)
	if err != nil {
		p.failCount.Add(1)
		return
	}

	var infoHash [20]byte
	copy(infoHash[:], hashBytes)

	// Get peer addresses to try
	var peers []string
	if job.peerAddr != "" {
		peers = append(peers, job.peerAddr)
	}

	// Find more peers via DHT if we don't have any
	if len(peers) == 0 {
		dhtPeers := p.dhtPool.FindPeers(ctx, infoHash)
		for _, peer := range dhtPeers {
			peers = append(peers, fmt.Sprintf("%s:%d", peer.IP, peer.Port))
		}
	}

	if len(peers) == 0 {
		// No peers found - this is expected for most hashes
		p.failCount.Add(1)
		return
	}

	// Try each peer until success
	var meta *bt.TorrentMeta
	var lastErr error
	for i, peerAddr := range peers {
		select {
		case <-ctx.Done():
			return
		default:
		}

		downloadCtx, cancel := context.WithTimeout(ctx, p.timeout)
		meta, err = p.downloader.Download(downloadCtx, infoHash, peerAddr)
		cancel()

		if err == nil && meta != nil {
			log.Printf("Downloaded: %s - %s (%d bytes)", job.infoHash[:8], meta.Name, meta.Size)
			break
		}
		lastErr = err
		// Log first few attempts per hash
		if i < 2 && p.downloading.Load() < 10 {
			log.Printf("Download attempt failed for %s from %s: %v", job.infoHash[:8], peerAddr, err)
		}
	}

	if meta == nil {
		if lastErr != nil && p.failCount.Load()%100 == 0 {
			// Log every 100th failure to avoid spam
			log.Printf("Download failed for %s (tried %d peers): %v", job.infoHash[:8], len(peers), lastErr)
		}
		p.failCount.Add(1)
		return
	}

	// Convert files
	files := make([]storage.FileRecord, len(meta.Files))
	for i, f := range meta.Files {
		files[i] = storage.FileRecord{
			Path: f.Path,
			Size: f.Size,
		}
	}

	// Save to database
	record := &storage.TorrentRecord{
		InfoHash:  job.infoHash,
		Name:      meta.Name,
		Size:      meta.Size,
		Files:     files,
		FileCount: len(meta.Files),
		CreatedAt: time.Now(),
		ReqCount:  1,
	}

	if err := p.storage.InsertTorrent(record); err != nil {
		log.Printf("Failed to save torrent: %v", err)
		p.failCount.Add(1)
		return
	}

	// Delete from pending hashes
	p.storage.DeleteHash(job.infoHash)

	p.successCount.Add(1)
}

// Submit submits a new download job
func (p *WorkerPool) Submit(infoHash, peerAddr string) bool {
	return p.queue.Add(infoHash, peerAddr)
}

// Stats returns worker pool statistics
func (p *WorkerPool) Stats() (success, fail uint64, downloading, queued int) {
	return p.successCount.Load(),
		p.failCount.Load(),
		int(p.downloading.Load()),
		p.queue.Size()
}
