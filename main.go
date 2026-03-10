package main

import (
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	log "github.com/sirupsen/logrus"
	"github.com/tinfoilsh/tinfoil-go"
)

var verbose = flag.Bool("v", false, "enable verbose logging")

type SummarizeRequest struct {
	Content string `json:"content"`
}

type SummarizeResponse struct {
	Summary string `json:"summary"`
}

func main() {
	flag.Parse()
	if *verbose {
		log.SetLevel(log.DebugLevel)
	}

	apiKey := os.Getenv("TINFOIL_API_KEY")
	model := getEnv("SUMMARY_MODEL", "llama3-3-70b")
	listenAddr := getEnv("LISTEN_ADDR", ":8089")

	client, err := tinfoil.NewClient(option.WithAPIKey(apiKey))
	if err != nil {
		log.Fatalf("Failed to create Tinfoil client: %v", err)
	}

	http.HandleFunc("/summarize", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req SummarizeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Content == "" {
			http.Error(w, "content is required", http.StatusBadRequest)
			return
		}

		resp, err := client.Chat.Completions.New(r.Context(), openai.ChatCompletionNewParams{
			Model: model,
			Messages: []openai.ChatCompletionMessageParamUnion{
				openai.SystemMessage("You are a summarization assistant. Summarize the provided content concisely."),
				openai.UserMessage(req.Content),
			},
		})
		if err != nil {
			log.Errorf("Chat completion error: %v", err)
			http.Error(w, "failed to generate summary", http.StatusInternalServerError)
			return
		}

		summary := ""
		if len(resp.Choices) > 0 {
			summary = resp.Choices[0].Message.Content
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(SummarizeResponse{Summary: summary})
	})

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	server := &http.Server{
		Addr:         listenAddr,
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 5 * time.Minute,
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Infof("Starting on %s (model: %s, enclave: %s)", listenAddr, model, client.Enclave())
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	<-sigChan
	log.Info("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server.Shutdown(ctx)
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
