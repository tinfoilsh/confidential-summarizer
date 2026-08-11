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

| Variable          | Default        | Description                  |
| ----------------- | -------------- | ---------------------------- |
| `TINFOIL_API_KEY` | -              | Tinfoil API key (required)   |
| `SUMMARY_MODEL`   | `llama3-3-70b` | Model used for summarization |
| `LISTEN_ADDR`     | `:8089`        | Address to listen on         |

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

| Field        | Type   | Required | Description                          |
| ------------ | ------ | -------- | ------------------------------------ |
| `content`    | string | yes      | Text to summarize                    |
| `style`      | string | no       | Summary style (default: `"default"`) |
| `min_words`  | int    | no       | Override minimum word count          |
| `max_words`  | int    | no       | Override maximum word count          |
| `max_tokens` | int    | no       | Override max tokens for generation   |

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

Requests are limited to one 1 MiB JSON object with known fields, non-empty content, word limits from 1-1000, and `max_tokens` from 1-32768. Every summarize response includes an `X-Request-ID` header.

**Errors:**

```json
{
  "error": "upstream service is temporarily unavailable",
  "code": "upstream_unavailable",
  "request_id": "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
  "retry_after_seconds": 7
}
```

Validation errors return `400` or `413`; upstream errors return `429`, `502`, or `503`; and the 30-second upstream timeout returns `504`. When supplied upstream, `Retry-After` is returned as a header and `retry_after_seconds`. Request content and summaries are not logged.

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
