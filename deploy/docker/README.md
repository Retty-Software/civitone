# Running Civitone in Docker

One container runs a whole site: the controller, snapserver and the console. Everything it keeps (poles, sources, settings, activity) is in `/data`.

## With Compose (one site, HTTPS included)

1. Point a hostname at your server, e.g. `speakers.example.com`.
2. Next to `compose.yaml`, create `.env`:

   ```
   CIVITONE_HOST=speakers.example.com
   CIVITONE_ADMIN_KEY=<output of: openssl rand -hex 32>
   ```

3. Start it and get a sign-in link:

   ```
   docker compose up -d
   docker compose exec civitone civitone signin-link --name "Your name"
   ```

4. Open the link. In the console, go to **Setup** and make an adopt code. Then set up each pole from its `civitone-xxxx` Wi-Fi.

Open ports 80 and 443 (Caddy: the console, poles and sources) and 1704 (audio).

## Plain `docker run`

```
docker run -d --name civitone --restart unless-stopped \
  -p 127.0.0.1:8080:8080 -p 1704:1704 -v civitone:/data \
  -e CIVITONE_ADMIN_KEY=$(openssl rand -hex 32) \
  -e CIVITONE_PUBLIC_URL=https://speakers.example.com \
  -e CIVITONE_SNAP_HOST=speakers.example.com \
  ghcr.io/retty-software/civitone:latest
```

Put your own HTTPS proxy in front of port 8080. Pass WebSockets through for `/ws`, `/node` and `/source`.

## Settings

| Variable | |
|---|---|
| `CIVITONE_ADMIN_KEY` | Turns on console sign-in and the admin API ([docs/admin-api.md](../../docs/admin-api.md)). Without it, the console is open to anyone who can reach port 8080. |
| `CIVITONE_PUBLIC_URL` | The console's address, used in sign-in links. |
| `CIVITONE_SNAP_HOST` / `CIVITONE_SNAP_PORT` | Where poles find the audio stream. Use the published host and port when they differ from what the poles dial. |

## Backups

`GET /admin/v1/backup` returns a consistent copy of the database. You can also stop the container and copy `/data/civitone.db`.
