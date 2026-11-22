# Thursday Latin Dancing at Scrumpy's — Instructor Scheduler

Fairness-based Thursday instructor & DJ scheduling web app + messaging bot engine for Scrumpy's Latin Dancing (~2 dozen instructors).

## Key Features (Phase 1 Core + Transport Abstraction)

- **Rolling 6-Month Thursday Calendar**: Automatically opens Thursdays `months_open_ahead` (default 6) months in advance without spamming the group channel.
- **Fairness + Veto Token Resolution (No Pure FCFS)**:
  - Every claim is a **bid**, resolved at `T - resolve_days_before` (default 7 days before the event, or force-resolved by Admin).
  - Priority order at resolution:
    1. **Veto Token (`token`) bid** beats normal bids (each instructor gets `tokens_per_year = 2` per calendar year).
    2. **Fairness score**: fewest confirmed teaching nights in the rolling `fairness_window_days` (default 90 days) wins. Equal start (0) at launch.
    3. **Earliest bid timestamp** breaks ties.
- **Channel Noise & Privacy Policy**:
  - Unresolved bid lists are **private** — other instructors never see who bid on a date unless the bidder checks **Need Lead / Need Follow / Need Co-Instructor**, which surfaces the help request so others can volunteer.
  - Group announcements happen only on **resolutions**, **token reservations**, **help requests**, and **T-14 orphan date recruitment**.
- **Event Details & Volunteering**:
  - Instructors configure Lesson Type (`salsa`, `bachata`, `mixed`, `other`), topic, DJ name, notes, and help flags (`need_lead`, `need_follow`, `need_co`).
  - One-tap volunteer buttons (`I'll Lead`, `I'll Follow`, `I'll Co-Teach`).
- **Semi-Automated Facebook Event Creation**:
  - Every resolved Thursday automatically generates a pre-filled Facebook Event title, schedule, instructor, volunteer list, and DJ block for ~30-second copy-paste.
- **Transport Abstraction (`internal/transport`)**:
  - `SendDM`, `PostToGroup`, `SendOTP`, and `ParseCommand` sit behind `transport.Transport`.
  - All messages are persisted to SQLite `outbox` (for manual copy-paste digest fallback) and optionally forwarded to a WhatsApp Web (`whatsapp-web.js` / Baileys on Google Voice) or Telegram Bot webhook.

## Quick Start

```bash
make test
make build
./dance-scheduler -port 8095 -db ./dance.db
```
