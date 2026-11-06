// simcheck runs every scenario in design and naive mode and prints the outcome.
package main

import (
	"fmt"
	"os"

	"archivesim/sim"
)

func main() {
	verbose := len(os.Args) > 1
	for _, sc := range sim.Scenarios {
		for _, mode := range []string{"design", "naive"} {
			cfg := sim.DesignConfig()
			if mode == "naive" {
				cfg = sim.NaiveConfig()
			}
			s, err := sim.New(cfg, sc.ID, 42)
			if err != nil {
				panic(err)
			}
			n := sc.Length
			if n == 0 {
				n = 300
				s.Do(sim.Action{Type: "chaos", Value: 1})
			}
			cps := 0
			for s.Now-s.T0 < n+60 {
				s.Step(n + 60)
				if s.Checkpoint != nil {
					cps++
				}
			}
			st := s.Snapshot()
			buf := 0
			for _, x := range st.Stations {
				buf += x.Buffer
			}
			i := st.Inv
			fmt.Printf("%-13s %-6s cps=%d acked=%d LOST=%d ackedDropped=%d dupRows=%d dupsIgnored=%d fast=%d conflicts=%d overwrites=%d quar=%d ooo=%d misordered=%d flagged=%d busy=%d noQ=%d elections=%d buffered=%d leader=%s maxBacklog=%d\n",
				sc.ID, mode, cps, i.Acked, i.LostAcked, i.AckedDropped, i.DupRows, i.DupsIgnored, i.FastPathDups,
				i.Conflicts, i.Overwrites, i.Quarantined, i.OutOfOrder, i.Misordered, i.Flagged,
				st.Stats.NackBusy, st.Stats.NackNoQuorum, st.Stats.Elections, buf, st.Leader, st.Stats.MaxBacklog)
			if sc.ID == "similar" && mode == "design" {
				r := s.Similar("A")
				for _, m := range r.Matches {
					fmt.Printf("    %s %-12s %.2f cov=%.2f %v %s\n", m.Station, m.Climate, m.Score, m.Coverage, m.Excluded, m.Reason)
				}
			}
			if verbose && os.Args[1] == sc.ID && mode == os.Args[2] {
				for _, e := range s.Events {
					if e.Kind == "append" || e.Kind == "ack" || e.Kind == "commit" || e.Kind == "upload" || e.Kind == "fsync" {
						continue
					}
					fmt.Printf("  [%d] %-11s %s\n", e.T-s.T0, e.Kind, e.Msg)
				}
			}
		}
	}
}
