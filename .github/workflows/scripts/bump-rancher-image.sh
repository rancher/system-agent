#!/usr/bin/env bash
# Bump RANCHER_IMAGE, CATTLE_AGENT_IMAGE and CATTLE_K3S_VERSION for the integration tests.
#
# Run from the root of the branch to update. The script keeps the Rancher line of the
# current tag (for example v2.16) and takes the newest "-head" tag of that line from
# Docker Hub. It reads the k3s version from the CATTLE_K3S_VERSION variable in the new
# Rancher image. The script writes the change list to the file in $1 when a value changes.
#
# Usage: bump-rancher-image.sh [changes-file]
set -euo pipefail

CHANGES_FILE=${1:-changes.md}

WORKFLOW_FILE=.github/workflows/integration-tests.yaml
RUN_SCRIPT=scripts/integration-tests
FETCH_SCRIPT=scripts/fetch-provisioning-tests

HUB_API=https://hub.docker.com/v2/repositories

for f in "$WORKFLOW_FILE" "$RUN_SCRIPT" "$FETCH_SCRIPT"; do
    [ -f "$f" ] || { echo "error: $f not found; run this script from the repository root" >&2; exit 1; }
done

# Print the first match of an extended regex in a file. Fail when nothing matches.
find_in_file() {
    local pattern=$1 file=$2 value
    value=$(grep -oE -m1 "$pattern" "$file" || true)
    [ -n "$value" ] || { echo "error: pattern '$pattern' not found in $file" >&2; exit 1; }
    printf '%s\n' "$value"
}

# Replace a fixed string in a file. Fail when the old string is not in the file.
replace_in_file() {
    local file=$1 old=$2 new=$3 escaped_old
    grep -qF -- "$old" "$file" || { echo "error: '$old' not found in $file" >&2; exit 1; }
    escaped_old=$(printf '%s' "$old" | sed 's/[][\.*^$|/]/\\&/g')
    sed -i.bak "s|${escaped_old}|${new}|g" "$file"
    rm -f "$file.bak"
}

hub_tag_exists() {
    curl -fsS -o /dev/null "$HUB_API/$1/tags/$2"
}

tag_regex='v[0-9]+\.[0-9]+-[0-9a-f]{40}-head'

current_rancher_image=$(find_in_file "rancher/rancher:$tag_regex" "$FETCH_SCRIPT")
current_agent_image=$(find_in_file "rancher/rancher-agent:$tag_regex" "$RUN_SCRIPT")
current_k3s_version=$(find_in_file 'CATTLE_K3S_VERSION: v[0-9.]+-k3s[0-9]+' "$WORKFLOW_FILE")

current_tag=${current_rancher_image#rancher/rancher:}
line=${current_tag%%-*}
echo "Current Rancher tag: $current_tag (line $line)"

# Newest tag of the line that exists for both the Rancher and the agent image.
# The -amd64 and -arm64 tags are not matched by the anchored expression.
line_regex="^${line//./\\.}-[0-9a-f]{40}-head\$"
candidates=$(curl -fsS "$HUB_API/rancher/rancher/tags?page_size=100&ordering=last_updated&name=${line}-" |
    jq -r --arg re "$line_regex" '.results | sort_by(.last_updated) | reverse | .[] | select(.name | test($re)) | .name')

new_tag=
for candidate in $candidates; do
    if hub_tag_exists rancher/rancher-agent "$candidate"; then
        new_tag=$candidate
        break
    fi
    echo "Skip $candidate: no rancher-agent image with the same tag yet"
done
[ -n "$new_tag" ] || { echo "error: no tag of line $line found for both images" >&2; exit 1; }
echo "Newest tag with both images: $new_tag"

new_rancher_image=rancher/rancher:$new_tag
new_agent_image=rancher/rancher-agent:$new_tag

if [ "$new_rancher_image" = "$current_rancher_image" ] && [ "$new_agent_image" = "$current_agent_image" ]; then
    echo "Images already at $new_tag; nothing to do"
    exit 0
fi

# The Rancher image sets CATTLE_K3S_VERSION, for example v1.36.4+k3s1. The rancher/k3s tags use "-".
docker pull --quiet "$new_rancher_image" >/dev/null
k3s_version=$(docker image inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$new_rancher_image" |
    sed -n 's/^CATTLE_K3S_VERSION=//p')
[ -n "$k3s_version" ] || { echo "error: CATTLE_K3S_VERSION not set in $new_rancher_image" >&2; exit 1; }
new_k3s_version=${k3s_version//+/-}
hub_tag_exists rancher/k3s "$new_k3s_version" ||
    { echo "error: rancher/k3s:$new_k3s_version not found on Docker Hub" >&2; exit 1; }
echo "k3s version in $new_rancher_image: $k3s_version (tag $new_k3s_version)"

replace_in_file "$FETCH_SCRIPT" "$current_rancher_image" "$new_rancher_image"
replace_in_file "$WORKFLOW_FILE" "$current_rancher_image" "$new_rancher_image"
replace_in_file "$RUN_SCRIPT" "$current_agent_image" "$new_agent_image"
replace_in_file "$WORKFLOW_FILE" "$current_agent_image" "$new_agent_image"
if [ "CATTLE_K3S_VERSION: $new_k3s_version" != "$current_k3s_version" ]; then
    replace_in_file "$WORKFLOW_FILE" "$current_k3s_version" "CATTLE_K3S_VERSION: $new_k3s_version"
fi

cat >"$CHANGES_FILE" <<EOF
| Value | Old | New |
| --- | --- | --- |
| \`RANCHER_IMAGE\` | \`$current_rancher_image\` | \`$new_rancher_image\` |
| \`CATTLE_AGENT_IMAGE\` | \`$current_agent_image\` | \`$new_agent_image\` |
| \`CATTLE_K3S_VERSION\` | \`${current_k3s_version#CATTLE_K3S_VERSION: }\` | \`$new_k3s_version\` |
EOF
echo "Wrote $CHANGES_FILE"
