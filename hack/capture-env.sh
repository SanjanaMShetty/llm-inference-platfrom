#!/usr/bin/env bash
# Print the environment details to store next to any measurement.
# Usage: hack/capture-env.sh > experiments/<name>/environment.txt
set -uo pipefail

echo "captured_at: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "git_commit:  $(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
echo "git_dirty:   $([ -n "$(git status --porcelain 2>/dev/null)" ] && echo yes || echo no)"
echo "os:          $(uname -srm)"
if command -v lscpu >/dev/null 2>&1; then
  echo "cpu:         $(lscpu | awk -F: '/Model name/ {gsub(/^ +/, "", $2); print $2}')"
  echo "cpu_count:   $(nproc)"
elif command -v sysctl >/dev/null 2>&1; then
  echo "cpu:         $(sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown)"
  echo "cpu_count:   $(sysctl -n hw.ncpu 2>/dev/null || echo unknown)"
fi
echo "go:          $(go version 2>/dev/null || echo not-installed)"
echo "docker:      $(docker --version 2>/dev/null || echo not-installed)"
echo "kind:        $(kind version 2>/dev/null || echo not-installed)"
echo "kubectl:     $(kubectl version --client 2>/dev/null | head -1 || echo not-installed)"
