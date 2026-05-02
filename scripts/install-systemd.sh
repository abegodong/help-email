#!/usr/bin/env bash
set -euo pipefail

SERVICE_NAME="${SERVICE_NAME:-help-email}"
SERVICE_USER="${SERVICE_USER:-help-email}"
SERVICE_GROUP="${SERVICE_GROUP:-help-email}"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
CONFIG_DIR="${CONFIG_DIR:-/etc/help-email}"
STATE_DIR="${STATE_DIR:-/var/lib/help-email}"
ENV_FILE="${ENV_FILE:-${CONFIG_DIR}/help-email.env}"
UNIT_FILE="${UNIT_FILE:-/etc/systemd/system/${SERVICE_NAME}.service}"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BINARY_NAME="${BINARY_NAME:-help-email}"
BINARY_PATH="${INSTALL_DIR}/${BINARY_NAME}"
GOARCH_VALUE="${GOARCH_VALUE:-amd64}"

require_root() {
	if [[ "${EUID}" -ne 0 ]]; then
		echo "Run this script as root: sudo $0"
		exit 1
	fi
}

install_go_if_missing() {
	if command -v go >/dev/null 2>&1; then
		return
	fi

	echo "Go is not installed. Installing golang-go with apt..."
	apt-get update
	apt-get install -y golang-go
}

create_service_user() {
	if ! getent group "${SERVICE_GROUP}" >/dev/null 2>&1; then
		groupadd --system "${SERVICE_GROUP}"
	fi

	if id "${SERVICE_USER}" >/dev/null 2>&1; then
		return
	fi

	useradd --system --no-create-home --shell /usr/sbin/nologin --gid "${SERVICE_GROUP}" "${SERVICE_USER}"
}

build_binary() {
	echo "Building ${BINARY_NAME}..."
	cd "${REPO_DIR}"
	go mod tidy
	CGO_ENABLED=0 GOOS=linux GOARCH="${GOARCH_VALUE}" go build -trimpath -ldflags="-s -w" -o "${REPO_DIR}/${BINARY_NAME}" ./cmd/help-email
	install -o root -g root -m 0755 "${REPO_DIR}/${BINARY_NAME}" "${BINARY_PATH}"
}

create_directories() {
	mkdir -p "${CONFIG_DIR}" "${STATE_DIR}"
	chown "${SERVICE_USER}:${SERVICE_GROUP}" "${STATE_DIR}"
	chmod 0750 "${STATE_DIR}"
}

create_env_file() {
	if [[ -f "${ENV_FILE}" ]]; then
		echo "Keeping existing ${ENV_FILE}"
	else
		cat >"${ENV_FILE}" <<'ENVEOF'
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
ENVEOF
		echo "Created ${ENV_FILE}. Edit it before relying on the service."
	fi

	chown "root:${SERVICE_GROUP}" "${ENV_FILE}"
	chmod 0640 "${ENV_FILE}"
}

create_systemd_unit() {
	cat >"${UNIT_FILE}" <<EOF
[Unit]
Description=Gmail IMAP to API email monitor
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_GROUP}
EnvironmentFile=${ENV_FILE}
ExecStart=${BINARY_PATH}
Restart=always
RestartSec=10
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${STATE_DIR}

[Install]
WantedBy=multi-user.target
EOF
}

enable_service() {
	systemctl daemon-reload
	systemctl enable "${SERVICE_NAME}"

	if config_is_ready; then
		systemctl restart "${SERVICE_NAME}"
	else
		echo "Service enabled but not started because ${ENV_FILE} still contains placeholder values."
	fi
}

config_is_ready() {
	! grep -Eq 'user@example\.com|app-password|https://api\.example\.com/email-webhook' "${ENV_FILE}"
}

main() {
	require_root
	install_go_if_missing
	create_service_user
	create_directories
	create_env_file
	build_binary
	create_systemd_unit
	enable_service

	cat <<EOF

Installed ${SERVICE_NAME}.

Next steps:
  1. Edit ${ENV_FILE} with real Gmail and API credentials.
  2. Start or restart after editing: sudo systemctl restart ${SERVICE_NAME}
  3. Watch logs: sudo journalctl -u ${SERVICE_NAME} -f
  4. Check status: sudo systemctl status ${SERVICE_NAME}

EOF
}

main "$@"
