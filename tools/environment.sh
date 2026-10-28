#!/usr/bin/env bash
# Writes the environment a measurement ran in (IP-16 DoD 7): the runner's CPU and memory, its
# kernel, its image version where GitHub sets one, Docker's version, and the digest or id of every
# image the stack runs. A figure in v0-1-evidence.md is quoted with this file beside it, so a reader
# can tell the reference host from the machine that happened to run the test.
set -euo pipefail
out="${1:?usage: tools/environment.sh FILE}"
mkdir -p "$(dirname "$out")"
{
  echo "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "runner image: ${ImageOS:-unknown} ${ImageVersion:-unknown}"
  echo "commit: ${GITHUB_SHA:-$(git rev-parse HEAD)}"
  echo "kernel: $(uname -srvm)"
  echo
  lscpu
  echo
  free -b
  echo
  docker version --format 'docker: client {{.Client.Version}}, server {{.Server.Version}}'
  echo
  echo "images:"
  docker image inspect --format '{{index .RepoTags 0}} {{.Id}} {{range .RepoDigests}}{{.}} {{end}}' quotacore:local
  for ref in $(grep -hoE 'image: [^ $]+@sha256:[0-9a-f]+' docker-compose.yml | cut -d' ' -f2); do
    echo "$ref"
  done
} >"$out"
cat "$out"
