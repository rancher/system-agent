#!/bin/sh

set -x

TMPDIRBASE=/var/lib/rancher/agent/tmp

mkdir -p /host${TMPDIRBASE}

TMPDIR=$(chroot /host /bin/sh -c "mktemp -d -p ${TMPDIRBASE}")

cleanup() {
    rm -rf "/host${TMPDIR}"
}

cp /opt/rancher-agent-suc/install.sh "/host${TMPDIR}"
chmod +x "/host${TMPDIR}/install.sh"

cp /opt/sucenv/environment "/host${TMPDIR}/env"

cat << 'EOF' > "/host${TMPDIR}/run-install.sh"
#!/bin/sh
env $(cat ${TMPDIR}/env) ${TMPDIR}/install.sh
EOF

chmod +x /host${TMPDIR}/run-install.sh

chroot /host /bin/sh -c "env TMPDIR=${TMPDIR} ${TMPDIR}/run-install.sh $@"

cleanup
