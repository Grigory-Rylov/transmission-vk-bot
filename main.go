package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"transmission-vk-bot/transmission"
)

// Config represents the application configuration.
type Config struct {
	VKToken        string            `json:"vk_token"`
	VKApiVersion   string            `json:"vk_api_version"`
	LongPollWait   int               `json:"long_poll_wait"`
	Transmission   *TransmissionConfig `json:"transmission"`
}

// TransmissionConfig holds Transmission RPC credentials.
type TransmissionConfig struct {
	URL         string `json:"url"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	DefaultFolder string `json:"default_folder"`
}

const defaultVKApiVersion = "5.200"
const defaultLongPollWait = 25

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Apply defaults
	if cfg.VKApiVersion == "" {
		cfg.VKApiVersion = defaultVKApiVersion
	}
	if cfg.LongPollWait <= 0 {
		cfg.LongPollWait = defaultLongPollWait
	}

	return &cfg, nil
}

type VKClient struct {
	token   string
	apiVer  string
	wait    int
	client  *http.Client
}

func NewVKClient(token, apiVer string, wait int) *VKClient {
	if wait <= 0 {
		wait = defaultLongPollWait
	}
	return &VKClient{
		token: token,
		apiVer: apiVer,
		wait: wait,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *VKClient) apiRequest(method string, params url.Values) (map[string]interface{}, error) {
	params.Set("access_token", c.token)
	params.Set("v", c.apiVer)
	
	url := fmt.Sprintf("https://api.vk.com/method/%s?%s", method, params.Encode())
	log.Printf("[API] Calling %s", method)
	
	resp, err := c.client.Get(url)
	if err != nil {
		log.Printf("[API] Request failed for %s: %v", method, err)
		return nil, err
	}
	defer resp.Body.Close()
	
	log.Printf("[API] Got response %d for %s", resp.StatusCode, method)
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[API] Failed to read body for %s: %v", method, err)
		return nil, err
	}
	
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		log.Printf("[API] Failed to parse JSON for %s: %v, body: %s", method, err, string(body))
		return nil, err
	}
	
	log.Printf("[API] Response for %s: %v", method, result)
	
	if errData, ok := result["error"].(map[string]interface{}); ok {
		log.Printf("[API] VK Error for %s: %v", method, errData)
		return nil, fmt.Errorf("VK API error: %v", errData)
	}
	
	return result, nil
}

func (c *VKClient) getLongPollServer() (server, key string, ts int, isUser bool, err error) {
	log.Printf("[LongPoll] Getting Long Poll server...")
	result, err := c.apiRequest("messages.getLongPollServer", url.Values{})
	if err != nil {
		log.Printf("[LongPoll] Failed to get server: %v", err)
		return "", "", 0, false, err
	}
	
	resp, ok := result["response"].(map[string]interface{})
	if !ok {
		log.Printf("[LongPoll] Invalid response format: %v", result)
		return "", "", 0, false, fmt.Errorf("invalid response format")
	}
	
	server = resp["server"].(string)
	key = resp["key"].(string)
	
	// Check if this is a personal (user) long poll server
	// Personal servers contain "nim" in the hostname (e.g., im.vk.com/nim238315447)
	isUser = strings.Contains(server, "/nim")
	
	// Both user and group modes use the same ts parameter for HTTP long poll
	// Try both "pts" and "ts" keys since VK API may return either
	var tsInt64 int64
	for _, key := range []string{"pts", "ts"} {
		if v, ok := resp[key]; ok {
			switch val := v.(type) {
			case float64:
				tsInt64 = int64(val)
			case string:
				if f, err := strconv.ParseFloat(val, 64); err == nil {
					tsInt64 = int64(f)
				}
			}
			break
		}
	}
	if tsInt64 < 0 {
		tsInt64 = 0
	}
	
	log.Printf("[LongPoll] Got server: %s, key: %s, ts: %d, isUser: %v", server, key[:20]+"...", tsInt64, isUser)
	return server, key, int(tsInt64), isUser, nil
}

func (c *VKClient) getMessagesById(msgIds []int) ([]map[string]interface{}, error) {
	idsStr := make([]string, len(msgIds))
	for i, id := range msgIds {
		idsStr[i] = strconv.Itoa(id)
	}
	
	params := url.Values{}
	params.Set("message_ids", strings.Join(idsStr, ","))
	
	log.Printf("[Messages] Getting messages by IDs: %v", msgIds)
	result, err := c.apiRequest("messages.getById", params)
	if err != nil {
		log.Printf("[Messages] Failed to get messages: %v", err)
		return nil, err
	}
	
	resp, ok := result["response"].(map[string]interface{})
	if !ok {
		log.Printf("[Messages] Invalid response format: %v", result)
		return nil, fmt.Errorf("invalid response format")
	}
	
	items, ok := resp["items"].([]interface{})
	if !ok {
		log.Printf("[Messages] No items in response")
		return nil, nil
	}
	
	log.Printf("[Messages] Got %d messages", len(items))
	messages := make([]map[string]interface{}, len(items))
	for i, item := range items {
		messages[i] = item.(map[string]interface{})
	}
	
	return messages, nil
}

func (c *VKClient) sendMessage(peerId int, text string) error {
	log.Printf("[Send] Sending message to peer %d: %q", peerId, text)
	params := url.Values{}
	params.Set("peer_id", strconv.Itoa(peerId))
	params.Set("random_id", strconv.FormatInt(time.Now().UnixMilli(), 10))
	params.Set("message", text)
	
	_, err := c.apiRequest("messages.send", params)
	if err != nil {
		log.Printf("[Send] Failed to send message: %v", err)
		return err
	}
	log.Printf("[Send] Message sent successfully to peer %d", peerId)
	return nil
}

type LongPollServer struct {
	server string
	key    string
	ts     int
}

func (lp *LongPollServer) refresh(vk *VKClient) error {
	server, key, ts, _, err := vk.getLongPollServer()
	if err != nil {
		return err
	}
	lp.server = server
	lp.key = key
	lp.ts = ts
	log.Printf("Group Long Poll server refreshed: %s", server)
	return nil
}

func (lp *LongPollServer) getEvents(vk *VKClient) ([]interface{}, int, error) {
	params := url.Values{}
	params.Set("act", "a_check")
	params.Set("key", lp.key)
	params.Set("ts", strconv.Itoa(lp.ts))
	params.Set("wait", strconv.Itoa(vk.wait))
	params.Set("mode", "74")
	params.Set("version", "3")

	serverURL := lp.server
	if !strings.HasPrefix(serverURL, "http://") && !strings.HasPrefix(serverURL, "https://") {
		serverURL = "https://" + serverURL
	}
	fullURL := fmt.Sprintf("%s?%s", serverURL, params.Encode())
	log.Printf("[LongPoll] Waiting for events from %s (ts=%d)...", lp.server, lp.ts)

	// Timeout should cover wait time + connection time
	// For user mode, wait can be longer
	timeout := time.Duration(vk.wait+15) * time.Second
	if timeout < 40*time.Second {
		timeout = 40 * time.Second
	}
	log.Printf("[LongPoll] HTTP client timeout: %v", timeout)
	client := &http.Client{
		Timeout: timeout,
	}
	
	resp, err := client.Get(fullURL)
	if err != nil {
		log.Printf("[LongPoll] Request failed: %v", err)
		return nil, 0, err
	}
	defer resp.Body.Close()
	
	log.Printf("[LongPoll] Connection established, reading body...")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[LongPoll] Failed to read body: %v", err)
		return nil, 0, err
	}
	log.Printf("[LongPoll] Read %d bytes", len(body))
	
	log.Printf("[LongPoll] Got response %d, body: %s", resp.StatusCode, string(body))
	
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		log.Printf("[LongPoll] Failed to parse JSON: %v, body: %s", err, string(body))
		return nil, 0, err
	}
	
	log.Printf("[LongPoll] Raw response: %v", result)
	
	if failed, ok := result["failed"]; ok {
		log.Printf("[LongPoll] Server returned failed: %v", failed)
		return nil, 0, fmt.Errorf("long poll failed: %v", result)
	}
	
	updates, ok := result["updates"].([]interface{})
	if !ok {
		log.Printf("[LongPoll] No updates in response")
		return []interface{}{}, int(result["ts"].(float64)), nil
	}
	
	log.Printf("[LongPoll] Got %d updates", len(updates))
	
	var ts int
	switch v := result["ts"].(type) {
	case float64:
		ts = int(v)
	case string:
		ts, _ = strconv.Atoi(v)
	default:
		ts = 0
	}
	
	return updates, ts, nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("=== Starting VK Bot ===")

	// Load configuration
	cfg, err := loadConfig("config.json")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	log.Printf("Config loaded: vk_token=%s...", cfg.VKToken[:min(len(cfg.VKToken), 10)])
	log.Printf("Long poll wait: %d", cfg.LongPollWait)

	// Initialize VK client
	vk := NewVKClient(cfg.VKToken, cfg.VKApiVersion, cfg.LongPollWait)

// Initialize Transmission client (optional)
	var txClient *transmission.Client
	if cfg.Transmission != nil && cfg.Transmission.URL != "" {
		txCfg := transmission.Config{
			URL:         cfg.Transmission.URL,
			Username:    cfg.Transmission.Username,
			Password:    cfg.Transmission.Password,
			DefaultFolder: cfg.Transmission.DefaultFolder,
		}
		txClient = transmission.NewClient(txCfg)
		log.Printf("Transmission client initialized: %s (default: %s)", cfg.Transmission.URL, cfg.Transmission.DefaultFolder)
	}

	// Initialize Long Poll
	log.Println("Initializing Long Poll...")
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Shutting down...")
		os.Exit(0)
	}()

	server, key, ts, isUserMode, err := vk.getLongPollServer()
	if err != nil {
		log.Fatalf("Failed to get Long Poll server: %v", err)
	}

	if isUserMode {
		log.Println("Detected personal bot - using User Long Poll (mode 74)")
	} else {
		log.Println("Detected community bot - using Group Long Poll")
	}

	// Use the same LongPollServer for both modes (HTTP long poll with mode 74)
	lp := &LongPollServer{server: server, key: key, ts: ts}

	log.Println("=== Bot is running. Press Ctrl+C to exit ===")

	for {
		log.Println("---> Starting Long Poll wait")
		updates, newTs, err := lp.getEvents(vk)
		if err != nil {
			log.Printf("!!! Long Poll error: %v. Reconnecting...", err)
			lp.refresh(vk)
			time.Sleep(3 * time.Second)
			continue
		}
		lp.ts = newTs

		log.Printf("<<< Got updates, ts=%d\n", newTs)

		if len(updates) == 0 {
			continue
		}

		log.Printf("Processing %d updates", len(updates))
		for i, update := range updates {
			log.Printf("[%d] Processing update", i)
			event, ok := update.([]interface{})
			if !ok {
				log.Printf("[%d] Invalid update format: %T", i, update)
				continue
			}

			log.Printf("[%d] Event array: %v", i, event)

			if len(event) < 2 {
				log.Printf("[%d] Event too short, skipping", i)
				continue
			}

			eventType := int(event[0].(float64))
			log.Printf("[%d] eventType=%d", i, eventType)

			if eventType != 4 {
				log.Printf("[%d] Not a message event (type=%d), skipping", i, eventType)
				continue
			}

			if len(event) < 6 {
				log.Printf("[%d] Message event too short (need 6 fields), skipping", i)
				continue
			}

			msgId := int(event[1].(float64))
			flags := int(event[2].(float64))
			peerId := int(event[3].(float64))

			log.Printf("[%d] msgId=%d, flags=%d, peerId=%d", i, msgId, flags, peerId)

			if flags&2 != 0 {
				log.Printf("[%d] Message is from itself (flag 2), skipping", i)
				continue
			}

			text := ""
			if len(event) > 5 {
				text = event[5].(string)
			}

			log.Printf("[%d] *** NEW MESSAGE from peer %d: %q ***", i, peerId, text)

			if strings.TrimSpace(text) == "" {
				log.Printf("[%d] Empty message, skipping", i)
				continue
			}

			// Check if this is a command
			text = strings.TrimSpace(text)
			if strings.HasPrefix(text, "/") && txClient != nil {
				// Handle Transmission commands
				handleCommand(vk, peerId, text, txClient)
				continue
			}

			// Fall back to echo for non-command messages
			log.Printf("[%d] Sending echo back...", i)
			if err := vk.sendMessage(peerId, text); err != nil {
				log.Printf("[%d] !!! Failed to send echo: %v", i, err)
			} else {
				log.Printf("[%d] *** Echo sent successfully to peer %d ***", i, peerId)
			}
		}
	}
}

// mapFolderTag converts a tag like "#ns" to a subfolder path.
func mapFolderTag(tag string) string {
	switch strings.ToLower(tag) {
	case "#ns":
		return "roms/ns"
	case "#games":
		return "games"
	case "#m", "#movie":
		return "movies"
	case "#music":
		return "music"
	default:
		return ""
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// handleCommand processes Transmission-related commands.
func handleCommand(vk *VKClient, peerId int, text string, txClient *transmission.Client) {
	parts := strings.SplitN(text, " ", 2)
	cmd := strings.ToLower(parts[0])
	args := ""
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}

	switch cmd {
	case "/help", "/h":
		handleHelp(vk, peerId)
	case "/add", "/a":
		// Split args: magnet link and optional folder tag
		magnetLink := args
		folderTag := ""
		spParts := strings.SplitN(args, " ", 2)
		if len(spParts) > 1 && strings.HasPrefix(spParts[1], "#") {
			magnetLink = spParts[0]
			folderTag = spParts[1]
		}
		handleAdd(vk, peerId, magnetLink, folderTag, txClient)
	case "/status", "/s":
		showAll := strings.EqualFold(args, "all")
		handleStatus(vk, peerId, txClient, showAll)
	default:
		// Unknown command - echo back
		if err := vk.sendMessage(peerId, fmt.Sprintf("Unknown command: %s. Use /help for list.", cmd)); err != nil {
			log.Printf("Failed to send message: %v", err)
		}
	}
}

func handleHelp(vk *VKClient, peerId int) {
	msg := "🔧 **Transmission Bot - Commands**\n\n" +
		"/add /a <magnet-link> [#tag] - Add torrent for download\n" +
		"   #ns -> roms/ns | #games -> games | #m/#movie -> movies | #music -> music\n" +
		"/status /s - Show active downloads (not 100%)\n" +
		"/status all - Show all downloads\n" +
		"/help /h - Show this help\n\n" +
		"Examples:\n" +
		"/add magnet:?xt=urn:btih:abc123... #ns\n" +
		"/add magnet:?xt=urn:btih:abc123... #games\n" +
		"/add magnet:?xt=urn:btih:abc123...\n" +
		"/status\n" +
		"/status all"
	if err := vk.sendMessage(peerId, msg); err != nil {
		log.Printf("Failed to send help: %v", err)
	}
}

func handleAdd(vk *VKClient, peerId int, magnetLink string, folderTag string, txClient *transmission.Client) {
	if magnetLink == "" {
		if err := vk.sendMessage(peerId, "❌ Usage: /add <magnet-link> [#tag]"); err != nil {
			log.Printf("Failed to send message: %v", err)
		}
		return
	}

	if !transmission.ValidateMagnetLink(magnetLink) {
		if err := vk.sendMessage(peerId, "❌ Invalid magnet link format. Use: magnet:?xt=urn:btih:..."); err != nil {
			log.Printf("Failed to send message: %v", err)
		}
		return
	}

	// Map folder tag to subdirectory
	var folderPath string
	if folderTag != "" {
		folderPath = mapFolderTag(folderTag)
		if folderPath == "" {
			if err := vk.sendMessage(peerId, "❌ Unknown folder tag: "+folderTag+"\nValid: #ns #games #m/#movie #music"); err != nil {
				log.Printf("Failed to send message: %v", err)
			}
			return
		}
	}

	log.Printf("Adding torrent: %s... (folder: %s)", magnetLink[:min(len(magnetLink), 50)], folderPath)
	torrent, err := txClient.AddTorrent(magnetLink, folderPath)
	if err != nil {
		var errMsg string
		if err == transmission.ErrTransmissionUnavailable {
			errMsg = "⚠️ Transmission is not available. Please check connection."
		} else if err == transmission.ErrTorrentExists {
			errMsg = "ℹ️ Torrent already exists in Transmission."
		} else {
			errMsg = fmt.Sprintf("❌ Error adding torrent: %v", err)
		}
		if sendErr := vk.sendMessage(peerId, errMsg); sendErr != nil {
			log.Printf("Failed to send message: %v", sendErr)
		}
		return
	}

	msg := transmission.FormatAddResponse(torrent)
	if folderPath != "" {
		msg += fmt.Sprintf("\n   📁 Folder: %s", folderPath)
	}
	if err := vk.sendMessage(peerId, msg); err != nil {
		log.Printf("Failed to send message: %v", err)
	}
}

func handleStatus(vk *VKClient, peerId int, txClient *transmission.Client, showAll bool) {
	var statuses []transmission.TorrentStatus
	var err error

	if showAll {
		statuses, err = txClient.GetStatus()
	} else {
		statuses, err = txClient.GetActiveStatus()
	}

	if err != nil {
		if err == transmission.ErrTransmissionUnavailable {
			if sendErr := vk.sendMessage(peerId, "⚠️ Transmission is not available. Please check connection."); sendErr != nil {
				log.Printf("Failed to send message: %v", sendErr)
			}
		} else {
			if sendErr := vk.sendMessage(peerId, fmt.Sprintf("❌ Error getting status: %v", err)); sendErr != nil {
				log.Printf("Failed to send message: %v", err)
			}
		}
		return
	}

	msg := transmission.FormatStatus(statuses)
	if err := vk.sendMessage(peerId, msg); err != nil {
		log.Printf("Failed to send message: %v", err)
	}
}
