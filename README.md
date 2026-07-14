# Dockyard

Docker container update manager with a web UI, forked from [watchtower](https://github.com/nicholas-fedor/watchtower).

Monitors running Docker containers and updates them when new images are released. Features a web dashboard for managing updates, per-container update modes, deferred updates, and self-updating.

## Install

Create a `docker-compose.yml` on your server:

```yaml
services:
  dockyard:
    image: ghcr.io/chiabotta35/dockyard:latest
    container_name: dockyard
    restart: unless-stopped
    ports:
      - "8082:8080"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - ./data:/app/data
    environment:
      - DOCKYARD_ADMIN_USER=admin
      - DOCKYARD_ADMIN_PASSWORD=changeme
      - DOCKYARD_SCHEDULE=0 3 * * *
      - DOCKYARD_CLEANUP=true
      - DOCKER_HOST=unix:///var/run/docker.sock
      - TZ=UTC
    security_opt:
      - no-new-privileges:true
    read_only: true
    tmpfs:
      - /tmp
    deploy:
      resources:
        limits:
          memory: 512M
          cpus: "1.0"
```

Then:

```bash
docker compose up -d
```

Open `http://<your-server-ip>:8082` and sign in with the credentials from `DOCKYARD_ADMIN_USER` / `DOCKYARD_ADMIN_PASSWORD`.

### Stopping watchtower

Dockyard replaces watchtower. If you have it running:

```bash
docker stop watchtower && docker rm watchtower
```

### Changing the port

Edit the `ports` line:

```yaml
ports:
  - "9090:8080"   # your-port:container-port
```

### Building from source

```bash
git clone https://github.com/chiabotta35/Dockyard.git
cd Dockyard
docker compose up -d --build
```

## Environment Variables

All settings below can be set in your `docker-compose.yml` and are also editable in the web UI after first launch.

### Admin Account

| Variable | Required | Description |
|----------|----------|-------------|
| `DOCKYARD_ADMIN_USER` | Yes | Admin username |
| `DOCKYARD_ADMIN_PASSWORD` | Yes | Admin password (min 8 chars). Changing this resets the password for the user. |

### Schedule

| Variable | Default | Description |
|----------|---------|-------------|
| `DOCKYARD_SCHEDULE` | `0 3 * * *` | Cron schedule for update checks |
| `DOCKYARD_TIMEZONE` | `UTC` | IANA timezone (e.g. `America/New_York`) |
| `DOCKYARD_UPDATE_ON_START` | `false` | Check for updates immediately on startup |

### Update Behavior

| Variable | Default | Description |
|----------|---------|-------------|
| `DOCKYARD_CLEANUP` | `true` | Remove old images after successful update |
| `DOCKYARD_MONITOR_ONLY` | `false` | Monitor only, never automatically update |
| `DOCKYARD_ROLLING_RESTART` | `false` | Update containers one at a time |
| `DOCKYARD_LIFECYCLE_HOOKS` | `false` | Run pre/post update lifecycle hooks |
| `DOCKYARD_COOLDOWN_DELAY` | `0s` | Minimum image age before update (e.g. `24h`, `3d`, `1w`) |
| `DOCKYARD_STOP_TIMEOUT` | `30s` | Timeout when stopping containers |

### Backup

| Variable | Default | Description |
|----------|---------|-------------|
| `DOCKYARD_BACKUP_RETENTION` | `false` | Keep old containers for rollback |
| `DOCKYARD_BACKUP_WINDOW_HOURS` | `24` | How long to keep backups (1-720 hours) |

### Notifications

| Variable | Default | Description |
|----------|---------|-------------|
| `DOCKYARD_NOTIFICATION_URL` | (empty) | [Shoutrrr](https://containrrr.dev/shoutrrr/) URL for notifications |

Examples:
- `ntfy://mytopic` -- ntfy.sh
- `discord://webhookid/webhooktoken` -- Discord
- `slack://token-a/token-b/token-c` -- Slack
- `email://user:pass@smtp.example.com/?to=recipient` -- Email

### Docker

| Variable | Default | Description |
|----------|---------|-------------|
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon socket |
| `DOCKER_TLS_VERIFY` | (empty) | Enable TLS for Docker connection |
| `DOCKER_API_VERSION` | (empty) | Docker API version override |
| `TZ` | `UTC` | Container timezone |

## Features

- **Web Dashboard** -- Dark-themed UI with real-time SSE log streaming
- **Per-Container Modes** -- Set each container to auto, manual, or ignore
- **Deferred Updates** -- Postpone updates for specific containers (7, 14, 30+ days)
- **Self-Update** -- Update Dockyard itself from the web UI
- **Authentication** -- bcrypt password hashing, session-based auth, CSRF-safe cookies
- **Scheduled Updates** -- Cron-based scheduling (default: daily at 3 AM)
- **SSE Live Logs** -- Real-time event streaming for container operations
- **Update History** -- Track all past updates with timestamps and status
- **Settings** -- Configure schedule, behavior, backup, and notifications from the UI

## Security

- **Authentication**: bcrypt password hashing, 32-byte random session tokens
- **Cookies**: HttpOnly, SameSite=Strict, configurable Secure flag
- **Sessions**: Invalidate all sessions on password change
- **Headers**: CSP, X-Frame-Options: DENY, X-Content-Type-Options: nosniff, X-XSS-Protection
- **Input Validation**: Container name sanitization, request body size limits (1 MB), URL scheme validation
- **File Permissions**: Auth and state files written with `0600`
- **Self-Update**: Direct HTTP download (no shell execution), SHA-256 checksum logging, backup/rollback on failure
- **Docker**: Non-root user in container, read-only root filesystem, `no-new-privileges`, resource limits

## License

Apache License 2.0 -- see [LICENSE](LICENSE) for details.

Originally based on [watchtower](https://github.com/nicholas-fedor/watchtower) by Nicholas Fedor.
