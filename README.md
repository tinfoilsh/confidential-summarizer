# Summary Model

A summarization service that runs inside a Tinfoil secure enclave at `summarizer.tinfoil.sh`. Tinfoil Chat (web, iOS, and the chat harness) uses it for chat titles and live summaries of model reasoning. It calls the model through the [Tinfoil Go SDK](https://github.com/tinfoilsh/tinfoil-go) with attestation, and does not log request content or summaries.

## Development

```bash
TINFOIL_API_KEY="your-api-key" go run . -v
```

## API

### `POST /summarize`

Request: a JSON object (at most 1 MiB, unknown fields rejected) with:

- `content` (string, required): non-empty text to summarize
- `style` (string): `default` (10-100 words), `thoughts_summary` (4-12 word status label for reasoning text), or `title_summary` (2-5 word title); defaults to `default`
- `min_words`, `max_words` (int, 1-1000): override the style's word range
- `max_tokens` (int, 1-32768): override the style's generation limit

Response: `{"summary": "..."}`. Every response carries an `X-Request-ID` header.

Errors return a JSON body with `error`, `code`, and `request_id`, plus `retry_after_seconds` when upstream supplied `Retry-After` (also returned as a header). Validation errors return `400` or `413`; upstream errors return `429`, `502`, or `503`; the 30-second upstream timeout returns `504`.

### `GET /health`

Returns `ok`.

## Reporting Vulnerabilities

Please report security vulnerabilities by either:

- Emailing [security@tinfoil.sh](mailto:security@tinfoil.sh)
- Opening an issue on GitHub on this repository

We aim to respond to (legitimate) security reports within 24 hours.
