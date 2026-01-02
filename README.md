# DHT Crawler Go

A fast BitTorrent DHT crawler written in Go. Discovers torrents by participating in the DHT network and fetching metadata from peers.

Inspired by [dhtcrawler2](https://github.com/btdig/dhtcrawler2).

## Features

- **Multi-node DHT** - Runs multiple DHT nodes (default 50) for better network coverage
- **Metadata fetching** - Downloads torrent metadata using BEP 9 extension protocol
- **Full-text search** - SQLite FTS5 support for fast torrent searching
- **Web UI** - Search, browse, and get magnet links through a web interface
- **Concurrent downloads** - Parallel metadata fetching with configurable workers
- **Real-time stats** - Monitor crawler activity through the web dashboard

## Installation

```bash
go build -o dhtcrawler ./cmd/crawler
```

## Usage

```bash
# Run with default configuration
./dhtcrawler

# Run with custom config file
./dhtcrawler -config myconfig.yaml

# Override specific settings
./dhtcrawler -nodes 100 -port 6881 -web 8080
```

### Command Line Flags

| Flag | Description | Default |
|------|-------------|---------|
| `-config` | Path to config file | `config.yaml` |
| `-nodes` | Number of DHT nodes | `50` |
| `-port` | Starting UDP port | `6881` |
| `-web` | Web server port | `8085` |
| `-db` | Database path | `crawler.db` |
| `-workers` | Download workers | `4` |
| `-no-web` | Disable web server | `false` |

## Configuration

Create a `config.yaml` file:

```yaml
dht:
  node_count: 50
  start_port: 6881
  bootstrap:
    - "router.bittorrent.com:6881"
    - "dht.transmissionbt.com:6881"
    - "router.utorrent.com:6881"

cache:
  max_size: 1000
  flush_interval: 30s

download:
  workers: 4
  per_worker: 100
  timeout: 30s

storage:
  path: "crawler.db"

web:
  port: 8085
  enabled: true
```

## Web Interface

Access the web UI at `http://localhost:8085` to:

- Search for torrents by name
- Browse recently discovered torrents
- View torrent details and file listings
- Copy magnet links

## API Endpoints

| Endpoint | Description |
|----------|-------------|
| `GET /api/search?q=query` | Search torrents |
| `GET /api/stats` | Crawler statistics |
| `GET /api/recent` | Recently discovered torrents |
| `GET /api/popular` | Popular torrents |

## How It Works

1. DHT nodes join the BitTorrent network using bootstrap servers
2. Nodes listen for `get_peers` and `announce_peer` requests
3. Discovered info hashes are cached and persisted to SQLite
4. Worker pool downloads metadata from peers using BEP 9
5. Torrent information (name, files, size) is stored in the database
6. Web UI provides search and browsing capabilities

## License

MIT License

Copyright (c) 2025

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

## Acknowledgments

This project is inspired by [dhtcrawler2](https://github.com/btdig/dhtcrawler2) by btdig.
