package dht

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// PoolStats contains aggregated statistics from all nodes
type PoolStats struct {
	QueriesReceived uint64
	QueriesSent     uint64
	GetPeersCount   uint64
	AnnounceCount   uint64
	TotalNodes      int
	RoutingSize     int
}

// Pool manages multiple DHT nodes for increased coverage
type Pool struct {
	nodes      []*Node
	eventChan  chan Event
	startPort  int
	count      int
	bootstraps []string

	// Aggregated stats
	stats atomic.Pointer[PoolStats]
}

// NewPool creates a new DHT node pool
func NewPool(count int, startPort int, bootstraps []string) (*Pool, error) {
	if count <= 0 {
		return nil, fmt.Errorf("node count must be positive")
	}

	// Generate distributed IDs for better coverage
	ids, err := GenerateDistributedIDs(count)
	if err != nil {
		return nil, fmt.Errorf("failed to generate IDs: %w", err)
	}

	// Create shared event channel with buffer
	eventChan := make(chan Event, count*1000)

	nodes := make([]*Node, count)
	for i := 0; i < count; i++ {
		nodes[i] = NewNode(ids[i], startPort+i, eventChan, bootstraps)
	}

	return &Pool{
		nodes:      nodes,
		eventChan:  eventChan,
		startPort:  startPort,
		count:      count,
		bootstraps: bootstraps,
	}, nil
}

// Start starts all DHT nodes in the pool
func (p *Pool) Start(ctx context.Context) error {
	var wg sync.WaitGroup
	errChan := make(chan error, len(p.nodes))

	log.Printf("Starting %d DHT nodes on ports %d-%d...", p.count, p.startPort, p.startPort+p.count-1)

	for i, node := range p.nodes {
		wg.Add(1)
		go func(idx int, n *Node) {
			defer wg.Done()
			if err := n.Start(ctx); err != nil {
				errChan <- fmt.Errorf("node %d: %w", idx, err)
			}
		}(i, node)
	}

	// Wait for all nodes to start
	wg.Wait()
	close(errChan)

	// Check for errors
	var errs []error
	for err := range errChan {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to start %d nodes: %v", len(errs), errs[0])
	}

	// Start stats collector
	go p.collectStats(ctx)

	log.Printf("All %d DHT nodes started successfully", p.count)
	return nil
}

// Stop stops all DHT nodes
func (p *Pool) Stop() {
	for _, node := range p.nodes {
		node.Stop()
	}
	close(p.eventChan)
}

// Events returns the channel for receiving DHT events
func (p *Pool) Events() <-chan Event {
	return p.eventChan
}

// Stats returns aggregated statistics from all nodes
func (p *Pool) Stats() PoolStats {
	if s := p.stats.Load(); s != nil {
		return *s
	}
	return PoolStats{}
}

// collectStats periodically aggregates stats from all nodes
func (p *Pool) collectStats(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var stats PoolStats
			stats.TotalNodes = len(p.nodes)

			for _, node := range p.nodes {
				recv, sent, getPeers, announces, routing := node.Stats()
				stats.QueriesReceived += recv
				stats.QueriesSent += sent
				stats.GetPeersCount += getPeers
				stats.AnnounceCount += announces
				stats.RoutingSize += routing
			}

			p.stats.Store(&stats)
		}
	}
}

// FindPeers searches for peers across all nodes
func (p *Pool) FindPeers(ctx context.Context, infoHash [20]byte) []CompactPeerInfo {
	var allPeers []CompactPeerInfo
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Query first few nodes
	nodesToQuery := 5
	if len(p.nodes) < nodesToQuery {
		nodesToQuery = len(p.nodes)
	}

	for i := 0; i < nodesToQuery; i++ {
		wg.Add(1)
		go func(node *Node) {
			defer wg.Done()
			peers := node.FindPeers(ctx, infoHash)
			if len(peers) > 0 {
				mu.Lock()
				allPeers = append(allPeers, peers...)
				mu.Unlock()
			}
		}(p.nodes[i])
	}

	wg.Wait()

	// Deduplicate peers
	seen := make(map[string]bool)
	unique := make([]CompactPeerInfo, 0, len(allPeers))
	for _, peer := range allPeers {
		key := fmt.Sprintf("%s:%d", peer.IP, peer.Port)
		if !seen[key] {
			seen[key] = true
			unique = append(unique, peer)
		}
	}

	return unique
}

// NodeCount returns the number of nodes in the pool
func (p *Pool) NodeCount() int {
	return len(p.nodes)
}
