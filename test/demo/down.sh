#!/usr/bin/env bash
# Copyright 2026 Raghav Gade
# SPDX-License-Identifier: Apache-2.0
# Deletes the demo cluster created by up.sh.
set -euo pipefail
kind delete cluster --name "${CLUSTER:-apva-demo}"
