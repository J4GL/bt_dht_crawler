// Package bt provides BitTorrent protocol implementations
package bt

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
)

var (
	ErrInvalidBencode = errors.New("invalid bencode data")
	ErrUnexpectedEOF  = errors.New("unexpected end of bencode data")
)

// Encode encodes a value to bencode format
// Supports: string, []byte, int, int64, []interface{}, map[string]interface{}
func Encode(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	if err := encode(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encode(w *bytes.Buffer, v interface{}) error {
	switch val := v.(type) {
	case string:
		fmt.Fprintf(w, "%d:%s", len(val), val)
	case []byte:
		fmt.Fprintf(w, "%d:", len(val))
		w.Write(val)
	case int:
		fmt.Fprintf(w, "i%de", val)
	case int64:
		fmt.Fprintf(w, "i%de", val)
	case uint16:
		fmt.Fprintf(w, "i%de", val)
	case uint32:
		fmt.Fprintf(w, "i%de", val)
	case []interface{}:
		w.WriteByte('l')
		for _, item := range val {
			if err := encode(w, item); err != nil {
				return err
			}
		}
		w.WriteByte('e')
	case map[string]interface{}:
		w.WriteByte('d')
		// Keys must be sorted
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "%d:%s", len(k), k)
			if err := encode(w, val[k]); err != nil {
				return err
			}
		}
		w.WriteByte('e')
	default:
		return fmt.Errorf("unsupported type: %T", v)
	}
	return nil
}

// Decode decodes bencode data into Go values
// Returns: string (as []byte), int64, []interface{}, map[string]interface{}
func Decode(data []byte) (interface{}, error) {
	r := bytes.NewReader(data)
	val, err := decode(r)
	if err != nil {
		return nil, err
	}
	return val, nil
}

// DecodeDict decodes bencode data and expects a dictionary
func DecodeDict(data []byte) (map[string]interface{}, error) {
	val, err := Decode(data)
	if err != nil {
		return nil, err
	}
	dict, ok := val.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("expected dictionary, got %T", val)
	}
	return dict, nil
}

func decode(r *bytes.Reader) (interface{}, error) {
	b, err := r.ReadByte()
	if err != nil {
		return nil, ErrUnexpectedEOF
	}

	switch {
	case b == 'i': // Integer
		return decodeInt(r)
	case b == 'l': // List
		return decodeList(r)
	case b == 'd': // Dictionary
		return decodeDict(r)
	case b >= '0' && b <= '9': // String
		r.UnreadByte()
		return decodeString(r)
	default:
		return nil, fmt.Errorf("%w: unexpected byte '%c'", ErrInvalidBencode, b)
	}
}

func decodeInt(r *bytes.Reader) (int64, error) {
	var buf bytes.Buffer
	for {
		b, err := r.ReadByte()
		if err != nil {
			return 0, ErrUnexpectedEOF
		}
		if b == 'e' {
			break
		}
		buf.WriteByte(b)
	}
	return strconv.ParseInt(buf.String(), 10, 64)
}

func decodeString(r *bytes.Reader) ([]byte, error) {
	// Read length
	var lenBuf bytes.Buffer
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, ErrUnexpectedEOF
		}
		if b == ':' {
			break
		}
		if b < '0' || b > '9' {
			return nil, fmt.Errorf("%w: invalid string length", ErrInvalidBencode)
		}
		lenBuf.WriteByte(b)
	}

	length, err := strconv.Atoi(lenBuf.String())
	if err != nil {
		return nil, err
	}

	// Read string content
	data := make([]byte, length)
	n, err := io.ReadFull(r, data)
	if err != nil || n != length {
		return nil, ErrUnexpectedEOF
	}
	return data, nil
}

func decodeList(r *bytes.Reader) ([]interface{}, error) {
	var list []interface{}
	for {
		b, err := peekByte(r)
		if err != nil {
			return nil, ErrUnexpectedEOF
		}
		if b == 'e' {
			r.ReadByte() // consume 'e'
			return list, nil
		}
		item, err := decode(r)
		if err != nil {
			return nil, err
		}
		list = append(list, item)
	}
}

func decodeDict(r *bytes.Reader) (map[string]interface{}, error) {
	dict := make(map[string]interface{})
	for {
		b, err := peekByte(r)
		if err != nil {
			return nil, ErrUnexpectedEOF
		}
		if b == 'e' {
			r.ReadByte() // consume 'e'
			return dict, nil
		}

		// Key must be a string
		keyBytes, err := decodeString(r)
		if err != nil {
			return nil, err
		}
		key := string(keyBytes)

		// Value
		val, err := decode(r)
		if err != nil {
			return nil, err
		}
		dict[key] = val
	}
}

// peekByte returns the next byte without advancing the reader
func peekByte(r *bytes.Reader) (byte, error) {
	b, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	r.UnreadByte()
	return b, nil
}

// Helper functions for extracting values from decoded bencode

// GetString extracts a string from a decoded value
func GetString(dict map[string]interface{}, key string) (string, bool) {
	val, ok := dict[key]
	if !ok {
		return "", false
	}
	switch v := val.(type) {
	case []byte:
		return string(v), true
	case string:
		return v, true
	default:
		return "", false
	}
}

// GetBytes extracts bytes from a decoded value
func GetBytes(dict map[string]interface{}, key string) ([]byte, bool) {
	val, ok := dict[key]
	if !ok {
		return nil, false
	}
	switch v := val.(type) {
	case []byte:
		return v, true
	case string:
		return []byte(v), true
	default:
		return nil, false
	}
}

// GetInt extracts an integer from a decoded value
func GetInt(dict map[string]interface{}, key string) (int64, bool) {
	val, ok := dict[key]
	if !ok {
		return 0, false
	}
	switch v := val.(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	default:
		return 0, false
	}
}

// GetDict extracts a nested dictionary from a decoded value
func GetDict(dict map[string]interface{}, key string) (map[string]interface{}, bool) {
	val, ok := dict[key]
	if !ok {
		return nil, false
	}
	d, ok := val.(map[string]interface{})
	return d, ok
}

// GetList extracts a list from a decoded value
func GetList(dict map[string]interface{}, key string) ([]interface{}, bool) {
	val, ok := dict[key]
	if !ok {
		return nil, false
	}
	l, ok := val.([]interface{})
	return l, ok
}
