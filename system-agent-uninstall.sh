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

DIRECTORY_RKE2="/usr/local/bin"
DIRECTORY_OPT_RKE2="/opt/rke2/bin"
DIRECTORY_K3S="/usr/local/bin"
DIRECTORY_OPT_K3S="/opt/k3s/bin"

# info logs the given argument at info log level.
info() {
	echo "[INFO] " "$@"
}

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

	if [ -z "${DISTRO_RKE2}" ]; then
		DISTRO_RKE2=false
	fi

	if [ -z "${DISTRO_UNINSTALL_K3S}" ]; then
		DISTRO_K3S=false
	fi

	if [ -z "${DISTRO_KILL_ALL}" ]; then
		DISTRO_KILL_ALL=false
	fi

	if [ -z "${DISTRO_UNINSTALL}" ]; then
		DISTRO_UNINSTALL=false
	fi

	if [ -z "$DELETE_NODE" ]; then
		DELETE_NODE=false
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

parse_args() {
	while [ $# -gt 0 ]; do
		case "$1" in
		"--rke2")
			info "Rke2 requested"
			DISTRO_RKE2=true
			shift 1
			;;
		"--k3s")
			info "K3s requested"
			DISTRO_K3S=true
			shift 1
			;;
		"--kill-all")
			info "Kill all requested"
			DISTRO_KILL_ALL=true
			shift 1
			;;
		"--uninstall")
			info "Uninstall requested"
			DISTRO_UNINSTALL=true
			shift 1
			;;
		"--delete-node")
			info "Delete node requested"
			DELETE_NODE=true
			shift 1
			;;
		"--node-name")
			NODE_NAME=$2
			shift 2
			;;
		*)
			fatal "Unknown argument passed in ($1)"
			;;
		esac
	done
}

uninstall_stop_services() {
	if command -v systemctl >/dev/null 2>&1; then
		systemctl stop rancher-system-agent
	fi
}

uninstall_remove_self() {
	rm -f "${CATTLE_AGENT_BIN_PREFIX}/bin/rancher-system-agent-uninstall.sh"
}

uninstall_disable_services() {
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

kill_all_rke2() {
	if [ -f "$DIRECTORY_RKE2/rke2-killall.sh" ]; then
		info "Running rke2-killall.sh"
		bash $DIRECTORY_RKE2/rke2-killall.sh
		return
	fi

	if [ -f "$DIRECTORY_OPT_RKE2/rke2-killall.sh" ]; then
		info "Running rke2-killall.sh"
		bash $DIRECTORY_OPT_RKE2/rke2-killall.sh
		return
	fi
}

kill_all_k3s() {
	if [ -f "$DIRECTORY_K3S/k3s-killall.sh" ]; then
		info "Running k3s-killall.sh"
		bash $DIRECTORY_K3S/k3s-killall.sh
		return
	fi

	if [ -f "$DIRECTORY_OPT_K3S/k3s-killall.sh" ]; then
		info "Running k3s-killall.sh"
		bash $DIRECTORY_OPT_K3S/k3s-killall.sh
		return
	fi
}

uninstall_rke2() {
	if [ -f "$DIRECTORY_RKE2/rke2-uninstall.sh" ]; then
		info "Running rke2-uninstall.sh"
		bash $DIRECTORY_RKE2/rke2-uninstall.sh
		return
	fi

	if [ -f "$DIRECTORY_OPT_RKE2/rke2-uninstall.sh" ]; then
		info "Running rke2-uninstall.sh"
		bash $DIRECTORY_OPT_RKE2/rke2-uninstall.sh
		return
	fi
}

uninstall_k3s() {
	if [ -f "$DIRECTORY_K3S/k3s-uninstall.sh" ]; then
		info "Running k3s-uninstall.sh"
		bash $DIRECTORY_K3S/k3s-uninstall.sh
		return
	fi

	if [ -f "$DIRECTORY_OPT_K3S/k3s-uninstall.sh" ]; then
		info "Running k3s-uninstall.sh"
		bash $DIRECTORY_OPT_K3S/k3s-uninstall.sh
		return
	fi
}

reschedule_node() {
	info "Draining $NODE_NAME"
	kubectl drain $NODE_NAME --ignore-daemonsets
}

delete_node() {
	info "Deleting $NODE_NAME"
	kubectl delete node $NODE_NAME
}

uninstall() {
	if [ $(id -u) != 0 ]; then
		fatal "This script must be run as root."
	fi

	parse_args "$@"
	setup_env

	if [ "$DELETE_NODE" = "true" ]; then
		reschedule_node
		delete_node
	fi

	if [ "$DISTRO_RKE2" = "true" ]; then
		if [ "$DISTRO_KILL_ALL" = "true" ]; then
			info "Killing all Rke2 pods"
			kill_all_rke2
		fi

		if [ "$DISTRO_UNINSTALL" = "true" ]; then
			info "Uninstalling Rke2"
			uninstall_rke2
		fi
	fi

	if [ "$DISTRO_K3S" = "true" ]; then
		if [ "$DISTRO_KILL_ALL" = "true" ]; then
			info "Killing all K3s nodes"
			kill_all_k3s
		fi

		if [ "$DISTRO_UNINSTALL" = "true" ]; then
			info "Uninstalling K3s"
			uninstall_k3s
		fi
	fi

	uninstall_stop_services
	trap uninstall_remove_self EXIT
	uninstall_disable_services
	uninstall_remove_files
}

uninstall "$@"
exit 0
