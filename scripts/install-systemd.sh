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

prompt_value() {
	local var_name="$1"
	local label="$2"
	local default_value="$3"
	local value

	read -r -p "${label} [${default_value}]: " value
	printf -v "${var_name}" '%s' "${value:-${default_value}}"
}

prompt_secret() {
	local var_name="$1"
	local label="$2"
	local value

	while [[ -z "${value:-}" ]]; do
		read -r -s -p "${label}: " value
		echo
		if [[ -z "${value}" ]]; then
			echo "Value is required."
		fi
	done

	printf -v "${var_name}" '%s' "${value}"
}

prompt_required() {
	local var_name="$1"
	local label="$2"
	local default_value="${3:-}"
	local value
	local suffix=""

	if [[ -n "${default_value}" ]]; then
		suffix=" [${default_value}]"
	fi

	while [[ -z "${value:-}" ]]; do
		read -r -p "${label}${suffix}: " value
		value="${value:-${default_value}}"
		if [[ -z "${value}" ]]; then
			echo "Value is required."
		fi
	done

	printf -v "${var_name}" '%s' "${value}"
}

prompt_bool() {
	local var_name="$1"
	local label="$2"
	local default_value="$3"
	local value

	while true; do
		read -r -p "${label} [${default_value}]: " value
		value="${value:-${default_value}}"
		case "${value,,}" in
			true|t|yes|y|1)
				printf -v "${var_name}" '%s' "true"
				return
				;;
			false|f|no|n|0)
				printf -v "${var_name}" '%s' "false"
				return
				;;
			*)
				echo "Enter true or false."
				;;
		esac
	done
}

env_value() {
	local value="$1"
	value="${value//\\/\\\\}"
	value="${value//\"/\\\"}"
	printf '"%s"' "${value}"
}

write_env_file() {
	cat >"${ENV_FILE}" <<ENVEOF
IMAP_HOST=$(env_value "${IMAP_HOST_VALUE}")
IMAP_USERNAME=$(env_value "${IMAP_USERNAME_VALUE}")
IMAP_PASSWORD=$(env_value "${IMAP_PASSWORD_VALUE}")
IMAP_MAILBOX=$(env_value "${IMAP_MAILBOX_VALUE}")
API_ENDPOINT=$(env_value "${API_ENDPOINT_VALUE}")
API_HMAC_SECRET=$(env_value "${API_HMAC_SECRET_VALUE}")
POLL_INTERVAL=$(env_value "${POLL_INTERVAL_VALUE}")
STATE_FILE=$(env_value "${STATE_FILE_VALUE}")
API_MAX_RETRIES=$(env_value "${API_MAX_RETRIES_VALUE}")
API_RETRY_BACKOFF=$(env_value "${API_RETRY_BACKOFF_VALUE}")
HTTP_TIMEOUT=$(env_value "${HTTP_TIMEOUT_VALUE}")
PROCESS_EXISTING=$(env_value "${PROCESS_EXISTING_VALUE}")
ENVEOF
}

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
		local replace_config
		prompt_bool replace_config "${ENV_FILE} already exists. Replace it?" "false"
		if [[ "${replace_config}" != "true" ]]; then
			echo "Keeping existing ${ENV_FILE}"
			chown "root:${SERVICE_GROUP}" "${ENV_FILE}"
			chmod 0640 "${ENV_FILE}"
			return
		fi
	fi

	echo "Enter service configuration values."
	prompt_value IMAP_HOST_VALUE "Gmail IMAP host" "imap.gmail.com:993"
	prompt_required IMAP_USERNAME_VALUE "Gmail username"
	prompt_secret IMAP_PASSWORD_VALUE "Gmail app password"
	prompt_value IMAP_MAILBOX_VALUE "IMAP mailbox" "INBOX"
	prompt_required API_ENDPOINT_VALUE "API endpoint URL"
	prompt_secret API_HMAC_SECRET_VALUE "API HMAC secret"
	prompt_value POLL_INTERVAL_VALUE "Poll interval" "30s"
	prompt_value STATE_FILE_VALUE "State file" "${STATE_DIR}/state.json"
	prompt_value API_MAX_RETRIES_VALUE "API max retries" "5"
	prompt_value API_RETRY_BACKOFF_VALUE "API retry backoff" "2s"
	prompt_value HTTP_TIMEOUT_VALUE "HTTP timeout" "30s"
	prompt_bool PROCESS_EXISTING_VALUE "Process existing mailbox messages on first run?" "false"

	write_env_file
	echo "Created ${ENV_FILE}"

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
	grep -Eq '^API_HMAC_SECRET=.+' "${ENV_FILE}" &&
		! grep -Eq 'user@example\.com|app-password|https://api\.example\.com/email-webhook|api-hmac-secret' "${ENV_FILE}"
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
