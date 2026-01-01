package dht

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"

	"dhtcrawler-go/internal/bt"
)

// KRPC message types
const (
	MsgTypeQuery    = "q"
	MsgTypeResponse = "r"
	MsgTypeError    = "e"
)

// KRPC query types
const (
	QueryPing         = "ping"
	QueryFindNode     = "find_node"
	QueryGetPeers     = "get_peers"
	QueryAnnouncePeer = "announce_peer"
)

// Message represents a KRPC message
type Message struct {
	TransactionID []byte
	Type          string // "q", "r", or "e"
	Query         string // Query method name (for queries)
	Args          map[string]interface{}
	Response      map[string]interface{}
	Error         []interface{} // [error_code, error_message]
}

// CompactNodeInfo represents a node in compact format (20 bytes ID + 6 bytes addr)
type CompactNodeInfo struct {
	ID   NodeID
	IP   net.IP
	Port int
}

// CompactPeerInfo represents a peer in compact format (4 bytes IP + 2 bytes port)
type CompactPeerInfo struct {
	IP   net.IP
	Port int
}

// GenerateTransactionID creates a random 2-byte transaction ID
func GenerateTransactionID() []byte {
	tid := make([]byte, 2)
	rand.Read(tid)
	return tid
}

// EncodePing creates a ping query message
func EncodePing(tid []byte, nodeID NodeID) ([]byte, error) {
	msg := map[string]interface{}{
		"t": tid,
		"y": MsgTypeQuery,
		"q": QueryPing,
		"a": map[string]interface{}{
			"id": nodeID[:],
		},
	}
	return bt.Encode(msg)
}

// EncodePingResponse creates a ping response message
func EncodePingResponse(tid []byte, nodeID NodeID) ([]byte, error) {
	msg := map[string]interface{}{
		"t": tid,
		"y": MsgTypeResponse,
		"r": map[string]interface{}{
			"id": nodeID[:],
		},
	}
	return bt.Encode(msg)
}

// EncodeFindNode creates a find_node query message
func EncodeFindNode(tid []byte, nodeID, target NodeID) ([]byte, error) {
	msg := map[string]interface{}{
		"t": tid,
		"y": MsgTypeQuery,
		"q": QueryFindNode,
		"a": map[string]interface{}{
			"id":     nodeID[:],
			"target": target[:],
		},
	}
	return bt.Encode(msg)
}

// EncodeFindNodeResponse creates a find_node response message
func EncodeFindNodeResponse(tid []byte, nodeID NodeID, nodes []CompactNodeInfo) ([]byte, error) {
	msg := map[string]interface{}{
		"t": tid,
		"y": MsgTypeResponse,
		"r": map[string]interface{}{
			"id":    nodeID[:],
			"nodes": EncodeCompactNodes(nodes),
		},
	}
	return bt.Encode(msg)
}

// EncodeGetPeers creates a get_peers query message
func EncodeGetPeers(tid []byte, nodeID NodeID, infoHash [20]byte) ([]byte, error) {
	msg := map[string]interface{}{
		"t": tid,
		"y": MsgTypeQuery,
		"q": QueryGetPeers,
		"a": map[string]interface{}{
			"id":        nodeID[:],
			"info_hash": infoHash[:],
		},
	}
	return bt.Encode(msg)
}

// EncodeGetPeersResponseNodes creates a get_peers response with nodes
func EncodeGetPeersResponseNodes(tid []byte, nodeID NodeID, token []byte, nodes []CompactNodeInfo) ([]byte, error) {
	msg := map[string]interface{}{
		"t": tid,
		"y": MsgTypeResponse,
		"r": map[string]interface{}{
			"id":    nodeID[:],
			"token": token,
			"nodes": EncodeCompactNodes(nodes),
		},
	}
	return bt.Encode(msg)
}

// EncodeGetPeersResponsePeers creates a get_peers response with peers
func EncodeGetPeersResponsePeers(tid []byte, nodeID NodeID, token []byte, peers []CompactPeerInfo) ([]byte, error) {
	peerList := make([]interface{}, len(peers))
	for i, p := range peers {
		peerList[i] = EncodeCompactPeer(p)
	}
	msg := map[string]interface{}{
		"t": tid,
		"y": MsgTypeResponse,
		"r": map[string]interface{}{
			"id":     nodeID[:],
			"token":  token,
			"values": peerList,
		},
	}
	return bt.Encode(msg)
}

// EncodeAnnouncePeer creates an announce_peer query message
func EncodeAnnouncePeer(tid []byte, nodeID NodeID, infoHash [20]byte, port int, token []byte, impliedPort bool) ([]byte, error) {
	implied := 0
	if impliedPort {
		implied = 1
	}
	msg := map[string]interface{}{
		"t": tid,
		"y": MsgTypeQuery,
		"q": QueryAnnouncePeer,
		"a": map[string]interface{}{
			"id":           nodeID[:],
			"info_hash":    infoHash[:],
			"port":         port,
			"token":        token,
			"implied_port": implied,
		},
	}
	return bt.Encode(msg)
}

// EncodeAnnouncePeerResponse creates an announce_peer response message
func EncodeAnnouncePeerResponse(tid []byte, nodeID NodeID) ([]byte, error) {
	msg := map[string]interface{}{
		"t": tid,
		"y": MsgTypeResponse,
		"r": map[string]interface{}{
			"id": nodeID[:],
		},
	}
	return bt.Encode(msg)
}

// EncodeError creates an error response message
func EncodeError(tid []byte, code int, message string) ([]byte, error) {
	msg := map[string]interface{}{
		"t": tid,
		"y": MsgTypeError,
		"e": []interface{}{int64(code), message},
	}
	return bt.Encode(msg)
}

// DecodeMessage parses a KRPC message from bencode
func DecodeMessage(data []byte) (*Message, error) {
	dict, err := bt.DecodeDict(data)
	if err != nil {
		return nil, fmt.Errorf("failed to decode message: %w", err)
	}

	msg := &Message{}

	// Transaction ID
	if tid, ok := bt.GetBytes(dict, "t"); ok {
		msg.TransactionID = tid
	}

	// Message type
	if y, ok := bt.GetString(dict, "y"); ok {
		msg.Type = y
	}

	switch msg.Type {
	case MsgTypeQuery:
		if q, ok := bt.GetString(dict, "q"); ok {
			msg.Query = q
		}
		if a, ok := bt.GetDict(dict, "a"); ok {
			msg.Args = a
		}

	case MsgTypeResponse:
		if r, ok := bt.GetDict(dict, "r"); ok {
			msg.Response = r
		}

	case MsgTypeError:
		if e, ok := bt.GetList(dict, "e"); ok {
			msg.Error = e
		}
	}

	return msg, nil
}

// GetQueryNodeID extracts the querying node's ID from a query message
func (m *Message) GetQueryNodeID() (NodeID, bool) {
	if m.Args == nil {
		return NodeID{}, false
	}
	if idBytes, ok := bt.GetBytes(m.Args, "id"); ok {
		id, err := FromBytes(idBytes)
		if err != nil {
			return NodeID{}, false
		}
		return id, true
	}
	return NodeID{}, false
}

// GetInfoHash extracts the info_hash from a get_peers or announce_peer query
func (m *Message) GetInfoHash() ([20]byte, bool) {
	var hash [20]byte
	if m.Args == nil {
		return hash, false
	}
	if hashBytes, ok := bt.GetBytes(m.Args, "info_hash"); ok {
		if len(hashBytes) == 20 {
			copy(hash[:], hashBytes)
			return hash, true
		}
	}
	return hash, false
}

// GetTarget extracts the target from a find_node query
func (m *Message) GetTarget() (NodeID, bool) {
	if m.Args == nil {
		return NodeID{}, false
	}
	if targetBytes, ok := bt.GetBytes(m.Args, "target"); ok {
		id, err := FromBytes(targetBytes)
		if err != nil {
			return NodeID{}, false
		}
		return id, true
	}
	return NodeID{}, false
}

// GetPort extracts the port from an announce_peer query
func (m *Message) GetPort() (int, bool) {
	if m.Args == nil {
		return 0, false
	}
	// Check implied_port first
	if implied, ok := bt.GetInt(m.Args, "implied_port"); ok && implied == 1 {
		return 0, false // Use source port
	}
	if port, ok := bt.GetInt(m.Args, "port"); ok {
		return int(port), true
	}
	return 0, false
}

// GetToken extracts the token from an announce_peer query
func (m *Message) GetToken() ([]byte, bool) {
	if m.Args == nil {
		return nil, false
	}
	return bt.GetBytes(m.Args, "token")
}

// GetResponseNodes extracts compact node info from a response
func (m *Message) GetResponseNodes() []CompactNodeInfo {
	if m.Response == nil {
		return nil
	}
	if nodesBytes, ok := bt.GetBytes(m.Response, "nodes"); ok {
		return DecodeCompactNodes(nodesBytes)
	}
	return nil
}

// GetResponsePeers extracts compact peer info from a response
func (m *Message) GetResponsePeers() []CompactPeerInfo {
	if m.Response == nil {
		return nil
	}
	if values, ok := bt.GetList(m.Response, "values"); ok {
		peers := make([]CompactPeerInfo, 0, len(values))
		for _, v := range values {
			if peerBytes, ok := v.([]byte); ok {
				if peer, ok := DecodeCompactPeer(peerBytes); ok {
					peers = append(peers, peer)
				}
			}
		}
		return peers
	}
	return nil
}

// GetResponseToken extracts the token from a get_peers response
func (m *Message) GetResponseToken() ([]byte, bool) {
	if m.Response == nil {
		return nil, false
	}
	return bt.GetBytes(m.Response, "token")
}

// EncodeCompactNodes encodes a list of nodes to compact format
// Each node is 26 bytes: 20 bytes ID + 4 bytes IP + 2 bytes port
func EncodeCompactNodes(nodes []CompactNodeInfo) []byte {
	data := make([]byte, len(nodes)*26)
	for i, node := range nodes {
		offset := i * 26
		copy(data[offset:offset+20], node.ID[:])
		copy(data[offset+20:offset+24], node.IP.To4())
		binary.BigEndian.PutUint16(data[offset+24:offset+26], uint16(node.Port))
	}
	return data
}

// DecodeCompactNodes decodes compact node info
func DecodeCompactNodes(data []byte) []CompactNodeInfo {
	if len(data)%26 != 0 {
		return nil
	}
	count := len(data) / 26
	nodes := make([]CompactNodeInfo, count)
	for i := 0; i < count; i++ {
		offset := i * 26
		copy(nodes[i].ID[:], data[offset:offset+20])
		nodes[i].IP = net.IP(data[offset+20 : offset+24])
		nodes[i].Port = int(binary.BigEndian.Uint16(data[offset+24 : offset+26]))
	}
	return nodes
}

// EncodeCompactPeer encodes a peer to compact format (6 bytes)
func EncodeCompactPeer(peer CompactPeerInfo) []byte {
	data := make([]byte, 6)
	copy(data[:4], peer.IP.To4())
	binary.BigEndian.PutUint16(data[4:6], uint16(peer.Port))
	return data
}

// DecodeCompactPeer decodes a compact peer
func DecodeCompactPeer(data []byte) (CompactPeerInfo, bool) {
	if len(data) != 6 {
		return CompactPeerInfo{}, false
	}
	return CompactPeerInfo{
		IP:   net.IP(data[:4]),
		Port: int(binary.BigEndian.Uint16(data[4:6])),
	}, true
}
