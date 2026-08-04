#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
workflow="$repo_root/.github/workflows/release.yml"

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

echo "release workflow tests passed"
