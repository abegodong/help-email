# help-email

A small Go service that polls a Gmail inbox over IMAP, parses new email, and forwards a structured JSON payload to an API endpoint.

## Payload sent to the API

```json
{
  "message_id": "<abc@example.com>",
  "subject": "Support request",
  "sent_at": "2026-05-02T12:34:56Z",
  "received_at": "2026-05-02T12:35:02Z",
  "sender": {
    "email": "real.sender@example.com",
    "name": "Real Sender",
    "source": "forwarded_body"
  },
  "forwarder": {
    "email": "agent@example.com",
    "name": "Agent"
  },
  "recipients": [
    {
      "email": "support@example.com",
      "name": "Support",
      "type": "to"
    }
  ],
  "body": {
    "text": "Plain text body...",
    "html": "<p>HTML body...</p>"
  },
  "attachments": [
    {
      "filename": "screenshot.png",
      "content_type": "image/png",
      "size": 12345,
      "sha256": "b94d27b9934d3e08a52e52d7da7dabfade...",
      "content_base64": "iVBORw0KGgo..."
    }
  ],
  "imap": {
    "mailbox": "INBOX",
    "uid": 123
  }
}
```

`sender.source` is `header` for a normal email and `forwarded_body` when a forwarded-message block reveals the original sender. In forwarded cases, `forwarder` contains the mailbox-visible sender.

Attachments are included inline as base64 by default. For large production mailboxes, a common next step is to upload attachments to object storage and replace `content_base64` with signed URLs.

Each API request includes an `Idempotency-Key` header. It uses `message_id` when available and falls back to `mailbox:uid`, so the receiving API can safely deduplicate retry attempts.

## Configuration

Set these environment variables:

```sh
IMAP_HOST=imap.gmail.com:993
IMAP_USERNAME=user@example.com
IMAP_PASSWORD=app-password
IMAP_MAILBOX=INBOX
API_ENDPOINT=https://api.example.com/email-webhook
POLL_INTERVAL=30s
STATE_FILE=.help-email-state.json
API_MAX_RETRIES=5
API_RETRY_BACKOFF=2s
HTTP_TIMEOUT=30s
PROCESS_EXISTING=false
```

`IMAP_HOST` defaults to Gmail (`imap.gmail.com:993`), so you can omit it for Gmail. For Gmail authentication, use an app password or replace the login flow with OAuth2 if your Workspace policy requires it.

`PROCESS_EXISTING=false` means the first run starts monitoring from the current Gmail `UIDNEXT` and only sends newly arriving messages. Set it to `true` if you want to submit already existing mailbox messages too.

Run:

```sh
go mod tidy
go run ./cmd/help-email
```

The service stores the last successfully submitted and read-marked UID in `STATE_FILE`, so restarts do not resend old mail. After the API endpoint returns a `2xx` response, the service marks the Gmail message as read with the IMAP `\Seen` flag. If submission fails, the message is not marked read and the service retries on the next poll.

## Build

Install Go on Ubuntu 24.04:

```sh
sudo apt update
sudo apt install -y golang-go
```

Fetch dependencies and build the service:

```sh
go mod tidy
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o help-email ./cmd/help-email
```

Run the binary locally:

```sh
./help-email
```

For another CPU architecture, change `GOARCH`. Common values are `amd64` for Intel/AMD servers and `arm64` for ARM servers.

## Run as a systemd service on Ubuntu 24.04

The easiest path is to run the installer script from the repository root:

```sh
sudo ./scripts/install-systemd.sh
```

The script installs Go if needed, builds the binary, creates the `help-email` system user, creates `/etc/help-email/help-email.env` if it does not already exist, installs the systemd unit, and enables the service. It starts or restarts the service only when the environment file no longer contains placeholder credentials.

After the script finishes, edit the environment file with real credentials and restart:

```sh
sudo nano /etc/help-email/help-email.env
sudo systemctl restart help-email
```

Manual setup steps are below if you prefer to install each piece yourself.

Create a dedicated user and install the binary:

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin help-email
sudo install -o root -g root -m 0755 help-email /usr/local/bin/help-email
sudo mkdir -p /var/lib/help-email /etc/help-email
sudo chown help-email:help-email /var/lib/help-email
```

Create `/etc/help-email/help-email.env`:

```sh
IMAP_HOST=imap.gmail.com:993
IMAP_USERNAME=user@example.com
IMAP_PASSWORD=app-password
IMAP_MAILBOX=INBOX
API_ENDPOINT=https://api.example.com/email-webhook
POLL_INTERVAL=30s
STATE_FILE=/var/lib/help-email/state.json
API_MAX_RETRIES=5
API_RETRY_BACKOFF=2s
HTTP_TIMEOUT=30s
PROCESS_EXISTING=false
```

Lock down the environment file because it contains the Gmail password:

```sh
sudo chown root:help-email /etc/help-email/help-email.env
sudo chmod 0640 /etc/help-email/help-email.env
```

Create `/etc/systemd/system/help-email.service`:

```ini
[Unit]
Description=Gmail IMAP to API email monitor
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=help-email
Group=help-email
EnvironmentFile=/etc/help-email/help-email.env
ExecStart=/usr/local/bin/help-email
Restart=always
RestartSec=10
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/help-email

[Install]
WantedBy=multi-user.target
```

Enable and start it:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now help-email
```

Useful operations:

```sh
sudo systemctl status help-email
sudo journalctl -u help-email -f
sudo systemctl restart help-email
sudo systemctl stop help-email
```

To deploy a new build, replace `/usr/local/bin/help-email` and restart the service:

```sh
sudo install -o root -g root -m 0755 help-email /usr/local/bin/help-email
sudo systemctl restart help-email
```
