// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

var binarySuffix = map[string]float64{
	"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40, "Pi": 1 << 50, "Ei": 1 << 60,
}

var decimalSuffix = map[byte]float64{
	'n': 1e-9, 'u': 1e-6, 'm': 1e-3, 'k': 1e3, 'M': 1e6, 'G': 1e9, 'T': 1e12, 'P': 1e15, 'E': 1e18,
}

// ParseQuantity parses a Kubernetes resource quantity ("250m", "0.5", "512Mi", "1G",
// "1e3") into a plain number (cores for CPU, bytes for memory).
func ParseQuantity(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty quantity")
	}
	if len(s) > 2 {
		if m, ok := binarySuffix[s[len(s)-2:]]; ok {
			v, err := strconv.ParseFloat(s[:len(s)-2], 64)
			return v * m, err
		}
	}
	if m, ok := decimalSuffix[s[len(s)-1]]; ok {
		// "1e3" ends in a digit, so a trailing E here really is the exa suffix.
		v, err := strconv.ParseFloat(s[:len(s)-1], 64)
		return v * m, err
	}
	return strconv.ParseFloat(s, 64)
}

// FormatCPU renders cores as whole millicores, rounding up, minimum 1m.
func FormatCPU(cores float64) string {
	return fmt.Sprintf("%dm", int64(math.Max(1, math.Ceil(cores*1000-1e-9))))
}

// FormatMemory renders bytes as whole MiB, rounding up, minimum 1Mi.
func FormatMemory(bytes float64) string {
	return fmt.Sprintf("%dMi", int64(math.Max(1, math.Ceil(bytes/(1<<20)-1e-9))))
}
