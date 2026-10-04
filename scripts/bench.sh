#!/usr/bin/env bash
# Runs the benchmarks reported in docs/benchmarks.md.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

cd "$ROOT"
go test ./internal/storage -run XXX -bench . -benchtime "${BENCHTIME:-2s}"
