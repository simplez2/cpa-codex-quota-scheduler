package main

import (
	"testing"
	"time"
)

func TestNormalizeQuotaWindowSetKeepsOneConservativeAuthoritativeWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(4 * time.Hour)
	windows := []quotaWindow{
		{Class: "5h", WindowSeconds: 18000, UsedPercent: 20, Allowed: true, ResetAt: reset, ObservedAt: now, Source: quotaSourceProbe},
		{Class: "5h", WindowSeconds: 18000, UsedPercent: 100, Allowed: false, LimitReached: true, ResetAt: reset, ObservedAt: now, Source: quotaSourceHeader},
		{Class: "future", WindowSeconds: 45 * 24 * 3600, UsedPercent: 5, Allowed: true, ObservedAt: now},
	}
	got := normalizeQuotaWindowSet(windows, now, now, 15*time.Minute)
	if len(got) != 2 {
		t.Fatalf("normalized windows=%d want 2: %#v", len(got), got)
	}
	var five quotaWindow
	for _, window := range got {
		if window.Class == "5h" {
			five = window
		}
	}
	if five.Class == "" || five.Allowed || !five.LimitReached || five.UsedPercent != 100 {
		t.Fatalf("hard duplicate was not retained conservatively: %#v", five)
	}
}
