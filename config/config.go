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
	Temperature  *float64
}

var Styles = map[string]Style{
	"default": {
		SystemPrompt: "Summarize the following text.",
		MinWords:     10,
		MaxWords:     100,
		MaxTokens:    1024,
	},
	"thoughts_summary": {
		SystemPrompt: `You produce a brief status label describing what an AI assistant is currently reasoning about. The label is shown to a user who is waiting for the AI to finish thinking.

Focus on the LAST part of the reasoning text — that reflects what the AI is working on right now. Ignore earlier parts that have already been resolved.

Format rules:
- Start with a present participle verb (Analyzing, Considering, Evaluating, Comparing, Weighing, Planning, etc.)
- Be specific to the actual content — never write vague filler like "Thinking about the question"
- Output ONLY the label, nothing else — no quotes, no punctuation, no markdown, no preamble
- Never refer to "the AI", "the model", "the assistant", "the person", or "the user"

Good examples:
- Evaluating post-surgery recovery risks
- Comparing authentication middleware approaches
- Breaking down database query performance
- Weighing privacy versus usability tradeoffs
- Planning step-by-step explanation structure
- Assessing UV exposure risks from skiing after laser treatment
- Deciding how to structure the response`,
		MinWords:    4,
		MaxWords:    12,
		MaxTokens:   64,
		Temperature: floatPtr(0.2),
	},
	"title_summary": {
		SystemPrompt: "Generate a concise, descriptive title for the following text. ONLY output plain text - never output markdown or code.",
		MinWords:     2,
		MaxWords:     5,
		MaxTokens:    32,
	},
}

func floatPtr(f float64) *float64 {
	return &f
}

func GetEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
