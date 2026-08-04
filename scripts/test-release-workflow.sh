#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
workflow="$repo_root/.github/workflows/release.yml"
makefile="$repo_root/Makefile"

command -v ruby >/dev/null 2>&1 || { echo "ruby is required" >&2; exit 1; }

ruby -ryaml -e '
workflow = YAML.safe_load(File.read(ARGV.fetch(0)), aliases: true)
jobs = workflow.fetch("jobs")
edge = jobs.fetch("edge-release")
raise "edge-release must wait for image publication" unless edge.fetch("needs") == "image"

build = jobs.fetch("build")
raise "release must build one universal package without an architecture matrix" if build.key?("strategy")
build_step = build.fetch("steps").find { |step| step["name"] == "Build universal Linux package" }
raise "missing universal package build step" unless build_step
raise "universal package build must call make package" unless build_step.fetch("run").match?(/^make package\s*$/)
upload = build.fetch("steps").find { |step| step["name"] == "Upload package artifact" }
raise "missing universal package upload step" unless upload
upload_path = upload.fetch("with").fetch("path")
raise "release uploads an architecture-specific server package" if upload_path.match?(/linux-(amd64|arm64)/)
raise "release does not upload the universal server package" unless upload_path.include?("linux.tar.xz")

publish = edge.fetch("steps").find { |step| step["name"] == "Publish versioned Edge release to CNB" }
raise "missing Edge publish step" unless publish
raise "Edge publish step must call the Make target" unless publish.fetch("run").include?("make publish-edge-version-attachments")
raise "Edge publish step must use the CNB_TOKEN secret" unless publish.fetch("env").fetch("CNB_TOKEN") == "${{ secrets.CNB_TOKEN }}"
go_setup = edge.fetch("steps").find { |step| step["name"] == "Set up Go" }
raise "missing Edge Go setup step" unless go_setup
raise "Edge Go toolchain must be exact for immutable binary comparison" unless go_setup.fetch("with").fetch("go-version") == "1.25.11"

release_needs = Array(jobs.fetch("release").fetch("needs"))
raise "GitHub Release must wait for Edge publication" unless release_needs.sort == ["build", "edge-release"]
release = jobs.fetch("release")
publish_release = release.fetch("steps").find { |step| step["name"] == "Publish GitHub Release" }
raise "missing GitHub Release publish step" unless publish_release
release_run = publish_release.fetch("run")
raise "GitHub Release still publishes architecture-specific server packages" if release_run.match?(/linux-(amd64|arm64)/)
raise "GitHub Release does not publish the universal server package" unless release_run.include?("linux.tar.xz")
' "$workflow"

if grep -Fq 'cnbcool/attachments:latest' "$makefile"; then
    echo "release configuration uses a mutable attachment uploader image" >&2
    exit 1
fi
grep -Eq '^CNB_ATTACHMENTS_IMAGE \?= cnbcool/attachments@sha256:[0-9a-f]{64}$' "$makefile" \
    || { echo "Makefile attachment uploader is not pinned by digest" >&2; exit 1; }
grep -Fq 'CNB dependency release $(EDGE_DEPS_TAG) is complete; skip build and upload' "$makefile" \
    || { echo "complete dependency Releases are not skipped before rebuilding" >&2; exit 1; }
grep -Fq 'scripts/verify-cnb-release-attachments.sh' "$makefile" \
    || { echo "Makefile release skip does not verify attachment contents" >&2; exit 1; }
[[ ! -e "$repo_root/.cnb.yml" ]] \
    || { echo "a second CNB tag publisher can bypass GitHub immutable checks" >&2; exit 1; }

ruby -e '
makefile = File.read(ARGV.fetch(0))
body = makefile[/^publish-edge-version-attachments:.*?\n(.*?)(?=^\S|\z)/m, 1]
raise "missing publish-edge-version-attachments recipe" unless body
build_at = body.index("build-edge-version-attachments")
verify_at = body.index("verify-edge-version-release")
publish_at = body.index("publish-cnb-release-attachments.sh")
raise "versioned Edge is not built before remote reuse is considered" unless build_at && verify_at && build_at < verify_at
raise "remote reuse is not compared through the immutable publisher" unless publish_at && verify_at < publish_at
' "$makefile"

echo "release workflow tests passed"
