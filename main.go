package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
	_ "github.com/mattn/go-sqlite3"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Server struct {
		Port          int    `yaml:"port"`
		SessionSecret string `yaml:"session_secret"`
	} `yaml:"server"`
	Auth struct {
		Username string `yaml:"username"`
		Password string `yaml:"password"`
	} `yaml:"auth"`
	Ollama struct {
		URL   string `yaml:"url"`
		Model string `yaml:"model"`
	} `yaml:"ollama"`
	Database struct {
		Path string `yaml:"path"`
	} `yaml:"database"`
}

type Chat struct {
	ID        int       `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Message struct {
	ID        int       `json:"id"`
	ChatID    int       `json:"chat_id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

type ChatRequest struct {
	Message string   `json:"message"`
	Files   []string `json:"files,omitempty"`
}

type OllamaMessage struct {
	Role    string   `json:"role"`
	Content string   `json:"content"`
	Images  []string `json:"images,omitempty"`
}

type OllamaRequest struct {
	Model    string          `json:"model"`
	Messages []OllamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

type OllamaResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Done bool `json:"done"`
}

type ModelInfo struct {
	NumCtx int `json:"num_ctx"`
}

type TokenUsage struct {
	Used  int `json:"used"`
	Total int `json:"total"`
}

var (
	config              Config
	db                  *sql.DB
	store               *sessions.CookieStore
	activeRequests      = make(map[string]context.CancelFunc)
	requestsMutex       sync.Mutex
	titleUpdateChannels = make(map[string][]chan string)
	titleUpdatesMutex   sync.Mutex
)

func main() {
	configData, err := os.ReadFile("config.yaml")
	if err != nil {
		log.Fatal("Error reading config:", err)
	}
	if err := yaml.Unmarshal(configData, &config); err != nil {
		log.Fatal("Error parsing config:", err)
	}

	store = sessions.NewCookieStore([]byte(config.Server.SessionSecret))
	initDB()

	r := mux.NewRouter()
	r.HandleFunc("/", authMiddleware(chatHandler)).Methods("GET")
	r.HandleFunc("/login", loginHandler)
	r.HandleFunc("/logout", logoutHandler).Methods("GET")
	r.HandleFunc("/api/chats", authMiddleware(apiChatsHandler)).Methods("GET")
	r.HandleFunc("/api/chats", authMiddleware(apiCreateChatHandler)).Methods("POST")
	r.HandleFunc("/api/chats/{id}", authMiddleware(apiUpdateChatHandler)).Methods("PUT")
	r.HandleFunc("/api/chats/{id}", authMiddleware(apiDeleteChatHandler)).Methods("DELETE")
	r.HandleFunc("/api/chats/{id}/messages", authMiddleware(apiMessagesHandler)).Methods("GET")
	r.HandleFunc("/api/chats/{id}/chat", authMiddleware(apiChatHandler)).Methods("POST")
	r.HandleFunc("/api/chats/{id}/stop", authMiddleware(apiStopHandler)).Methods("POST")
	r.HandleFunc("/api/config", authMiddleware(apiConfigHandler)).Methods("GET")
	r.HandleFunc("/api/model/info", authMiddleware(apiModelInfoHandler)).Methods("GET")
	r.HandleFunc("/api/chats/{id}/tokens", authMiddleware(apiTokenUsageHandler)).Methods("GET")
	r.HandleFunc("/api/chats/{id}/title-stream", authMiddleware(apiChatTitleStreamHandler)).Methods("GET")

	addr := fmt.Sprintf(":%d", config.Server.Port)
	log.Printf("Server starting on http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, r))
}

func initDB() {
	var err error
	db, err = sql.Open("sqlite3", config.Database.Path)
	if err != nil {
		log.Fatal("Error opening database:", err)
	}

	createTables := `
	CREATE TABLE IF NOT EXISTS chats (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		title TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_id INTEGER NOT NULL,
		role TEXT NOT NULL,
		content TEXT NOT NULL,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (chat_id) REFERENCES chats(id) ON DELETE CASCADE
	);`

	if _, err := db.Exec(createTables); err != nil {
		log.Fatal("Error creating tables:", err)
	}

	// Enable foreign keys
	db.Exec("PRAGMA foreign_keys = ON")
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "session")
		if auth, ok := session.Values["authenticated"].(bool); !ok || !auth {
			if r.Header.Get("Accept") == "application/json" {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		http.ServeFile(w, r, "static/login.html")
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	if username == config.Auth.Username && password == config.Auth.Password {
		session, _ := store.Get(r, "session")
		session.Values["authenticated"] = true
		session.Save(r, w)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, "session")
	session.Values["authenticated"] = false
	session.Save(r, w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func chatHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "static/chat.html")
}

func apiChatsHandler(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query("SELECT id, title, created_at, updated_at FROM chats ORDER BY updated_at DESC")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var chat Chat
		if err := rows.Scan(&chat.ID, &chat.Title, &chat.CreatedAt, &chat.UpdatedAt); err != nil {
			continue
		}
		chats = append(chats, chat)
	}

	if chats == nil {
		chats = []Chat{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(chats)
}

func apiCreateChatHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.Title == "" {
		req.Title = "New Chat"
	}

	result, err := db.Exec("INSERT INTO chats (title) VALUES (?)", req.Title)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	id, _ := result.LastInsertId()
	var chat Chat
	db.QueryRow("SELECT id, title, created_at, updated_at FROM chats WHERE id = ?", id).
		Scan(&chat.ID, &chat.Title, &chat.CreatedAt, &chat.UpdatedAt)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(chat)
}

func apiUpdateChatHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	chatID := vars["id"]

	var req struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	_, err := db.Exec("UPDATE chats SET title = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?", req.Title, chatID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func apiDeleteChatHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	chatID := vars["id"]

	_, err := db.Exec("DELETE FROM chats WHERE id = ?", chatID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func apiMessagesHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	chatID := vars["id"]

	rows, err := db.Query("SELECT id, chat_id, role, content, timestamp FROM messages WHERE chat_id = ? ORDER BY timestamp ASC", chatID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		if err := rows.Scan(&msg.ID, &msg.ChatID, &msg.Role, &msg.Content, &msg.Timestamp); err != nil {
			continue
		}
		messages = append(messages, msg)
	}

	if messages == nil {
		messages = []Message{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(messages)
}

func apiChatHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	chatID := vars["id"]

	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Auto-name chat if it's still "New Chat" and this is the first message
	var messageCount int
	db.QueryRow("SELECT COUNT(*) FROM messages WHERE chat_id = ?", chatID).Scan(&messageCount)
	shouldGenerateTitle := false
	if messageCount == 0 {
		var currentTitle string
		db.QueryRow("SELECT title FROM chats WHERE id = ?", chatID).Scan(&currentTitle)
		if currentTitle == "New Chat" {
			shouldGenerateTitle = true
		}
	}

	// Save user message
	saveMessage(chatID, "user", req.Message)
	updateChatTimestamp(chatID)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	requestID := fmt.Sprintf("%s-%d", chatID, time.Now().UnixNano())

	requestsMutex.Lock()
	activeRequests[requestID] = cancel
	requestsMutex.Unlock()

	defer func() {
		requestsMutex.Lock()
		delete(activeRequests, requestID)
		requestsMutex.Unlock()
		cancel()
	}()

	// Get conversation history
	history, err := getConversationHistory(chatID)
	if err != nil {
		log.Printf("Error getting history: %v", err)
		history = []OllamaMessage{}
	}

	// Add current message with images if present
	currentMsg := OllamaMessage{
		Role:    "user",
		Content: req.Message,
		Images:  req.Files,
	}
	history = append(history, currentMsg)

	ollamaReq := OllamaRequest{
		Model:    config.Ollama.Model,
		Messages: history,
		Stream:   true,
	}

	reqBody, _ := json.Marshal(ollamaReq)
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", config.Ollama.URL+"/api/chat", bytes.NewBuffer(reqBody))
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		if ctx.Err() == context.Canceled {
			fmt.Fprintf(w, "data: {\"stopped\": true}\n\n")
			flusher.Flush()
			return
		}
		fmt.Fprintf(w, "data: {\"error\": \"%s\"}\n\n", err.Error())
		flusher.Flush()
		return
	}
	defer resp.Body.Close()

	var fullResponse string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			fmt.Fprintf(w, "data: {\"stopped\": true}\n\n")
			flusher.Flush()
			return
		default:
			var ollamaResp OllamaResponse
			if err := json.Unmarshal(scanner.Bytes(), &ollamaResp); err != nil {
				continue
			}

			fullResponse += ollamaResp.Message.Content

			data, _ := json.Marshal(map[string]interface{}{
				"content": ollamaResp.Message.Content,
				"done":    ollamaResp.Done,
			})
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()

			if ollamaResp.Done {
				break
			}
		}
	}

	if fullResponse != "" {
		saveMessage(chatID, "assistant", fullResponse)

		// Generate title using LLM if this is the first exchange
		if shouldGenerateTitle {
			go generateTitleWithLLM(chatID, req.Message, fullResponse)
		}
	}
}
func generateTitleWithLLM(chatID, userMessage, assistantResponse string) {
	// Create a prompt for title generation
	titlePrompt := fmt.Sprintf(`Based on this conversation, generate a short, concise title (maximum 6 words). Only respond with the title, nothing else.

User: %s
Assistant: %s

Title:`, userMessage, assistantResponse)

	ollamaReq := OllamaRequest{
		Model: config.Ollama.Model,
		Messages: []OllamaMessage{
			{
				Role:    "user",
				Content: titlePrompt,
			},
		},
		Stream: false,
	}

	reqBody, err := json.Marshal(ollamaReq)
	if err != nil {
		log.Printf("Error marshaling title request: %v", err)
		return
	}

	resp, err := http.Post(config.Ollama.URL+"/api/chat", "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		log.Printf("Error generating title: %v", err)
		return
	}
	defer resp.Body.Close()

	var ollamaResp OllamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		log.Printf("Error decoding title response: %v", err)
		return
	}

	title := strings.TrimSpace(ollamaResp.Message.Content)

	// Clean up the title
	title = strings.Trim(title, `"'`)
	if len(title) > 60 {
		title = title[:60] + "..."
	}

	if title == "" {
		title = generateTitle(userMessage) // Fallback to old method
	}

	// Update the chat title
	_, err = db.Exec("UPDATE chats SET title = ? WHERE id = ?", title, chatID)
	if err != nil {
		log.Printf("Error updating chat title: %v", err)
		return
	}

	// Broadcast the title update to all listening clients
	titleUpdatesMutex.Lock()
	if channels, exists := titleUpdateChannels[chatID]; exists {
		for _, ch := range channels {
			select {
			case ch <- title:
			default:
			}
		}
	}
	titleUpdatesMutex.Unlock()
}

func apiChatTitleStreamHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	chatID := vars["id"]

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Store this connection in a map so we can send updates to it
	titleUpdatesMutex.Lock()
	if titleUpdateChannels[chatID] == nil {
		titleUpdateChannels[chatID] = make([]chan string, 0)
	}
	ch := make(chan string, 1)
	titleUpdateChannels[chatID] = append(titleUpdateChannels[chatID], ch)
	titleUpdatesMutex.Unlock()

	defer func() {
		titleUpdatesMutex.Lock()
		// Remove this channel from the list
		channels := titleUpdateChannels[chatID]
		for i, c := range channels {
			if c == ch {
				titleUpdateChannels[chatID] = append(channels[:i], channels[i+1:]...)
				break
			}
		}
		close(ch)
		titleUpdatesMutex.Unlock()
	}()

	// Wait for title updates
	for {
		select {
		case <-r.Context().Done():
			return
		case title := <-ch:
			data, _ := json.Marshal(map[string]string{"title": title})
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			return // Close after sending one update
		}
	}
}

func apiStopHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	chatID := vars["id"]

	requestsMutex.Lock()
	defer requestsMutex.Unlock()

	for requestID, cancel := range activeRequests {
		if strings.HasPrefix(requestID, chatID+"-") {
			cancel()
			delete(activeRequests, requestID)
		}
	}

	w.WriteHeader(http.StatusOK)
}

func apiConfigHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"model": config.Ollama.Model,
	})
}

func getConversationHistory(chatID string) ([]OllamaMessage, error) {
	rows, err := db.Query("SELECT role, content FROM messages WHERE chat_id = ? ORDER BY timestamp ASC", chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []OllamaMessage
	for rows.Next() {
		var role, content string
		if err := rows.Scan(&role, &content); err != nil {
			continue
		}
		messages = append(messages, OllamaMessage{
			Role:    role,
			Content: content,
		})
	}

	return messages, nil
}

func saveMessage(chatID, role, content string) {
	_, err := db.Exec("INSERT INTO messages (chat_id, role, content) VALUES (?, ?, ?)", chatID, role, content)
	if err != nil {
		log.Printf("Error saving message: %v", err)
	}
}

func updateChatTimestamp(chatID string) {
	db.Exec("UPDATE chats SET updated_at = CURRENT_TIMESTAMP WHERE id = ?", chatID)
}

func generateTitle(message string) string {
	words := strings.Fields(message)
	if len(words) == 0 {
		return "New Chat"
	}

	title := strings.Join(words, " ")
	if len(title) > 50 {
		title = title[:50] + "..."
	}

	return title
}
func apiModelInfoHandler(w http.ResponseWriter, r *http.Request) {
	resp, err := http.Get(config.Ollama.URL + "/api/show")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	var result struct {
		Parameters string `json:"parameters"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Parse num_ctx from parameters string
	numCtx := 2048 // default
	lines := strings.Split(result.Parameters, "\n")
	for _, line := range lines {
		if strings.Contains(line, "num_ctx") {
			fmt.Sscanf(line, "num_ctx %d", &numCtx)
			break
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{
		"num_ctx": numCtx,
	})
}

func estimateTokens(text string) int {
	// Rough estimate: ~4 characters per token
	return len(text) / 4
}

func apiTokenUsageHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	chatID := vars["id"]

	rows, err := db.Query("SELECT role, content FROM messages WHERE chat_id = ? ORDER BY timestamp ASC", chatID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	totalTokens := 0
	for rows.Next() {
		var role, content string
		if err := rows.Scan(&role, &content); err != nil {
			continue
		}
		totalTokens += estimateTokens(content)
	}

	// Get num_ctx
	resp, _ := http.Post(config.Ollama.URL+"/api/show", "application/json",
		strings.NewReader(fmt.Sprintf(`{"name":"%s"}`, config.Ollama.Model)))

	numCtx := 2048 // default
	if resp != nil {
		defer resp.Body.Close()
		var result struct {
			Parameters string `json:"parameters"`
		}
		if json.NewDecoder(resp.Body).Decode(&result) == nil {
			lines := strings.Split(result.Parameters, "\n")
			for _, line := range lines {
				if strings.Contains(line, "num_ctx") {
					fmt.Sscanf(line, "num_ctx %d", &numCtx)
					break
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(TokenUsage{
		Used:  totalTokens,
		Total: numCtx,
	})
}
