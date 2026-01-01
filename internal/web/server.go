// Package web provides the HTTP server for the crawler
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dhtcrawler-go/internal/crawler"
	"dhtcrawler-go/internal/storage"
)

//go:embed templates/*
var templateFS embed.FS

// Server is the HTTP server for the crawler
type Server struct {
	addr     string
	crawler  *crawler.Crawler
	storage  *storage.Storage
	template *template.Template
	server   *http.Server
}

// NewServer creates a new web server
func NewServer(port int, c *crawler.Crawler) (*Server, error) {
	// Parse templates
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"formatSize":     formatSize,
		"formatDuration": formatDuration,
		"formatNumber":   formatNumber,
	}).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		// If template not found, use embedded template
		tmpl = template.Must(template.New("search").Funcs(template.FuncMap{
			"formatSize":     formatSize,
			"formatDuration": formatDuration,
			"formatNumber":   formatNumber,
		}).Parse(defaultTemplate))
	}

	return &Server{
		addr:     fmt.Sprintf(":%d", port),
		crawler:  c,
		storage:  c.Storage(),
		template: tmpl,
	}, nil
}

// Start starts the HTTP server
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	// Routes
	mux.HandleFunc("/", s.handleHome)
	mux.HandleFunc("/search", s.handleSearch)
	mux.HandleFunc("/sitemap", s.handleSitemap)
	mux.HandleFunc("/torrent/", s.handleTorrent)
	mux.HandleFunc("/api/search", s.handleAPISearch)
	mux.HandleFunc("/api/stats", s.handleAPIStats)
	mux.HandleFunc("/api/recent", s.handleAPIRecent)
	mux.HandleFunc("/api/popular", s.handleAPIPopular)
	mux.HandleFunc("/api/torrent/", s.handleAPITorrent)

	s.server = &http.Server{
		Addr:         s.addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	log.Printf("Starting web server on http://localhost%s", s.addr)

	go func() {
		<-ctx.Done()
		s.server.Shutdown(context.Background())
	}()

	if err := s.server.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// handleHome serves the main search page
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	stats := s.crawler.Stats()

	data := map[string]interface{}{
		"Stats":   stats,
		"Query":   "",
		"Results": nil,
		"Total":   0,
		"Page":    1,
	}

	s.renderTemplate(w, "search", data)
}

// handleSearch handles search form submissions
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	const perPage = 20
	offset := (page - 1) * perPage

	var results []storage.SearchResult
	var total int

	if query != "" {
		var err error
		results, total, err = s.storage.Search(query, perPage, offset)
		if err != nil {
			log.Printf("Search error: %v", err)
		}
	}

	stats := s.crawler.Stats()
	totalPages := (total + perPage - 1) / perPage

	data := map[string]interface{}{
		"Stats":      stats,
		"Query":      query,
		"Results":    results,
		"Total":      total,
		"Page":       page,
		"TotalPages": totalPages,
		"HasPrev":    page > 1,
		"HasNext":    page < totalPages,
		"PrevPage":   page - 1,
		"NextPage":   page + 1,
	}

	s.renderTemplate(w, "search", data)
}

// handleAPISearch handles JSON search API
func (s *Server) handleAPISearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	const perPage = 20
	offset := (page - 1) * perPage

	results, total, err := s.storage.Search(query, perPage, offset)
	if err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response := map[string]interface{}{
		"query":       query,
		"results":     results,
		"total":       total,
		"page":        page,
		"per_page":    perPage,
		"total_pages": (total + perPage - 1) / perPage,
	}

	s.jsonResponse(w, response)
}

// handleAPIStats handles the stats API
func (s *Server) handleAPIStats(w http.ResponseWriter, r *http.Request) {
	stats := s.crawler.Stats()
	s.jsonResponse(w, stats)
}

// handleAPIRecent handles the recent torrents API
func (s *Server) handleAPIRecent(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	torrents, err := s.storage.RecentTorrents(limit)
	if err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.jsonResponse(w, torrents)
}

// handleAPIPopular handles the popular torrents API
func (s *Server) handleAPIPopular(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	torrents, err := s.storage.PopularTorrents(limit)
	if err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.jsonResponse(w, torrents)
}

// handleSitemap handles the sitemap page listing all torrents
func (s *Server) handleSitemap(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	const perPage = 50
	offset := (page - 1) * perPage

	torrents, err := s.storage.GetAllTorrents(perPage, offset)
	if err != nil {
		log.Printf("Sitemap error: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	_, totalTorrents, _ := s.storage.Stats()
	totalPages := (totalTorrents + perPage - 1) / perPage

	stats := s.crawler.Stats()

	data := map[string]interface{}{
		"Stats":      stats,
		"Torrents":   torrents,
		"Total":      totalTorrents,
		"Page":       page,
		"TotalPages": totalPages,
		"HasPrev":    page > 1,
		"HasNext":    page < totalPages,
		"PrevPage":   page - 1,
		"NextPage":   page + 1,
	}

	s.renderTemplate(w, "sitemap", data)
}

// handleTorrent handles torrent detail page requests
func (s *Server) handleTorrent(w http.ResponseWriter, r *http.Request) {
	hash := strings.TrimPrefix(r.URL.Path, "/torrent/")
	if hash == "" {
		http.NotFound(w, r)
		return
	}

	torrent, err := s.storage.GetTorrent(hash)
	if err != nil {
		log.Printf("Torrent lookup error: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if torrent == nil {
		http.NotFound(w, r)
		return
	}

	stats := s.crawler.Stats()
	magnetLink := fmt.Sprintf("magnet:?xt=urn:btih:%s&dn=%s", torrent.InfoHash, template.URLQueryEscaper(torrent.Name))

	data := map[string]interface{}{
		"Stats":      stats,
		"Torrent":    torrent,
		"MagnetLink": magnetLink,
	}

	s.renderTemplate(w, "torrent", data)
}

// handleAPITorrent handles JSON API for torrent details
func (s *Server) handleAPITorrent(w http.ResponseWriter, r *http.Request) {
	hash := strings.TrimPrefix(r.URL.Path, "/api/torrent/")
	if hash == "" {
		s.jsonError(w, "missing hash", http.StatusBadRequest)
		return
	}

	torrent, err := s.storage.GetTorrent(hash)
	if err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if torrent == nil {
		s.jsonError(w, "not found", http.StatusNotFound)
		return
	}

	s.jsonResponse(w, torrent)
}

// renderTemplate renders an HTML template
func (s *Server) renderTemplate(w http.ResponseWriter, name string, data interface{}) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Try embedded template first
	err := s.template.ExecuteTemplate(w, name, data)
	if err != nil {
		// Fallback to inline templates
		var tmplStr string
		switch name {
		case "sitemap":
			tmplStr = sitemapTemplate
		case "torrent":
			tmplStr = torrentTemplate
		default:
			tmplStr = defaultTemplate
		}

		t := template.Must(template.New(name).Funcs(template.FuncMap{
			"formatSize":     formatSize,
			"formatDuration": formatDuration,
			"formatNumber":   formatNumber,
		}).Parse(tmplStr))
		t.Execute(w, data)
	}
}

// jsonResponse writes a JSON response
func (s *Server) jsonResponse(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

// jsonError writes a JSON error response
func (s *Server) jsonError(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// Helper functions for templates

func formatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func formatNumber(n interface{}) string {
	switch v := n.(type) {
	case int:
		return formatInt(int64(v))
	case int64:
		return formatInt(v)
	case uint64:
		return formatInt(int64(v))
	default:
		return fmt.Sprintf("%v", n)
	}
}

func formatInt(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1000000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	if n < 1000000000 {
		return fmt.Sprintf("%.1fM", float64(n)/1000000)
	}
	return fmt.Sprintf("%.1fB", float64(n)/1000000000)
}

// Common CSS styles with CSS variables and mobile-first approach
const commonStyles = `
:root {
  --color-bg: #1a1a2e;
  --color-surface: #252547;
  --color-surface-hover: #303060;
  --color-primary: #00d4ff;
  --color-primary-hover: #00b8e6;
  --color-text: #eeeeee;
  --color-text-muted: #888888;
  --color-text-dim: #666666;
  --color-highlight: rgba(0, 212, 255, 0.2);
  --spacing-xs: 0.5rem;
  --spacing-sm: 1rem;
  --spacing-md: 1.5rem;
  --spacing-lg: 2rem;
  --spacing-xl: 2.5rem;
  --radius-sm: 0.5rem;
  --radius-md: 0.75rem;
  --radius-full: 1.25rem;
  --font-mono: 'SF Mono', 'Fira Code', 'Consolas', monospace;
  --transition: 0.2s ease-out;
  --shadow: 0 4px 12px rgba(0, 0, 0, 0.3);
}
*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
body {
  font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
  background: var(--color-bg);
  color: var(--color-text);
  min-height: 100vh;
  line-height: 1.6;
  font-size: 1rem;
}
main { padding: var(--spacing-sm); max-width: 56rem; margin: 0 auto; }
header { text-align: center; padding: var(--spacing-lg) 0; }
h1 { font-size: 2rem; margin-bottom: var(--spacing-xs); color: var(--color-primary); }
h1 a { color: var(--color-primary); text-decoration: none; transition: opacity var(--transition); }
h1 a:hover { opacity: 0.8; }
nav { display: flex; gap: var(--spacing-sm); justify-content: center; margin-bottom: var(--spacing-sm); flex-wrap: wrap; }
nav a {
  color: var(--color-primary);
  text-decoration: none;
  padding: var(--spacing-sm) var(--spacing-md);
  background: var(--color-surface);
  border-radius: var(--radius-sm);
  min-height: 2.75rem;
  display: inline-flex;
  align-items: center;
  transition: background var(--transition), transform var(--transition), color var(--transition);
}
nav a:hover { background: var(--color-surface-hover); transform: translateY(-1px); }
nav a:focus { outline: 2px solid var(--color-primary); outline-offset: 2px; }
nav a:active { transform: translateY(0); background: var(--color-primary); color: var(--color-bg); }
.stats { display: flex; gap: var(--spacing-sm); justify-content: center; flex-wrap: wrap; margin-bottom: var(--spacing-md); font-size: 0.875rem; color: var(--color-text-muted); }
.stat { background: var(--color-surface); padding: var(--spacing-xs) var(--spacing-sm); border-radius: var(--radius-full); }
.search-form { display: flex; flex-direction: column; gap: var(--spacing-sm); margin-bottom: var(--spacing-lg); }
.search-form label { font-size: 0.875rem; color: var(--color-text-muted); }
.search-form input {
  width: 100%;
  padding: var(--spacing-md);
  font-size: 1.125rem;
  border: 2px solid transparent;
  border-radius: var(--radius-md);
  background: var(--color-surface);
  color: var(--color-text);
  transition: border-color var(--transition), box-shadow var(--transition);
}
.search-form input:focus {
  outline: none;
  border-color: var(--color-primary);
  box-shadow: 0 0 0 3px var(--color-highlight);
}
.search-form button {
  padding: var(--spacing-md) var(--spacing-lg);
  font-size: 1.125rem;
  border: none;
  border-radius: var(--radius-md);
  background: var(--color-primary);
  color: var(--color-bg);
  cursor: pointer;
  font-weight: 600;
  min-height: 3rem;
  transition: background var(--transition), transform var(--transition), box-shadow var(--transition);
}
.search-form button:hover { background: var(--color-primary-hover); transform: translateY(-2px); box-shadow: var(--shadow); }
.search-form button:focus { outline: 2px solid var(--color-text); outline-offset: 2px; }
.search-form button:active { transform: translateY(0); }
.results-info { color: var(--color-text-muted); margin-bottom: var(--spacing-sm); font-size: 0.875rem; }
article {
  background: var(--color-surface);
  padding: var(--spacing-sm);
  border-radius: var(--radius-md);
  margin-bottom: var(--spacing-sm);
  transition: transform var(--transition), box-shadow var(--transition);
}
article:hover { transform: translateY(-2px); box-shadow: var(--shadow); }
.torrent-name { font-size: 1.125rem; color: var(--color-primary); margin-bottom: var(--spacing-xs); word-break: break-word; }
.torrent-name a { color: var(--color-primary); text-decoration: none; transition: opacity var(--transition); }
.torrent-name a:hover { opacity: 0.8; text-decoration: underline; }
.torrent-name a:focus { outline: 2px solid var(--color-primary); outline-offset: 2px; }
mark { background: var(--color-highlight); color: var(--color-primary); padding: 0 0.125rem; border-radius: 2px; }
.torrent-meta { display: flex; flex-wrap: wrap; gap: var(--spacing-sm); color: var(--color-text-muted); font-size: 0.875rem; }
.torrent-hash { font-family: var(--font-mono); font-size: 0.75rem; color: var(--color-text-dim); margin-top: var(--spacing-xs); word-break: break-all; }
.torrent-hash a { color: var(--color-primary); text-decoration: none; transition: opacity var(--transition); }
.torrent-hash a:hover { opacity: 0.8; }
.pagination { display: flex; justify-content: center; gap: var(--spacing-xs); margin-top: var(--spacing-md); flex-wrap: wrap; }
.pagination a {
  padding: var(--spacing-sm) var(--spacing-md);
  background: var(--color-surface);
  color: var(--color-primary);
  text-decoration: none;
  border-radius: var(--radius-sm);
  min-height: 2.75rem;
  display: inline-flex;
  align-items: center;
  transition: background var(--transition), transform var(--transition), color var(--transition);
}
.pagination a:hover { background: var(--color-surface-hover); transform: translateY(-1px); }
.pagination a:focus { outline: 2px solid var(--color-primary); outline-offset: 2px; }
.pagination a:active { transform: translateY(0); background: var(--color-primary); color: var(--color-bg); }
.detail-card { background: var(--color-surface); padding: var(--spacing-md); border-radius: var(--radius-md); margin-bottom: var(--spacing-md); }
.detail-title { font-size: 1.375rem; color: var(--color-primary); margin-bottom: var(--spacing-sm); word-break: break-word; line-height: 1.4; }
.detail-grid { display: grid; grid-template-columns: 1fr; gap: var(--spacing-sm); margin-bottom: var(--spacing-md); }
.detail-item { background: var(--color-bg); padding: var(--spacing-sm); border-radius: var(--radius-sm); }
.detail-label { color: var(--color-text-muted); font-size: 0.75rem; margin-bottom: 0.25rem; text-transform: uppercase; letter-spacing: 0.05em; }
.detail-value { color: var(--color-text); font-size: 1rem; }
.detail-value--mono { font-family: var(--font-mono); font-size: 0.75rem; word-break: break-all; }
.magnet-section { margin: var(--spacing-md) 0; }
.magnet-section label { display: block; color: var(--color-text-muted); font-size: 0.75rem; margin-bottom: var(--spacing-xs); text-transform: uppercase; letter-spacing: 0.05em; }
.magnet-box { background: var(--color-bg); padding: var(--spacing-sm); border-radius: var(--radius-sm); word-break: break-all; font-family: var(--font-mono); font-size: 0.75rem; color: var(--color-text-muted); line-height: 1.5; }
.btn {
  display: inline-block;
  padding: var(--spacing-sm) var(--spacing-md);
  background: var(--color-primary);
  color: var(--color-bg);
  text-decoration: none;
  border-radius: var(--radius-md);
  font-weight: 600;
  margin-top: var(--spacing-sm);
  min-height: 2.75rem;
  cursor: pointer;
  transition: background var(--transition), transform var(--transition), box-shadow var(--transition);
}
.btn:hover { background: var(--color-primary-hover); transform: translateY(-2px); box-shadow: var(--shadow); }
.btn:focus { outline: 2px solid var(--color-text); outline-offset: 2px; }
.btn:active { transform: translateY(0); }
.file-section { margin-top: var(--spacing-md); }
.file-section h2 { color: var(--color-primary); font-size: 1rem; margin-bottom: var(--spacing-sm); }
.file-item {
  padding: var(--spacing-xs) var(--spacing-sm);
  background: var(--color-bg);
  border-radius: var(--radius-sm);
  margin-bottom: var(--spacing-xs);
  font-size: 0.875rem;
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
}
.file-path { word-break: break-all; }
.file-size { color: var(--color-text-muted); font-size: 0.75rem; }
.back-link { display: inline-block; color: var(--color-primary); text-decoration: none; transition: opacity var(--transition); }
.back-link:hover { opacity: 0.8; }
.back-link:focus { outline: 2px solid var(--color-primary); outline-offset: 2px; }
.empty-state { text-align: center; padding: var(--spacing-xl); color: var(--color-text-muted); }

@media (min-width: 640px) {
  main { padding: var(--spacing-md); }
  h1 { font-size: 2.5rem; }
  .search-form { flex-direction: row; align-items: flex-end; }
  .search-form .field { flex: 1; }
  .search-form button { align-self: flex-end; }
  article { padding: var(--spacing-md); }
  .detail-grid { grid-template-columns: repeat(2, 1fr); }
  .file-item { flex-direction: row; justify-content: space-between; align-items: center; }
  .file-size { margin-left: var(--spacing-sm); }
}
@media (min-width: 1024px) {
  .detail-grid { grid-template-columns: repeat(4, 1fr); }
}
`

// Default HTML template for search page
const defaultTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <meta name="description" content="Search and discover torrents from the BitTorrent DHT network">
    <title>DHT Crawler - Search Torrents</title>
    <style>` + commonStyles + `</style>
</head>
<body>
    <main>
        <header>
            <h1><a href="/">DHT Crawler</a></h1>
            <nav>
                <a href="/">Search</a>
                <a href="/sitemap">All Torrents</a>
            </nav>
            <div class="stats">
                <span class="stat">Torrents: {{formatNumber .Stats.TotalTorrents}}</span>
                <span class="stat">Hashes: {{formatNumber .Stats.TotalHashes}}</span>
                <span class="stat">DHT Nodes: {{.Stats.DHTNodesActive}}</span>
                <span class="stat">Uptime: {{formatDuration .Stats.Uptime}}</span>
            </div>
        </header>

        <form class="search-form" action="/search" method="GET" role="search">
            <div class="field">
                <label for="search-input">Search torrents</label>
                <input type="search" id="search-input" name="q" placeholder="Enter torrent name..." value="{{.Query}}" autocomplete="off" autofocus>
            </div>
            <button type="submit">Search</button>
        </form>

        {{if .Query}}
        <p class="results-info">Found {{.Total}} results for "{{.Query}}"</p>
        {{end}}

        <section>
        {{range .Results}}
        <article>
            <h2 class="torrent-name"><a href="/torrent/{{.InfoHash}}">{{if .NameHighlight}}{{.NameHighlight | html}}{{else}}{{.Name}}{{end}}</a></h2>
            <div class="torrent-meta">
                <span>{{formatSize .Size}}</span>
                <span>{{.FileCount}} files</span>
                <span>{{.ReqCount}} requests</span>
            </div>
            <div class="torrent-hash">
                <a href="magnet:?xt=urn:btih:{{.InfoHash}}">magnet:?xt=urn:btih:{{.InfoHash}}</a>
            </div>
        </article>
        {{end}}
        </section>

        {{if .Query}}
        <nav class="pagination">
            {{if .HasPrev}}<a href="/search?q={{.Query}}&page={{.PrevPage}}">← Previous</a>{{end}}
            {{if .HasNext}}<a href="/search?q={{.Query}}&page={{.NextPage}}">Next →</a>{{end}}
        </nav>
        {{end}}
    </main>
</body>
</html>`

// Sitemap template
const sitemapTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <meta name="description" content="Browse all torrents discovered from the BitTorrent DHT network">
    <title>All Torrents - DHT Crawler</title>
    <style>` + commonStyles + `</style>
</head>
<body>
    <main>
        <header>
            <h1><a href="/">DHT Crawler</a></h1>
            <nav>
                <a href="/">Search</a>
                <a href="/sitemap">All Torrents</a>
            </nav>
            <div class="stats">
                <span class="stat">Torrents: {{formatNumber .Stats.TotalTorrents}}</span>
                <span class="stat">Hashes: {{formatNumber .Stats.TotalHashes}}</span>
                <span class="stat">DHT Nodes: {{.Stats.DHTNodesActive}}</span>
                <span class="stat">Uptime: {{formatDuration .Stats.Uptime}}</span>
            </div>
        </header>

        <p class="results-info">All Torrents ({{.Total}} total) — Page {{.Page}} of {{.TotalPages}}</p>

        <section>
        {{range .Torrents}}
        <article>
            <h2 class="torrent-name"><a href="/torrent/{{.InfoHash}}">{{.Name}}</a></h2>
            <div class="torrent-meta">
                <span>{{formatSize .Size}}</span>
                <span>{{.FileCount}} files</span>
                <span>{{.ReqCount}} requests</span>
            </div>
            <div class="torrent-hash">
                <a href="magnet:?xt=urn:btih:{{.InfoHash}}">magnet:?xt=urn:btih:{{.InfoHash}}</a>
            </div>
        </article>
        {{else}}
        <div class="empty-state">
            <p>No torrents found yet.</p>
            <p>The crawler is still discovering torrents from the DHT network.</p>
        </div>
        {{end}}
        </section>

        <nav class="pagination">
            {{if .HasPrev}}<a href="/sitemap?page={{.PrevPage}}">← Previous</a>{{end}}
            {{if .HasNext}}<a href="/sitemap?page={{.NextPage}}">Next →</a>{{end}}
        </nav>
    </main>
</body>
</html>`

// Torrent detail template
const torrentTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <meta name="description" content="Download {{.Torrent.Name}} - {{formatSize .Torrent.Size}} torrent via magnet link">
    <title>{{.Torrent.Name}} - DHT Crawler</title>
    <style>` + commonStyles + `</style>
</head>
<body>
    <main>
        <header>
            <h1><a href="/">DHT Crawler</a></h1>
            <nav>
                <a href="/">Search</a>
                <a href="/sitemap">All Torrents</a>
            </nav>
        </header>

        <article class="detail-card">
            <h2 class="detail-title">{{.Torrent.Name}}</h2>

            <div class="detail-grid">
                <div class="detail-item">
                    <div class="detail-label">Size</div>
                    <div class="detail-value">{{formatSize .Torrent.Size}}</div>
                </div>
                <div class="detail-item">
                    <div class="detail-label">Files</div>
                    <div class="detail-value">{{.Torrent.FileCount}}</div>
                </div>
                <div class="detail-item">
                    <div class="detail-label">Requests</div>
                    <div class="detail-value">{{.Torrent.ReqCount}}</div>
                </div>
                <div class="detail-item">
                    <div class="detail-label">Info Hash</div>
                    <div class="detail-value detail-value--mono">{{.Torrent.InfoHash}}</div>
                </div>
            </div>

            <div class="magnet-section">
                <label for="magnet-link">Magnet Link</label>
                <div class="magnet-box" id="magnet-link">{{.MagnetLink}}</div>
                <a class="btn" href="{{.MagnetLink}}">Open Magnet Link</a>
            </div>

            {{if .Torrent.Files}}
            <section class="file-section">
                <h2>Files ({{.Torrent.FileCount}})</h2>
                {{range .Torrent.Files}}
                <div class="file-item">
                    <span class="file-path">{{.Path}}</span>
                    {{if .Size}}<span class="file-size">{{formatSize .Size}}</span>{{end}}
                </div>
                {{end}}
            </section>
            {{end}}
        </article>

        <a href="/sitemap" class="back-link">← Back to All Torrents</a>
    </main>
</body>
</html>`
