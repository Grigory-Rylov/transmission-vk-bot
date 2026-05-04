package transmission

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Client communicates with a Transmission RPC daemon.
type Client struct {
	url          string
	username     string
	password     string
	sessionID    string
	defaultFolder string
	currentDir    string
	http         *http.Client
}

// NewClient creates a new Transmission RPC client.
func NewClient(cfg Config) *Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	return &Client{
		url:          cfg.URL,
		username:     cfg.Username,
		password:     cfg.Password,
		defaultFolder: cfg.DefaultFolder,
		http: &http.Client{
			Timeout:   15 * time.Second,
			Transport: transport,
		},
	}
}

// rpcRequest represents a Transmission RPC request.
type rpcRequest struct {
	Method    string       `json:"method"`
	Arguments any          `json:"arguments,omitempty"`
	Tag       int          `json:"tag,omitempty"`
}

// rpcResponse represents a Transmission RPC response.
type rpcResponse struct {
	Result  string          `json:"result"`
	Tag     int             `json:"tag"`
	Errors []int           `json:"errors"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// doRPC executes an RPC call and returns the response body.
func (c *Client) doRPC(method string, args any, tag int) (*rpcResponse, error) {
	return c.doRPCLoop(method, args, tag)
}

// doRPCLoop handles CSRF session-id rotation with retry on 409.
func (c *Client) doRPCLoop(method string, args any, tag int) (*rpcResponse, error) {
	var argsJSON json.RawMessage
	if raw, ok := args.(json.RawMessage); ok {
		argsJSON = raw
	} else {
		raw, err := json.Marshal(args)
		if err != nil {
			return nil, fmt.Errorf("marshal args: %w", err)
		}
		argsJSON = raw
	}
	reqBody, err := json.Marshal(rpcRequest{
		Method:    method,
		Arguments: argsJSON,
		Tag:       tag,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Retry loop for CSRF session-id handling
	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		log.Printf("[TX] Attempt %d, request body: %s", attempt, string(reqBody))
		if c.sessionID != "" {
			log.Printf("[TX] Using session ID: %s", c.sessionID)
		}
		req, err := http.NewRequest("POST", c.url, bytes.NewReader(reqBody))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		req.SetBasicAuth(c.username, c.password)
		req.Header.Set("Content-Type", "application/json")
		if c.sessionID != "" {
			req.Header.Set("X-Transmission-Session-Id", c.sessionID)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, ErrTransmissionUnavailable
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		// Handle 409 Conflict - Transmission CSRF protection
		if resp.StatusCode == 409 {
			newSessionID := resp.Header.Get("X-Transmission-Session-Id")
			if newSessionID != "" {
				c.sessionID = newSessionID
				continue // Retry with new session ID
			}
			return nil, ErrTransmissionUnavailable
		}

		if resp.StatusCode != 200 {
			return nil, ErrTransmissionUnavailable
		}

		// Update session ID from successful response if present
		newSessionID := resp.Header.Get("X-Transmission-Session-Id")
		if newSessionID != "" {
			c.sessionID = newSessionID
		}

		var rpcResp rpcResponse
		if err := json.Unmarshal(body, &rpcResp); err != nil {
			// Could be an HTML error page
			if len(body) > 0 && body[0] == '<' {
				log.Printf("[TX] HTML response: %s", string(body))
				return nil, ErrTransmissionUnavailable
			}
			log.Printf("[TX] Unmarshal error: %v, body: %s", err, string(body))
			return nil, fmt.Errorf("unmarshal response: %w", err)
		}

		log.Printf("[TX] RPC response: result=%s, errors=%v, body: %s", rpcResp.Result, rpcResp.Errors, string(body))
		if rpcResp.Result != "success" {
			return nil, ErrTransmissionUnavailable
		}

		return &rpcResp, nil
	}

	return nil, ErrTransmissionUnavailable
}

// fieldsJSON returns a JSON object with "fields" key for Transmission RPC.
func fieldsJSON(fields ...string) json.RawMessage {
	m := map[string]interface{}{"fields": fields}
	b, _ := json.Marshal(m)
	return b
}

// torrentFields are the fields we request for each torrent.
var torrentFields = fieldsJSON(
	"id", "name", "status", "downloadedEver", "leftUntilDone",
	"sizeWhenDone", "eta", "rateDownload",
)

// SetDownloadDir sets the download directory for subsequent torrent additions.
func (c *Client) SetDownloadDir(subDir string) {
	if subDir == "" {
		subDir = ""
		return
	}
	c.currentDir = c.defaultFolder + "/" + subDir
}

// AddTorrent adds a torrent by magnet link.
func (c *Client) AddTorrent(magnetLink string, downloadDir string) (*TorrentStatus, error) {
	// Set download directory if specified
	if downloadDir != "" {
		c.SetDownloadDir(downloadDir)
	}

	// Build arguments with optional download directory
	argsMap := map[string]interface{}{
		"metainfo": magnetLink,
	}
	if c.currentDir != "" {
		argsMap["download-dir"] = c.currentDir
	}
	args, _ := json.Marshal(argsMap)

	resp, err := c.doRPC("torrent-add", args, 1)
	if err != nil {
		return nil, err
	}

	// Parse the added torrent ID from the response
	var added map[string]interface{}
	if err := json.Unmarshal(resp.Arguments, &added); err != nil {
		return nil, fmt.Errorf("parse add response: %w", err)
	}

	torrents, ok := added["torrent-added"].([]interface{})
	if !ok || len(torrents) == 0 {
		return nil, fmt.Errorf("no torrent returned from add")
	}

	torrentData, ok := torrents[0].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid torrent data in response")
	}

	// Get the torrent ID
	idFloat, _ := torrentData["id"].(float64)
	id := int(idFloat)

	// Fetch full status
	return c.getTorrentStatusByID(id)
}

// getTorrentStatusByID fetches status for a single torrent by ID.
func (c *Client) getTorrentStatusByID(id int) (*TorrentStatus, error) {
	args := json.RawMessage(fmt.Sprintf(`{"ids":[%d]}`, id))
	resp, err := c.doRPC("torrent-get", args, 1)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(resp.Arguments, &result); err != nil {
		return nil, fmt.Errorf("parse status response: %w", err)
	}

	torrents, ok := result["torrents"].([]interface{})
	if !ok || len(torrents) == 0 {
		return nil, fmt.Errorf("torrent not found")
	}

	return parseTorrent(torrents[0])
}

// GetStatus returns status of all torrents.
func (c *Client) GetStatus() ([]TorrentStatus, error) {
	resp, err := c.doRPC("torrent-get", torrentFields, 1)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(resp.Arguments, &result); err != nil {
		return nil, fmt.Errorf("parse status response: %w", err)
	}

	torrents, ok := result["torrents"].([]interface{})
	if !ok {
		return []TorrentStatus{}, nil
	}

	var statuses []TorrentStatus
	for _, t := range torrents {
		ts, err := parseTorrent(t)
		if err != nil {
			continue
		}
		statuses = append(statuses, *ts)
	}

	return statuses, nil
}

// GetActiveStatus returns status of torrents that are not yet fully downloaded.
func (c *Client) GetActiveStatus() ([]TorrentStatus, error) {
	resp, err := c.doRPC("torrent-get", torrentFields, 1)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(resp.Arguments, &result); err != nil {
		return nil, fmt.Errorf("parse status response: %w", err)
	}

	torrents, ok := result["torrents"].([]interface{})
	if !ok {
		return []TorrentStatus{}, nil
	}

	var statuses []TorrentStatus
	for _, t := range torrents {
		ts, err := parseTorrent(t)
		if err != nil {
			continue
		}
		// Filter out completed torrents (100% downloaded)
		if ts.Progress < 100.0 {
			statuses = append(statuses, *ts)
		}
	}

	return statuses, nil
}

// GetCompletedStatus returns status of completed (100% downloaded) torrents.
func (c *Client) GetCompletedStatus() ([]TorrentStatus, error) {
	resp, err := c.doRPC("torrent-get", torrentFields, 1)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(resp.Arguments, &result); err != nil {
		return nil, fmt.Errorf("parse status response: %w", err)
	}

	torrents, ok := result["torrents"].([]interface{})
	if !ok {
		return []TorrentStatus{}, nil
	}

	var statuses []TorrentStatus
	for _, t := range torrents {
		ts, err := parseTorrent(t)
		if err != nil {
			continue
		}
		// Filter for completed torrents (100% downloaded)
		if ts.Progress >= 100.0 {
			statuses = append(statuses, *ts)
		}
	}

	return statuses, nil
}

// parseTorrent converts a raw torrent map to TorrentStatus.
func parseTorrent(raw interface{}) (*TorrentStatus, error) {
	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid torrent data")
	}

	getFloat := func(key string) float64 {
		v, _ := m[key].(float64)
		return v
	}
	getString := func(key string) string {
		v, _ := m[key].(string)
		return v
	}

	status := getString("status")
	statusMap := map[string]string{
		"1":  "downloading",
		"2":  "seeding",
		"3":  "stopped",
		"4":  "checking",
	}
	statusLabel := statusMap[status]
	if statusLabel == "" {
		statusLabel = status
	}

	downloaded := int64(getFloat("downloadedEver"))
	totalSize := int64(getFloat("sizeWhenDone"))
	var progress float64
	if totalSize > 0 {
		progress = (float64(downloaded) / float64(totalSize)) * 100
	}
	if progress > 100 {
		progress = 100
	}

	speed := int64(getFloat("rateDownload"))
	eta := int64(getFloat("eta"))
	if eta < 0 {
		eta = -1
	}

	return &TorrentStatus{
		ID:            int(getFloat("id")),
		Name:          getString("name"),
		Status:        statusLabel,
		Progress:      progress,
		Downloaded:    downloaded,
		TotalSize:     totalSize,
		DownloadSpeed: speed,
		ETA:           eta,
	}, nil
}

// FormatStatus returns a human-readable status string.
func FormatStatus(statuses []TorrentStatus) string {
	if len(statuses) == 0 {
		return "📥 No active downloads."
	}

	var sb strings.Builder
	sb.WriteString("📥 Active Downloads:\n\n")

	for i, ts := range statuses {
		sb.WriteString(fmt.Sprintf("%d. 🔥 %s\n", i+1, ts.Name))
		sb.WriteString(fmt.Sprintf("   ⬇️  Speed: %s\n", formatSpeed(ts.DownloadSpeed)))
		sb.WriteString(fmt.Sprintf("   📊 Progress: %.0f%%\n", ts.Progress))
		sb.WriteString(fmt.Sprintf("   💾 Downloaded: %s / %s\n",
			formatBytes(ts.Downloaded),
			formatBytes(ts.TotalSize),
		))
		if ts.ETA < 0 {
			sb.WriteString("   ⏱️  ETA: —\n")
		} else if ts.Status == "downloading" {
			sb.WriteString(fmt.Sprintf("   ⏱️  ETA: %s\n", formatETA(ts.ETA)))
		} else {
			sb.WriteString("   ⏱️  ETA: —\n")
		}

		if i < len(statuses)-1 {
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// FormatStatusWithCompleted returns a human-readable status string including completed torrents section.
func FormatStatusWithCompleted(active, completed []TorrentStatus) string {
	var sb strings.Builder

	if len(active) == 0 && len(completed) == 0 {
		return "📥 No downloads found."
	}

	if len(active) > 0 {
		sb.WriteString("📥 Active Downloads:\n\n")
		for i, ts := range active {
			sb.WriteString(fmt.Sprintf("%d. 🔥 %s\n", i+1, ts.Name))
			sb.WriteString(fmt.Sprintf("   ⬇️  Speed: %s\n", formatSpeed(ts.DownloadSpeed)))
			sb.WriteString(fmt.Sprintf("   📊 Progress: %.0f%%\n", ts.Progress))
			sb.WriteString(fmt.Sprintf("   💾 Downloaded: %s / %s\n",
				formatBytes(ts.Downloaded),
				formatBytes(ts.TotalSize),
			))
			if ts.ETA < 0 {
				sb.WriteString("   ⏱️  ETA: —\n")
			} else if ts.Status == "downloading" {
				sb.WriteString(fmt.Sprintf("   ⏱️  ETA: %s\n", formatETA(ts.ETA)))
			} else {
				sb.WriteString("   ⏱️  ETA: —\n")
			}

			if i < len(active)-1 {
				sb.WriteString("\n")
			}
		}
		sb.WriteString("\n")
	}

	if len(completed) > 0 {
		sb.WriteString("✅ Completed Downloads:\n\n")
		for i, ts := range completed {
			sb.WriteString(fmt.Sprintf("%d. ✅ %s\n", i+1, ts.Name))
			sb.WriteString(fmt.Sprintf("   💾 Size: %s\n", formatBytes(ts.TotalSize)))
			sb.WriteString(fmt.Sprintf("   📊 Status: %s\n", ts.Status))

			if i < len(completed)-1 {
				sb.WriteString("\n")
			}
		}
	}

	return sb.String()
}

// FormatAddResponse returns a success message after adding a torrent.
func FormatAddResponse(ts *TorrentStatus) string {
	return fmt.Sprintf("✅ Torrent added: %s\n   📊 Size: %s", ts.Name, formatBytes(ts.TotalSize))
}

// Helper formatting functions.

func formatSpeed(bps int64) string {
	if bps <= 0 {
		return "0 B/s"
	}
	return formatBytes(bps) + "/s"
}

func formatBytes(b int64) string {
	const (
		_  = iota
		KB = 1 << (10 * iota)
		MB
		GB
	)
	switch {
	case b >= GB:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.0f MB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.0f KB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func formatETA(seconds int64) string {
	if seconds <= 0 {
		return "—"
	}
	m := seconds / 60
	h := m / 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m%60)
	}
	return fmt.Sprintf("%dm", m)
}

// ValidateMagnetLink checks if a string looks like a valid magnet link.
func ValidateMagnetLink(link string) bool {
	return strings.HasPrefix(link, "magnet:?xt=urn:btih:")
}
