package dht

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// UDPBufferSize is the maximum UDP packet size
	UDPBufferSize = 65535
	// DefaultTokenExpiry is how long tokens are valid
	DefaultTokenExpiry = 10 * time.Minute
	// RefreshInterval is how often to refresh the routing table
	RefreshInterval = 15 * time.Minute
	// BootstrapInterval is how often to bootstrap if routing table is small
	BootstrapInterval = 30 * time.Second
)

// EventType represents the type of DHT event
type EventType int

const (
	EventGetPeers EventType = iota
	EventAnnounce
)

// Event represents a discovered info hash event
type Event struct {
	Type     EventType
	InfoHash [20]byte
	PeerIP   net.IP
	PeerPort int
}

// Node represents a single DHT node
type Node struct {
	ID           NodeID
	Port         int
	conn         *net.UDPConn
	routingTable *RoutingTable
	eventChan    chan<- Event
	bootstraps   []string
	tokenSecret  []byte
	lastSecret   []byte
	secretMu     sync.Mutex

	// Statistics
	queriesReceived atomic.Uint64
	queriesSent     atomic.Uint64
	getPeersCount   atomic.Uint64
	announceCount   atomic.Uint64

	// Transaction tracking
	pendingMu    sync.Mutex
	pendingTxns  map[string]chan *Message
	txnIDCounter uint16
}

// NewNode creates a new DHT node
func NewNode(id NodeID, port int, eventChan chan<- Event, bootstraps []string) *Node {
	secret := make([]byte, 16)
	rand.Read(secret)

	return &Node{
		ID:          id,
		Port:        port,
		eventChan:   eventChan,
		bootstraps:  bootstraps,
		tokenSecret: secret,
		lastSecret:  secret,
		pendingTxns: make(map[string]chan *Message),
	}
}

// Start starts the DHT node
func (n *Node) Start(ctx context.Context) error {
	// Bind UDP socket
	addr := &net.UDPAddr{Port: n.Port}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return fmt.Errorf("failed to bind UDP port %d: %w", n.Port, err)
	}
	n.conn = conn
	n.routingTable = NewRoutingTable(n.ID)

	// Start background goroutines
	go n.readLoop(ctx)
	go n.refreshLoop(ctx)
	go n.rotateSecrets(ctx)

	// Bootstrap
	if err := n.Bootstrap(ctx); err != nil {
		log.Printf("[Node %d] Bootstrap warning: %v", n.Port, err)
	}

	return nil
}

// Stop stops the DHT node
func (n *Node) Stop() {
	if n.conn != nil {
		n.conn.Close()
	}
}

// Bootstrap connects to bootstrap nodes to populate the routing table
func (n *Node) Bootstrap(ctx context.Context) error {
	// Query all bootstrap nodes
	for _, addr := range n.bootstraps {
		udpAddr, err := net.ResolveUDPAddr("udp4", addr)
		if err != nil {
			continue
		}

		// Send find_node for our own ID to find nearby nodes
		tid := n.nextTxnID()
		msg, _ := EncodeFindNode(tid, n.ID, n.ID)
		n.conn.WriteToUDP(msg, udpAddr)
		n.queriesSent.Add(1)

		// Also send find_node for random IDs to discover more of the network
		for i := 0; i < 3; i++ {
			var randomID NodeID
			rand.Read(randomID[:])
			tid := n.nextTxnID()
			msg, _ := EncodeFindNode(tid, n.ID, randomID)
			n.conn.WriteToUDP(msg, udpAddr)
			n.queriesSent.Add(1)
		}
	}

	// Give bootstrap responses time to arrive
	time.Sleep(3 * time.Second)

	// Iteratively query nodes we discover
	for round := 0; round < 3; round++ {
		closest := n.routingTable.FindClosest(n.ID, K*2)
		for _, node := range closest {
			// Find nodes close to us
			tid := n.nextTxnID()
			msg, _ := EncodeFindNode(tid, n.ID, n.ID)
			n.conn.WriteToUDP(msg, node.UDPAddr())
			n.queriesSent.Add(1)

			// Also find random nodes
			var randomID NodeID
			rand.Read(randomID[:])
			tid = n.nextTxnID()
			msg, _ = EncodeFindNode(tid, n.ID, randomID)
			n.conn.WriteToUDP(msg, node.UDPAddr())
			n.queriesSent.Add(1)
		}
		time.Sleep(1 * time.Second)
	}

	return nil
}

// readLoop continuously reads and processes UDP messages
func (n *Node) readLoop(ctx context.Context) {
	buf := make([]byte, UDPBufferSize)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		size, addr, err := n.conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			continue
		}

		// Process message in goroutine to not block reading
		data := make([]byte, size)
		copy(data, buf[:size])
		go n.handleMessage(data, addr)
	}
}

// handleMessage processes a single KRPC message
func (n *Node) handleMessage(data []byte, addr *net.UDPAddr) {
	msg, err := DecodeMessage(data)
	if err != nil {
		return
	}

	n.queriesReceived.Add(1)

	switch msg.Type {
	case MsgTypeQuery:
		n.handleQuery(msg, addr)
	case MsgTypeResponse:
		n.handleResponse(msg, addr)
	case MsgTypeError:
		// Ignore errors for now
	}
}

// handleQuery processes an incoming query
func (n *Node) handleQuery(msg *Message, addr *net.UDPAddr) {
	// Update routing table with querying node
	if nodeID, ok := msg.GetQueryNodeID(); ok {
		n.routingTable.Insert(nodeID, addr.IP, addr.Port)
	}

	switch msg.Query {
	case QueryPing:
		n.handlePing(msg, addr)

	case QueryFindNode:
		n.handleFindNode(msg, addr)

	case QueryGetPeers:
		n.handleGetPeers(msg, addr)

	case QueryAnnouncePeer:
		n.handleAnnouncePeer(msg, addr)
	}
}

// handlePing responds to a ping query
func (n *Node) handlePing(msg *Message, addr *net.UDPAddr) {
	resp, _ := EncodePingResponse(msg.TransactionID, n.ID)
	n.conn.WriteToUDP(resp, addr)
}

// handleFindNode responds to a find_node query
func (n *Node) handleFindNode(msg *Message, addr *net.UDPAddr) {
	target, ok := msg.GetTarget()
	if !ok {
		return
	}

	closest := n.routingTable.FindClosest(target, K)
	nodes := make([]CompactNodeInfo, len(closest))
	for i, node := range closest {
		nodes[i] = CompactNodeInfo{
			ID:   node.ID,
			IP:   node.IP,
			Port: node.Port,
		}
	}

	resp, _ := EncodeFindNodeResponse(msg.TransactionID, n.ID, nodes)
	n.conn.WriteToUDP(resp, addr)
}

// handleGetPeers responds to a get_peers query
// This is the main crawling signal - we capture the info_hash being requested
func (n *Node) handleGetPeers(msg *Message, addr *net.UDPAddr) {
	infoHash, ok := msg.GetInfoHash()
	if !ok {
		return
	}

	n.getPeersCount.Add(1)

	// Emit event for the crawler
	select {
	case n.eventChan <- Event{
		Type:     EventGetPeers,
		InfoHash: infoHash,
		PeerIP:   addr.IP,
		PeerPort: addr.Port,
	}:
	default:
		// Channel full, drop event
	}

	// We don't have actual peers, so return closest nodes
	target, _ := FromBytes(infoHash[:])
	closest := n.routingTable.FindClosest(target, K)
	nodes := make([]CompactNodeInfo, len(closest))
	for i, node := range closest {
		nodes[i] = CompactNodeInfo{
			ID:   node.ID,
			IP:   node.IP,
			Port: node.Port,
		}
	}

	token := n.generateToken(addr.IP)
	resp, _ := EncodeGetPeersResponseNodes(msg.TransactionID, n.ID, token, nodes)
	n.conn.WriteToUDP(resp, addr)
}

// handleAnnouncePeer handles an announce_peer query
// This tells us a peer actually has the torrent
func (n *Node) handleAnnouncePeer(msg *Message, addr *net.UDPAddr) {
	infoHash, ok := msg.GetInfoHash()
	if !ok {
		return
	}

	// Verify token
	token, ok := msg.GetToken()
	if !ok || !n.validateToken(token, addr.IP) {
		return
	}

	n.announceCount.Add(1)

	// Get the BT port
	btPort, ok := msg.GetPort()
	if !ok {
		btPort = addr.Port // Use source port if implied_port
	}

	// Emit event with peer info
	select {
	case n.eventChan <- Event{
		Type:     EventAnnounce,
		InfoHash: infoHash,
		PeerIP:   addr.IP,
		PeerPort: btPort,
	}:
	default:
		// Channel full, drop event
	}

	// Send response
	resp, _ := EncodeAnnouncePeerResponse(msg.TransactionID, n.ID)
	n.conn.WriteToUDP(resp, addr)
}

// handleResponse processes an incoming response
func (n *Node) handleResponse(msg *Message, addr *net.UDPAddr) {
	// Update routing table
	if msg.Response != nil {
		if idBytes, ok := msg.Response["id"].([]byte); ok {
			if id, err := FromBytes(idBytes); err == nil {
				n.routingTable.Insert(id, addr.IP, addr.Port)
			}
		}
	}

	// Check for pending transaction
	txnKey := string(msg.TransactionID)
	n.pendingMu.Lock()
	ch, ok := n.pendingTxns[txnKey]
	if ok {
		delete(n.pendingTxns, txnKey)
	}
	n.pendingMu.Unlock()

	if ok {
		select {
		case ch <- msg:
		default:
		}
	}

	// Process nodes from find_node/get_peers responses
	nodes := msg.GetResponseNodes()
	for _, node := range nodes {
		n.routingTable.Insert(node.ID, node.IP, node.Port)
	}
}

// refreshLoop periodically refreshes the routing table and actively queries the network
func (n *Node) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(RefreshInterval)
	bootstrapTicker := time.NewTicker(BootstrapInterval)
	// Active query interval - more aggressive than before
	queryTicker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	defer bootstrapTicker.Stop()
	defer queryTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-bootstrapTicker.C:
			// Re-bootstrap if routing table is small
			if n.routingTable.Size() < K*2 {
				n.Bootstrap(ctx)
			}

		case <-queryTicker.C:
			// Actively query the network - this attracts traffic back to us
			// Send multiple queries to increase visibility
			for i := 0; i < 3; i++ {
				n.activeQuery()
			}

		case <-ticker.C:
			// Refresh by finding random IDs
			n.refresh()
		}
	}
}

// activeQuery sends get_peers queries to attract network traffic
// This is the key technique from dhtcrawler2's tell_more_nodes
func (n *Node) activeQuery() {
	// Generate a random infohash to search for
	var randomHash [20]byte
	rand.Read(randomHash[:])

	// Query closest nodes for this hash
	target, _ := FromBytes(randomHash[:])
	closest := n.routingTable.FindClosest(target, 3)

	for _, node := range closest {
		tid := n.nextTxnID()
		msg, _ := EncodeGetPeers(tid, n.ID, randomHash)
		n.conn.WriteToUDP(msg, node.UDPAddr())
		n.queriesSent.Add(1)
	}
}

// refresh sends find_node queries to refresh the routing table
func (n *Node) refresh() {
	// Generate random target in each bucket region
	for i := 0; i < 10; i++ {
		var target NodeID
		rand.Read(target[:])

		closest := n.routingTable.FindClosest(target, 3)
		for _, node := range closest {
			tid := n.nextTxnID()
			msg, _ := EncodeFindNode(tid, n.ID, target)
			n.conn.WriteToUDP(msg, node.UDPAddr())
			n.queriesSent.Add(1)
		}
	}
}

// generateToken creates a token for a given IP
func (n *Node) generateToken(ip net.IP) []byte {
	n.secretMu.Lock()
	defer n.secretMu.Unlock()

	// Simple token: hash of secret + IP
	token := make([]byte, 8)
	for i := 0; i < len(n.tokenSecret) && i < 4; i++ {
		token[i] = n.tokenSecret[i] ^ ip[i%len(ip)]
	}
	return token
}

// validateToken checks if a token is valid for a given IP
func (n *Node) validateToken(token []byte, ip net.IP) bool {
	n.secretMu.Lock()
	defer n.secretMu.Unlock()

	// Check against current and last secret
	expected := make([]byte, 8)
	for i := 0; i < len(n.tokenSecret) && i < 4; i++ {
		expected[i] = n.tokenSecret[i] ^ ip[i%len(ip)]
	}
	if string(token) == string(expected[:len(token)]) {
		return true
	}

	for i := 0; i < len(n.lastSecret) && i < 4; i++ {
		expected[i] = n.lastSecret[i] ^ ip[i%len(ip)]
	}
	return string(token) == string(expected[:len(token)])
}

// rotateSecrets periodically rotates the token secret
func (n *Node) rotateSecrets(ctx context.Context) {
	ticker := time.NewTicker(DefaultTokenExpiry / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.secretMu.Lock()
			n.lastSecret = n.tokenSecret
			n.tokenSecret = make([]byte, 16)
			rand.Read(n.tokenSecret)
			n.secretMu.Unlock()
		}
	}
}

// nextTxnID generates the next transaction ID
func (n *Node) nextTxnID() []byte {
	n.pendingMu.Lock()
	n.txnIDCounter++
	id := n.txnIDCounter
	n.pendingMu.Unlock()

	return []byte{byte(id >> 8), byte(id)}
}

// Stats returns node statistics
func (n *Node) Stats() (queriesRecv, queriesSent, getPeers, announces uint64, routingSize int) {
	return n.queriesReceived.Load(),
		n.queriesSent.Load(),
		n.getPeersCount.Load(),
		n.announceCount.Load(),
		n.routingTable.Size()
}

// FindPeers actively searches for peers for a given info hash using iterative lookup
func (n *Node) FindPeers(ctx context.Context, infoHash [20]byte) []CompactPeerInfo {
	target, _ := FromBytes(infoHash[:])

	var allPeers []CompactPeerInfo
	queried := make(map[string]bool)
	totalNodesQueried := 0
	totalResponses := 0

	// Iterative lookup - keep querying closer nodes
	for round := 0; round < 3; round++ {
		closest := n.routingTable.FindClosest(target, K)
		if len(closest) == 0 {
			break
		}

		var mu sync.Mutex
		var wg sync.WaitGroup
		var newNodes []CompactNodeInfo
		roundResponses := 0

		for _, node := range closest {
			nodeKey := node.ID.Hex()
			if queried[nodeKey] {
				continue
			}
			queried[nodeKey] = true
			totalNodesQueried++

			wg.Add(1)
			go func(node *RoutingNode) {
				defer wg.Done()

				tid := n.nextTxnID()
				respChan := make(chan *Message, 1)

				n.pendingMu.Lock()
				n.pendingTxns[string(tid)] = respChan
				n.pendingMu.Unlock()

				msg, _ := EncodeGetPeers(tid, n.ID, infoHash)
				n.conn.WriteToUDP(msg, node.UDPAddr())
				n.queriesSent.Add(1)

				select {
				case resp := <-respChan:
					mu.Lock()
					roundResponses++
					mu.Unlock()
					// Check for actual peers
					peers := resp.GetResponsePeers()
					if len(peers) > 0 {
						mu.Lock()
						allPeers = append(allPeers, peers...)
						mu.Unlock()
					}
					// Also collect nodes for next round
					nodes := resp.GetResponseNodes()
					if len(nodes) > 0 {
						mu.Lock()
						newNodes = append(newNodes, nodes...)
						mu.Unlock()
					}
				case <-time.After(3 * time.Second):
				case <-ctx.Done():
				}
			}(node)
		}

		wg.Wait()
		totalResponses += roundResponses

		// If we found peers, we're done
		if len(allPeers) > 0 {
			break
		}

		// Add new nodes to routing table for next round
		for _, node := range newNodes {
			n.routingTable.Insert(node.ID, node.IP, node.Port)
		}
	}

	return allPeers
}
