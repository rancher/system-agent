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
#   - CATTLE_SERVER
#   - CATTLE_TOKEN
#   - CATTLE_CA_CHECKSUM
#
#   - CATTLE_AGENT_LOGLEVEL
#   - CATTLE_AGENT_CONFIG_DIR (default: /etc/rancher/agent)
#   - CATTLE_AGENT_VAR_DIR (default: /var/lib/rancher/agent)
#
#CATTLE_AGENT_LOGLEVEL=debug
#CATTLE_AGENT_CONFIG_DIR=/etc/rancher/agent
#CATTLE_AGENT_VAR_DIR=
#CATTLE_ROLE_CONTROLPLANE=false
#CATTLE_ROLE_ETCD=false
#CATTLE_ROLE_WORKER=false
#CATTLE_CA_CHECKSUM=
#CATTLE_REMOTE_ENABLED=true // defaults to true
#CATTLE_SERVER=
#CATTLE_TOKEN=
#
#CATTLE_LABELS=
#CATTLE_TAINTS=
#
#CATTLE_ID
#CATTLE_AGENT_BINARY_URL

CACERTS_PATH=cacerts
CATTLE_AGENT_BINARY_URL=https://github.com/Oats87/rancher-agent/releases/download/v0.0.2/rancher-agent

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


# parse_args will inspect the argv for --server, --token, --controlplane, --etcd, and --worker, --label x=y, and --taint dead=beef:NoSchedule
parse_args() {
    args=("$@")
    argC=$(($#-1))
    i=0
    firstLabelSet=0
    firstTaintSet=0
    while [ $i -lt $argC ]; do
        case ${args[$i]} in
            "--controlplane")
                info "Control plane node"
                CATTLE_ROLE_CONTROLPLANE=true
                ;;
            "--etcd")
                info "etcd node"
                CATTLE_ROLE_ETCD=true
                ;;
            "--worker")
                info "worker node"
                CATTLE_ROLE_WORKER=true
                ;;
            "--label")
                i=$(($i+1))
                info "Label: ${args[$i]}"
                if [ -n "${CATTLE_LABELS}" ]; then
                    CATTLE_LABELS="${CATTLE_LABELS},${args[$i]}"
                else
                    CATTLE_LABELS="${args[$i]}"
                fi
                ;;
            "--taint")
                i=$(($i+1))
                info "Taint: ${args[$i]}"
                if [ -n "${CATTLE_TAINTS}" ]; then
                    CATTLE_TAINTS="${CATTLE_TAINTS},${args[$i]}"
                else
                    CATTLE_TAINTS="${args[$i]}"
                fi
                ;;
            "--server")
                i=$(($i+1))
                CATTLE_SERVER="${args[$i]}"
                ;;
            "--token")
                i=$(($i+1))
                CATTLE_TOKEN="${args[$i]}"
                ;;
            *)
                fatal "Unknown argument passed in (${args[$i]}) at $i"
                ;;
        esac
        i=$(($i+1))
    done
}

setup_env() {
    if [ -z "${CATTLE_ROLE_CONTROLPLANE}" ]; then
        CATTLE_ROLE_CONTROLPLANE=false
    fi

    if [ -z "${CATTLE_ROLE_ETCD}" ]; then
        CATTLE_ROLE_ETCD=false
    fi

    if [ -z "${CATTLE_ROLE_WORKER}" ]; then
        CATTLE_ROLE_WORKER=false
    fi

    if [ -z "${CATTLE_REMOTE_ENABLED}" ]; then
        CATTLE_REMOTE_ENABLED=true
    else
        CATTLE_REMOTE_ENABLED=$(echo "${CATTLE_REMOTE_ENABLED}" | tr '[:upper:]' '[:lower:]')
    fi

    if [ -z "${CATTLE_AGENT_LOGLEVEL}" ]; then
        CATTLE_AGENT_LOGLEVEL=debug
    else
        CATTLE_AGENT_LOGLEVEL=$(echo "${CATTLE_AGENT_LOGLEVEL}" | tr '[:upper:]' '[:lower:]')
    fi

    if [ "${CATTLE_REMOTE_ENABLED}" == "true" ]; then
        if [ -z "${CATTLE_SERVER}" ]; then
            fatal "\$CATTLE_SERVER was not set"
        fi

        if [ -z "${CATTLE_TOKEN}" ]; then
            fatal "\$CATTLE_TOKEN was not set"
        fi
    fi

    if [ -z "${CATTLE_AGENT_CONFIG_DIR}" ]; then
        CATTLE_AGENT_CONFIG_DIR=/etc/rancher/agent
        info "Using default agent configuration directory ${CATTLE_AGENT_CONFIG_DIR}"
    fi

    if [ -z "${CATTLE_AGENT_VAR_DIR}" ]; then
        CATTLE_AGENT_VAR_DIR=/var/lib/rancher/agent
        info "Using default agent var directory ${CATTLE_AGENT_VAR_DIR}"
    fi

}

ensure_directories() {
    mkdir -p ${CATTLE_AGENT_VAR_DIR}
    mkdir -p ${CATTLE_AGENT_CONFIG_DIR}
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
Environment=CATTLE_LOGLEVEL=${CATTLE_AGENT_LOGLEVEL}
Environment=CATTLE_AGENT_CONFIG=${CATTLE_AGENT_CONFIG_DIR}/config.yaml
ExecStart=/usr/bin/rancher-agent
EOF
}

download_rancher_agent() {
    info "Downloading rancher-agent from ${CATTLE_AGENT_BINARY_URL}"
    curl -sfL "${CATTLE_AGENT_BINARY_URL}" -o /usr/bin/rancher-agent
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
        curl --insecure -s -fL ${CATTLE_SERVER}/${CACERTS_PATH} > $temp
        if [ ! -s $temp ]; then
          error "The environment variable CATTLE_CA_CHECKSUM is set but there is no CA certificate configured at ${CATTLE_SERVER}/${CACERTS_PATH}"
          exit 1
        fi
        err=$(check_x509_cert $temp)
        if [[ $err ]]; then
            error "Value from ${CATTLE_SERVER}/${CACERTS_PATH} does not look like an x509 certificate (${err})"
            error "Retrieved cacerts:"
            cat $temp
            exit 1
        else
            info "Value from ${CATTLE_SERVER}/${CACERTS_PATH} is an x509 certificate"
        fi
        CATTLE_SERVER_CHECKSUM=$(sha256sum $temp | awk '{print $1}')
        if [ $CATTLE_SERVER_CHECKSUM != $CATTLE_CA_CHECKSUM ]; then
            rm -f $temp
            error "Configured cacerts checksum ($CATTLE_SERVER_CHECKSUM) does not match given --ca-checksum ($CATTLE_CA_CHECKSUM)"
            error "Please check if the correct certificate is configured at${CATTLE_SERVER}/${CACERTS_PATH}"
            exit 1
        fi
    fi
}

retrieve_connection_info() {
    if [ "${CATTLE_REMOTE_ENABLED}" == "true" ]; then
        if [ -z "${CATTLE_CA_CHECKSUM}" ]; then
            curl -v -H "Authorization: Bearer ${CATTLE_TOKEN}" -H "X-Cattle-Id: ${CATTLE_ID}" -H "X-Cattle-Role-Etcd: ${CATTLE_ROLE_ETCD}" -H "X-Cattle-Role-Control-Plane: ${CATTLE_ROLE_CONTROLPLANE}" -H "X-Cattle-Role-Worker: ${CATTLE_ROLE_WORKER}" -H "X-Cattle-Labels: ${CATTLE_LABELS}" -H "X-Cattle-Taints: ${CATTLE_TAINTS}" ${CATTLE_SERVER}/v3/connect/agent -o ${CATTLE_AGENT_VAR_DIR}/rancher2_connection_info.json
        else
            curl --insecure -k -v -H "Authorization: Bearer ${CATTLE_TOKEN}" -H "X-Cattle-Id: ${CATTLE_ID}" -H "X-Cattle-Role-Etcd: ${CATTLE_ROLE_ETCD}" -H "X-Cattle-Role-Control-Plane: ${CATTLE_ROLE_CONTROLPLANE}" -H "X-Cattle-Role-Worker: ${CATTLE_ROLE_WORKER}"  -H "X-Cattle-Labels: ${CATTLE_LABELS}" -H "X-Cattle-Taints: ${CATTLE_TAINTS}" ${CATTLE_SERVER}/v3/connect/agent -o ${CATTLE_AGENT_VAR_DIR}/rancher2_connection_info.json
        fi
    fi
}

generate_config() {

cat <<-EOF >"${CATTLE_AGENT_CONFIG_DIR}/config.yaml"
workDirectory: ${CATTLE_AGENT_VAR_DIR}/work
localPlanDirectory: ${CATTLE_AGENT_VAR_DIR}/plans
remoteEnabled: ${CATTLE_REMOTE_ENABLED}
EOF

    if [ "${CATTLE_REMOTE_ENABLED}" == "true" ]; then
        echo connectionInfoFile: ${CATTLE_AGENT_VAR_DIR}/rancher2_connection_info.json >> "${CATTLE_AGENT_CONFIG_DIR}/config.yaml"
    fi
}

generate_cattle_identifier() {
    if [ -z "${CATTLE_ID}" ]; then
        info "Generating Cattle ID"
        if [ -f "${CATTLE_AGENT_CONFIG_DIR}/cattle-id" ]; then
            CATTLE_ID=$(cat ${CATTLE_AGENT_CONFIG_DIR}/cattle-id);
            info "Cattle ID was already detected as ${CATTLE_ID}. Not generating a new one."
            return
        fi

        CATTLE_ID=$(sha256sum /etc/machine-id | awk '{print $1}'); # awk may not be installed. need to think of a way around this.
        echo "${CATTLE_ID}" > ${CATTLE_AGENT_CONFIG_DIR}/cattle-id
        return
    fi
    info "Not generating Cattle ID"
}

do_install() {
    parse_args $@
    setup_env
    ensure_directories
    generate_cattle_identifier
    setup_arch
    verify_downloader curl || fatal "can not find curl for downloading files"

    if [ -n "${CATTLE_CA_CHECKSUM}" ]; then
        validate_ca_checksum
    fi

    download_rancher_agent

    generate_config
    retrieve_connection_info

    create_systemd_service_file
    systemctl enable rancher-agent
    systemctl daemon-reload >/dev/null
    systemctl restart rancher-agent
}

do_install $@
exit 0
