#!/usr/bin/env bash
# Checks the Helm chart against every values file in ci/: helm lint, schema
# validation of the rendered manifests with kubeconform, and the generated
# agent.yaml loaded by the agent itself, which rejects unknown keys.
#
# Needs helm, kubeconform and yq. Run it with `make helm`.
set -euo pipefail

chart="$(dirname "$0")/nabbed-agent"
kube_version="${KUBE_VERSION:-1.31.0}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

go build -o "$tmp/agent" ./cmd/agent

for values in "$chart"/ci/*.yaml; do
	echo "== $(basename "$values")"

	helm lint --strict --quiet "$chart" -f "$values"

	helm template check "$chart" -f "$values" >"$tmp/manifests.yaml"
	kubeconform -strict -summary -kubernetes-version "$kube_version" "$tmp/manifests.yaml"

	helm template check "$chart" -f "$values" --show-only templates/configmap.yaml |
		yq '.data["agent.yaml"]' >"$tmp/agent.yaml"
	# The chart sets these through the environment.
	NABBED_NAME=check-0 NABBED_DATA_DIR=/data/agent NABBED_DOWNLOAD_DIR=/downloads NABBED_TORRENT_PORT=6881 \
		"$tmp/agent" --config "$tmp/agent.yaml" --log-level warn config >/dev/null
	echo "agent.yaml accepted by the agent"
done
