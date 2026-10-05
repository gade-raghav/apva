#!/usr/bin/env bash
# Copyright 2026 Raghav Gade
# SPDX-License-Identifier: Apache-2.0
#
# Prints KWOK's "fast" stages with one change: running containers report a (empty)
# status.containerStatuses[].resources, as a real kubelet with in-place resize does.
# The API server only accepts pods/resize for pods whose containers report it.
#   ./kwok-stages.sh v0.7.0 > stages.yaml
set -euo pipefail
curl -fsSL "https://github.com/kubernetes-sigs/kwok/releases/download/${1:-v0.7.0}/stage-fast.yaml" |
  awk '/name: pod-ready/{ready=1} /^---/{ready=0} {print} ready && /^        ready: true$/{print "        resources: {}"}'
