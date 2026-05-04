# TASK.md - Transmission VK Bot Enhancement

## Context

Current project is a VK bot (Go) for LLM interaction via OpenCode. The bot needs to be extended with **Transmission torrent client management** capabilities, allowing users to control a local Transmission daemon through VK messages.

---

## Requirements

### 1. Transmission RPC Integration

**Transmission Server Configuration:**
- **URL:** `http://192.168.0.192:9091/transmission/rpc`
- **Authentication:** Basic Auth
  - **Username:** `transmission`
  - **Password:** `1`

**Required Transmission RPC Methods:**

| Method | Description | Parameters |
|--------|-------------|------------|
| `torrent-add` | Add a torrent | `magnet:?xt=urn:btih:...` |
| `torrent-get` | Get torrent status | `fields: ["id","name","status","downloadedEver","leftUntilDone","sizeWhenDone","eta","rateDownload"]` |
| `torrent-list` | List torrents | Same as torrent-get |

**Transmission RPC Request Format:**
```json
{
  "method": "torrent-add",
  "arguments": {
    "metainfo": "base64-encoded-magnet-or-torrent-data"
  },
  "tag": 1
}
```

**For magnet links, use:**
```json
{
  "method": "torrent-add",
  "arguments": {
    "metainfo": "base64-encoded-magnet-link"
  },
  "tag": 1
}
```

---

### 2. New VK Bot Commands

| Command | Description | Example |
|---------|-------------|---------|
| `/help` or `/h` | Show help with all available commands | — |
| `/add <magnet-link>` | Add a torrent for download | `/add magnet:?xt=urn:btih:...` |
| `/status` | Show current downloads and their progress | — |

---

### 3. `/status` Response Format

```
📥 Active Downloads:

1. 🔥 Movie.Name.2024.1080p.BluRay
   ⬇️ Speed: 1.2 MB/s
   📊 Progress: 45%
   💾 Downloaded: 450 MB / 1000 MB
   ⏱️ ETA: 8 min

2. 🔥 Another.Torrent
   ⬇️ Speed: 0 MB/s (waiting...)
   📊 Progress: 100%
   💾 Downloaded: 200 MB / 200 MB
   ⏱️ ETA: —
```

---

### 4. `/add` Response Format

**Success:**
```
✅ Torrent added: Movie.Name.2024.1080p.BluRay
   📊 Size: 1000 MB
```

**Error:**
```
❌ Error adding torrent: [error description]
```

---

### 5. `/help` Response Format

```
🔧 **Transmission Bot - Commands**

/add <magnet-link> - Add torrent for download
/status - Show status of all downloads
/help - Show this help

Examples:
/add magnet:?xt=urn:btih:abc123...
/status
```

---

## Architectural Requirements

### 1. Transmission Client Module

Create a new package `transmission/` with the following structure:

```
transmission/
├── client.go      - Main client implementation
├── types.go       - Data structures for torrents/status
└── errors.go      - Custom error types
```

**Required Methods:**

```go
type Client struct {
    url      string
    username string
    password string
    http     *http.Client
}

func NewClient(url, username, password string) *Client

// AddTorrent adds a torrent by magnet link
func (c *Client) AddTorrent(magnetLink string) (*Torrent, error)

// GetStatus returns status of all active torrents
func (c *Client) GetStatus() ([]TorrentStatus, error)

// FormatStatus returns human-readable status string
func (c *Client) FormatStatus() string
```

**TorrentStatus struct:**
```go
type TorrentStatus struct {
    ID            int
    Name          string
    Status        string  // "downloading", "seeding", "stopped", "checking"
    Progress      float64 // 0-100
    Downloaded    int64   // bytes
    TotalSize     int64   // bytes
    DownloadSpeed int64   // bytes/s
    ETA           int64   // seconds (-1 if unknown)
}
```

### 2. Bot Integration

In the bot's main command handler, add new branches:

```go
switch {
case cmd == "/help" || cmd == "/h":
    handleHelp()
case cmd == "/add":
    handleAddTorrent(args)
case cmd == "/status":
    handleStatus()
}
```

### 3. Configuration

Add Transmission credentials to `config.json`:

```json
{
  "transmission": {
    "url": "http://192.168.0.192:9091/transmission/rpc",
    "username": "transmission",
    "password": "1"
  }
}
```

### 4. Error Handling

- If Transmission is unreachable, return: `"⚠️ Transmission is not available. Please check connection."`
- If magnet link is invalid, return: `"❌ Invalid magnet link format. Use: magnet:?xt=urn:btih:..."`
- If torrent already exists, return: `"ℹ️ Torrent already exists in Transmission."`

---

## Task Priorities

### P0 - Critical
1. Implement Transmission RPC client (HTTP + Basic Auth)
2. Implement `/add` and `/status` commands
3. Add Transmission credentials to config.json

### P1 - High
1. Implement `/help` / `/h` command
2. Proper error handling for Transmission unavailability
3. Validate magnet link format before sending

### P2 - Medium
1. Improve output formatting (emoji, alignment)
2. Add `/remove` command to remove torrents
3. Add `/stop` / `/start` commands for torrent control
4. Add streaming progress updates via long poll

---

## Testing

### 1. Manual Transmission Connection Test

```bash
curl -u "transmission:1" \
  http://192.168.0.192:9091/transmission/rpc \
  -d '{"method":"torrent-get","arguments":{"fields":["id","name","status"]}}'
```

Expected response:
```json
{
  "result": "success",
  "arguments": {
    "torrents": []
  }
}
```

### 2. Unit Tests

- Test Transmission client connection
- Test torrent addition mock
- Test status formatting
- Test error handling

### 3. Integration Tests

- Send `/add` command with test magnet link
- Verify torrent appears in `/status`
- Check progress updates work correctly

---

## Files to Create/Modify

### New Files:
- `transmission/client.go` - Transmission RPC client
- `transmission/types.go` - Data structures
- `transmission/errors.go` - Error handling

### Modified Files:
- `main.go` - Add command handlers
- `config.json` - Add Transmission credentials

### Build Verification:
```bash
go build -o bot .
./bot  # Verify startup
```

---

## Notes

- The bot currently uses `messages.getLongPollServer` (personal messages)
- Keep existing LLM functionality intact
- Transmission commands should only work if Transmission is reachable
- Consider adding rate limiting for `/status` queries
