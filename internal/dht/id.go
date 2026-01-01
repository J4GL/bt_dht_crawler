// Package dht implements the BitTorrent DHT protocol (BEP 5)
package dht

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
)

const (
	// IDLength is the length of a DHT node ID in bytes (160 bits)
	IDLength = 20
)

// NodeID represents a 160-bit DHT node identifier
type NodeID [IDLength]byte

// GenerateID creates a random 160-bit node ID
func GenerateID() (NodeID, error) {
	var id NodeID
	_, err := rand.Read(id[:])
	return id, err
}

// GenerateDistributedIDs creates node IDs spread evenly across the 160-bit keyspace
// This is a key optimization from dhtcrawler2 - spreading nodes increases coverage
func GenerateDistributedIDs(count int) ([]NodeID, error) {
	if count <= 0 {
		return nil, fmt.Errorf("count must be positive")
	}

	// Max ID is 2^160
	maxID := new(big.Int).Exp(big.NewInt(2), big.NewInt(160), nil)
	piece := new(big.Int).Div(maxID, big.NewInt(int64(count)))

	ids := make([]NodeID, count)
	for i := 0; i < count; i++ {
		// Each ID is in range [i*piece, (i+1)*piece)
		offset := new(big.Int).Mul(piece, big.NewInt(int64(i)))

		// Add random offset within the piece
		randomOffset, err := rand.Int(rand.Reader, piece)
		if err != nil {
			return nil, err
		}

		id := new(big.Int).Add(offset, randomOffset)

		// Convert to bytes (pad to 20 bytes)
		idBytes := id.Bytes()
		if len(idBytes) < IDLength {
			// Pad with zeros at the front
			padded := make([]byte, IDLength)
			copy(padded[IDLength-len(idBytes):], idBytes)
			copy(ids[i][:], padded)
		} else {
			copy(ids[i][:], idBytes[:IDLength])
		}
	}

	return ids, nil
}

// Distance calculates the XOR distance between two node IDs
func Distance(a, b NodeID) NodeID {
	var result NodeID
	for i := 0; i < IDLength; i++ {
		result[i] = a[i] ^ b[i]
	}
	return result
}

// Less returns true if a < b (for sorting by distance)
func (id NodeID) Less(other NodeID) bool {
	for i := 0; i < IDLength; i++ {
		if id[i] < other[i] {
			return true
		}
		if id[i] > other[i] {
			return false
		}
	}
	return false
}

// LeadingZeros returns the number of leading zero bits in the ID
// Used for determining which k-bucket a node belongs to
func (id NodeID) LeadingZeros() int {
	zeros := 0
	for _, b := range id {
		if b == 0 {
			zeros += 8
			continue
		}
		// Count leading zeros in this byte
		for mask := byte(0x80); mask != 0 && (b&mask) == 0; mask >>= 1 {
			zeros++
		}
		break
	}
	return zeros
}

// BucketIndex returns the k-bucket index for the distance between two IDs
// Bucket 0 is for the closest nodes, bucket 159 for the farthest
func BucketIndex(local, remote NodeID) int {
	dist := Distance(local, remote)
	lz := dist.LeadingZeros()
	if lz >= IDLength*8 {
		return 0 // Same ID
	}
	return IDLength*8 - 1 - lz
}

// Hex returns the hexadecimal string representation of the ID
func (id NodeID) Hex() string {
	return hex.EncodeToString(id[:])
}

// String returns a short string representation for debugging
func (id NodeID) String() string {
	return id.Hex()[:8] + "..."
}

// FromHex creates a NodeID from a hexadecimal string
func FromHex(s string) (NodeID, error) {
	var id NodeID
	if len(s) != IDLength*2 {
		return id, fmt.Errorf("invalid hex length: expected %d, got %d", IDLength*2, len(s))
	}
	decoded, err := hex.DecodeString(s)
	if err != nil {
		return id, err
	}
	copy(id[:], decoded)
	return id, nil
}

// FromBytes creates a NodeID from a byte slice
func FromBytes(b []byte) (NodeID, error) {
	var id NodeID
	if len(b) != IDLength {
		return id, fmt.Errorf("invalid byte length: expected %d, got %d", IDLength, len(b))
	}
	copy(id[:], b)
	return id, nil
}

// IsZero returns true if the ID is all zeros
func (id NodeID) IsZero() bool {
	for _, b := range id {
		if b != 0 {
			return false
		}
	}
	return true
}

// Bytes returns the ID as a byte slice
func (id NodeID) Bytes() []byte {
	return id[:]
}
