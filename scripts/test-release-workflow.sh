#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
workflow="$repo_root/.github/workflows/release.yml"
makefile="$repo_root/Makefile"
cnb_pipeline="$repo_root/.cnb.yml"

command -v ruby >/dev/null 2>&1 || { echo "ruby is required" >&2; exit 1; }

ruby -ryaml -e '
workflow = YAML.safe_load(File.read(ARGV.fetch(0)), aliases: true)
jobs = workflow.fetch("jobs")
edge = jobs.fetch("edge-release")
raise "edge-release must wait for image publication" unless edge.fetch("needs") == "image"

publish = edge.fetch("steps").find { |step| step["name"] == "Publish versioned Edge release to CNB" }
raise "missing Edge publish step" unless publish
raise "Edge publish step must call the Make target" unless publish.fetch("run").include?("make publish-edge-version-attachments")
raise "Edge publish step must use the CNB_TOKEN secret" unless publish.fetch("env").fetch("CNB_TOKEN") == "${{ secrets.CNB_TOKEN }}"

release_needs = Array(jobs.fetch("release").fetch("needs"))
raise "GitHub Release must wait for Edge publication" unless release_needs.sort == ["build", "edge-release"]
' "$workflow"

if grep -Fq 'cnbcool/attachments:latest' "$makefile" "$cnb_pipeline"; then
    echo "release configuration uses a mutable attachment uploader image" >&2
    exit 1
fi
grep -Eq '^CNB_ATTACHMENTS_IMAGE \?= cnbcool/attachments@sha256:[0-9a-f]{64}$' "$makefile" \
    || { echo "Makefile attachment uploader is not pinned by digest" >&2; exit 1; }
grep -Eq 'image: cnbcool/attachments@sha256:[0-9a-f]{64}$' "$cnb_pipeline" \
    || { echo "CNB attachment uploader is not pinned by digest" >&2; exit 1; }
grep -Fq 'CNB dependency release $(EDGE_DEPS_TAG) is complete; skip build and upload' "$makefile" \
    || { echo "complete dependency Releases are not skipped before rebuilding" >&2; exit 1; }
grep -Fq 'CNB Edge release $(VERSION) is complete; skip build and upload' "$makefile" \
    || { echo "complete versioned Edge Releases are not skipped before rebuilding" >&2; exit 1; }
grep -Fq 'scripts/verify-cnb-release-attachments.sh' "$makefile" \
    || { echo "Makefile release skip does not verify attachment contents" >&2; exit 1; }
grep -Fq 'make verify-edge-deps-release' "$cnb_pipeline" \
    || { echo "CNB version pipeline does not verify dependency contents" >&2; exit 1; }
if grep -Fq 'curl -fsSIL' "$cnb_pipeline"; then
    echo "CNB pipeline still treats attachment existence as integrity verification" >&2
    exit 1
fi

echo "release workflow tests passed"
