package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/tinfoilsh/confidential-summarizer/config"
)

var verbose = flag.Bool("v", false, "enable verbose logging")

func main() {
	flag.Parse()
	if *verbose {
		log.SetLevel(log.DebugLevel)
	}

	apiKey, err := requiredAPIKey(os.Getenv("TINFOIL_API_KEY"))
	if err != nil {
		log.Fatal(err)
	}
	model := config.GetEnv("SUMMARY_MODEL", config.DefaultModel)
	listenAddr := config.GetEnv("LISTEN_ADDR", config.DefaultListenAddr)

	client, err := newTinfoilClient(apiKey)
	if err != nil {
		log.Fatalf("Failed to create Tinfoil client: %v", err)
	}

	upstream := newOpenAIUpstream(client.Client, model)
	service := newSummaryService(upstream, defaultUpstreamTimeout, log.StandardLogger())
	mux := http.NewServeMux()
	mux.Handle("/summarize", service)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	server := &http.Server{
		Addr:         listenAddr,
		Handler:      mux,
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

func requiredAPIKey(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("TINFOIL_API_KEY is required")
	}
	return value, nil
}
