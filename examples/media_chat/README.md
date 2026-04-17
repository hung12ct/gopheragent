# Media Chat — Upload & Ask

A chat UI where you **upload an image, video, or document** and ask
questions about it. The agent calls a multimodal model (Gemini for
images + video, OpenAI gpt-4o for images-only fallback) and answers with
context from the file.

```
┌──────────────────────────────────────────────────────────────┐
│  Media Chat                          [Upload]                │
├──────────────────────────────────────────────────────────────┤
│  📎 sunset.jpg uploaded                                      │
│                                                              │
│  > what's the dominant color and mood?                       │
│  ⚙ describe_file("dominant color and mood?")                 │
│  Warm amber and deep crimson dominate. The mood is …        │
│                                                              │
│  > how many people are in the frame?                         │
│  ⚙ describe_file("count people in the frame")                │
│  Three silhouettes against the horizon.                      │
└──────────────────────────────────────────────────────────────┘
```

## Capabilities

| Feature | Details |
|---|---|
| 🖼️ **Image Q&A** | jpg, png, gif, webp, svg — analyzed with Gemini or OpenAI gpt-4o |
| 🎥 **Video Q&A** | mp4, webm, mov, ogv — analyzed with Gemini (multimodal) |
| 📄 **Text docs** | txt, md, csv, json — content returned verbatim to the agent |
| 📡 **Streaming** | Tool calls and tokens stream over SSE |
| 🔁 **Re-ask** | One upload, many questions — the agent re-queries the model each time |
| 🧠 **Context-aware** | The agent is told upfront a file is uploaded, so it knows to call `describe_file` |

## Quickstart

```bash
# 1. API key (need at least one)
export GEMINI_API_KEY=AIza...    # images + video  ← recommended
# or
export OPENAI_API_KEY=sk-...     # images only

# 2. Reasoning LLM (optional — defaults to OpenAI)
export LLM_PROVIDER=gemini       # or openai, anthropic

# 3. Run
make example-media
# or:  cd examples/media_chat && go run .

# 4. Open
open http://localhost:8889
```

> **Why two analyzers?** OpenAI gpt-4o handles images well but not video.
> Gemini handles both. The example registers whichever you've configured
> and routes uploads accordingly.

## Things to try

**Image — open-ended**
```
[upload a chart]
What does this chart tell me? What's the trend?
```

**Image — targeted**
```
[upload a screenshot]
Extract any text visible in the UI and list it as bullets
```

**Video — describe motion**
```
[upload a 10s clip]
What's happening in this video? Describe the key moments in order.
```

**Document — analyze**
```
[upload a CSV]
Which row has the highest revenue? Summarize the dataset.
```

**Multi-turn on one file**
```
[upload an X-ray or product photo]
What do you see? → What might be unusual? → How would you describe
this for a non-expert?
```

Each follow-up triggers a fresh `describe_file` call with the new
question — the model re-examines the media every time, so you can drill
down without re-uploading.

## Endpoints

| Endpoint | Method | Purpose |
|---|---|---|
| `GET /` | GET | Chat UI |
| `POST /api/upload` | POST (multipart) | `file` + `session_id` fields |
| `GET /api/chat?session_id=&message=` | GET (SSE) | Stream agent response |

Upload limit: **20 MB**. Allowed MIME types are listed in `main.go` under
`allowedMIME`.

## Configuration

| Variable | Purpose | Default |
|---|---|---|
| `GEMINI_API_KEY` | Gemini multimodal (images + video) | recommended |
| `OPENAI_API_KEY` | OpenAI gpt-4o vision (images only) | fallback |
| `LLM_PROVIDER` | Reasoning LLM: `openai`, `anthropic`, `gemini` | `openai` |
| `OPENAI_MODEL` / `ANTHROPIC_MODEL` / `GEMINI_MODEL` | Override model | provider default |

Files are stored in the system temp directory
(`$TMPDIR/gopheragent-media/` on macOS, `/tmp/gopheragent-media/` on
Linux) and persist for the lifetime of the server.

## How it works

- **`DescribeFileTool`** (`main.go`) is registered as the only tool. It
  has two analyzer slots: `vision` (OpenAI, images only) and `media`
  (Gemini, images + video). On each call it picks the right analyzer for
  the file's MIME type.
- **Context injection**: when the user sends a message and a file is
  uploaded, the handler prepends a system note like *"[Context: file
  X.mp4 (video/mp4) uploaded]"* so the LLM doesn't need to be told twice
  to call `describe_file`.
- **Multimodal calls**: images and videos are read into memory, base64-
  encoded as a `data:` URI, and passed to the analyzer. Gemini receives
  them as `genai.Blob{MIMEType, Data}`; OpenAI receives them inline in
  the `image_url` field.

## Adding new file types

Two places in `main.go`:

1. Add the MIME type to `allowedMIME` with a category (`image`, `video`,
   `text`, or a new one).
2. If it's a new category, extend `describeMedia()` to route it to the
   right analyzer.
