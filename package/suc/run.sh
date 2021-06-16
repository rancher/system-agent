#!/bin/sh

set -x -e

TMPDIRBASE=/var/lib/rancher/agent/tmp

mkdir -p "/host${TMPDIRBASE}"

TMPDIR=$(chroot /host /bin/sh -c "mktemp -d -p ${TMPDIRBASE}")

cleanup() {
    rm -rf "/host${TMPDIR}"
}

cp /opt/rancher-system-agent-suc/install.sh "/host${TMPDIR}"
cp /opt/rancher-system-agent-suc/rancher-system-agent "/host${TMPDIR}"
chmod +x "/host${TMPDIR}/install.sh"

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
export CATTLE_AGENT_BINARY_LOCAL_LOCATION=${TMPDIR}/rancher-system-agent
chroot /host ${TMPDIR}/install.sh "$@"

cleanup
