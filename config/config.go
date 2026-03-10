package config

import "os"

const (
	DefaultModel      = "llama3-3-70b"
	DefaultListenAddr = ":8089"
	DefaultStyle      = "default"
)

type Style struct {
	SystemPrompt string
	MinWords     int
	MaxWords     int
	MaxTokens    int
}

var Styles = map[string]Style{
	"default": {
		SystemPrompt: "Summarize the following text.",
		MinWords:     10,
		MaxWords:     100,
		MaxTokens:    1024,
	},
	"thoughts_summary": {
		SystemPrompt: "Describe what's going through this person's mind. ONLY output plain text - never output markdown or code.",
		MinWords:     5,
		MaxWords:     15,
		MaxTokens:    64,
	},
	"title_summary": {
		SystemPrompt: "Generate a concise, descriptive title for the following text. ONLY output plain text - never output markdown or code.",
		MinWords:     2,
		MaxWords:     5,
		MaxTokens:    32,
	},
}

func GetEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
