package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"dhtcrawler-go/internal/crawler"
	"dhtcrawler-go/internal/web"
)

// Config represents the full configuration file
type Config struct {
	DHT      DHTConfig      `yaml:"dht"`
	Cache    CacheConfig    `yaml:"cache"`
	Download DownloadConfig `yaml:"download"`
	Storage  StorageConfig  `yaml:"storage"`
	Web      WebConfig      `yaml:"web"`
}

type DHTConfig struct {
	NodeCount  int      `yaml:"node_count"`
	StartPort  int      `yaml:"start_port"`
	Bootstraps []string `yaml:"bootstrap"`
}

type CacheConfig struct {
	MaxSize       int    `yaml:"max_size"`
	FlushInterval string `yaml:"flush_interval"`
}

type DownloadConfig struct {
	Workers   int    `yaml:"workers"`
	PerWorker int    `yaml:"per_worker"`
	Timeout   string `yaml:"timeout"`
}

type StorageConfig struct {
	Path string `yaml:"path"`
}

type WebConfig struct {
	Port    int  `yaml:"port"`
	Enabled bool `yaml:"enabled"`
}

func main() {
	// CLI flags
	configPath := flag.String("config", "", "Path to config file (optional)")
	nodeCount := flag.Int("nodes", 0, "Number of DHT nodes (overrides config)")
	startPort := flag.Int("port", 0, "Starting UDP port (overrides config)")
	webPort := flag.Int("web", 0, "HTTP server port (overrides config)")
	dbPath := flag.String("db", "", "SQLite database path (overrides config)")
	workers := flag.Int("workers", 0, "Number of download workers (overrides config)")
	noWeb := flag.Bool("no-web", false, "Disable web server")
	flag.Parse()

	// Load default config
	cfg := defaultConfig()

	// Load config file if provided
	if *configPath != "" {
		if err := loadConfig(*configPath, cfg); err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}
	} else {
		// Try to load default config file
		loadConfig("config.yaml", cfg)
	}

	// Apply CLI overrides
	if *nodeCount > 0 {
		cfg.DHT.NodeCount = *nodeCount
	}
	if *startPort > 0 {
		cfg.DHT.StartPort = *startPort
	}
	if *webPort > 0 {
		cfg.Web.Port = *webPort
	}
	if *dbPath != "" {
		cfg.Storage.Path = *dbPath
	}
	if *workers > 0 {
		cfg.Download.Workers = *workers
	}
	if *noWeb {
		cfg.Web.Enabled = false
	}

	// Convert to crawler config
	crawlerCfg := toCrawlerConfig(cfg)

	// Print configuration
	log.Printf("DHT Crawler Configuration:")
	log.Printf("  DHT Nodes: %d (ports %d-%d)", crawlerCfg.NodeCount, crawlerCfg.StartPort, crawlerCfg.StartPort+crawlerCfg.NodeCount-1)
	log.Printf("  Download Workers: %d (x%d concurrent each)", crawlerCfg.DownloadWorkers, crawlerCfg.DownloadPerWorker)
	log.Printf("  Database: %s", crawlerCfg.DBPath)
	if crawlerCfg.WebEnabled {
		log.Printf("  Web UI: http://localhost:%d", crawlerCfg.WebPort)
	}

	// Create crawler
	c, err := crawler.New(crawlerCfg)
	if err != nil {
		log.Fatalf("Failed to create crawler: %v", err)
	}

	// Create context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		log.Printf("Received signal %v, shutting down...", sig)
		cancel()
	}()

	// Start web server if enabled
	if crawlerCfg.WebEnabled {
		webServer, err := web.NewServer(crawlerCfg.WebPort, c)
		if err != nil {
			log.Fatalf("Failed to create web server: %v", err)
		}
		go func() {
			if err := webServer.Start(ctx); err != nil {
				log.Printf("Web server error: %v", err)
			}
		}()
	}

	// Run crawler (blocking)
	if err := c.Run(ctx); err != nil {
		log.Fatalf("Crawler error: %v", err)
	}

	log.Println("Crawler stopped")
}

func defaultConfig() *Config {
	return &Config{
		DHT: DHTConfig{
			NodeCount:  50,
			StartPort:  6881,
			Bootstraps: []string{
				"router.bittorrent.com:6881",
				"dht.transmissionbt.com:6881",
				"router.utorrent.com:6881",
			},
		},
		Cache: CacheConfig{
			MaxSize:       1000,
			FlushInterval: "30s",
		},
		Download: DownloadConfig{
			Workers:   4,
			PerWorker: 100,
			Timeout:   "30s",
		},
		Storage: StorageConfig{
			Path: "crawler.db",
		},
		Web: WebConfig{
			Port:    8085,
			Enabled: true,
		},
	}
}

func loadConfig(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, cfg)
}

func toCrawlerConfig(cfg *Config) *crawler.Config {
	flushInterval, err := time.ParseDuration(cfg.Cache.FlushInterval)
	if err != nil {
		flushInterval = 30 * time.Second
	}

	timeout, err := time.ParseDuration(cfg.Download.Timeout)
	if err != nil {
		timeout = 30 * time.Second
	}

	return &crawler.Config{
		NodeCount:         cfg.DHT.NodeCount,
		StartPort:         cfg.DHT.StartPort,
		Bootstraps:        cfg.DHT.Bootstraps,
		CacheMaxSize:      cfg.Cache.MaxSize,
		CacheFlushInterval: flushInterval,
		DownloadWorkers:   cfg.Download.Workers,
		DownloadPerWorker: cfg.Download.PerWorker,
		DownloadTimeout:   timeout,
		DBPath:            cfg.Storage.Path,
		WebPort:           cfg.Web.Port,
		WebEnabled:        cfg.Web.Enabled,
	}
}

func init() {
	// Set up logging
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	// Print banner
	fmt.Println(`
    ____  __  ________   ______                    __
   / __ \/ / / /_  __/  / ____/________ __      __/ /__  _____
  / / / / /_/ / / /    / /   / ___/ __ '/ | /| / / / _ \/ ___/
 / /_/ / __  / / /    / /___/ /  / /_/ /| |/ |/ / /  __/ /
/_____/_/ /_/ /_/     \____/_/   \__,_/ |__/|__/_/\___/_/

    Fast BitTorrent DHT Crawler - Inspired by dhtcrawler2
`)
}
