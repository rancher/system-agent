#!/bin/sh

set -x -e

CATTLE_AGENT_VAR_DIR=${CATTLE_AGENT_VAR_DIR:-/var/lib/rancher/agent}
TMPDIRBASE=${CATTLE_AGENT_VAR_DIR}/tmp

if [ -f /host${CATTLE_AGENT_VAR_DIR}/env/CATTLE_CACERTS_VALID ]; then
  export CATTLE_CACERTS_VALID="$(cat /host${CATTLE_AGENT_VAR_DIR}/env/CATTLE_CACERTS_VALID)"
  echo Using CATTLE_CACERTS_VALID from secret
fi

if [ -f /host${CATTLE_AGENT_VAR_DIR}/env/CATTLE_CA_CHECKSUM ]; then
  export CATTLE_CA_CHECKSUM="$(cat /host${CATTLE_AGENT_VAR_DIR}/env/CATTLE_CA_CHECKSUM)"
  echo Using CATTLE_CA_CHECKSUM from secret
fi

if [ -f /host${CATTLE_AGENT_VAR_DIR}/env/CATTLE_SERVER ]; then
  export CATTLE_SERVER="$(cat /host${CATTLE_AGENT_VAR_DIR}/env/CATTLE_SERVER)"
  echo Using CATTLE_SERVER from secret
fi

if [ -f /host${CATTLE_AGENT_VAR_DIR}/env/CATTLE_TOKEN ]; then
  export CATTLE_TOKEN="$(cat /host${CATTLE_AGENT_VAR_DIR}/env/CATTLE_TOKEN)"
  echo Using CATTLE_TOKEN from secret
fi

mkdir -p "/host${TMPDIRBASE}"

TMPDIR=$(chroot /host /bin/sh -c "mktemp -d -p ${TMPDIRBASE}")

cleanup() {
    rm -rf "/host${TMPDIR}"
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
  for line in $(grep -v '^#' /host/etc/systemd/system/rancher-system-agent.env); do
    var=${line%%=*}
    val=${line##*=}
    eval v=\"\$$var\"
    if [ -z "$v" ]; then
      export "$var=$val"
    fi
  done
fi
chroot /host ${TMPDIR}/install.sh "$@"
