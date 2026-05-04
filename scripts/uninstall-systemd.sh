#!/usr/bin/env bash
set -euo pipefail

SERVICE_NAME="${SERVICE_NAME:-help-email}"
SERVICE_USER="${SERVICE_USER:-help-email}"
SERVICE_GROUP="${SERVICE_GROUP:-help-email}"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
CONFIG_DIR="${CONFIG_DIR:-/etc/help-email}"
STATE_DIR="${STATE_DIR:-/var/lib/help-email}"
UNIT_FILE="${UNIT_FILE:-/etc/systemd/system/${SERVICE_NAME}.service}"
BINARY_NAME="${BINARY_NAME:-help-email}"
BINARY_PATH="${INSTALL_DIR}/${BINARY_NAME}"

require_root() {
	if [[ "${EUID}" -ne 0 ]]; then
		echo "Run this script as root: sudo $0"
		exit 1
	fi
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

stop_service() {
	if systemctl list-unit-files "${SERVICE_NAME}.service" >/dev/null 2>&1; then
		systemctl disable --now "${SERVICE_NAME}" >/dev/null 2>&1 || true
	fi
}

remove_unit_and_binary() {
	rm -f "${UNIT_FILE}"
	rm -f "${BINARY_PATH}"
	systemctl daemon-reload
	systemctl reset-failed "${SERVICE_NAME}" >/dev/null 2>&1 || true
}

remove_optional_paths() {
	local remove_config
	local remove_state

	if [[ -d "${CONFIG_DIR}" ]]; then
		prompt_bool remove_config "Remove config directory ${CONFIG_DIR}? This deletes stored credentials." "false"
		if [[ "${remove_config}" == "true" ]]; then
			rm -rf "${CONFIG_DIR}"
		fi
	fi

	if [[ -d "${STATE_DIR}" ]]; then
		prompt_bool remove_state "Remove state directory ${STATE_DIR}? This resets processed-message history." "false"
		if [[ "${remove_state}" == "true" ]]; then
			rm -rf "${STATE_DIR}"
		fi
	fi
}

remove_user_and_group() {
	local remove_identity

	if id "${SERVICE_USER}" >/dev/null 2>&1; then
		prompt_bool remove_identity "Remove service user ${SERVICE_USER}?" "false"
		if [[ "${remove_identity}" == "true" ]]; then
			userdel "${SERVICE_USER}" >/dev/null 2>&1 || true
		fi
	fi

	if getent group "${SERVICE_GROUP}" >/dev/null 2>&1; then
		if ! getent passwd | cut -d: -f4 | grep -qx "$(getent group "${SERVICE_GROUP}" | cut -d: -f3)"; then
			groupdel "${SERVICE_GROUP}" >/dev/null 2>&1 || true
		fi
	fi
}

main() {
	require_root
	stop_service
	remove_unit_and_binary
	remove_optional_paths
	remove_user_and_group

	cat <<EOF

Uninstalled ${SERVICE_NAME}.

Removed:
  - ${UNIT_FILE}
  - ${BINARY_PATH}

Config, state, and service user were removed only if you confirmed those prompts.

EOF
}

main "$@"
