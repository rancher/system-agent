#!/bin/sh

set -x -e

CATTLE_AGENT_VAR_DIR=${CATTLE_AGENT_VAR_DIR:-/var/lib/rancher/agent}
TMPDIRBASE=${CATTLE_AGENT_VAR_DIR}/tmp

mkdir -p "/host${TMPDIRBASE}"

TMPDIR=$(chroot /host /bin/sh -c "mktemp -d -p ${TMPDIRBASE}")

cleanup() {
    rm -rf "/host${TMPDIR}"
}

no_proxy_usage() {
    echo "WARNING: Malformed NO_PROXY environment variable value format, detected whitespace in value. Value must be a comma-delimited string with no spaces containing one or more IP address prefixes (1.2.3.4, 1.2.3.4:80), IP address prefixes in CIDR notation (1.2.3.4/8), domain names, or special DNS labels (*)"
    echo "WARNING: Will automatically remove detected whitespace. This may lead to unexpected behavior."
}

trap cleanup EXIT
trap exit INT HUP TERM

cp /opt/rancher-system-agent-suc/install.sh "/host${TMPDIR}"
cp /opt/rancher-system-agent-suc/rancher-system-agent "/host${TMPDIR}"
cp /opt/rancher-system-agent-suc/system-agent-uninstall.sh "/host${TMPDIR}/rancher-system-agent-uninstall.sh"
chmod +x "/host${TMPDIR}/install.sh"
chmod +x "/host${TMPDIR}/rancher-system-agent-uninstall.sh"

if [ -n "$SYSTEM_UPGRADE_NODE_NAME" ]; then
    NODE_FILE=/host${TMPDIR}/node.yaml
    kubectl get node ${SYSTEM_UPGRADE_NODE_NAME} -o yaml > $NODE_FILE
    if [ -z "$CATTLE_ROLE_ETCD" ] && grep -q 'node-role.kubernetes.io/etcd: "true"' $NODE_FILE; then
        export CATTLE_ROLE_ETCD=true
    fi
    if [ -z "$CATTLE_ROLE_CONTROLPLANE" ] && grep -q 'node-role.kubernetes.io/controlplane: "true"' $NODE_FILE; then
        export CATTLE_ROLE_CONTROLPLANE=true
    fi
    if [ -z "$CATTLE_ROLE_CONTROLPLANE" ] && grep -q 'node-role.kubernetes.io/control-plane: "true"' $NODE_FILE; then
        export CATTLE_ROLE_CONTROLPLANE=true
    fi
    if [ -z "$CATTLE_ROLE_WORKER" ] && grep -q 'node-role.kubernetes.io/worker: "true"' $NODE_FILE; then
        export CATTLE_ROLE_WORKER=true
    fi
fi

export CATTLE_AGENT_BINARY_LOCAL=true
export CATTLE_AGENT_UNINSTALL_LOCAL=true
export CATTLE_AGENT_BINARY_LOCAL_LOCATION=${TMPDIR}/rancher-system-agent
export CATTLE_AGENT_UNINSTALL_LOCAL_LOCATION=${TMPDIR}/rancher-system-agent-uninstall.sh

if [ -s /host/etc/systemd/system/rancher-system-agent.env ]; then
    # Read line by line, skipping any variables that are commented out (#)
    grep -v '^#' /host/etc/systemd/system/rancher-system-agent.env | while IFS= read -r line; do

    # Determine the name and previously set
    # value of the environment variable
    var=${line%%=*}
    val=${line##*=}

    # Check if the variable is already exported.
    eval v=\"\$$var\"
    # If a previously seen environment variable isn't currently exported,
    # reuse the last known value stored in the environment variables file
    if [ -z "$v" ]; then
      export "$var=$val"
      v="$val"
    fi

    # If NO_PROXY is set, ensure it meets the minimum format requirements (no spaces).
    if [ "$var" = "NO_PROXY" ] || [ "$var" = "no_proxy" ]; then
        if [ -n "$v" ]; then
            if echo "$v" | grep -q " "; then
                no_proxy_usage
                v="$(echo "$v" | tr -d '[:space:]')"
                export "$var=$v"
            fi
        fi
    fi

  done
fi

chroot /host ${TMPDIR}/install.sh "$@"
