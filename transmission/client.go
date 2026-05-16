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
	url           string
	username      string
	password      string
	sessionID     string
	defaultFolder string
	categories    map[string]string
	currentDir    string
	http          *http.Client
}

// NewClient creates a new Transmission RPC client.
func NewClient(cfg Config) *Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	return &Client{
		url:           cfg.URL,
		username:      cfg.Username,
		password:      cfg.Password,
		defaultFolder: cfg.DefaultFolder,
		categories:    cfg.Categories,
		http: &http.Client{
			Timeout:   15 * time.Second,
			Transport: transport,
		},
	}
}

// GetConfig returns the client configuration.
func (c *Client) GetConfig() *Config {
	return &Config{
		URL:           c.url,
		Username:      c.username,
		Password:      c.password,
		DefaultFolder: c.defaultFolder,
		Categories:    c.categories,
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
	"id", "name", "status", "percentDone", "totalSize",
	"sizeWhenDone", "leftUntilDone", "eta", "rateDownload",
	"rateUpload", "uploadRatio", "downloadDir", "isFinished",
	"addedDate", "doneDate",
)

// torrentFieldsFull includes all available fields for detailed status.
var torrentFieldsFull = fieldsJSON(
	"id", "name", "status", "percentDone", "totalSize", "sizeWhenDone",
	"leftUntilDone", "eta", "rateDownload", "rateUpload", "uploadRatio",
	"downloadDir", "isFinished", "addedDate", "doneDate", "error", "errorString",
)

// SetDownloadDir sets the download directory for subsequent torrent additions.
func (c *Client) SetDownloadDir(subDir string) {
	c.currentDir = ""
	if subDir != "" {
		c.currentDir = c.defaultFolder + "/" + subDir
	}
}

// AddTorrent adds a torrent by magnet link or base64-encoded .torrent file.
// If the link starts with "magnet:", it's treated as a magnet link.
// Otherwise, it's treated as base64-encoded torrent metainfo.
func (c *Client) AddTorrent(data string, downloadDir string) (*TorrentStatus, error) {
	// Set download directory if specified
	if downloadDir != "" {
		c.SetDownloadDir(downloadDir)
	}

	// Build arguments based on whether it's a magnet or file
	argsMap := map[string]interface{}{}
	if strings.HasPrefix(data, "magnet:") {
		argsMap["filename"] = data
	} else {
		argsMap["metainfo"] = data
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

	// Check for duplicate torrent
	if dup, ok := added["torrent-duplicate"].(map[string]interface{}); ok {
		if idFloat, _ := dup["id"].(float64); idFloat > 0 {
			id := int(idFloat)
			return c.getTorrentStatusByID(id)
		}
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

// AddTorrentWithMagnet adds a torrent specifically by magnet link.
func (c *Client) AddTorrentWithMagnet(magnetLink string, downloadDir string) (*TorrentStatus, error) {
	return c.AddTorrent(magnetLink, downloadDir)
}

// AddTorrentFile adds a torrent from base64-encoded .torrent file content.
func (c *Client) AddTorrentFile(base64Content string, downloadDir string) (*TorrentStatus, error) {
	return c.AddTorrent(base64Content, downloadDir)
}

// CheckTorrentDuplicate checks if a torrent already exists and returns its ID.
func (c *Client) CheckTorrentDuplicate(magnetLink string) (int, string, error) {
	argsMap := map[string]interface{}{
		"filename": magnetLink,
	}
	args, _ := json.Marshal(argsMap)

	resp, err := c.doRPC("torrent-add", args, 0)
	if err != nil {
		return 0, "", err
	}

	var added map[string]interface{}
	if err := json.Unmarshal(resp.Arguments, &added); err != nil {
		return 0, "", err
	}

	// Check for duplicate
	if dup, ok := added["torrent-duplicate"].(map[string]interface{}); ok {
		if idFloat, _ := dup["id"].(float64); idFloat > 0 {
			name, _ := dup["name"].(string)
			return int(idFloat), name, ErrTorrentExists
		}
	}

	// Check for added torrent
	if torrents, ok := added["torrent-added"].([]interface{}); ok && len(torrents) > 0 {
		if torrentData, ok := torrents[0].(map[string]interface{}); ok {
			idFloat, _ := torrentData["id"].(float64)
			name, _ := torrentData["name"].(string)
			return int(idFloat), name, nil
		}
	}

	return 0, "", nil
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
	resp, err := c.doRPC("torrent-get", torrentFieldsFull, 1)
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
	resp, err := c.doRPC("torrent-get", torrentFieldsFull, 1)
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

// GetSessionStats returns Transmission session statistics.
func (c *Client) GetSessionStats() (*SessionStats, error) {
	resp, err := c.doRPC("session-stats", nil, 1)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(resp.Arguments, &result); err != nil {
		return nil, fmt.Errorf("parse session stats: %w", err)
	}

	stats := &SessionStats{}

	// Parse top-level fields
	if v, ok := result["active-torrent-count"].(float64); ok {
		stats.ActiveTorrentCount = int64(v)
	}
	if v, ok := result["paused-torrent-count"].(float64); ok {
		stats.PausedTorrentCount = int64(v)
	}
	if v, ok := result["torrent-count"].(float64); ok {
		stats.TorrentCount = int64(v)
	}
	if v, ok := result["download-speed"].(float64); ok {
		stats.DownloadSpeed = int64(v)
	}
	if v, ok := result["upload-speed"].(float64); ok {
		stats.UploadSpeed = int64(v)
	}

	// Parse current-stats
	if cs, ok := result["current-stats"].(map[string]interface{}); ok {
		parseStats(&stats.CurrentStats, cs)
	}

	// Parse cumulative-stats
	if cs, ok := result["cumulative-stats"].(map[string]interface{}); ok {
		parseStats(&stats.CumulativeStats, cs)
	}

	return stats, nil
}

// parseStats extracts stats from a map.
func parseStats(s *Stats, m map[string]interface{}) {
	if v, ok := m["downloadedBytes"].(float64); ok {
		s.DownloadedBytes = int64(v)
	}
	if v, ok := m["uploadedBytes"].(float64); ok {
		s.UploadedBytes = int64(v)
	}
	if v, ok := m["filesAdded"].(float64); ok {
		s.FilesAdded = int64(v)
	}
	if v, ok := m["sessionCount"].(float64); ok {
		s.SessionCount = int64(v)
	}
	if v, ok := m["secondsActive"].(float64); ok {
		s.SecondsActive = int64(v)
	}
}

// PauseTorrent stops a torrent by ID.
func (c *Client) PauseTorrent(id int) error {
	args, _ := json.Marshal(map[string]interface{}{
		"ids": []int{id},
	})
	_, err := c.doRPC("torrent-stop", args, 1)
	if err != nil {
		return fmt.Errorf("pause torrent %d: %w", id, err)
	}
	return nil
}

// ResumeTorrent starts/resumes a torrent by ID.
func (c *Client) ResumeTorrent(id int) error {
	args, _ := json.Marshal(map[string]interface{}{
		"ids": []int{id},
	})
	_, err := c.doRPC("torrent-start", args, 1)
	if err != nil {
		return fmt.Errorf("resume torrent %d: %w", id, err)
	}
	return nil
}

// RemoveTorrent removes a torrent by ID.
func (c *Client) RemoveTorrent(id int, deleteLocalData bool) error {
	args, _ := json.Marshal(map[string]interface{}{
		"ids":             []int{id},
		"delete-local-data": deleteLocalData,
	})
	_, err := c.doRPC("torrent-remove", args, 1)
	if err != nil {
		return fmt.Errorf("remove torrent %d: %w", id, err)
	}
	return nil
}

// RemoveTorrents removes multiple torrents.
func (c *Client) RemoveTorrents(ids []int, deleteLocalData bool) error {
	args, _ := json.Marshal(map[string]interface{}{
		"ids":             ids,
		"delete-local-data": deleteLocalData,
	})
	_, err := c.doRPC("torrent-remove", args, 1)
	if err != nil {
		return fmt.Errorf("remove torrents: %w", err)
	}
	return nil
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
		"0": "stopped",
		"1": "check wait",
		"2": "checking",
		"3": "download wait",
		"4": "downloading",
		"5": "seed wait",
		"6": "seeding",
	}
	statusLabel := statusMap[status]
	if statusLabel == "" {
		statusLabel = status
	}

	totalSize := int64(getFloat("totalSize"))
	percentDone := getFloat("percentDone")
	var progress float64
	if percentDone > 1.0 {
		progress = 100.0
	} else {
		progress = percentDone * 100.0
	}

	// Calculate downloaded from percentDone if available
	downloaded := int64(percentDone * float64(totalSize))
	if downloaded > totalSize {
		downloaded = totalSize
	}

	speed := int64(getFloat("rateDownload"))
	uploadSpeed := int64(getFloat("rateUpload"))
	ratio := getFloat("uploadRatio")
	eta := int64(getFloat("eta"))
	if eta < 0 {
		eta = -1
	}

	isFinished := false
	if v, ok := m["isFinished"].(bool); ok {
		isFinished = v
	}
	leftUntilDone := int64(getFloat("leftUntilDone"))

	return &TorrentStatus{
		ID:            int(getFloat("id")),
		Name:          getString("name"),
		Status:        statusLabel,
		Progress:      progress,
		Downloaded:    downloaded,
		TotalSize:     totalSize,
		DownloadSpeed: speed,
		UploadSpeed:   uploadSpeed,
		UploadRatio:   ratio,
		ETA:           eta,
		DownloadDir:   getString("downloadDir"),
		IsFinished:    isFinished,
		LeftUntilDone: leftUntilDone,
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
		statusEmoji := getTorrentStatusEmoji(ts.Status)
		sb.WriteString(fmt.Sprintf("%d. %s %s\n", i+1, statusEmoji, ts.Name))
		sb.WriteString(fmt.Sprintf("   ⬇️  Speed: %s\n", formatSpeed(ts.DownloadSpeed)))
		sb.WriteString(fmt.Sprintf("   ⬆️  Speed: %s\n", formatSpeed(ts.UploadSpeed)))
		sb.WriteString(fmt.Sprintf("   📊 Progress: %.1f%%\n", ts.Progress))
		sb.WriteString(fmt.Sprintf("   💾 Downloaded: %s / %s\n",
			formatBytes(ts.Downloaded),
			formatBytes(ts.TotalSize),
		))
		sb.WriteString(fmt.Sprintf("   📁 Upload Ratio: %.2f\n", ts.UploadRatio))
		if ts.ETA < 0 {
			sb.WriteString("   ⏱️  ETA: —\n")
		} else if ts.Status == "downloading" || ts.Status == "download wait" {
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

// FormatStatusWithSessionStats returns a human-readable status string with session stats.
func FormatStatusWithSessionStats(stats *SessionStats, active []TorrentStatus) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("📊 <b>Transmission Statistics</b>\n\n"))
	sb.WriteString(fmt.Sprintf("• Total torrents: %d\n", stats.TorrentCount))
	sb.WriteString(fmt.Sprintf("• Active downloads: %d\n", stats.ActiveTorrentCount))
	sb.WriteString(fmt.Sprintf("• Paused: %d\n", stats.PausedTorrentCount))
	sb.WriteString(fmt.Sprintf("• Download speed: %s\n", formatSpeed(stats.DownloadSpeed)))
	sb.WriteString(fmt.Sprintf("• Upload speed: %s\n", formatSpeed(stats.UploadSpeed)))
	sb.WriteString(fmt.Sprintf("• Total downloaded: %s\n", formatBytes(stats.CumulativeStats.DownloadedBytes)))
	sb.WriteString(fmt.Sprintf("• Total uploaded: %s\n", formatBytes(stats.CumulativeStats.UploadedBytes)))
	if stats.CumulativeStats.DownloadedBytes > 0 {
		ratio := float64(stats.CumulativeStats.UploadedBytes) / float64(stats.CumulativeStats.DownloadedBytes)
		sb.WriteString(fmt.Sprintf("• Overall ratio: %.2f\n", ratio))
	}
	sb.WriteString("\n")

	if len(active) > 0 {
		sb.WriteString("📥 <b>Active downloads:</b>\n\n")
		for i, ts := range active {
			statusEmoji := getTorrentStatusEmoji(ts.Status)
			sb.WriteString(fmt.Sprintf("%d. %s %s\n", i+1, statusEmoji, ts.Name))
			sb.WriteString(fmt.Sprintf("   Progress: %.1f%% | Speed: ⬇️%s ⬆️%s\n",
				ts.Progress, formatSpeed(ts.DownloadSpeed), formatSpeed(ts.UploadSpeed)))
			if i < len(active)-1 {
				sb.WriteString("\n")
			}
		}
	}

	return sb.String()
}

// FormatList returns a human-readable list of all torrents.
func FormatList(statuses []TorrentStatus) string {
	if len(statuses) == 0 {
		return "📭 No torrents found."
	}

	var sb strings.Builder
	sb.WriteString("📋 <b>All Torrents:</b>\n\n")

	for _, ts := range statuses {
		statusEmoji := getTorrentStatusEmoji(ts.Status)
		sb.WriteString(fmt.Sprintf("<b>%s</b> %s\n", statusEmoji, ts.Name))
		sb.WriteString(fmt.Sprintf("   ID: %d | Progress: %.1f%%\n", ts.ID, ts.Progress))
		sb.WriteString(fmt.Sprintf("   Size: %s | ⬇️ %s | ⬆️ %s\n",
			formatBytes(ts.TotalSize), formatSpeed(ts.DownloadSpeed), formatSpeed(ts.UploadSpeed)))
		sb.WriteString(fmt.Sprintf("   Ratio: %.2f | ETA: %s\n", ts.UploadRatio, formatETA(ts.ETA)))
		sb.WriteString(fmt.Sprintf("   Path: %s\n", ts.DownloadDir))
		sb.WriteString("\n")
	}

	return sb.String()
}

// getTorrentStatusEmoji returns an emoji for the torrent status.
func getTorrentStatusEmoji(status string) string {
	switch status {
	case "stopped":
		return "⏸️"
	case "check wait", "checking":
		return "🔍"
	case "download wait", "downloading":
		return "⬇️"
	case "seed wait", "seeding":
		return "⬆️"
	default:
		return "❓"
	}
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
