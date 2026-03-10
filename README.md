# Summary Model

A summarization service that runs inside a Tinfoil secure enclave. Accepts text content and returns summaries in configurable styles.

## Quick Start

```bash
export TINFOIL_API_KEY="your-api-key"

go run .

# With verbose logging
go run . -v
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `TINFOIL_API_KEY` | - | Tinfoil API key (required) |
| `SUMMARY_MODEL` | `llama3-3-70b` | Model used for summarization |
| `LISTEN_ADDR` | `:8089` | Address to listen on |

## API

### Summarize

`POST /summarize`

```bash
curl http://localhost:8089/summarize \
  -H "Content-Type: application/json" \
  -d '{
    "content": "Your text to summarize...",
    "style": "thoughts_summary",
    "min_words": 5,
    "max_words": 20,
    "max_tokens": 128
  }'
```

**Request fields:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `content` | string | yes | Text to summarize |
| `style` | string | no | Summary style (default: `"default"`) |
| `min_words` | int | no | Override minimum word count |
| `max_words` | int | no | Override maximum word count |
| `max_tokens` | int | no | Override max tokens for generation |

**Styles:**

| Style | Description | Words | Max Tokens |
|-------|-------------|-------|------------|
| `default` | General summary | 10-100 | 1024 |
| `thoughts_summary` | Describes what's on the person's mind | 5-15 | 64 |
| `title_summary` | Short descriptive title | 2-5 | 32 |

**Response:**

```json
{
  "summary": "A concise summary of the provided text."
}
```

### Health Check

`GET /health` - Returns `ok`

## Docker

```bash
docker build -t summary-model .
docker run -p 8089:8089 \
  -e TINFOIL_API_KEY=$TINFOIL_API_KEY \
  summary-model
```

## Security

Uses the [Tinfoil Go SDK](https://github.com/tinfoilsh/tinfoil-go) for attested communication with Tinfoil enclaves.

## Reporting Vulnerabilities

Please report security vulnerabilities by either:

- Emailing [security@tinfoil.sh](mailto:security@tinfoil.sh)

- Opening an issue on GitHub on this repository

We aim to respond to (legitimate) security reports within 24 hours.
