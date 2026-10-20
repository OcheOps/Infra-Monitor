#!/bin/sh
# Generates shell completion scripts for release packages (run by GoReleaser).
set -eu
mkdir -p completions
for shell in bash zsh fish; do
  go run ./cmd/infra-monitor completion "$shell" > "completions/infra-monitor.$shell"
done
