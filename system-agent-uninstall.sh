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

#Providing default ALPINE_LOG_DIR
ALPINE_LOG_DIR=/var/log

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

# ^@ Made Changes for Alpine Linux
uninstall_stop_services() {
    
    if [ "$LINUX_VER"=="Alpine Linux" ]; then
        if [[ "$(rc-service rancher-system-agent status &> /dev/null)" == "* status: started" ]]; then
            rc-service rancher-system-agent stop
        fi
    elif command -v systemctl >/dev/null 2>&1; then
        systemctl stop rancher-system-agent
    fi
}

uninstall_remove_self() {
    rm -f "${CATTLE_AGENT_BIN_PREFIX}/bin/rancher-system-agent-uninstall.sh"
}

# ^@ Made Changes for Alpine Linux
uninstall_disable_services()
{
    if [ "$LINUX_VER"=="Alpine Linux" ]; then
        rc-update delete rancher-system-agent

    elif command -v systemctl >/dev/null 2>&1; then
        systemctl disable rancher-system-agent || true
        systemctl reset-failed rancher-system-agent || true
        systemctl daemon-reload
    fi
}

# ^@ Made Changes for Alpine Linux
uninstall_remove_files() {
    
    if [ "$LINUX_VER"=="Alpine Linux" ]; then
        rm -f /etc/init.d/rancher-system-agent
        rm -rf $ALPINE_LOG_DIR
        rm -f ${CATTLE_AGENT_CONFIG_DIR}/rancher/rancher-system-agent.env
    elif
        rm -f /etc/systemd/system/rancher-system-agent.service
        rm -f /etc/systemd/system/rancher-system-agent.env
    fi
    rm -rf ${CATTLE_AGENT_VAR_DIR}
    rm -rf ${CATTLE_AGENT_CONFIG_DIR}
    rm -f "${CATTLE_AGENT_BIN_PREFIX}/bin/rancher-system-agent"
}

# ^@ Added OS Detection for Alpine Linux
detect_os() {
    LINUX_VER=$(head -1 /etc/os-release | cut -d'=' -f2 | awk '{print substr($0, 2, length($0) - 2)}')
    #Alternate Function
    #LINUX_VER=$(head -1 /etc/os-release | cut -d'=' -f2 | tr -d '"')
    
    #Overriding Detection in case Env File is present
    if [[ -f ${CATTLE_AGENT_CONFIG_DIR}/rancher-service-uninstall.env ]]; then 
    LINUX_VER=$(awk "NR==1" ${CATTLE_AGENT_CONFIG_DIR}/rancher-service-uninstall.env | cut -d '=' -f2)
    ALPINE_LOG_DIR=$(awk "NR==2" ${CATTLE_AGENT_CONFIG_DIR}/rancher-service-uninstall.env | cut -d '=' -f2)
    fi
}

detect_os
setup_env
uninstall_stop_services
trap uninstall_remove_self EXIT
uninstall_disable_services
uninstall_remove_files
