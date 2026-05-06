# help-email

A small Go service that polls an email inbox, parses new email, and forwards a structured JSON payload to an API endpoint. It supports Gmail over IMAP and Microsoft 365 shared or user mailboxes through Microsoft Graph.

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
  },
  "graph": {
    "mailbox": "shared-mailbox@example.com",
    "message_id": "AAMkAG...",
    "conversation_id": "AAQkAG...",
    "received_at": "2026-05-02T12:35:02Z"
  }
}
```

Only one provider metadata object is sent per message: `imap` for Gmail/IMAP or `graph` for Microsoft 365.

`sender.source` is `header` for a normal email and `forwarded_body` when a forwarded-message block reveals the original sender. In forwarded cases, `forwarder` contains the mailbox-visible sender.

Attachments are included inline as base64 by default. For large production mailboxes, a common next step is to upload attachments to object storage and replace `content_base64` with signed URLs.

Each API request includes an `Idempotency-Key` header. It uses `message_id` when available and falls back to provider metadata, so the receiving API can safely deduplicate retry attempts.

Each API request is also signed with HMAC-SHA256. The service signs this string:

```text
<X-Help-Email-Timestamp>.<raw JSON request body>
```

It sends these headers:

```text
X-Help-Email-Signature-Algorithm: hmac-sha256
X-Help-Email-Timestamp: 2026-05-02T12:35:02Z
X-Help-Email-Signature: <hex-encoded HMAC-SHA256>
```

The API should recompute the signature with the shared `API_HMAC_SECRET` and compare it using a constant-time comparison. The timestamp lets the API reject stale replay attempts.

## Configuration

Set these environment variables:

```sh
MAIL_PROVIDER=imap
IMAP_HOST=imap.gmail.com:993
IMAP_USERNAME=user@example.com
IMAP_PASSWORD=app-password
IMAP_MAILBOX=INBOX
API_ENDPOINT=https://api.example.com/email-webhook
API_HMAC_SECRET=replace-with-shared-secret
POLL_INTERVAL=30s
STATE_FILE=.help-email-state.json
API_MAX_RETRIES=5
API_RETRY_BACKOFF=2s
HTTP_TIMEOUT=30s
PROCESS_EXISTING=false
```

`MAIL_PROVIDER=imap` uses Gmail or another IMAP mailbox. `MAIL_PROVIDER=graph` uses Microsoft Graph for Microsoft 365.

`IMAP_HOST` defaults to Gmail (`imap.gmail.com:993`), so you can omit it for Gmail. For Gmail authentication, use an app password or replace the login flow with OAuth2 if your Workspace policy requires it.

`PROCESS_EXISTING=false` means the first run starts monitoring from the current Gmail `UIDNEXT` and only sends newly arriving messages. Set it to `true` if you want to submit already existing mailbox messages too.

For Microsoft 365 Graph, use:

```sh
MAIL_PROVIDER=graph
GRAPH_TENANT_ID=00000000-0000-0000-0000-000000000000
GRAPH_CLIENT_ID=00000000-0000-0000-0000-000000000000
GRAPH_CLIENT_SECRET=client-secret
GRAPH_MAILBOX=shared-mailbox@example.com,second-mailbox@example.com
API_ENDPOINT=https://api.example.com/email-webhook
API_HMAC_SECRET=replace-with-shared-secret
POLL_INTERVAL=30s
STATE_FILE=.help-email-state.json
API_MAX_RETRIES=5
API_RETRY_BACKOFF=2s
HTTP_TIMEOUT=30s
PROCESS_EXISTING=false
```

For unattended system service use, create a Microsoft Entra app registration and grant Microsoft Graph application permission `Mail.ReadWrite`, then admin-consent it. In production, restrict the app to the intended mailboxes with an Exchange Online application access policy. `GRAPH_MAILBOX` is a comma-separated list of user mailbox or shared mailbox email addresses.

For Graph, the service queries unread Inbox messages in each configured mailbox and marks each message read only after successful API submission. `PROCESS_EXISTING=false` skips messages that are already read, but unread messages in the mailbox are still eligible on the first run. After successful API submission, Graph messages are marked read by setting `isRead` to `true`. State is tracked separately per Graph mailbox.

## Get Microsoft 365 Graph config from Entra

You need these values for `MAIL_PROVIDER=graph`:

```sh
GRAPH_TENANT_ID=
GRAPH_CLIENT_ID=
GRAPH_CLIENT_SECRET=
GRAPH_MAILBOX=
```

Create the app registration:

1. Sign in to the [Microsoft Entra admin center](https://entra.microsoft.com/).
2. Go to **Entra ID > App registrations > New registration**.
3. Give the app a name, for example `help-email`.
4. Choose **Accounts in this organizational directory only** unless you intentionally need a multi-tenant app.
5. Select **Register**.

Copy IDs from the app overview:

1. Open the app registration.
2. Go to **Overview**.
3. Copy **Directory (tenant) ID** into `GRAPH_TENANT_ID`.
4. Copy **Application (client) ID** into `GRAPH_CLIENT_ID`.

Create the client secret:

1. In the app registration, go to **Certificates & secrets**.
2. Select **Client secrets > New client secret**.
3. Add a description and expiration.
4. Select **Add**.
5. Copy the secret **Value** immediately into `GRAPH_CLIENT_SECRET`. The portal only shows this value once.

Grant Microsoft Graph mail permissions:

1. In the app registration, go to **API permissions**.
2. Select **Add a permission**.
3. Select **Microsoft Graph**.
4. Select **Application permissions**.
5. Add `Mail.ReadWrite`.
6. Select **Grant admin consent** for the tenant.

Set the mailbox:

```sh
GRAPH_MAILBOX=shared-mailbox@example.com,second-mailbox@example.com
```

`GRAPH_MAILBOX` should contain one or more comma-separated primary SMTP addresses or user principal names of the Microsoft 365 mailboxes to monitor. Each entry can be a normal user mailbox or a shared mailbox, and all entries use the same Entra app credentials.

Recommended production scoping:

`Mail.ReadWrite` as an application permission can allow access to mailboxes across the tenant. Ask the Exchange/Microsoft 365 admin to restrict this app to only the target mailbox or mailbox group. Microsoft now documents Exchange Online RBAC for Applications for resource-scoped Graph access; older tenants may also use Application Access Policies.

Run:

```sh
go mod tidy
go run ./cmd/help-email
```

The service stores provider state in `STATE_FILE`, so restarts do not resend old mail. After the API endpoint returns a `2xx` response, the service marks the source message as read. If submission fails, the message is not marked read and the service retries on the next poll.

## Build

Install Go on Ubuntu 24.04:

```sh
sudo apt update
sudo apt install -y golang-go
```

Fetch dependencies and build the service:

```sh
go mod tidy
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath -ldflags="-s -w" -o help-email ./cmd/help-email
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

The script installs Go if needed, builds the binary, creates the `help-email` system user, asks interactively for the mail provider and API configuration, writes `/etc/help-email/help-email.env`, installs the systemd unit, and enables the service. If the environment file already exists, the script asks whether to keep or replace it. Secret prompts are hidden.

After the script finishes, review the environment file and restart if you changed anything:

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
MAIL_PROVIDER=imap
IMAP_HOST=imap.gmail.com:993
IMAP_USERNAME=user@example.com
IMAP_PASSWORD=app-password
IMAP_MAILBOX=INBOX
GRAPH_TENANT_ID=
GRAPH_CLIENT_ID=
GRAPH_CLIENT_SECRET=
GRAPH_MAILBOX=
API_ENDPOINT=https://api.example.com/email-webhook
API_HMAC_SECRET=replace-with-shared-secret
POLL_INTERVAL=30s
STATE_FILE=/var/lib/help-email/state.json
API_MAX_RETRIES=5
API_RETRY_BACKOFF=2s
HTTP_TIMEOUT=30s
PROCESS_EXISTING=false
```

Lock down the environment file because it contains mail provider credentials and API HMAC secret:

```sh
sudo chown root:help-email /etc/help-email/help-email.env
sudo chmod 0640 /etc/help-email/help-email.env
```

Create `/etc/systemd/system/help-email.service`:

```ini
[Unit]
Description=Email to API monitor
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

## Uninstall the systemd service

Run the uninstaller from the repository root:

```sh
sudo ./scripts/uninstall-systemd.sh
```

The script stops and disables the service, removes `/etc/systemd/system/help-email.service`, removes `/usr/local/bin/help-email`, reloads systemd, and asks before deleting `/etc/help-email`, `/var/lib/help-email`, or the `help-email` service user.
