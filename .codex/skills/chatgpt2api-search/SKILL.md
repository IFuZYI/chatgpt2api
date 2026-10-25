---
name: chatgpt2api-search
description: Use when current web search is needed through this chatgpt2api server. Call the configured HTTP search endpoint with a prompt and return the answer with source URLs.
---

# ChatGPT2API Search

Use this skill when the user asks for current web search, online lookup, recent information, or source-backed answers.

## Endpoint

The port depends on how the server was started:

| Deployment | Base URL |
|:--|:--|
| Docker Compose (default) | `http://127.0.0.1:3000` |
| Docker Compose with `CHATGPT2API_PORT` set | `http://127.0.0.1:<that port>` |
| Source run (`go build && ./chatgpt2api`) | `http://127.0.0.1:8000` |
| Single binary with `--addr :8080` | `http://127.0.0.1:8080` |

If you are unsure which one is running, probe them in that order:

```bash
for p in 3000 8000 8080; do
  curl -fsS -o /dev/null "http://127.0.0.1:$p/health" && echo "found on :$p" && break
done
```

Then:

```
POST http://127.0.0.1:3000/v1/search
```

Headers:

```
Authorization: Bearer <your-auth-key>
Content-Type: application/json
```

`<your-auth-key>` is whatever was put in `config.json` under `auth-key`, or the
`CHATGPT2API_AUTH_KEY` environment variable. It is **not** a literal placeholder —
substitute the real value.

Body:

```json
{
  "prompt": "<search question>"
}
```

## Response

Use `answer` as the main response. Include URLs from `sources` when available.
