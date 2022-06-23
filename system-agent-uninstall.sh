#!/bin/sh

if [ ! $(id -u) -eq 0 ]; then
  fatal "This script must be run as root."
fi

# Environment variables:
#   System Agent Variables
#   - CATTLE_AGENT_CONFIG_DIR (default: /etc/rancher/agent)
#   - CATTLE_AGENT_VAR_DIR (default: /var/lib/rancher/agent)
#   - CATTLE_AGENT_BIN_PREFIX (default: /usr/local)
#

# warn logs the given argument at warn log level.
warn() {
    echo "[WARN] " "$@" >&2
}

# check_target_mountpoint return success if the target directory is on a dedicated mount point
check_target_mountpoint() {
    mountpoint -q "${CATTLE_AGENT_BIN_PREFIX}"
}

# check_target_ro returns success if the target directory is read-only
check_target_ro() {
    touch "${CATTLE_AGENT_BIN_PREFIX}"/.r-sa-ro-test && rm -rf "${CATTLE_AGENT_BIN_PREFIX}"/.r-sa-ro-test
    test $? -ne 0
}

setup_env() {
    if [ -z "${CATTLE_AGENT_CONFIG_DIR}" ]; then
        CATTLE_AGENT_CONFIG_DIR=/etc/rancher/agent
    fi

    if [ -z "${CATTLE_AGENT_VAR_DIR}" ]; then
        CATTLE_AGENT_VAR_DIR=/var/lib/rancher/agent
    fi

    # --- resources are installed to /usr/local by default, except if /usr/local is on a separate partition or is
    # --- read-only in which case we go into /opt/rancher-system-agent. If variable isn't passed and this criteria is
    # --- true, assume that is what was done, since removing from /usr/local wouldn't be possible anyway.
    if [ -z "${CATTLE_AGENT_BIN_PREFIX}" ]; then
        CATTLE_AGENT_BIN_PREFIX="/usr/local"
        if check_target_mountpoint || check_target_ro; then
            CATTLE_AGENT_BIN_PREFIX="/opt/rancher-system-agent"
            warn "/usr/local is read-only or a mount point; checking ${CATTLE_AGENT_BIN_PREFIX}"
        fi
    fi

}

uninstall_stop_services() {
    if command -v systemctl >/dev/null 2>&1; then
        systemctl stop rancher-system-agent
    fi
}

uninstall_remove_self() {
    rm -f "${CATTLE_AGENT_BIN_PREFIX}/bin/rancher-system-agent-uninstall.sh"
}

uninstall_disable_services()
{
    if command -v systemctl >/dev/null 2>&1; then
        systemctl disable rancher-system-agent || true
        systemctl reset-failed rancher-system-agent || true
        systemctl daemon-reload
    fi
}

uninstall_remove_files() {
    rm -f /etc/systemd/system/rancher-system-agent.service
    rm -f /etc/systemd/system/rancher-system-agent.env
    rm -rf ${CATTLE_AGENT_VAR_DIR}
    rm -rf ${CATTLE_AGENT_CONFIG_DIR}
    rm -f "${CATTLE_AGENT_BIN_PREFIX}/bin/rancher-system-agent"
}

setup_env
uninstall_stop_services
trap uninstall_remove_self EXIT
uninstall_disable_services
uninstall_remove_files
