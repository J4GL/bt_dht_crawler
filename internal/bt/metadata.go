package bt

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	// Protocol constants
	ProtocolName = "BitTorrent protocol"
	// Extension message IDs
	ExtMsgHandshake = 0
	// Metadata piece size (16 KB)
	MetadataPieceSize = 16384
	// Extension bit for extended messaging
	ExtensionBit = 0x10
)

var (
	ErrTimeout           = errors.New("connection timeout")
	ErrNoMetadataSupport = errors.New("peer does not support ut_metadata")
	ErrInvalidMetadata   = errors.New("metadata hash mismatch")
	ErrMetadataTooBig    = errors.New("metadata too large")
)

// TorrentMeta contains parsed torrent metadata
type TorrentMeta struct {
	InfoHash  [20]byte
	Name      string
	Size      int64
	Files     []FileInfo
	PieceLen  int64
	RawInfo   []byte // Raw info dict for verification
}

// FileInfo contains information about a single file
type FileInfo struct {
	Path string
	Size int64
}

// MetadataDownloader downloads torrent metadata from peers using BEP 9
type MetadataDownloader struct {
	Timeout     time.Duration
	MaxMetaSize int // Maximum metadata size in bytes
}

// NewMetadataDownloader creates a new metadata downloader
func NewMetadataDownloader(timeout time.Duration) *MetadataDownloader {
	return &MetadataDownloader{
		Timeout:     timeout,
		MaxMetaSize: 10 * 1024 * 1024, // 10 MB max
	}
}

// Download downloads metadata from a peer
func (d *MetadataDownloader) Download(ctx context.Context, infoHash [20]byte, peerAddr string) (*TorrentMeta, error) {
	// Create connection with timeout
	dialer := net.Dialer{Timeout: d.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", peerAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}
	defer conn.Close()

	// Set overall deadline
	conn.SetDeadline(time.Now().Add(d.Timeout))

	// Perform BT handshake
	if err := d.sendHandshake(conn, infoHash); err != nil {
		return nil, fmt.Errorf("handshake failed: %w", err)
	}

	// Read peer's handshake
	peerExtensions, err := d.readHandshake(conn, infoHash)
	if err != nil {
		return nil, fmt.Errorf("read handshake failed: %w", err)
	}

	// Check if peer supports extensions
	if peerExtensions[5]&ExtensionBit == 0 {
		return nil, ErrNoMetadataSupport
	}

	// Send extension handshake
	if err := d.sendExtHandshake(conn); err != nil {
		return nil, fmt.Errorf("ext handshake failed: %w", err)
	}

	// Read extension handshake and get metadata info
	metadataMsgID, metadataSize, err := d.readExtHandshake(conn)
	if err != nil {
		return nil, fmt.Errorf("read ext handshake failed: %w", err)
	}

	if metadataMsgID == 0 {
		return nil, ErrNoMetadataSupport
	}

	if metadataSize > d.MaxMetaSize {
		return nil, ErrMetadataTooBig
	}

	// Download metadata pieces
	metadata, err := d.downloadMetadata(conn, metadataMsgID, metadataSize)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}

	// Verify hash
	hash := sha1.Sum(metadata)
	if hash != infoHash {
		return nil, ErrInvalidMetadata
	}

	// Parse metadata
	meta, err := d.parseMetadata(infoHash, metadata)
	if err != nil {
		return nil, fmt.Errorf("parse failed: %w", err)
	}

	return meta, nil
}

// sendHandshake sends the BT handshake
func (d *MetadataDownloader) sendHandshake(conn net.Conn, infoHash [20]byte) error {
	// Handshake: <pstrlen><pstr><reserved><info_hash><peer_id>
	buf := make([]byte, 68)
	buf[0] = 19 // Protocol name length
	copy(buf[1:20], ProtocolName)
	// Reserved bytes - set extension bit
	buf[25] = ExtensionBit
	copy(buf[28:48], infoHash[:])
	// Generate random peer ID
	copy(buf[48:68], "-GO0001-123456789012")

	_, err := conn.Write(buf)
	return err
}

// readHandshake reads the peer's handshake
func (d *MetadataDownloader) readHandshake(conn net.Conn, infoHash [20]byte) ([8]byte, error) {
	var extensions [8]byte

	// Read protocol length
	pstrlen := make([]byte, 1)
	if _, err := io.ReadFull(conn, pstrlen); err != nil {
		return extensions, err
	}

	// Read protocol name
	pstr := make([]byte, pstrlen[0])
	if _, err := io.ReadFull(conn, pstr); err != nil {
		return extensions, err
	}

	// Read reserved bytes
	reserved := make([]byte, 8)
	if _, err := io.ReadFull(conn, reserved); err != nil {
		return extensions, err
	}
	copy(extensions[:], reserved)

	// Read info hash
	peerInfoHash := make([]byte, 20)
	if _, err := io.ReadFull(conn, peerInfoHash); err != nil {
		return extensions, err
	}

	// Verify info hash matches
	if !bytes.Equal(peerInfoHash, infoHash[:]) {
		return extensions, fmt.Errorf("info hash mismatch")
	}

	// Read peer ID
	peerID := make([]byte, 20)
	if _, err := io.ReadFull(conn, peerID); err != nil {
		return extensions, err
	}

	return extensions, nil
}

// sendExtHandshake sends the extension handshake
func (d *MetadataDownloader) sendExtHandshake(conn net.Conn) error {
	// Extension handshake payload
	payload := map[string]interface{}{
		"m": map[string]interface{}{
			"ut_metadata": int64(1), // Our metadata message ID
		},
	}

	payloadBytes, err := Encode(payload)
	if err != nil {
		return err
	}

	return d.sendExtMessage(conn, ExtMsgHandshake, payloadBytes)
}

// readExtHandshake reads the extension handshake
func (d *MetadataDownloader) readExtHandshake(conn net.Conn) (int, int, error) {
	for {
		msgType, payload, err := d.readMessage(conn)
		if err != nil {
			return 0, 0, err
		}

		// We're looking for extension message (type 20)
		if msgType != 20 {
			continue
		}

		// First byte is extension message ID
		if len(payload) < 1 {
			continue
		}

		extMsgID := payload[0]
		if extMsgID != ExtMsgHandshake {
			continue
		}

		// Parse extension handshake
		dict, err := DecodeDict(payload[1:])
		if err != nil {
			return 0, 0, err
		}

		// Get ut_metadata message ID
		var metadataMsgID int
		if m, ok := GetDict(dict, "m"); ok {
			if id, ok := GetInt(m, "ut_metadata"); ok {
				metadataMsgID = int(id)
			}
		}

		// Get metadata size
		metadataSize := 0
		if size, ok := GetInt(dict, "metadata_size"); ok {
			metadataSize = int(size)
		}

		return metadataMsgID, metadataSize, nil
	}
}

// downloadMetadata downloads all metadata pieces
func (d *MetadataDownloader) downloadMetadata(conn net.Conn, msgID int, totalSize int) ([]byte, error) {
	numPieces := (totalSize + MetadataPieceSize - 1) / MetadataPieceSize
	metadata := make([]byte, totalSize)
	received := make([]bool, numPieces)

	// Request all pieces
	for i := 0; i < numPieces; i++ {
		if err := d.requestMetadataPiece(conn, msgID, i); err != nil {
			return nil, err
		}
	}

	// Receive pieces
	piecesReceived := 0
	for piecesReceived < numPieces {
		msgType, payload, err := d.readMessage(conn)
		if err != nil {
			return nil, err
		}

		if msgType != 20 || len(payload) < 1 {
			continue
		}

		extMsgID := payload[0]
		if int(extMsgID) != msgID {
			continue
		}

		// Parse metadata response
		piece, data, err := d.parseMetadataPiece(payload[1:])
		if err != nil {
			continue
		}

		if piece < 0 || piece >= numPieces || received[piece] {
			continue
		}

		// Copy data to correct position
		start := piece * MetadataPieceSize
		copy(metadata[start:], data)
		received[piece] = true
		piecesReceived++
	}

	return metadata, nil
}

// requestMetadataPiece sends a request for a metadata piece
func (d *MetadataDownloader) requestMetadataPiece(conn net.Conn, msgID int, piece int) error {
	request := map[string]interface{}{
		"msg_type": int64(0), // Request
		"piece":    int64(piece),
	}

	payload, err := Encode(request)
	if err != nil {
		return err
	}

	return d.sendExtMessage(conn, byte(msgID), payload)
}

// parseMetadataPiece parses a metadata piece response
func (d *MetadataDownloader) parseMetadataPiece(data []byte) (int, []byte, error) {
	// Find the end of the bencoded dict
	r := bytes.NewReader(data)
	_, err := decode(r)
	if err != nil {
		return 0, nil, err
	}

	// Position after dict is where the raw piece data starts
	dictEnd := len(data) - r.Len()

	// Parse the dict
	dict, err := DecodeDict(data[:dictEnd])
	if err != nil {
		return 0, nil, err
	}

	// Check message type (1 = data, 2 = reject)
	msgType, ok := GetInt(dict, "msg_type")
	if !ok || msgType != 1 {
		return 0, nil, fmt.Errorf("not a data message")
	}

	piece, ok := GetInt(dict, "piece")
	if !ok {
		return 0, nil, fmt.Errorf("missing piece index")
	}

	return int(piece), data[dictEnd:], nil
}

// sendExtMessage sends an extension message
func (d *MetadataDownloader) sendExtMessage(conn net.Conn, extMsgID byte, payload []byte) error {
	// Message: <length><type><ext_msg_id><payload>
	msgLen := 2 + len(payload) // type + ext_msg_id + payload
	buf := make([]byte, 4+msgLen)
	binary.BigEndian.PutUint32(buf[:4], uint32(msgLen))
	buf[4] = 20 // Extended message type
	buf[5] = extMsgID
	copy(buf[6:], payload)

	_, err := conn.Write(buf)
	return err
}

// readMessage reads a BT message
func (d *MetadataDownloader) readMessage(conn net.Conn) (byte, []byte, error) {
	// Read message length
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(conn, lenBuf); err != nil {
		return 0, nil, err
	}

	msgLen := binary.BigEndian.Uint32(lenBuf)
	if msgLen == 0 {
		return 0, nil, nil // Keep-alive
	}

	if msgLen > 1024*1024*10 { // 10 MB limit
		return 0, nil, fmt.Errorf("message too large: %d", msgLen)
	}

	// Read message
	msg := make([]byte, msgLen)
	if _, err := io.ReadFull(conn, msg); err != nil {
		return 0, nil, err
	}

	return msg[0], msg[1:], nil
}

// parseMetadata parses the raw metadata into TorrentMeta
func (d *MetadataDownloader) parseMetadata(infoHash [20]byte, data []byte) (*TorrentMeta, error) {
	dict, err := DecodeDict(data)
	if err != nil {
		return nil, err
	}

	meta := &TorrentMeta{
		InfoHash: infoHash,
		RawInfo:  data,
	}

	// Get name
	if name, ok := GetString(dict, "name"); ok {
		meta.Name = name
	} else if nameUtf8, ok := GetString(dict, "name.utf-8"); ok {
		meta.Name = nameUtf8
	}

	// Get piece length
	if pieceLen, ok := GetInt(dict, "piece length"); ok {
		meta.PieceLen = pieceLen
	}

	// Check for multi-file torrent
	if files, ok := GetList(dict, "files"); ok {
		// Multi-file torrent
		for _, f := range files {
			if fileDict, ok := f.(map[string]interface{}); ok {
				fileInfo := FileInfo{}

				if length, ok := GetInt(fileDict, "length"); ok {
					fileInfo.Size = length
					meta.Size += length
				}

				if pathList, ok := GetList(fileDict, "path"); ok {
					var pathParts []string
					for _, p := range pathList {
						if pb, ok := p.([]byte); ok {
							pathParts = append(pathParts, string(pb))
						}
					}
					if len(pathParts) > 0 {
						fileInfo.Path = pathParts[len(pathParts)-1]
						for i := 0; i < len(pathParts)-1; i++ {
							fileInfo.Path = pathParts[i] + "/" + fileInfo.Path
						}
					}
				}

				meta.Files = append(meta.Files, fileInfo)
			}
		}
	} else {
		// Single-file torrent
		if length, ok := GetInt(dict, "length"); ok {
			meta.Size = length
			meta.Files = []FileInfo{{
				Path: meta.Name,
				Size: length,
			}}
		}
	}

	return meta, nil
}
