#!/bin/sh

set -e

if [ "${DEBUG}" = 1 ]; then
    set -x
fi

# Usage:
#   curl ... | ENV_VAR=... sh -
#       or
#   ENV_VAR=... ./install.sh
#

# Environment variables:
#
#   - URL
#   - TOKEN
#   - CATTLE_CA_CHECKSUM
#

# info logs the given argument at info log level.
info() {
    echo "[INFO] " "$@"
}

# warn logs the given argument at warn log level.
warn() {
    echo "[WARN] " "$@" >&2
}

# fatal logs the given argument at fatal log level.
fatal() {
    echo "[ERROR] " "$@" >&2
    if [ -n "${SUFFIX}" ]; then
        echo "[ALT] Fill me in" >&2
    fi
    exit 1
}

# setup_arch set arch and suffix,
# fatal if architecture not supported.
setup_arch() {
    case ${ARCH:=$(uname -m)} in
    amd64)
        ARCH=amd64
        SUFFIX=$(uname -s | tr '[:upper:]' '[:lower:]')-${ARCH}
        ;;
    x86_64)
        ARCH=amd64
        SUFFIX=$(uname -s | tr '[:upper:]' '[:lower:]')-${ARCH}
        ;;
    *)
        fatal "unsupported architecture ${ARCH}"
        ;;
    esac
}

# verify_downloader verifies existence of
# network downloader executable.
verify_downloader() {
    cmd="$(command -v "${1}")"
    if [ -z "${cmd}" ]; then
        return 1
    fi
    if [ ! -x "${cmd}" ]; then
        return 1
    fi

    # Set verified executable as our downloader program and return success
    DOWNLOADER=${cmd}
    return 0
}

# --- write systemd service file ---
create_systemd_service_file() {
    info "systemd: Creating service file"
    cat <<-EOF >"/etc/systemd/system/rancher-agent.service"
[Unit]
Description=Next Generation Rancher Agent
Documentation=https://www.rancher.com
Wants=network-online.target
After=network-online.target
[Install]
WantedBy=multi-user.target
[Service]
Type=simple
Restart=always
RestartSec=5s
ExecStart=/usr/bin/rancher-agent
EOF
}

download_rancher_agent() {
    info "Downloading rancher-agent from GitHub"
    curl -sfL https://github.com/Oats87/rancher-agent/releases/download/v0.0.1/rancher-agent -o /usr/bin/rancher-agent
    chmod +x /usr/bin/rancher-agent
}

do_install() {
    setup_arch
    verify_downloader curl || fatal "can not find curl for downloading files"

    download_rancher_agent

    mkdir -p /etc/rancher/agent

cat <<-EOF >"/etc/rancher/agent/config.yaml"
workDirectory: /etc/rancher/agent/work
localPlanDirectory: /etc/rancher/agent/plans
remoteEnabled: true
connectionInfoFile: /etc/rancher/agent/conninfo.json
EOF

    create_systemd_service_file
    systemctl enable rancher-agent
    systemctl daemon-reload >/dev/null
    systemctl restart rancher-agent
}

do_install
exit 0