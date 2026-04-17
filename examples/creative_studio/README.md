# Creative Studio — AI Creative Director

A streaming chat experience where **ARIA**, an opinionated AI creative
director, rewrites your idea into a cinematic prompt and generates the
result with **DALL-E 3** (images) or **Veo 2** (5–8s video clips).

```
┌─────────────────────────┬──────────────────────────────────────────┐
│  Gallery                │   ARIA — AI Creative Director            │
├─────────────────────────┼──────────────────────────────────────────┤
│  [img] cyberpunk alley  │   > a lone wizard in a storm             │
│  [img] desert dunes     │   ⚙ generate_image (vivid · 1792×1024)  │
│  [vid] dolly-in cafe    │   [image displayed inline]               │
│                         │                                          │
│                         │   I went hyper-real with a low angle …  │
│                         │   Want a 5s slow-zoom video version?    │
└─────────────────────────┴──────────────────────────────────────────┘
```

## Capabilities

| Feature | What ARIA can do |
|---|---|
| 🎨 **Image generation** | DALL-E 3 — landscape / portrait / square, vivid or natural, hd quality |
| 🎬 **Video generation** | Veo 2 — 5–8 second clips, 16:9 or 9:16, with camera motion |
| 🎯 **Creative direction** | Rewrites bare ideas into cinematic prompts (camera, lighting, palette, mood) |
| 🖼️ **Inline streaming** | Media renders in chat the moment the tool returns — no upload step |
| 🗂️ **Persistent gallery** | Sidebar collects every image/video from the session, click to zoom |
| 💾 **Local storage** | Files saved to `./generated/` — easy to find, no temp-dir hunting |

## Quickstart

```bash
# 1. API keys (both required for full functionality)
export OPENAI_API_KEY=sk-...        # DALL-E 3
export GEMINI_API_KEY=AIza...       # Veo 2

# 2. Pick your reasoning LLM (defaults to OpenAI)
export LLM_PROVIDER=openai          # or anthropic, gemini

# 3. Run
make example-creative
# or:  cd examples/creative_studio && go run .

# 4. Open
open http://localhost:8890
```

The UI has a row of **spark prompts** — click one for an instant demo.

## Things to try

**Image — let ARIA cook**
```
a wizard in a storm
```
→ ARIA rewrites this into something like *"Low-angle wide shot of a hooded
wizard, staff raised against a roiling thunderhead. Cinematic lighting,
volumetric rain, painterly Frank Frazetta palette, depth of field."* — then
calls `generate_image` with `quality=hd`.

**Video — describe motion, not just a scene**
```
slow dolly-in on a neon Tokyo alley at night
```
→ Veo 2 renders 5–8 seconds with camera motion baked in. Plays inline.

**Compare alternatives**
```
generate three versions of a desert sunset: photorealistic,
oil painting, and anime concept art
```
→ ARIA fires three separate generations side-by-side in the gallery.

**Mix media**
```
make a square image of a rainy cyberpunk diner, then turn it into a
9:16 vertical video clip
```
→ Image first, then a vertical video version with the same vibe.

## Endpoints

| Endpoint | Purpose |
|---|---|
| `GET /` | Chat UI |
| `GET /api/chat?session_id=&message=` | SSE stream — agent turns + media |
| `GET /media/<filename>` | Generated images & videos (served from `./generated/`) |

## Configuration

| Variable | Purpose | Default |
|---|---|---|
| `OPENAI_API_KEY` | DALL-E 3 image generation | required for images |
| `GEMINI_API_KEY` | Veo 2 video generation | required for videos |
| `LLM_PROVIDER` | Reasoning LLM: `openai`, `anthropic`, `gemini` | `openai` |
| `OPENAI_MODEL` / `ANTHROPIC_MODEL` / `GEMINI_MODEL` | Override model | provider default |
| `MEDIA_DIR` | Where to save generated files | `./generated/` |

If `OPENAI_API_KEY` is set but `GEMINI_API_KEY` isn't, only `generate_image`
will be registered (and vice-versa). ARIA adapts to whichever tools exist.

## Where files land

By default, generated media goes to `./generated/` next to where you ran
the command — easy to `ls`, easy to share. Override with `MEDIA_DIR` if
you want them elsewhere:

```bash
MEDIA_DIR=~/Pictures/aria make example-creative
```

The HTTP server mounts whatever directory you point at as `/media/`, so URLs
in the chat (`/media/img-1742...png`) always resolve.

## How the tools work

`pkg/tools/builtin/generate_image.go` and `generate_video.go`:

- **Image**: calls OpenAI's `images.generate` API, downloads the CDN URL
  before it expires (~1h), and saves to `SaveDir`. Returns markdown
  `![alt](localURL)` so the SSE stream renders it in place.
- **Video**: calls Veo 2's `GenerateVideos`, polls the long-running
  operation every 10s (timeout 5 min), downloads the resulting Gemini
  Files API URI **server-side** (the URI requires the API key — the
  browser can't fetch it directly), and returns an inline `<video>` tag.

Both tools implement `tools.InlineRenderer` (`InlineResult() bool`) so the
frontend streams the media into the assistant message immediately, without
waiting for the model to format it.

## Customizing ARIA

The persona lives in `main.go` as `systemPrompt`. Tweak the tone, add new
decision rules, or wire in a different toolset — the agent loop handles
the rest.
