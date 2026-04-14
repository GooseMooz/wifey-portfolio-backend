package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/joho/godotenv"
)

var chatStoreMu sync.Mutex

type Message struct {
	Name        string    `json:"name"`
	Email       string    `json:"email"`
	Type        string    `json:"type"`
	Message     string    `json:"message"`
	SubmittedAt time.Time `json:"submittedAt"`
}

type chatStore struct {
	ChatIDs []int64 `json:"chatIds"`
}

func main() {
	_ = godotenv.Load()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	opts := []bot.Option{
		bot.WithDefaultHandler(handler),
	}

	b, err := bot.New(os.Getenv("TG_BOT_KEY"), opts...)
	if err != nil {
		panic(err)
	}

	go b.Start(ctx)

	port := os.Getenv("PORT")
	if port == "" {
		panic("No PORT")
	}

	mux := http.NewServeMux()
	submitHandler := makeSubmitHandler(b)
	mux.HandleFunc("/", makeRootHandler(submitHandler))
	mux.HandleFunc("/submit", submitHandler)

	server := &http.Server{
		Addr:    ":" + port,
		Handler: withCORS(mux),
	}

	srvErr := server.ListenAndServe()
	if srvErr != nil {
		panic(srvErr)
	}
}

func handler(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update == nil || update.Message == nil {
		return
	}

	if strings.TrimSpace(update.Message.Text) != subscribeCode() {
		return
	}

	if err := saveChatID(update.Message.Chat.ID); err != nil {
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: update.Message.Chat.ID,
			Text:   "Failed to save this chat for notifications.",
		})
		return
	}

	b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   "This chat is now subscribed to notifications.",
	})
}

func makeRootHandler(submitHandler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		if r.Method == http.MethodPost || r.Method == http.MethodOptions {
			submitHandler(w, r)
			return
		}

		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":   "ok",
			"endpoint": "/submit",
		})
	}
}

func makeSubmitHandler(b *bot.Bot) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		defer r.Body.Close()

		var msg Message
		err := json.NewDecoder(r.Body).Decode(&msg)
		if err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		if err := validateMessage(msg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		chatIDs, err := loadChatIDs()
		if err != nil {
			http.Error(w, "failed to load subscribed chats", http.StatusInternalServerError)
			return
		}

		if len(chatIDs) == 0 {
			http.Error(w, "no subscribed chats saved", http.StatusFailedDependency)
			return
		}

		formatted := formatTelegramMessage(msg)
		for _, chatID := range chatIDs {
			_, err := b.SendMessage(r.Context(), &bot.SendMessageParams{
				ChatID:    chatID,
				Text:      formatted,
				ParseMode: models.ParseMode("HTML"),
			})
			if err != nil {
				http.Error(w, "failed to send telegram message", http.StatusBadGateway)
				return
			}
		}

		w.WriteHeader(http.StatusOK)
	}
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		next.ServeHTTP(w, r)
	})
}

func validateMessage(msg Message) error {
	switch {
	case strings.TrimSpace(msg.Name) == "":
		return errors.New("name is required")
	case strings.TrimSpace(msg.Email) == "":
		return errors.New("email is required")
	case strings.TrimSpace(msg.Type) == "":
		return errors.New("type is required")
	case strings.TrimSpace(msg.Message) == "":
		return errors.New("message is required")
	case msg.SubmittedAt.IsZero():
		return errors.New("submittedAt is required")
	default:
		return nil
	}
}

func formatTelegramMessage(msg Message) string {
	name := html.EscapeString(msg.Name)
	email := html.EscapeString(msg.Email)
	messageType := html.EscapeString(msg.Type)
	body := html.EscapeString(msg.Message)
	submittedAt := html.EscapeString(msg.SubmittedAt.Format(time.RFC1123))

	return fmt.Sprintf(
		"<b>Name:</b> %s\nEmail: <code>%s</code>\n<b>Type:</b> %s\n<blockquote>%s</blockquote>\n<i>Submitted at: %s</i>",
		name,
		email,
		messageType,
		body,
		submittedAt,
	)
}

func saveChatID(chatID int64) error {
	chatStoreMu.Lock()
	defer chatStoreMu.Unlock()

	store, err := readChatStore()
	if err != nil {
		return err
	}

	if slices.Contains(store.ChatIDs, chatID) {
		return nil
	}

	store.ChatIDs = append(store.ChatIDs, chatID)
	return writeChatStore(store)
}

func loadChatIDs() ([]int64, error) {
	chatStoreMu.Lock()
	defer chatStoreMu.Unlock()

	store, err := readChatStore()
	if err != nil {
		return nil, err
	}

	return store.ChatIDs, nil
}

func readChatStore() (chatStore, error) {
	path := chatStorePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return chatStore{}, nil
		}
		return chatStore{}, err
	}

	if len(data) == 0 {
		return chatStore{}, nil
	}

	var store chatStore
	if err := json.Unmarshal(data, &store); err != nil {
		return chatStore{}, err
	}

	return store, nil
}

func writeChatStore(store chatStore) error {
	path := chatStorePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0o644)
}

func chatStorePath() string {
	if path := strings.TrimSpace(os.Getenv("CHAT_STORE_PATH")); path != "" {
		return path
	}

	return filepath.Join("data", "chats.json")
}

func subscribeCode() string {
	if value := strings.TrimSpace(os.Getenv("SUBSCRIBE_CODE")); value != "" {
		return value
	}

	return "giratina67"
}

func isServerClosed(err error) bool {
	return err == nil || errors.Is(err, http.ErrServerClosed) || isClosedNetworkError(err)
}

func isClosedNetworkError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && strings.Contains(strings.ToLower(opErr.Err.Error()), "use of closed network connection")
}
