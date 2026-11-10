# LinkedIn Post Manager

Self-hosted **Go + HTMX + MongoDB + Gemini API** LinkedIn post generator and weekly workflow board, built around your `linkedin-post` and `linkedin-post-image` skills.

## Quick Start (Single Docker Compose)

1. Copy `.env.example` to `.env` and set your `GEMINI_API_KEY`:
   ```bash
   cp .env.example .env
   ```
2. Start the stack:
   ```bash
   docker compose up -d --build
   ```
3. Open **http://localhost:8080**:
   - Go to **Subjects & Priority** (`/subjects`) and add your topics with weights from `1–10` (e.g., `.NET = 10`, `Angular = 10`, `Golang = 4`).
   - Every Sunday at `08:00` (configurable via `SUNDAY_CRON` & `TZ`) — or immediately on container boot / wake if Sunday was missed — the app researches fresh blog posts on Google Search and generates **3 posts + custom editorial images** for the current week (compensating at most 1 week).
   - Click **Copy Post Text**, paste into LinkedIn, click **Copy Image (Cmd+V)**, paste directly into LinkedIn's composer, schedule it, and click **Mark Scheduled**.
