package dht

import (
	"net"
	"sort"
	"sync"
	"time"
)

const (
	// K is the maximum number of nodes per bucket (Kademlia k parameter)
	K = 8
	// NumBuckets is the number of k-buckets (one per bit of 160-bit ID)
	NumBuckets = 160
	// NodeTimeout is the duration after which a node is considered stale
	NodeTimeout = 15 * time.Minute
)

// RoutingNode represents a node in the routing table
type RoutingNode struct {
	ID       NodeID
	IP       net.IP
	Port     int
	LastSeen time.Time
}

// Addr returns the UDP address string
func (n *RoutingNode) Addr() string {
	return net.JoinHostPort(n.IP.String(), string(rune(n.Port)))
}

// UDPAddr returns the net.UDPAddr
func (n *RoutingNode) UDPAddr() *net.UDPAddr {
	return &net.UDPAddr{IP: n.IP, Port: n.Port}
}

// Bucket represents a k-bucket containing up to K nodes
type Bucket struct {
	nodes []*RoutingNode
}

// RoutingTable implements the Kademlia routing table with k-buckets
type RoutingTable struct {
	mu      sync.RWMutex
	localID NodeID
	buckets [NumBuckets]*Bucket
}

// NewRoutingTable creates a new routing table
func NewRoutingTable(localID NodeID) *RoutingTable {
	rt := &RoutingTable{
		localID: localID,
	}
	for i := 0; i < NumBuckets; i++ {
		rt.buckets[i] = &Bucket{
			nodes: make([]*RoutingNode, 0, K),
		}
	}
	return rt
}

// Insert adds or updates a node in the routing table
// Returns true if the node was added/updated
func (rt *RoutingTable) Insert(id NodeID, ip net.IP, port int) bool {
	if id == rt.localID {
		return false // Don't add ourselves
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()

	bucketIdx := BucketIndex(rt.localID, id)
	bucket := rt.buckets[bucketIdx]

	// Check if node already exists
	for i, node := range bucket.nodes {
		if node.ID == id {
			// Update existing node
			bucket.nodes[i].IP = ip
			bucket.nodes[i].Port = port
			bucket.nodes[i].LastSeen = time.Now()
			// Move to end (most recently seen)
			n := bucket.nodes[i]
			bucket.nodes = append(bucket.nodes[:i], bucket.nodes[i+1:]...)
			bucket.nodes = append(bucket.nodes, n)
			return true
		}
	}

	// Add new node
	if len(bucket.nodes) < K {
		bucket.nodes = append(bucket.nodes, &RoutingNode{
			ID:       id,
			IP:       ip,
			Port:     port,
			LastSeen: time.Now(),
		})
		return true
	}

	// Bucket is full - check for stale nodes
	for i, node := range bucket.nodes {
		if time.Since(node.LastSeen) > NodeTimeout {
			// Replace stale node
			bucket.nodes[i] = &RoutingNode{
				ID:       id,
				IP:       ip,
				Port:     port,
				LastSeen: time.Now(),
			}
			return true
		}
	}

	// Bucket is full with fresh nodes - don't add
	return false
}

// Remove removes a node from the routing table
func (rt *RoutingTable) Remove(id NodeID) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	bucketIdx := BucketIndex(rt.localID, id)
	bucket := rt.buckets[bucketIdx]

	for i, node := range bucket.nodes {
		if node.ID == id {
			bucket.nodes = append(bucket.nodes[:i], bucket.nodes[i+1:]...)
			return
		}
	}
}

// FindClosest returns the K closest nodes to the target ID
func (rt *RoutingTable) FindClosest(target NodeID, count int) []*RoutingNode {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	// Collect all nodes
	var allNodes []*RoutingNode
	for _, bucket := range rt.buckets {
		allNodes = append(allNodes, bucket.nodes...)
	}

	// Sort by distance to target
	sort.Slice(allNodes, func(i, j int) bool {
		distI := Distance(allNodes[i].ID, target)
		distJ := Distance(allNodes[j].ID, target)
		return distI.Less(distJ)
	})

	// Return up to count nodes
	if len(allNodes) > count {
		allNodes = allNodes[:count]
	}
	return allNodes
}

// GetNode returns a specific node if it exists in the routing table
func (rt *RoutingTable) GetNode(id NodeID) *RoutingNode {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	bucketIdx := BucketIndex(rt.localID, id)
	bucket := rt.buckets[bucketIdx]

	for _, node := range bucket.nodes {
		if node.ID == id {
			return node
		}
	}
	return nil
}

// RandomFromBucket returns a random node from a specific bucket
func (rt *RoutingTable) RandomFromBucket(bucketIdx int) *RoutingNode {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	if bucketIdx < 0 || bucketIdx >= NumBuckets {
		return nil
	}

	bucket := rt.buckets[bucketIdx]
	if len(bucket.nodes) == 0 {
		return nil
	}

	// Return first node (simple approach)
	return bucket.nodes[0]
}

// Size returns the total number of nodes in the routing table
func (rt *RoutingTable) Size() int {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	count := 0
	for _, bucket := range rt.buckets {
		count += len(bucket.nodes)
	}
	return count
}

// GetAllNodes returns all nodes in the routing table
func (rt *RoutingTable) GetAllNodes() []*RoutingNode {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	var nodes []*RoutingNode
	for _, bucket := range rt.buckets {
		nodes = append(nodes, bucket.nodes...)
	}
	return nodes
}

// BucketSizes returns the number of nodes in each bucket
func (rt *RoutingTable) BucketSizes() []int {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	sizes := make([]int, NumBuckets)
	for i, bucket := range rt.buckets {
		sizes[i] = len(bucket.nodes)
	}
	return sizes
}

// Touch updates the last seen time for a node
func (rt *RoutingTable) Touch(id NodeID) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	bucketIdx := BucketIndex(rt.localID, id)
	bucket := rt.buckets[bucketIdx]

	for _, node := range bucket.nodes {
		if node.ID == id {
			node.LastSeen = time.Now()
			return
		}
	}
}

// GetStaleNodes returns nodes that haven't been seen recently
func (rt *RoutingTable) GetStaleNodes(maxAge time.Duration) []*RoutingNode {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	var stale []*RoutingNode
	cutoff := time.Now().Add(-maxAge)

	for _, bucket := range rt.buckets {
		for _, node := range bucket.nodes {
			if node.LastSeen.Before(cutoff) {
				stale = append(stale, node)
			}
		}
	}
	return stale
}
