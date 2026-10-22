# Admin API

The admin API lets a hosting service (civitone.com) or a self-hoster's own
scripts run a controller from outside: set its plan limits, give people
access, sign them in to the console, and read health and usage.

It's off unless the controller has an admin key:

```
civitone --admin-key "$CIVITONE_ADMIN_KEY" --public-url https://main-street.civitone.com ...
# or the environment: CIVITONE_ADMIN_KEY, CIVITONE_PUBLIC_URL
```

With an admin key set, **the console needs a sign-in**. People sign in with a
one-time link from `POST /admin/v1/signin`. Without one, the controller works
as before: the console is open and something in front of it (nginx basic
auth) keeps strangers out.

Every request sends the key as a bearer token:

```
Authorization: Bearer <admin key>
```

Wrong or missing key: `401`. Bodies are JSON. Errors are `{"error": "..."}`
with a 4xx/5xx status.

## GET /admin/v1/health

```json
{
  "ok": true,
  "version": "0.5.0",
  "uptime_s": 86400,
  "snapserver": true,
  "suspended": false,
  "poles":   {"adopted": 10, "online": 9, "limit": 24},
  "sources": {"adopted": 2, "online": 1},
  "playing": true,
  "pending": 0
}
```

- `ok` is false when snapserver isn't reachable.
- `limit` is 0 when there's no limit.
- `playing` means at least one zone has an online pole and a source with audio.

## GET /admin/v1/usage?days=31

Usage per day, in the site's time zone, newest first. `pole_hours` counts each online pole while its zone played audio. Bandwidth is about 150 kbps per pole hour, so `est_gb` = `pole_hours` × 0.0675.

```json
{"days": [{"day": "2026-10-21", "playing_hours": 6.5, "pole_hours": 58.2, "est_gb": 3.93}]}
```

## GET /admin/v1/inventory

What the site has, for building access rules. Each zone is the snapserver group with its poles.

```json
{
  "zones":   [{"id": "6d1f…", "name": "Main St", "poles": ["civ-1a2b", "civ-2c3d"], "stream_id": "Music"}],
  "poles":   [{"id": "civ-1a2b", "name": "Pole 1 (Lot A)", "online": true, "fw": "0.4.2"}],
  "sources": [{"id": "src-91ef", "name": "RadioDJ laptop", "stream_id": "src-91ef", "online": true}],
  "streams": [{"id": "Music", "name": "Music"}]
}
```

## PUT /admin/v1/plan

Plan limits, set when a site is created and whenever its subscription changes.

```json
{"plan": "downtown", "pole_limit": 24, "suspended": false, "timezone": "America/New_York"}
```

- **`pole_limit`:** 0 or missing means unlimited. Approving a pole beyond the limit fails: the console shows why, and so does auto-approve in Activity. Sources don't count. Lowering the limit below the adopted count never drops a pole; it only stops new ones.
- **`suspended`:** the console turns read-only for everyone, with a banner. Audio keeps playing as it is.
- **`timezone`:** an IANA name. Event windows and usage days use it. The default is `UTC`.

Every field is optional. Fields you leave out keep their value. The response is the full plan.

## PUT /admin/v1/access

Replaces who may use the console and when. The whole document is sent every time, and the controller keeps the last one it received.

```json
{
  "default_source": "Music",
  "people": [
    {"id": "u1", "name": "Jordan Lee", "email": "jordan@example.org", "role": "owner"},
    {"id": "u7", "name": "Pat Kim",    "email": "pat@example.org",    "role": "operator"},
    {"id": "u12","name": "Sam Ortiz",  "email": "sam@example.org",    "role": "guest"}
  ],
  "events": [
    {
      "id": "e3",
      "name": "Christmas in Springfield",
      "members": ["u12"],
      "poles": ["civ-1a2b", "civ-2c3d"],
      "sources": ["src-91ef"],
      "volume_cap": 80,
      "paging": false,
      "buffer_minutes": 120,
      "windows": [
        {"kind": "once", "start": "2026-12-05T08:00", "end": "2026-12-07T23:00"}
      ]
    },
    {
      "id": "e4",
      "name": "Food Truck Friday",
      "members": ["u15"],
      "poles": ["civ-2c3d"],
      "sources": ["src-77aa"],
      "volume_cap": 65,
      "windows": [
        {"kind": "monthly", "nth": 3, "weekday": "fri", "start": "16:00", "end": "21:00"}
      ]
    }
  ]
}
```

**Roles:**

| Role | Can do |
|---|---|
| `owner`, `operator` | Everything in the console, at any time. |
| `guest` | Only while one of their events is open. Then: the volume and mute of that event's poles (up to `volume_cap` percent), and the source of a zone whose poles all belong to the event. Even then, only the event's `sources` or `default_source`. Mute-all mutes only the event's poles. Paging control only when `paging` is true. Adopting, firmware, settings and speakers stay off. |

**Windows** are in the plan's `timezone`. An end at or before the start runs into the next day.

| `kind` | Fields | Opens |
|---|---|---|
| `once` | `start`, `end`: `YYYY-MM-DDTHH:MM` | Once |
| `weekly` | `days`: `["fri", "sat"]`, `start`, `end`: `HH:MM` | Each listed day |
| `monthly` | `nth` (1–5, or -1 for the last), `weekday`, `start`, `end` | That weekday each month, e.g. the 3rd Friday |

`weekly` and `monthly` take optional `from` and `until` dates (`YYYY-MM-DD`).

An event opens `buffer_minutes` before each window's start (default 0) and closes at its end.

**When an event closes:**
- Every zone still playing one of its sources goes back to `default_source`. Nothing goes silent.
- Its guests drop to a waiting screen.
- The console warns them 15 minutes before.

Outside every window, an event's sources stay connected, and the console shows them as standby with the next start time. Owners and operators can still play them, for example to test the laptop the week before.

A person who isn't in `people` can't sign in. Sessions of people removed from the document end at their next request.

## POST /admin/v1/signin

Request:

```json
{"person": "u12", "redirect": "/"}
```

Response:

```json
{"url": "https://main-street.civitone.com/signin?token=…", "expires_at": "2026-10-21T22:05:00Z"}
```

- The link works once, within 5 minutes.
- Opening it starts a 12-hour console session (an HttpOnly, Secure, SameSite=Lax cookie).
- A guest can sign in outside their windows too. They get the waiting screen with the next opening time.

## GET /admin/v1/backup

A consistent copy of the controller's SQLite database (`application/vnd.sqlite3`): poles, sources, settings, plan, access and activity. To restore, stop the container and put the file back at `/data/civitone.db`.

## GET /api/me (console, not admin)

Who the console is signed in as, and what they may do now. The console reads it at start and every minute. The `/ws` state carries what everyone may see: `plan` (limit, poles used, suspended), `events` (name, sources, open, until, next) and `default_source`.

```json
{
  "auth": "session",
  "person": {"id": "u12", "name": "Sam Ortiz", "role": "guest"},
  "suspended": false,
  "plan": {"plan": "downtown", "pole_limit": 24, "poles_used": 10},
  "open": true,
  "until": "2026-12-07T23:00:00-05:00",
  "next": null,
  "events": [{"id": "e3", "name": "Christmas in Springfield", "open": true, "until": "…", "next": "…"}],
  "can": {"poles": ["civ-1a2b", "civ-2c3d"], "sources": ["src-91ef", "Music"], "volume_cap": 80, "paging": false, "admin": false}
}
```

- **`auth`:** `"open"` when there's no admin key. Everyone is treated as an owner.
- **`can.poles`:** null for owners and operators, meaning all poles.

## Sign-in for self-hosters

With an admin key set, `civitone signin-link` asks the running controller for a link. If the access document has no owner yet, it adds one first.

```
docker compose exec civitone civitone signin-link --name "Pat"
```

## Security notes

- **The admin key** is a bearer secret. Use 32+ random bytes and keep it off command lines. The controller refuses keys shorter than 24 characters.
- **Sign-in links and session cookies** are stored hashed (SHA-256). Sign-in links expire in 5 minutes. Sessions last 12 hours and end at once when the person leaves the access document.
- **With sign-in on,** the console's WebSocket accepts only same-origin browsers. Cookies are HttpOnly and SameSite=Lax.
- **Guest checks happen in the controller,** on every request. The console hiding a control is a convenience, not the check.
