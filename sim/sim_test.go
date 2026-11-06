package sim

import "testing"

func run(t *testing.T, id string, cfg Config, extra int) *Sim {
	t.Helper()
	s, err := New(cfg, id, 7)
	if err != nil {
		t.Fatal(err)
	}
	n := s.scenario.Length
	if n == 0 {
		n = 400
		s.Do(Action{Type: "chaos", Value: 1})
	}
	for s.rel() < n+extra {
		s.Step(n + extra)
	}
	return s
}

// The design must never lose acknowledged data or store duplicates,
// except in the two-disks scenario, which exists to show its limit.
func TestDesignInvariants(t *testing.T) {
	for _, sc := range Scenarios {
		for seed := int64(1); seed <= 5; seed++ {
			s, _ := New(DesignConfig(), sc.ID, seed)
			n := sc.Length
			if n == 0 {
				n = 1500
				s.Do(Action{Type: "chaos", Value: 1})
			}
			for s.rel() < n+120 {
				s.Step(n + 120)
			}
			inv := s.Invariants()
			if sc.ID != "two-disks" && inv.LostAcked != 0 {
				t.Errorf("%s seed %d: lost %d acknowledged readings", sc.ID, seed, inv.LostAcked)
			}
			if inv.DupRows != 0 || inv.AckedDropped != 0 || inv.Misordered != 0 {
				t.Errorf("%s seed %d: dup=%d dropped=%d misordered=%d", sc.ID, seed, inv.DupRows, inv.AckedDropped, inv.Misordered)
			}
			// Every acknowledged reading that survives must be queryable or quarantined.
			for k := range s.Acked {
				if s.LostEver[k] {
					continue
				}
				if s.Store.Records[k] == nil && !s.Store.appliedCorr[k] && !inQuarantine(s, k) && !pendingApply(s, k) {
					t.Errorf("%s seed %d: acked %s not in history", sc.ID, seed, k)
					break
				}
			}
		}
	}
}

func inQuarantine(s *Sim, k string) bool {
	for _, q := range s.Store.Quarantine {
		if q.Reading.Key() == k {
			return true
		}
	}
	return false
}

func pendingApply(s *Sim, k string) bool {
	ld := s.leaderNode()
	if ld == nil {
		return true
	}
	for i := s.Applied; i < len(ld.Log); i++ {
		for _, r := range ld.Log[i].Batch.Readings {
			if r.Key() == k {
				return true
			}
		}
	}
	return false
}

func TestNaiveBreaks(t *testing.T) {
	cases := map[string]func(Invariants) bool{
		"crash":        func(i Invariants) bool { return i.LostAcked > 0 },
		"no-quorum":    func(i Invariants) bool { return i.LostAcked > 0 },
		"duplicates":   func(i Invariants) bool { return i.DupRows > 0 },
		"delayed":      func(i Invariants) bool { return i.AckedDropped > 0 },
		"out-of-order": func(i Invariants) bool { return i.Misordered > 0 },
		"corrections":  func(i Invariants) bool { return i.Overwrites > 0 },
	}
	for id, broken := range cases {
		s := run(t, id, NaiveConfig(), 60)
		if !broken(s.Invariants()) {
			t.Errorf("naive %s: expected visible failure, got %+v", id, s.Invariants())
		}
	}
}

func TestUploadIntervalShrinksLoss(t *testing.T) {
	c30 := DesignConfig()
	c5 := DesignConfig()
	c5.UploadEvery = 5
	a := run(t, "two-disks", c30, 60).Invariants().LostAcked
	b := run(t, "two-disks", c5, 60).Invariants().LostAcked
	if !(a > 0 && b < a) {
		t.Errorf("expected loss to shrink with faster uploads: every30=%d every5=%d", a, b)
	}
}

func TestSimilarityFindsTwins(t *testing.T) {
	s := run(t, "similar", DesignConfig(), 0)
	twins := map[string]string{"A": "G", "B": "H", "C": "I", "D": "J", "E": "K"}
	for a, b := range twins {
		r := s.Similar(a)
		if r.Matches[0].Station != b {
			t.Errorf("most similar to %s: got %s, want %s", a, r.Matches[0].Station, b)
		}
	}
	for _, m := range s.Similar("A").Matches {
		if m.Station == "L" && !m.Excluded {
			t.Errorf("silent station L should be excluded")
		}
	}
}

func TestDelayedLandsInDelta(t *testing.T) {
	s := run(t, "delayed", DesignConfig(), 0)
	rew := 0
	for _, p := range s.Store.Partitions {
		rew += p.Rewrites
	}
	if rew == 0 {
		t.Errorf("expected a partition rewrite after late data")
	}
	if s.Stats.NackBusy == 0 {
		t.Errorf("expected backpressure during the reconnect burst")
	}
}
