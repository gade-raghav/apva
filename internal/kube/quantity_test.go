// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"math"
	"testing"
)

func TestParseQuantity(t *testing.T) {
	cases := map[string]float64{
		"250m": 0.25, "1": 1, "0.5": 0.5, "2": 2,
		"512Mi": 512 << 20, "1Gi": 1 << 30, "128974848": 128974848,
		"1G": 1e9, "100k": 1e5, "1e3": 1000, "129e6": 129e6,
	}
	for in, want := range cases {
		got, err := ParseQuantity(in)
		if err != nil || math.Abs(got-want) > 1e-9*math.Max(1, want) {
			t.Errorf("ParseQuantity(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseQuantity("abc"); err == nil {
		t.Error("expected error for garbage")
	}
}

func TestFormat(t *testing.T) {
	if got := FormatCPU(0.045); got != "45m" {
		t.Errorf("FormatCPU = %s", got)
	}
	if got := FormatCPU(0.0001); got != "1m" {
		t.Errorf("FormatCPU min = %s", got)
	}
	if got := FormatMemory(32 << 20); got != "32Mi" {
		t.Errorf("FormatMemory = %s", got)
	}
	if got := FormatMemory(32<<20 + 1); got != "33Mi" {
		t.Errorf("FormatMemory round-up = %s", got)
	}
}
