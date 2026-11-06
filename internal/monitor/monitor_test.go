package monitor

import (
	"testing"
	"time"
)

func TestObservationSafety(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	zero := 0
	policy := Policy{MissingFor: time.Hour, MaxSyncAge: 30 * time.Minute, MaxPollGap: 15 * time.Minute}
	tests := []struct {
		name   string
		change func(*Observation, *State)
		want   time.Duration
		reason string
		death  bool
	}{
		{"threshold", func(*Observation, *State) {}, time.Hour, "monitoring", true},
		{"empty", func(o *Observation, _ *State) { o.Battery = "Empty" }, 50 * time.Minute, "battery_empty", false},
		{"zero battery", func(o *Observation, _ *State) { o.Level = &zero }, 50 * time.Minute, "battery_empty", false},
		{"unknown battery", func(o *Observation, _ *State) { o.Battery = "" }, 50 * time.Minute, "battery_unknown", false},
		{"stale", func(o *Observation, _ *State) { o.Sync = now.Add(-time.Hour) }, 50 * time.Minute, "sync_stale", false},
		{"future sync", func(o *Observation, _ *State) { o.Sync = now.Add(time.Minute) }, 50 * time.Minute, "sync_stale", false},
		{"first sample absent", func(_ *Observation, s *State) { s.LastHeartRate = time.Time{} }, 50 * time.Minute, "waiting_for_first_heart_rate", false},
		{"new heartbeat", func(o *Observation, _ *State) { o.HeartRate = now.Add(-time.Minute) }, 0, "monitoring", false},
		{"missed polls", func(_ *Observation, s *State) { s.LastCheck = now.Add(-time.Hour) }, 50 * time.Minute, "monitoring", false},
		{"recovery interval", func(_ *Observation, s *State) { s.Eligible = false }, 50 * time.Minute, "monitoring", false},
		{"clock reversal", func(_ *Observation, s *State) { s.LastCheck = now.Add(time.Minute) }, 50 * time.Minute, "monitoring", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := State{LastHeartRate: now.Add(-2 * time.Hour), LastCheck: now.Add(-10 * time.Minute), Missing: 50 * time.Minute, Eligible: true}
			o := Observation{Sync: now, Battery: "High"}
			tt.change(&o, &s)
			policy.Observe(&s, o, now)
			if s.Missing != tt.want || s.Reason != tt.reason || s.Confirmed != tt.death {
				t.Fatalf("got %+v; want missing=%s reason=%s death=%v", s, tt.want, tt.reason, tt.death)
			}
		})
	}
}

func TestBatteryOutageExcludedAndHeartbeatRearms(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	p := Policy{MissingFor: 20 * time.Minute, MaxSyncAge: time.Hour, MaxPollGap: 15 * time.Minute}
	s := State{}
	p.Observe(&s, Observation{HeartRate: now, Sync: now, Battery: "High"}, now)
	step := func(delta time.Duration, battery string) {
		now = now.Add(delta)
		p.Observe(&s, Observation{Sync: now, Battery: battery}, now)
	}
	step(10*time.Minute, "High")
	step(10*time.Minute, "Empty")
	step(10*time.Hour, "Empty")
	step(10*time.Minute, "High")
	if s.Missing != 10*time.Minute || s.Confirmed {
		t.Fatalf("battery outage counted: %+v", s)
	}
	step(10*time.Minute, "High")
	if !s.Confirmed {
		t.Fatal("eligible missing time did not trigger")
	}
	event := s.EventID
	step(time.Minute, "High")
	if s.EventID != event {
		t.Fatal("same outage generated a new event")
	}
	p.Observe(&s, Observation{HeartRate: now, Sync: now, Battery: "High"}, now)
	if s.Confirmed || s.Missing != 0 || s.EventID != "" {
		t.Fatalf("heartbeat did not rearm: %+v", s)
	}
}
