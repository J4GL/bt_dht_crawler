// Package storage provides SQLite-based storage for the DHT crawler
package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Storage provides database operations for the crawler
type Storage struct {
	db *sql.DB
	mu sync.RWMutex

	// Feature flags
	hasFTS5 bool

	// Prepared statements
	insertHash    *sql.Stmt
	insertTorrent *sql.Stmt
	getTorrent    *sql.Stmt
	hashExists    *sql.Stmt
}

// TorrentRecord represents a torrent in the database
type TorrentRecord struct {
	InfoHash  string     `json:"info_hash"`
	Name      string     `json:"name"`
	Size      int64      `json:"size"`
	Files     []FileRecord `json:"files,omitempty"`
	FileCount int        `json:"file_count"`
	CreatedAt time.Time  `json:"created_at"`
	ReqCount  int        `json:"req_count"`
}

// FileRecord represents a file within a torrent
type FileRecord struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// HashRecord represents a pending hash in the database
type HashRecord struct {
	InfoHash  string
	ReqCount  int
	FirstSeen time.Time
	LastSeen  time.Time
}

// SearchResult represents a search result
type SearchResult struct {
	InfoHash      string `json:"info_hash"`
	Name          string `json:"name"`
	NameHighlight string `json:"name_highlight,omitempty"`
	Size          int64  `json:"size"`
	FileCount     int    `json:"file_count"`
	ReqCount      int    `json:"req_count"`
}

// New creates a new storage instance
func New(dbPath string) (*Storage, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Set connection pool settings
	db.SetMaxOpenConns(1) // SQLite works best with single writer
	db.SetMaxIdleConns(1)

	s := &Storage{db: db}

	if err := s.initSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	if err := s.prepareStatements(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to prepare statements: %w", err)
	}

	return s, nil
}

// initSchema creates the database schema
func (s *Storage) initSchema() error {
	// Core tables (always created)
	coreSchema := `
	-- Pending hashes (discovered but not yet downloaded)
	CREATE TABLE IF NOT EXISTS hashes (
		info_hash   TEXT PRIMARY KEY,
		req_count   INTEGER DEFAULT 1,
		first_seen  INTEGER NOT NULL,
		last_seen   INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_hashes_last_seen ON hashes(last_seen);

	-- Downloaded torrent metadata
	CREATE TABLE IF NOT EXISTS torrents (
		info_hash    TEXT PRIMARY KEY,
		name         TEXT NOT NULL,
		size         INTEGER NOT NULL,
		files        TEXT,
		file_count   INTEGER NOT NULL,
		created_at   INTEGER NOT NULL,
		req_count    INTEGER DEFAULT 1
	);
	CREATE INDEX IF NOT EXISTS idx_torrents_name ON torrents(name);
	CREATE INDEX IF NOT EXISTS idx_torrents_created ON torrents(created_at);

	-- Stats table
	CREATE TABLE IF NOT EXISTS stats (
		key   TEXT PRIMARY KEY,
		value TEXT
	);
	`

	if _, err := s.db.Exec(coreSchema); err != nil {
		return err
	}

	// Try to create FTS5 table (may fail if FTS5 not available)
	// First check if FTS5 module is available
	var fts5Available int
	err := s.db.QueryRow("SELECT 1 FROM pragma_compile_options WHERE compile_options LIKE '%FTS5%'").Scan(&fts5Available)
	if err != nil {
		// Pragma not available or FTS5 not compiled in
		s.hasFTS5 = false
		log.Printf("FTS5 not available, using LIKE search")
		return nil
	}

	ftsSchema := `
	CREATE VIRTUAL TABLE IF NOT EXISTS torrents_fts USING fts5(
		name,
		files,
		content='torrents',
		content_rowid='rowid',
		tokenize='trigram'
	);
	`

	if _, err := s.db.Exec(ftsSchema); err != nil {
		// FTS5 not available, use LIKE search
		s.hasFTS5 = false
		log.Printf("FTS5 not available (create failed: %v), using LIKE search", err)
		return nil
	}

	// Verify FTS5 table works by doing a simple query
	_, err = s.db.Exec("SELECT 1 FROM torrents_fts LIMIT 0")
	if err != nil {
		s.hasFTS5 = false
		log.Printf("FTS5 not functional (query failed: %v), using LIKE search", err)
		// Drop the non-functional FTS table
		s.db.Exec("DROP TABLE IF EXISTS torrents_fts")
		return nil
	}

	s.hasFTS5 = true
	log.Printf("FTS5 enabled for fuzzy search")

	// FTS5 triggers
	triggers := `
	CREATE TRIGGER IF NOT EXISTS torrents_ai AFTER INSERT ON torrents BEGIN
		INSERT INTO torrents_fts(rowid, name, files)
		VALUES (new.rowid, new.name, new.files);
	END;

	CREATE TRIGGER IF NOT EXISTS torrents_ad AFTER DELETE ON torrents BEGIN
		INSERT INTO torrents_fts(torrents_fts, rowid, name, files)
		VALUES ('delete', old.rowid, old.name, old.files);
	END;

	CREATE TRIGGER IF NOT EXISTS torrents_au AFTER UPDATE ON torrents BEGIN
		INSERT INTO torrents_fts(torrents_fts, rowid, name, files)
		VALUES ('delete', old.rowid, old.name, old.files);
		INSERT INTO torrents_fts(rowid, name, files)
		VALUES (new.rowid, new.name, new.files);
	END;
	`

	_, err = s.db.Exec(triggers)
	return err
}

// prepareStatements prepares frequently used SQL statements
func (s *Storage) prepareStatements() error {
	var err error

	s.insertHash, err = s.db.Prepare(`
		INSERT INTO hashes (info_hash, req_count, first_seen, last_seen)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(info_hash) DO UPDATE SET
			req_count = req_count + excluded.req_count,
			last_seen = excluded.last_seen
	`)
	if err != nil {
		return err
	}

	s.insertTorrent, err = s.db.Prepare(`
		INSERT OR REPLACE INTO torrents (info_hash, name, size, files, file_count, created_at, req_count)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}

	s.getTorrent, err = s.db.Prepare(`
		SELECT info_hash, name, size, files, file_count, created_at, req_count
		FROM torrents WHERE info_hash = ?
	`)
	if err != nil {
		return err
	}

	s.hashExists, err = s.db.Prepare(`
		SELECT 1 FROM torrents WHERE info_hash = ? LIMIT 1
	`)
	if err != nil {
		return err
	}

	return nil
}

// Close closes the database connection
func (s *Storage) Close() error {
	if s.insertHash != nil {
		s.insertHash.Close()
	}
	if s.insertTorrent != nil {
		s.insertTorrent.Close()
	}
	if s.getTorrent != nil {
		s.getTorrent.Close()
	}
	if s.hashExists != nil {
		s.hashExists.Close()
	}
	return s.db.Close()
}

// InsertHashes batch inserts discovered hashes
func (s *Storage) InsertHashes(hashes map[string]int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt := tx.Stmt(s.insertHash)
	now := time.Now().Unix()

	for hash, count := range hashes {
		if _, err := stmt.Exec(hash, count, now, now); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// InsertTorrent inserts a torrent record
func (s *Storage) InsertTorrent(t *TorrentRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	filesJSON := ""
	if len(t.Files) > 0 {
		// Create searchable file list
		var fileNames []string
		for _, f := range t.Files {
			fileNames = append(fileNames, f.Path)
		}
		filesJSON = strings.Join(fileNames, " ")
	}

	_, err := s.insertTorrent.Exec(
		t.InfoHash,
		t.Name,
		t.Size,
		filesJSON,
		t.FileCount,
		t.CreatedAt.Unix(),
		t.ReqCount,
	)
	return err
}

// GetTorrent retrieves a torrent by info hash
func (s *Storage) GetTorrent(infoHash string) (*TorrentRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var t TorrentRecord
	var filesStr string
	var createdAt int64

	err := s.getTorrent.QueryRow(infoHash).Scan(
		&t.InfoHash,
		&t.Name,
		&t.Size,
		&filesStr,
		&t.FileCount,
		&createdAt,
		&t.ReqCount,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	t.CreatedAt = time.Unix(createdAt, 0)

	// Parse files if present
	if filesStr != "" {
		// Files are stored as space-separated paths for FTS
		paths := strings.Fields(filesStr)
		for _, p := range paths {
			t.Files = append(t.Files, FileRecord{Path: p})
		}
	}

	return &t, nil
}

// TorrentExists checks if a torrent already exists
func (s *Storage) TorrentExists(infoHash string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var exists int
	err := s.hashExists.QueryRow(infoHash).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// GetPendingHashes retrieves hashes that haven't been downloaded yet
func (s *Storage) GetPendingHashes(limit int) ([]HashRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT h.info_hash, h.req_count, h.first_seen, h.last_seen
		FROM hashes h
		LEFT JOIN torrents t ON h.info_hash = t.info_hash
		WHERE t.info_hash IS NULL
		ORDER BY h.req_count DESC, h.last_seen DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hashes []HashRecord
	for rows.Next() {
		var h HashRecord
		var firstSeen, lastSeen int64
		if err := rows.Scan(&h.InfoHash, &h.ReqCount, &firstSeen, &lastSeen); err != nil {
			return nil, err
		}
		h.FirstSeen = time.Unix(firstSeen, 0)
		h.LastSeen = time.Unix(lastSeen, 0)
		hashes = append(hashes, h)
	}

	return hashes, rows.Err()
}

// DeleteHash removes a hash from the pending queue
func (s *Storage) DeleteHash(infoHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM hashes WHERE info_hash = ?", infoHash)
	return err
}

// Search performs a fuzzy search on torrents
func (s *Storage) Search(query string, limit, offset int) ([]SearchResult, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// If FTS5 not available, use LIKE search
	if !s.hasFTS5 {
		return s.searchLike(query, limit, offset)
	}

	// First get total count
	var total int
	countQuery := `
		SELECT COUNT(*) FROM torrents_fts
		WHERE torrents_fts MATCH ?
	`
	if err := s.db.QueryRow(countQuery, query).Scan(&total); err != nil {
		// If FTS fails, try simple LIKE search
		return s.searchLike(query, limit, offset)
	}

	// Get results with highlights
	searchQuery := `
		SELECT t.info_hash, t.name, t.size, t.file_count, t.req_count,
			   highlight(torrents_fts, 0, '<b>', '</b>') as name_hl
		FROM torrents_fts
		JOIN torrents t ON torrents_fts.rowid = t.rowid
		WHERE torrents_fts MATCH ?
		ORDER BY rank
		LIMIT ? OFFSET ?
	`

	rows, err := s.db.Query(searchQuery, query, limit, offset)
	if err != nil {
		return s.searchLike(query, limit, offset)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.InfoHash, &r.Name, &r.Size, &r.FileCount, &r.ReqCount, &r.NameHighlight); err != nil {
			return nil, 0, err
		}
		results = append(results, r)
	}

	return results, total, rows.Err()
}

// searchLike performs a simple LIKE search as fallback
func (s *Storage) searchLike(query string, limit, offset int) ([]SearchResult, int, error) {
	likeQuery := "%" + query + "%"

	var total int
	countQuery := `SELECT COUNT(*) FROM torrents WHERE name LIKE ?`
	if err := s.db.QueryRow(countQuery, likeQuery).Scan(&total); err != nil {
		return nil, 0, err
	}

	searchQuery := `
		SELECT info_hash, name, size, file_count, req_count
		FROM torrents
		WHERE name LIKE ?
		ORDER BY req_count DESC, created_at DESC
		LIMIT ? OFFSET ?
	`

	rows, err := s.db.Query(searchQuery, likeQuery, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.InfoHash, &r.Name, &r.Size, &r.FileCount, &r.ReqCount); err != nil {
			return nil, 0, err
		}
		results = append(results, r)
	}

	return results, total, rows.Err()
}

// Stats returns database statistics
func (s *Storage) Stats() (hashCount, torrentCount int, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	err = s.db.QueryRow("SELECT COUNT(*) FROM hashes").Scan(&hashCount)
	if err != nil {
		return
	}

	err = s.db.QueryRow("SELECT COUNT(*) FROM torrents").Scan(&torrentCount)
	return
}

// RecentTorrents returns recently added torrents
func (s *Storage) RecentTorrents(limit int) ([]TorrentRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT info_hash, name, size, files, file_count, created_at, req_count
		FROM torrents
		ORDER BY created_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var torrents []TorrentRecord
	for rows.Next() {
		var t TorrentRecord
		var filesStr string
		var createdAt int64

		if err := rows.Scan(&t.InfoHash, &t.Name, &t.Size, &filesStr, &t.FileCount, &createdAt, &t.ReqCount); err != nil {
			return nil, err
		}

		t.CreatedAt = time.Unix(createdAt, 0)
		torrents = append(torrents, t)
	}

	return torrents, rows.Err()
}

// PopularTorrents returns torrents with highest request count
func (s *Storage) PopularTorrents(limit int) ([]TorrentRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT info_hash, name, size, files, file_count, created_at, req_count
		FROM torrents
		ORDER BY req_count DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var torrents []TorrentRecord
	for rows.Next() {
		var t TorrentRecord
		var filesStr string
		var createdAt int64

		if err := rows.Scan(&t.InfoHash, &t.Name, &t.Size, &filesStr, &t.FileCount, &createdAt, &t.ReqCount); err != nil {
			return nil, err
		}

		t.CreatedAt = time.Unix(createdAt, 0)
		torrents = append(torrents, t)
	}

	return torrents, rows.Err()
}

// GetAllTorrents returns all torrents with pagination (for sitemap)
func (s *Storage) GetAllTorrents(limit, offset int) ([]TorrentRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT info_hash, name, size, files, file_count, created_at, req_count
		FROM torrents
		ORDER BY created_at DESC
		LIMIT ? OFFSET ?
	`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var torrents []TorrentRecord
	for rows.Next() {
		var t TorrentRecord
		var filesStr string
		var createdAt int64

		if err := rows.Scan(&t.InfoHash, &t.Name, &t.Size, &filesStr, &t.FileCount, &createdAt, &t.ReqCount); err != nil {
			return nil, err
		}

		t.CreatedAt = time.Unix(createdAt, 0)

		// Parse files if present
		if filesStr != "" {
			paths := strings.Fields(filesStr)
			for _, p := range paths {
				t.Files = append(t.Files, FileRecord{Path: p})
			}
		}

		torrents = append(torrents, t)
	}

	return torrents, rows.Err()
}

// Helper to convert files to JSON
func filesToJSON(files []FileRecord) string {
	if len(files) == 0 {
		return ""
	}
	data, _ := json.Marshal(files)
	return string(data)
}
