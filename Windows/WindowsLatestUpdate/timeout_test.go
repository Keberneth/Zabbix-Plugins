package main

import (
	"testing"
	"time"
)

func TestEffectiveTimeout(t *testing.T) {
	tests := []struct {
		name         string
		agentSeconds int
		hardCap      time.Duration
		want         time.Duration
	}{
		{name: "standalone fallback", agentSeconds: 0, hardCap: 60 * time.Second, want: 60 * time.Second},
		{name: "hard cap wins", agentSeconds: 70, hardCap: 60 * time.Second, want: 60 * time.Second},
		{name: "agent timeout minus margin", agentSeconds: 30, hardCap: 60 * time.Second, want: 29 * time.Second},
		{name: "short timeout remains positive", agentSeconds: 1, hardCap: 60 * time.Second, want: 100 * time.Millisecond},
		{name: "invalid hard cap remains positive", agentSeconds: 0, hardCap: 0, want: 100 * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveTimeout(tt.agentSeconds, tt.hardCap); got != tt.want {
				t.Fatalf("effectiveTimeout(%d, %s) = %s; want %s", tt.agentSeconds, tt.hardCap, got, tt.want)
			}
		})
	}
}
