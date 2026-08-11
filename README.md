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

| Variable                  | Default        | Description                                  |
| ------------------------- | -------------- | -------------------------------------------- |
| `TINFOIL_API_KEY`         | -              | Tinfoil API key (required)                   |
| `SUMMARY_MODEL`           | `gpt-oss-120b` | Model used for summarization                 |
| `LISTEN_ADDR`             | `:8089`        | Address to listen on                         |
| `MAX_CONCURRENT_REQUESTS` | `16`           | Maximum in-flight upstream requests (1-1024) |

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
    "max_tokens": 1024
  }'
```

**Request fields:**

| Field        | Type   | Required | Description                                                   |
| ------------ | ------ | -------- | ------------------------------------------------------------- |
| `content`    | string | yes      | Text to summarize                                             |
| `style`      | string | no       | Summary style (default: `"default"`)                          |
| `min_words`  | int    | no       | Override minimum word count (1-1000)                          |
| `max_words`  | int    | no       | Override maximum word count (1-1000 and at least `min_words`) |
| `max_tokens` | int    | no       | Override max tokens for generation (1-32768)                  |

**Styles:**

| Style              | Description                           | Words  | Max Tokens |
| ------------------ | ------------------------------------- | ------ | ---------- |
| `default`          | General summary                       | 10-100 | 2048       |
| `thoughts_summary` | Describes what's on the person's mind | 4-12   | 1024       |
| `title_summary`    | Short descriptive title               | 2-5    | 1024       |

**Response:**

```json
{
  "summary": "A concise summary of the provided text."
}
```

Request bodies are limited to 1 MiB and must contain exactly one JSON object. Unknown fields are rejected. Every response includes an `X-Request-ID` header.

**Errors:**

```json
{
  "error": "upstream service is temporarily unavailable",
  "code": "upstream_unavailable",
  "message": "upstream service is temporarily unavailable",
  "request_id": "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
  "retry_after_seconds": 1
}
```

| Status | Meaning                                                                  |
| ------ | ------------------------------------------------------------------------ |
| `400`  | Malformed JSON, missing content, unsupported style, or invalid overrides |
| `408`  | Client request canceled                                                  |
| `413`  | Request body exceeds 1 MiB                                               |
| `429`  | Upstream rate limit                                                      |
| `502`  | Upstream rejection or malformed/empty response                           |
| `503`  | Concurrency limit, open circuit, or transient upstream failure           |
| `504`  | Upstream request exceeded the 30-second timeout                          |

Retryable errors include both `Retry-After` and `retry_after_seconds`. The service disables SDK retries, limits concurrent upstream work, and uses a process-wide circuit breaker with exponential recovery cooldowns and one half-open probe. Request content and summaries are not logged or cached server-side.

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
