# octopus-dns-failover

Cloudflare DNS failover monitor for octopus-review.ai. Runs on AWS, health checks the primary (Proxmox/Tunnel) origin via dual-check (HTTP + Tunnel API), and switches DNS to AWS failover IP when primary is down. Notifies via Slack and Twilio phone calls.

## How it works

```
Every 5s:
  1. HTTP check: GET health.octopus-review.ai/api/version (Host: octopus-review.ai)
     → Always routes through Cloudflare Tunnel, DNS-independent
  2. Tunnel API check: GET /accounts/{id}/cfd_tunnel/{id}
     → Checks tunnel status via Cloudflare API

  IF both checks fail 3 times AND on primary:
    Delete CNAME record (tunnel)
    Create A record (AWS IP)
    Notify Slack immediately
    Schedule Twilio call (5 min delay)

  IF both checks pass 3 times AND on failover:
    Delete A record (AWS IP)
    Create CNAME record (tunnel)
    Notify Slack immediately
    Cancel pending Twilio call
```

On startup, reads current DNS records to detect initial state (primary or failover).

## Health Check Design

The `health.octopus-review.ai` subdomain is a CNAME pointing directly to the tunnel. This means health checks always go through the tunnel regardless of where `octopus-review.ai` DNS points. Combined with the Tunnel API status check, this provides reliable detection of both:
- **Tunnel down** (entire Proxmox unreachable)
- **App down** (nginx/web crashed but tunnel is healthy)

## Notifications

### Slack
Instant notification on failover and failback to `#octopus-events`.

### Twilio Phone Calls
On failover, waits **5 minutes** before calling. If primary recovers within 5 minutes (e.g. during deploys), the call is cancelled. This prevents false alarms during routine deployments.

Call chain: calls numbers in order from `TWILIO_TO_NUMBER` (comma-separated). If a person answers, stops. If voicemail is detected, hangs up and tries the next number.

## Setup

### 1. Cloudflare Configuration

**API Token:** Cloudflare Dashboard → My Profile → API Tokens → Create Token
- Zone / DNS / Edit
- Account / Cloudflare Tunnel / Read

**Account ID:** Cloudflare Dashboard → any site → Overview → right sidebar

**Zone ID:** Cloudflare Dashboard → octopus-review.ai → Overview → right sidebar

**Health subdomain:** Create a CNAME record `health.octopus-review.ai` → `<tunnel-id>.cfargotunnel.com` (proxied). Do not touch this record — failover only swaps the main domain record.

**DNS Record:** The main `octopus-review.ai` record must be a regular CNAME (not Tunnel type). If using dashboard-managed tunnel, remove the hostname from tunnel config and manually create the CNAME.

### 2. Twilio Configuration

Buy a US phone number from Twilio Console. Enable Geo Permissions for target countries (e.g. United Kingdom). Upload alert voice message to Twilio Assets and note the URL.

### 3. Create .env

```bash
cp .env.example .env
# Fill in your values
```

### 4. Deploy on AWS

```bash
# Test with dry run first
DRY_RUN=true docker compose up

# Run for real
docker compose up -d
```

### 5. Verify

```bash
docker logs -f cf-failover
```

## Config

| Env Var | Default | Description |
|---------|---------|-------------|
| CHECK_INTERVAL | 5s | Time between checks |
| FAIL_THRESHOLD | 3 | Failures before failover |
| RECOVER_THRESHOLD | 3 | Successes before failback |
| REQUEST_TIMEOUT | 10s | HTTP timeout per check |
| CF_API_TOKEN | (required) | Cloudflare API token (DNS edit + Tunnel read) |
| CF_ACCOUNT_ID | (required) | Cloudflare account ID |
| CF_ZONE_ID | (required) | Cloudflare zone ID |
| DOMAIN | octopus-review.ai | Domain name |
| PRIMARY_CNAME | (required) | Tunnel CNAME (e.g. `<uuid>.cfargotunnel.com`) |
| FAILOVER_IP | (required) | AWS failover IP |
| SLACK_WEBHOOK_URL | (optional) | Slack incoming webhook URL |
| TWILIO_ACCOUNT_SID | (optional) | Twilio Account SID |
| TWILIO_AUTH_TOKEN | (optional) | Twilio Auth Token |
| TWILIO_FROM_NUMBER | (optional) | Twilio caller number |
| TWILIO_TO_NUMBER | (optional) | Numbers to call, comma-separated, in priority order |
| DRY_RUN | false | Log only, don't change DNS |

## Failover timing

- Detection: 3 checks x 5s = **15 seconds**
- DNS switch: near-instant (Cloudflare proxied)
- Twilio call: **5 minutes** after failover (cancelled if primary recovers)
- Total downtime: **~15-20 seconds**

## AWS nginx

AWS nginx needs SSL (self-signed) because Cloudflare proxied mode connects to origin via HTTPS. Use `nginx.aws.conf` which adds `listen 443 ssl` to the standard config. Generate certs:

```bash
openssl req -x509 -nodes -days 3650 -newkey rsa:2048 \
  -keyout ssl.key -out ssl.crt -subj "/CN=octopus-review.ai"
```

## Files

| File | Description |
|------|-------------|
| `main.go` | Failover monitor (single binary, no dependencies) |
| `nginx.aws.conf` | AWS nginx config with SSL |
| `.env.example` | Environment variable template |
| `Dockerfile` | Multi-stage build for the monitor |
| `docker-compose.yml` | Docker Compose config for deployment |
