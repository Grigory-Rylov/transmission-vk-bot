package main

import (
	"bytes"
	"encoding/base64"
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
	VKToken        string              `json:"vk_token"`
	VKApiVersion   string              `json:"vk_api_version"`
	LongPollWait   int                 `json:"long_poll_wait"`
	AdminIDs       []int64             `json:"admin_ids"`
	Transmission   *TransmissionConfig `json:"transmission"`
}

// TransmissionConfig holds Transmission RPC credentials.
type TransmissionConfig struct {
	URL           string            `json:"url"`
	Username      string            `json:"username"`
	Password      string            `json:"password"`
	DefaultFolder string            `json:"default_folder"`
	Categories    map[string]string `json:"categories"`
}

const defaultVKApiVersion = "5.200"
const defaultLongPollWait = 25

// txClientGlobal - глобальный клиент Transmission для обработки keyboard events
var txClientGlobal *transmission.Client

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
	
	u := fmt.Sprintf("https://api.vk.com/method/%s?%s", method, params.Encode())
	log.Printf("[API] Calling %s", method)
	
	resp, err := c.client.Get(u)
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

func (c *VKClient) sendMessage(peerId int, text string, keyboard interface{}) error {
	log.Printf("[Send] Sending message to peer %d: %q", peerId, text)
	params := url.Values{}
	params.Set("peer_id", strconv.Itoa(peerId))
	params.Set("random_id", strconv.FormatInt(time.Now().UnixMilli(), 10))
	params.Set("message", text)
	
	// Добавляем клавиатуру если она передана
	if keyboard != nil {
		if kb, ok := keyboard.(map[string]interface{}); ok {
			kbJSON, err := json.Marshal(kb)
			if err != nil {
				log.Printf("[Send] Failed to marshal keyboard: %v", err)
			} else {
				params.Set("keyboard", string(kbJSON))
				log.Printf("[Send] Keyboard attached: %s", string(kbJSON[:min(len(kbJSON), 100)]))
			}
		}
	}
	
	_, err := c.apiRequest("messages.send", params)
	if err != nil {
		log.Printf("[Send] Failed to send message: %v", err)
		return err
	}
	log.Printf("[Send] Message sent successfully to peer %d", peerId)
	return nil
}

// sendMessagePlain sends a message without keyboard.
func (c *VKClient) sendMessagePlain(peerId int, text string) error {
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

// downloadVKFile скачивает файл из VK по attachment reference (attachment_12345_67890)
func (c *VKClient) downloadVKFile(attachment string) ([]byte, error) {
	// Формат: type_ownerid_fileid (например: photo_12345_67890 или doc_12345_67890)
	parts := strings.Split(attachment, "_")
	if len(parts) < 3 {
		return nil, fmt.Errorf("invalid attachment format: %s", attachment)
	}
	
	ownerID := parts[1]
	fileID := parts[2]
	
	// Получаем URL файла
	params := url.Values{}
	params.Set("access_token", c.token)
	params.Set("v", c.apiVer)
	params.Set("oid", ownerID)
	params.Set("fid", fileID)
	
	uploadServerURL := fmt.Sprintf("https://api.vk.com/method/docs.getMessageUploadServer?%s", params.Encode())
	resp, err := c.client.Get(uploadServerURL)
	if err != nil {
		return nil, fmt.Errorf("get upload server: %w", err)
	}
	defer resp.Body.Close()
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read upload server response: %w", err)
	}
	
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse upload server response: %w", err)
	}
	
	if errData, ok := result["error"].(map[string]interface{}); ok {
		return nil, fmt.Errorf("VK API error getting upload server: %v", errData)
	}
	
	respData, ok := result["response"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid upload server response format")
	}
	
	_, ok = respData["upload_url"]
	if !ok {
		return nil, fmt.Errorf("no upload_url in response")
	}
	
	// Получаем информацию о документе для получения download URL
	params2 := url.Values{}
	params2.Set("access_token", c.token)
	params2.Set("v", c.apiVer)
	params2.Set("owner_id", ownerID)
	params2.Set("doc_id", fileID)
	
	docsURL := fmt.Sprintf("https://api.vk.com/method/docs.get?%s", params2.Encode())
	docsResp, err := c.client.Get(docsURL)
	if err != nil {
		return nil, fmt.Errorf("get doc info: %w", err)
	}
	defer docsResp.Body.Close()
	
	docsBody, err := io.ReadAll(docsResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read doc info: %w", err)
	}
	
	var docsResult map[string]interface{}
	if err := json.Unmarshal(docsBody, &docsResult); err != nil {
		return nil, fmt.Errorf("parse doc info: %w", err)
	}
	
	if errData, ok := docsResult["error"].(map[string]interface{}); ok {
		return nil, fmt.Errorf("VK API error getting doc: %v", errData)
	}
	
	docsRespData, ok := docsResult["response"].([]interface{})
	if !ok || len(docsRespData) == 0 {
		return nil, fmt.Errorf("no doc data")
	}
	
	docInfo, ok := docsRespData[0].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid doc info format")
	}
	
	fileURL, ok := docInfo["url"].(string)
	if !ok || fileURL == "" {
		// Если нет прямого URL, используем alternative способ
		log.Printf("[File] No direct URL, trying alternative download method")
		// Альтернативный метод через attachments
		return c.downloadViaAttachments(ownerID, fileID)
	}
	
	// Скачиваем файл
	fileResp, err := c.client.Get(fileURL)
	if err != nil {
		return nil, fmt.Errorf("download file: %w", err)
	}
	defer fileResp.Body.Close()
	
	fileData, err := io.ReadAll(fileResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read file data: %w", err)
	}
	
	log.Printf("[File] Downloaded %d bytes from %s", len(fileData), fileURL)
	return fileData, nil
}

// downloadViaAttachments скачивает файл через метод attachments
func (c *VKClient) downloadViaAttachments(ownerID, fileID string) ([]byte, error) {
	// Используем метод docs.get для получения информации о файле
	params := url.Values{}
	params.Set("access_token", c.token)
	params.Set("v", c.apiVer)
	params.Set("docs", ownerID+"_"+fileID)
	
	docsURL := fmt.Sprintf("https://api.vk.com/method/docs.get?%s", params.Encode())
	docsResp, err := c.client.Get(docsURL)
	if err != nil {
		return nil, fmt.Errorf("get doc info via attachments: %w", err)
	}
	defer docsResp.Body.Close()
	
	docsBody, err := io.ReadAll(docsResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read doc info: %w", err)
	}
	
	var docsResult map[string]interface{}
	if err := json.Unmarshal(docsBody, &docsResult); err != nil {
		return nil, fmt.Errorf("parse doc info: %w", err)
	}
	
	if errData, ok := docsResult["error"].(map[string]interface{}); ok {
		log.Printf("[File] Error getting doc via attachments: %v", errData)
		return nil, fmt.Errorf("VK API error getting doc: %v", errData)
	}
	
	docsRespData, ok := docsResult["response"].([]interface{})
	if !ok || len(docsRespData) == 0 {
		return nil, fmt.Errorf("no doc data via attachments")
	}
	
	docInfo, ok := docsRespData[0].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid doc info format via attachments")
	}
	
	fileURL, ok := docInfo["url"].(string)
	if !ok || fileURL == "" {
		return nil, fmt.Errorf("no download URL available")
	}
	
	// Скачиваем файл
	fileResp, err := c.client.Get(fileURL)
	if err != nil {
		return nil, fmt.Errorf("download file via attachments: %w", err)
	}
	defer fileResp.Body.Close()
	
	fileData, err := io.ReadAll(fileResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read file data: %w", err)
	}
	
	log.Printf("[File] Downloaded %d bytes via attachments", len(fileData))
	return fileData, nil
}

// sendDocument отправляет документ (файл) в VK
func (c *VKClient) sendDocument(peerId int, fileData []byte, fileName string) error {
	log.Printf("[File] Sending document: %s (%d bytes)", fileName, len(fileData))
	
	// Шаг 1: Получаем сервер для загрузки
	params := url.Values{}
	params.Set("access_token", c.token)
	params.Set("v", c.apiVer)
	params.Set("act", "a_do_upload")
	
	uploadURL := fmt.Sprintf("https://api.vk.com/method/docs.sendMessageUpload?%s", params.Encode())
	
	// Шаг 2: Загружаем файл
	resp, err := http.Post(uploadURL, "application/octet-stream", bytes.NewReader(fileData))
	if err != nil {
		log.Printf("[File] Upload failed: %v", err)
		return fmt.Errorf("upload file: %w", err)
	}
	defer resp.Body.Close()
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read upload response: %w", err)
	}
	
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parse upload response: %w", err)
	}
	
	// Проверяем на ошибки
	if errData, ok := result["error"].(map[string]interface{}); ok {
		return fmt.Errorf("VK API upload error: %v", errData)
	}
	
	// Получаем данные для отправки
	respData, ok := result["response"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid upload response format")
	}
	
	// Шаг 3: Сохраняем документ
	saveParams := url.Values{}
	saveParams.Set("access_token", c.token)
	saveParams.Set("v", c.apiVer)
	saveParams.Set("user_id", strconv.Itoa(peerId))
	saveParams.Set("title", fileName)
	
	if fileURL, ok := respData["file_url"].(string); ok {
		saveParams.Set("file", fileURL)
	}
	
	saveURL := fmt.Sprintf("https://api.vk.com/method/docs.add?%s", saveParams.Encode())
	saveResp, err := http.Post(saveURL, "application/x-www-form-urlencoded", strings.NewReader(saveParams.Encode()))
	if err != nil {
		log.Printf("[File] Save failed: %v", err)
		return fmt.Errorf("save document: %w", err)
	}
	defer saveResp.Body.Close()
	
	saveBody, err := io.ReadAll(saveResp.Body)
	if err != nil {
		return fmt.Errorf("read save response: %w", err)
	}
	
	var saveResult map[string]interface{}
	if err := json.Unmarshal(saveBody, &saveResult); err != nil {
		return fmt.Errorf("parse save response: %w", err)
	}
	
	if errData, ok := saveResult["error"].(map[string]interface{}); ok {
		return fmt.Errorf("VK API save error: %v", errData)
	}
	
	log.Printf("[File] Document saved successfully")
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
	log.Printf("Admin IDs: %v", cfg.AdminIDs)

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
			Categories:  cfg.Transmission.Categories,
		}
		txClient = transmission.NewClient(txCfg)
		txClientGlobal = txClient
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

			// Обработка клика по кнопке клавиатуры (VK API event type 18)
			if eventType == 18 && len(event) >= 6 {
				log.Printf("[%d] Keyboard button click detected", i)
				handleKeyboardClick(vk, event)
				continue
			}

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

			// Обработка документов (VK API event type 9 - document)
			if eventType == 4 && len(event) >= 10 {
				// Проверяем, есть ли документ в событии
				docAttachment := ""
				for j := 6; j < len(event); j++ {
					if str, ok := event[j].(string); ok && strings.HasPrefix(str, "doc_") {
						docAttachment = str
						break
					}
				}
				
				if docAttachment != "" {
					log.Printf("[%d] Document found: %s", i, docAttachment)
					handleDocument(vk, peerId, docAttachment, txClient)
					continue
				}
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

			// Проверка доступа администратора
			if !isAdmin(peerId, cfg.AdminIDs) {
				log.Printf("[%d] Access denied for non-admin peer %d", i, peerId)
				if err := vk.sendMessage(peerId, "❌ У вас нет доступа к этому боту", MainKeyboard()); err != nil {
					log.Printf("[%d] Failed to send access denied: %v", i, err)
				}
				continue
			}

			// Check if this is a command
			text = strings.TrimSpace(text)
			if strings.HasPrefix(text, "/") && txClient != nil {
				// Handle Transmission commands
				handleCommand(vk, peerId, text, txClient, cfg)
				continue
			}

			// Fall back to echo for non-command messages
			log.Printf("[%d] Sending echo back...", i)
			if err := vk.sendMessage(peerId, text, MainKeyboard()); err != nil {
				log.Printf("[%d] !!! Failed to send echo: %v", i, err)
			} else {
				log.Printf("[%d] *** Echo sent successfully to peer %d ***", i, peerId)
			}
		}
	}
}

// isAdmin проверяет, является ли пользователь администратором
func isAdmin(peerId int, adminIDs []int64) bool {
	if len(adminIDs) == 0 {
		return true // Если админы не настроены, все имеют доступ
	}
	for _, id := range adminIDs {
		if int64(peerId) == id {
			return true
		}
	}
	return false
}

// mapFolderTag converts a tag like "#ns" to a subfolder path.
func mapFolderTag(tag string, categories map[string]string) string {
	tag = strings.ToLower(tag)
	
	// Сначала проверяем в конфигурации
	if categories != nil {
		if dir, ok := categories[tag]; ok {
			return dir
		}
		// Без #
		if dir, ok := categories[strings.TrimPrefix(tag, "#")]; ok {
			return dir
		}
	}
	
	// Дефолтные значения
	switch tag {
	case "#ns":
		return "roms/ns"
	case "#games":
		return "games"
	case "#g":
		return "games"
	case "#m", "#movie":
		return "movies"
	case "#music":
		return "music"
	case "#book":
		return "books"
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
func handleCommand(vk *VKClient, peerId int, text string, txClient *transmission.Client, cfg *Config) {
	parts := strings.SplitN(text, " ", 2)
	cmd := strings.ToLower(parts[0])
	args := ""
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}

	switch cmd {
	case "/start", "/begin":
		handleStart(vk, peerId)
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
		handleAdd(vk, peerId, magnetLink, folderTag, txClient, cfg)
	case "/status", "/s":
		showAll := strings.EqualFold(args, "all")
		handleStatus(vk, peerId, txClient, showAll, cfg)
	case "/list", "/ls":
		handleList(vk, peerId, txClient)
	case "/pause", "/p":
		handlePause(vk, peerId, args, txClient)
	case "/resume", "/r":
		handleResume(vk, peerId, args, txClient)
	case "/remove", "/rm", "/del":
		handleRemove(vk, peerId, args, txClient)
	default:
		// Unknown command - echo back
		if err := vk.sendMessage(peerId, fmt.Sprintf("Unknown command: %s. Use /help for list.", cmd), MainKeyboard()); err != nil {
			log.Printf("Failed to send message: %v", err)
		}
	}
}

func handleStart(vk *VKClient, peerId int) {
	msg := "🎬 <b>Transmission Bot - VK Edition</b>\n\n" +
	"<b>DAvailable commands:</b>\n" +
	"<code>/add /a</code> <magnet-link> [#tag] - Add torrent\n" +
	"<code>/status /s</code> - Show active downloads\n" +
	"<code>/status all</code> - Show all downloads\n" +
	"<code>/list /ls</code> - List all torrents\n" +
	"<code>/pause /p</code> <id> - Pause torrent\n" +
	"<code>/resume /r</code> <id> - Resume torrent\n" +
	"<code>/remove /rm</code> <id> [-d] - Remove torrent\n" +
	"<code>/help /h</code> - Show this help\n\n" +
	"<b>CATEGORIES:</b> #movie #m #ns #games #g #music #book\n" +
	"Or send a .torrent file as a document with category caption\n\n" +
	"<b>Examples:</b>\n" +
	"/add magnet:?xt=urn:btih:abc123... #ns\n" +
	"/add magnet:?xt=urn:btih:abc123... #games"
	
	if err := vk.sendMessage(peerId, msg, MainKeyboard()); err != nil {
		log.Printf("Failed to send start message: %v", err)
	}
}

func handleHelp(vk *VKClient, peerId int) {
	msg := "🔧 <b>Transmission Bot - Commands</b>\n\n" +
	"/add /a <magnet-link> [#tag] - Add torrent for download\n" +
	"   #ns -> roms/ns | #games -> games | #m/#movie -> movies | #music -> music | #book -> books\n" +
	"/status /s - Show active downloads (not 100%)\n" +
	"/status all - Show all downloads\n" +
	"/list /ls - List all torrents\n" +
	"/pause /p <id> - Pause torrent\n" +
	"/resume /r <id> - Resume torrent\n" +
	"/remove /rm <id> [-d] - Remove torrent (optionally delete files)\n" +
	"/help /h - Show this help\n\n" +
	"Also you can send a .torrent file as a document with category in caption.\n\n" +
	"Examples:\n" +
	"/add magnet:?xt=urn:btih:abc123... #ns\n" +
	"/add magnet:?xt=urn:btih:abc123... #games\n" +
	"/add magnet:?xt=urn:btih:abc123...\n" +
	"/status\n" +
	"/status all\n" +
	"/list\n" +
	"/pause 1\n" +
	"/resume 1\n" +
	"/remove 1\n" +
	"/remove 1 -d"
	if err := vk.sendMessage(peerId, msg, MainKeyboard()); err != nil {
		log.Printf("Failed to send help: %v", err)
	}
}

func handleAdd(vk *VKClient, peerId int, magnetLink string, folderTag string, txClient *transmission.Client, cfg *Config) {
	if magnetLink == "" {
		if err := vk.sendMessage(peerId, "❌ Usage: /add <magnet-link> [#tag]", MainKeyboard()); err != nil {
			log.Printf("Failed to send message: %v", err)
		}
		return
	}

	if !transmission.ValidateMagnetLink(magnetLink) {
		if err := vk.sendMessage(peerId, "❌ Invalid magnet link format. Use: magnet:?xt=urn:btih:...", MainKeyboard()); err != nil {
			log.Printf("Failed to send message: %v", err)
		}
		return
	}

	// Map folder tag to subdirectory
	var folderPath string
	if folderTag != "" {
		folderPath = mapFolderTag(folderTag, cfg.Transmission.Categories)
		if folderPath == "" {
			if err := vk.sendMessage(peerId, "❌ Unknown folder tag: "+folderTag+"\nValid: #ns #games #m/#movie #music #book", MainKeyboard()); err != nil {
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
		if sendErr := vk.sendMessage(peerId, errMsg, MainKeyboard()); sendErr != nil {
			log.Printf("Failed to send message: %v", sendErr)
		}
		return
	}

	msg := transmission.FormatAddResponse(torrent)
	if folderPath != "" {
		msg += fmt.Sprintf("\n   📁 Folder: %s", folderPath)
	}
	if err := vk.sendMessage(peerId, msg, MainKeyboard()); err != nil {
		log.Printf("Failed to send message: %v", err)
	}
}

func handleStatus(vk *VKClient, peerId int, txClient *transmission.Client, showAll bool, cfg *Config) {
	var statuses []transmission.TorrentStatus
	var err error

	if showAll {
		statuses, err = txClient.GetStatus()
	} else {
		statuses, err = txClient.GetActiveStatus()
	}

	if err != nil {
		if err == transmission.ErrTransmissionUnavailable {
			if sendErr := vk.sendMessage(peerId, "⚠️ Transmission is not available. Please check connection.", MainKeyboard()); sendErr != nil {
				log.Printf("Failed to send message: %v", sendErr)
			}
		} else {
			if sendErr := vk.sendMessage(peerId, fmt.Sprintf("❌ Error getting status: %v", err), MainKeyboard()); sendErr != nil {
				log.Printf("Failed to send message: %v", sendErr)
			}
		}
		return
	}

	// Получаем session stats
	sessionStats, statsErr := txClient.GetSessionStats()
	
	var msg string
	if statsErr == nil && sessionStats != nil {
		msg = transmission.FormatStatusWithSessionStats(sessionStats, statuses)
	} else {
		msg = transmission.FormatStatus(statuses)
	}
	
	if err := vk.sendMessage(peerId, msg, MainKeyboard()); err != nil {
		log.Printf("Failed to send message: %v", err)
	}
}

func handleList(vk *VKClient, peerId int, txClient *transmission.Client) {
	statuses, err := txClient.GetStatus()
	if err != nil {
		if err == transmission.ErrTransmissionUnavailable {
			if sendErr := vk.sendMessage(peerId, "⚠️ Transmission is not available. Please check connection.", MainKeyboard()); sendErr != nil {
				log.Printf("Failed to send message: %v", sendErr)
			}
		} else {
			if sendErr := vk.sendMessage(peerId, fmt.Sprintf("❌ Error getting list: %v", err), MainKeyboard()); sendErr != nil {
				log.Printf("Failed to send message: %v", sendErr)
			}
		}
		return
	}

	msg := transmission.FormatList(statuses)
	if err := vk.sendMessage(peerId, msg, MainKeyboard()); err != nil {
		log.Printf("Failed to send message: %v", err)
	}
}

func handlePause(vk *VKClient, peerId int, args string, txClient *transmission.Client) {
	if args == "" {
		if err := vk.sendMessage(peerId, "❌ Usage: /pause <id>", MainKeyboard()); err != nil {
			log.Printf("Failed to send message: %v", err)
		}
		return
	}

	id, err := strconv.Atoi(args)
	if err != nil {
		if sendErr := vk.sendMessage(peerId, "❌ Invalid ID format", MainKeyboard()); sendErr != nil {
			log.Printf("Failed to send message: %v", sendErr)
		}
		return
	}

	err = txClient.PauseTorrent(id)
	if err != nil {
		if sendErr := vk.sendMessage(peerId, "❌ Error pausing torrent: "+err.Error(), MainKeyboard()); sendErr != nil {
			log.Printf("Failed to send message: %v", sendErr)
		}
		return
	}

	if err := vk.sendMessage(peerId, fmt.Sprintf("⏸️ Torrent ID %d paused", id), MainKeyboard()); err != nil {
		log.Printf("Failed to send message: %v", err)
	}
}

func handleResume(vk *VKClient, peerId int, args string, txClient *transmission.Client) {
	if args == "" {
		if err := vk.sendMessage(peerId, "❌ Usage: /resume <id>", MainKeyboard()); err != nil {
			log.Printf("Failed to send message: %v", err)
		}
		return
	}

	id, err := strconv.Atoi(args)
	if err != nil {
		if sendErr := vk.sendMessage(peerId, "❌ Invalid ID format", MainKeyboard()); sendErr != nil {
			log.Printf("Failed to send message: %v", sendErr)
		}
		return
	}

	err = txClient.ResumeTorrent(id)
	if err != nil {
		if sendErr := vk.sendMessage(peerId, "❌ Error resuming torrent: "+err.Error(), MainKeyboard()); sendErr != nil {
			log.Printf("Failed to send message: %v", sendErr)
		}
		return
	}

	if err := vk.sendMessage(peerId, fmt.Sprintf("▶️ Torrent ID %d resumed", id), MainKeyboard()); err != nil {
		log.Printf("Failed to send message: %v", err)
	}
}

func handleRemove(vk *VKClient, peerId int, args string, txClient *transmission.Client) {
	if args == "" {
		if err := vk.sendMessage(peerId, "❌ Usage: /remove <id> [-d]", MainKeyboard()); err != nil {
			log.Printf("Failed to send message: %v", err)
		}
		return
	}

	// Parse arguments
	var id int
	deleteData := false
	parts := strings.Fields(args)
	if len(parts) >= 1 {
		var err error
		id, err = strconv.Atoi(parts[0])
		if err != nil {
			if sendErr := vk.sendMessage(peerId, "❌ Invalid ID format", MainKeyboard()); sendErr != nil {
				log.Printf("Failed to send message: %v", sendErr)
			}
			return
		}
	}
	if len(parts) > 1 && (parts[1] == "-d" || parts[1] == "delete") {
		deleteData = true
	}

	err := txClient.RemoveTorrent(id, deleteData)
	if err != nil {
		if sendErr := vk.sendMessage(peerId, "❌ Error removing torrent: "+err.Error(), MainKeyboard()); sendErr != nil {
			log.Printf("Failed to send message: %v", sendErr)
		}
		return
	}

	action := "удален"
	if deleteData {
		action = "удален с файлами"
	}
	if err := vk.sendMessage(peerId, fmt.Sprintf("🗑️ Torrent ID %d %s", id, action), MainKeyboard()); err != nil {
		log.Printf("Failed to send message: %v", err)
	}
}

// handleDocument обрабатывает полученный .torrent файл
func handleDocument(vk *VKClient, peerId int, attachment string, txClient *transmission.Client) {
	if txClient == nil {
		if err := vk.sendMessage(peerId, "⚠️ Transmission не настроен", MainKeyboard()); err != nil {
			log.Printf("Failed to send message: %v", err)
		}
		return
	}

	log.Printf("[Doc] Processing document attachment: %s", attachment)
	
	// Скачиваем файл
	fileData, err := vk.downloadVKFile(attachment)
	if err != nil {
		log.Printf("[Doc] Error downloading file: %v", err)
		if err := vk.sendMessage(peerId, "❌ Ошибка скачивания файла: "+err.Error(), MainKeyboard()); err != nil {
			log.Printf("Failed to send message: %v", err)
		}
		return
	}

	log.Printf("[Doc] Downloaded %d bytes", len(fileData))

	// Кодируем в base64 для Transmission
	encoded := base64.StdEncoding.EncodeToString(fileData)

	// Определяем категорию из имени файла (пока без подписи - VK не дает caption для документов)
	category := "other"
	if strings.Contains(attachment, "doc_") {
		// Попробуем определить категорию из контекста (пока просто other)
		// В будущем можно добавить распознавание по имени файла
	}

	// Получаем путь для загрузки
	txConfig := txClientGlobal.GetConfig()
	downloadDir := txConfig.GetDownloadDir(category)

	// Добавляем торрент
	_, err = txClient.AddTorrent(encoded, downloadDir)
	if err != nil {
		var errMsg string
		if err == transmission.ErrTransmissionUnavailable {
			errMsg = "⚠️ Transmission is not available."
		} else if err == transmission.ErrTorrentExists {
			errMsg = "ℹ️ Torrent already exists."
		} else {
			errMsg = fmt.Sprintf("❌ Error adding torrent: %v", err)
		}
		if sendErr := vk.sendMessage(peerId, errMsg, MainKeyboard()); sendErr != nil {
			log.Printf("Failed to send message: %v", sendErr)
		}
		return
	}

	msg := fmt.Sprintf("✅ Torrent file added in category: <b>%s</b>\n📁 Path: %s", category, downloadDir)
	if err := vk.sendMessage(peerId, msg, MainKeyboard()); err != nil {
		log.Printf("Failed to send message: %v", err)
	}
}

// handleKeyboardClick обрабатывает нажатие кнопки на клавиатуре.
// VK API возвращает событие типа 18 с payload в формате JSON.
func handleKeyboardClick(vk *VKClient, event []interface{}) {
	if len(event) < 6 {
		log.Printf("[Keyboard] Event too short for keyboard click")
		return
	}

	peerId := int(event[3].(float64))
	payloadStr, ok := event[5].(string)
	if !ok {
		log.Printf("[Keyboard] Payload is not a string")
		return
	}

	log.Printf("[Keyboard] Button click from peer %d, payload: %s", peerId, payloadStr)

	// Парсим payload JSON: {"action":{"type":"text","label":"/help"}}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
		log.Printf("[Keyboard] Failed to parse payload: %v", err)
		return
	}

	action, ok := payload["action"].(map[string]interface{})
	if !ok {
		log.Printf("[Keyboard] No action in payload")
		return
	}

	// VK API использует "label" для text кнопок
	var label string
	if l, ok := action["label"].(string); ok {
		label = l
	} else if t, ok := action["text"].(string); ok {
		label = t
	}

	if label == "" {
		log.Printf("[Keyboard] No label in action")
		return
	}

	log.Printf("[Keyboard] Button pressed: %s", label)

	// Обработка команды из кнопки
	if txClientGlobal == nil {
		// Transmission не настроен - просто эхо
		if err := vk.sendMessage(peerId, label, MainKeyboard()); err != nil {
			log.Printf("[Keyboard] Failed to echo: %v", err)
		}
		return
	}

	// Перенаправляем на тот же обработчик команд
	text := label
	if strings.HasPrefix(text, "/") {
		// Загружаем config для передачи в handleCommand
		cfg, err := loadConfig("config.json")
		if err != nil {
			log.Printf("[Keyboard] Failed to load config: %v", err)
			if err := vk.sendMessage(peerId, "⚠️ Configuration error", MainKeyboard()); err != nil {
				log.Printf("[Keyboard] Failed to send error: %v", err)
			}
			return
		}
		handleCommand(vk, peerId, text, txClientGlobal, cfg)
	} else {
		// Не-команда - эхо с клавиатурой
		if err := vk.sendMessage(peerId, text, MainKeyboard()); err != nil {
			log.Printf("[Keyboard] Failed to send echo: %v", err)
		}
	}
}
