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

CACERTS_PATH=cacerts

# info logs the given argument at info log level.
info() {
    echo "[INFO] " "$@"
}

# warn logs the given argument at warn log level.
warn() {
    echo "[WARN] " "$@" >&2
}

# error logs the given argument at error log level.
error() {
    echo "[ERROR] " "$@" >&2
}

# fatal logs the given argument at fatal log level.
fatal() {
    echo "[ERROR] " "$@" >&2
    if [ -n "${SUFFIX}" ]; then
        echo "[ALT] Fill me in" >&2
    fi
    exit 1
}

setup_env() {
    if [ -z "${URL}" ]; then
        fatal "\$URL was not set"
    fi

    if [ -z "${TOKEN}" ]; then
        fatal "\$TOKEN was not set"
    fi

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
    curl -sfL https://github.com/Oats87/rancher-agent/releases/download/old-checksum/rancher-agent -o /usr/bin/rancher-agent
    chmod +x /usr/bin/rancher-agent
}

check_x509_cert()
{
    local cert=$1
    local err
    err=$(openssl x509 -in $cert -noout 2>&1)
    if [ $? -eq 0 ]
    then
        echo ""
    else
        echo ${err}
    fi
}

validate_ca_checksum() {
    if [ -n "$CATTLE_CA_CHECKSUM" ]; then
        temp=$(mktemp)
        curl --insecure -s -fL ${URL}/${CACERTS_PATH} > $temp
        if [ ! -s $temp ]; then
          error "The environment variable CATTLE_CA_CHECKSUM is set but there is no CA certificate configured at ${URL}/${CACERTS_PATH}"
          exit 1
        fi
        err=$(check_x509_cert $temp)
        if [[ $err ]]; then
            error "Value from ${URL}/${CACERTS_PATH} does not look like an x509 certificate (${err})"
            error "Retrieved cacerts:"
            cat $temp
            exit 1
        else
            info "Value from ${URL}/${CACERTS_PATH} is an x509 certificate"
        fi
        CATTLE_SERVER_CHECKSUM=$(sha256sum $temp | awk '{print $1}')
        if [ $CATTLE_SERVER_CHECKSUM != $CATTLE_CA_CHECKSUM ]; then
            rm -f $temp
            error "Configured cacerts checksum ($CATTLE_SERVER_CHECKSUM) does not match given --ca-checksum ($CATTLE_CA_CHECKSUM)"
            error "Please check if the correct certificate is configured at ${URL}/${CACERTS_PATH}"
            exit 1
        fi
    fi
}

retrieve_connection_info() {
    if [ -z "${CATTLE_CA_CHECKSUM}" ]; then
        curl -v -H "Authorization: Bearer ${TOKEN}" ${URL}/v3/connect/agent -o /etc/rancher/agent/conninfo.json
    else
        curl --insecure -k -v -H "Authorization: Bearer ${TOKEN}" ${URL}/v3/connect/agent -o /etc/rancher/agent/conninfo.json
    fi
}

do_install() {
    setup_env
    setup_arch
    verify_downloader curl || fatal "can not find curl for downloading files"

    if [ -n "${CATTLE_CA_CHECKSUM}" ]; then
        validate_ca_checksum
    fi

    download_rancher_agent

    mkdir -p /etc/rancher/agent

cat <<-EOF >"/etc/rancher/agent/config.yaml"
workDirectory: /etc/rancher/agent/work
localPlanDirectory: /etc/rancher/agent/plans
remoteEnabled: true
connectionInfoFile: /etc/rancher/agent/conninfo.json
EOF

    retrieve_connection_info

    create_systemd_service_file
    systemctl enable rancher-agent
    systemctl daemon-reload >/dev/null
    systemctl restart rancher-agent
}

do_install
exit 0
