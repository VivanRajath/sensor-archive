package sim

import (
	"fmt"
	"math"
)

// Step is something that happens at a scenario-relative tick. If Title is set,
// the simulation pauses there and explains what to look at.
type Step struct {
	At    int
	Do    func(s *Sim)
	Title string
	Text  string
}

// Checkpoint is a pause with an explanation.
type Checkpoint struct {
	At    int    `json:"at"`
	Title string `json:"title"`
	Text  string `json:"text"`
	Index int    `json:"index"`
	Total int    `json:"total"`
}

// Scenario is one guided edge case.
type Scenario struct {
	ID        string   `json:"id"`
	Case      string   `json:"case"`
	Title     string   `json:"title"`
	Summary   string   `json:"summary"`
	Watch     []string `json:"watch"`
	Focus     string   `json:"focus"`
	Length    int      `json:"length"`
	PreWarmup func(s *Sim) `json:"-"`
	Setup     func(s *Sim) `json:"-"`
	Steps     []Step       `json:"-"`
}

func (sc *Scenario) checkpoints() int {
	n := 0
	for _, st := range sc.Steps {
		if st.Title != "" {
			n++
		}
	}
	return n
}

func (s *Sim) runScenario() {
	sc := s.scenario
	for s.actionIdx < len(sc.Steps) && sc.Steps[s.actionIdx].At <= s.rel() {
		st := sc.Steps[s.actionIdx]
		s.actionIdx++
		if st.Do != nil {
			st.Do(s)
		}
		if st.Title != "" {
			s.checkIdx++
			s.Checkpoint = &Checkpoint{At: st.At, Title: st.Title, Text: st.Text, Index: s.checkIdx, Total: sc.checkpoints()}
			s.emit("checkpoint", "info", "", "", "▶ "+st.Title)
		}
	}
}

// ---------- actions (used by scenarios and by the UI) ----------

func (s *Sim) nodeIndex(name string) int {
	for i, n := range s.Nodes {
		if n.Name == name {
			return i
		}
	}
	return -1
}

func (s *Sim) followers(up bool) []int {
	var out []int
	for i, n := range s.Nodes {
		if i != s.Leader && n.Up == up {
			out = append(out, i)
		}
	}
	return out
}

func (s *Sim) mark(k string, v int) {
	if s.marks == nil {
		s.marks = map[string]int{}
	}
	s.marks[k] = v
}

func (s *Sim) crashLeader(loseDisk bool) int {
	i := s.Leader
	if i < 0 {
		return -1
	}
	s.emit("fault", "bad", "", s.Nodes[i].Name, fmt.Sprintf("FAULT: leader %s goes down%s.", s.Nodes[i].Name, diskText(loseDisk)))
	s.crash(i, loseDisk)
	return i
}

func (s *Sim) crashNode(i int, loseDisk bool) {
	if i < 0 || i >= len(s.Nodes) || !s.Nodes[i].Up {
		return
	}
	s.emit("fault", "bad", "", s.Nodes[i].Name, fmt.Sprintf("FAULT: %s goes down%s.", s.Nodes[i].Name, diskText(loseDisk)))
	s.crash(i, loseDisk)
}

// SendCorrection queues a correction event at the station; it travels like any reading.
func (s *Sim) SendCorrection(station string, refSeq int, tempC float64, version int) {
	st := s.byName[station]
	if st == nil {
		return
	}
	st.NextSeq++
	r := Reading{Station: station, Seq: st.NextSeq, EventTime: s.Now, Schema: st.Schema, Kind: "correction",
		RefSeq: refSeq, Version: version, Fields: encode(st.Schema, tempC, 0)}
	st.buffer = append(st.buffer, r)
	s.emit("correction", "info", station, "", fmt.Sprintf("%s sends correction %s: reading #%d should be %.1f°C (v%d).", station, r.Key(), refSeq, tempC, version))
}

func (s *Sim) setOnline(station string, on bool) {
	st := s.byName[station]
	if st == nil || st.Online == on {
		return
	}
	st.Online = on
	if on {
		s.emit("network", "good", station, "", fmt.Sprintf("%s is back online with %d buffered readings to upload.", station, len(st.buffer)))
	} else {
		s.emit("network", "warn", station, "", fmt.Sprintf("%s lost its network. It keeps measuring and buffers readings locally.", station))
	}
}

func (s *Sim) dropAcks(station string, ticks int) {
	if st := s.byName[station]; st != nil {
		st.AckDropUntil = s.Now + ticks
		s.emit("fault", "warn", station, "", fmt.Sprintf("FAULT: acknowledgements to %s will be lost for the next %d min.", station, ticks))
	}
}

func (s *Sim) resendLast(station string) {
	st := s.byName[station]
	if st == nil || st.lastSent == nil {
		return
	}
	b := *st.lastSent
	s.batchSeq++
	b.ID = s.batchSeq
	s.send(&Message{Kind: "batch", Station: station, Batch: b})
	s.emit("fault", "warn", station, "", fmt.Sprintf("FAULT: %s's firmware sends its last batch again (seq %s).", station, seqRange(b.seqs())))
}

// AddConverter deploys a schema converter and replays quarantined events for it.
func (s *Sim) AddConverter(v int) {
	s.converters[v] = true
	old := s.Store.Quarantine
	s.Store.Quarantine = nil
	replayed := 0
	for _, q := range old {
		if q.Reading.Schema == v {
			s.applyReading(q.Reading, q.Ingest)
			replayed++
		} else {
			s.Store.Quarantine = append(s.Store.Quarantine, q)
		}
	}
	s.emit("schema", "good", "", "", fmt.Sprintf("Deployed a converter for schema v%d and replayed %d quarantined readings into history. Nothing had been lost.", v, replayed))
}

func (s *Sim) recordNear(station string, eventTime int) *Record {
	var best *Record
	for _, r := range s.Store.Rows {
		if r.Station == station && !r.Dup && (best == nil || abs(r.EventTime-eventTime) < abs(best.EventTime-eventTime)) {
			best = r
		}
	}
	return best
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func (s *Sim) chaos() {
	if s.Now < s.chaosNext {
		return
	}
	s.chaosNext = s.Now + 15 + s.rng.Intn(25)
	// Bring back anything that has been down a while.
	for i, n := range s.Nodes {
		if !n.Up {
			if s.rng.Intn(2) == 0 {
				s.restart(i, len(n.Log) == 0 && s.rng.Intn(2) == 0)
			}
			return
		}
	}
	st := s.Stations[s.rng.Intn(len(s.Stations))]
	switch s.rng.Intn(6) {
	case 0, 1:
		i := s.rng.Intn(3)
		s.crashNode(i, s.rng.Intn(3) == 0)
	case 2:
		s.dropAcks(st.Name, 10+s.rng.Intn(20))
	case 3:
		st.Jitter = 8
		s.emit("fault", "warn", st.Name, "", fmt.Sprintf("FAULT: %s's network turns jittery (1–9 min delays).", st.Name))
	case 4:
		if st.Online {
			s.setOnline(st.Name, false)
		} else {
			s.setOnline(st.Name, true)
		}
	case 5:
		if r := s.recordNear(st.Name, s.Now-DayTicks); r != nil {
			s.SendCorrection(st.Name, r.Seq, r.Current().TempC-0.5, r.Current().Version+1)
		}
	}
}

// Action is a user-triggered intervention from the UI.
type Action struct {
	Type    string  `json:"type"`
	Node    string  `json:"node"`
	Station string  `json:"station"`
	Value   float64 `json:"value"`
	Seq     int     `json:"seq"`
}

// Do applies a user action.
func (s *Sim) Do(a Action) {
	ni := s.nodeIndex(a.Node)
	switch a.Type {
	case "crash":
		s.crashNode(ni, false)
	case "destroy":
		s.crashNode(ni, true)
	case "restart":
		if ni >= 0 && !s.Nodes[ni].Up {
			s.restart(ni, false)
		}
	case "replace":
		if ni >= 0 && !s.Nodes[ni].Up {
			s.restart(ni, true)
		}
	case "crashAfterAppend":
		s.hookCrashAfterAppend = &crashHook{}
		s.emit("fault", "warn", "", "", "Armed: the leader will crash right after its next append, before fsync.")
	case "crashAfterAck":
		s.hookCrashAfterAck = &crashHook{loseDisk: true}
		s.emit("fault", "warn", "", "", "Armed: the leader will crash (disk lost) right after its next acknowledgement.")
	case "offline":
		s.setOnline(a.Station, false)
	case "online":
		s.setOnline(a.Station, true)
	case "dropAcks":
		s.dropAcks(a.Station, max(5, int(a.Value)))
	case "resend":
		s.resendLast(a.Station)
	case "jitter":
		if st := s.byName[a.Station]; st != nil {
			st.Jitter = int(a.Value)
			if st.Jitter > 1 {
				st.BatchMax, st.Window = 1, 4
			} else {
				st.BatchMax, st.Window = 12, 2
			}
			s.emit("fault", "warn", a.Station, "", fmt.Sprintf("%s's network delay is now 1–%d min per request.", a.Station, st.Jitter+1))
		}
	case "skew":
		if st := s.byName[a.Station]; st != nil {
			st.ClockSkew = int(a.Value)
			s.emit("fault", "warn", a.Station, "", fmt.Sprintf("%s's clock is now off by %+d min.", a.Station, st.ClockSkew))
		}
	case "schema":
		if st := s.byName[a.Station]; st != nil {
			st.Schema = int(a.Value)
			s.emit("schema", "info", a.Station, "", fmt.Sprintf("%s now sends schema v%d.", a.Station, st.Schema))
		}
	case "converter":
		s.AddConverter(int(a.Value))
	case "correct":
		if st := s.byName[a.Station]; st != nil {
			r := s.Store.Records[fmt.Sprintf("%s#%d", a.Station, a.Seq)]
			v := 2
			if r != nil {
				for _, x := range r.Versions {
					v = max(v, x.Version+1)
				}
			}
			s.SendCorrection(a.Station, a.Seq, a.Value, v)
		}
	case "chaos":
		s.Chaos = a.Value > 0
		s.chaosNext = s.Now + 5
		if s.Chaos {
			s.emit("fault", "warn", "", "", "Chaos mode on: random crashes, lost acks, jitter, outages and corrections every 15–40 min.")
		} else {
			s.emit("fault", "info", "", "", "Chaos mode off.")
		}
	case "uploadEvery":
		s.Cfg.UploadEvery = max(1, int(a.Value))
	}
}

// ---------- scenario catalogue ----------

var Scenarios = []*Scenario{
	{
		ID: "out-of-order", Case: "Case 1", Title: "Out-of-order data", Focus: "A", Length: 150,
		Summary: "Station A's readings overtake each other on a jittery network. History must follow event time, not arrival time.",
		Watch:   []string{"Out-of-order arrivals counter", "A's history: the 'late' column", "Naive mode: misordered rows"},
		Steps: []Step{
			{At: 0, Do: func(s *Sim) { s.Do(Action{Type: "jitter", Station: "A", Value: 9}); s.byName["A"].Interval = 2 },
				Title: "Station A's network gets unreliable",
				Text:  "A switches to a reading every 2 minutes, and each reading now travels in its own request that takes 1–10 minutes to arrive. Later readings will overtake earlier ones, like the 09:55 reading arriving after the 10:00 one. Press Play and watch the out-of-order counter and A's history."},
			{At: 60, Title: "Arrival order is not recorded order",
				Text: "Several of A's readings arrived after later ones. A's history is still in the right order, because every reading carries its event time (when it was measured) separately from its ingestion time (when it arrived). The 'late' column shows the gap. Switch to Naive mode and restart: history is kept in arrival order and the 'misordered rows' counter climbs."},
			{At: 120, Do: func(s *Sim) { s.Do(Action{Type: "jitter", Station: "A", Value: 1}); s.byName["A"].Interval = 10 },
				Title: "Network back to normal",
				Text:  "Same data, two possible orders. Using event time means the archive doesn't depend on how kind the network was."},
		},
	},
	{
		ID: "duplicates", Case: "Case 2", Title: "Duplicate data", Focus: "B", Length: 110,
		Summary: "Acknowledgements to Station B get lost, so B resends readings that were already stored, including to a new leader after a crash.",
		Watch:   []string{"Duplicates ignored", "Fast-path duplicates", "B's history: one row per sequence number"},
		Steps: []Step{
			{At: 0, Do: func(s *Sim) { s.dropAcks("B", 30) },
				Title: "Acknowledgements to B are lost",
				Text:  "For 30 minutes every 'success' sent to B disappears on the way back. B's data is safe on 2 machines, but B can't know that, so after 8 minutes it resends the same readings with the same event IDs (station + sequence number)."},
			{At: 35, Title: "Retries recognised by event ID",
				Text: "Look at 'fast-path duplicates': the leader saw those event IDs were already stored and acknowledged them again without writing anything. B's history has exactly one row per sequence number. In Naive mode every retry becomes a new row, and B's averages are quietly wrong."},
			{At: 40, Do: func(s *Sim) { s.mark("crashed", s.crashLeader(false)); s.dropAcks("B", 12) },
				Title: "A retry after a leader change",
				Text:  "The leader just crashed while B's acks are still being lost. B's next retry goes to a newly elected leader. Correctness doesn't depend on any in-memory state of the old leader: the event ID stored with the data is what catches the duplicate."},
			{At: 75, Do: func(s *Sim) { s.restart(s.marks["crashed"], false) }},
			{At: 100, Title: "Result",
				Text: "Every retry was recognised, before and after the leader change. Duplicates stored: 0. Try Naive mode to see the duplicate rows appear."},
		},
	},
	{
		ID: "delayed", Case: "Case 3", Title: "Delayed data", Focus: "C", Length: 170,
		Summary: "Station C was offline for 2 days and E and F for 6 hours. They all reconnect at once with hundreds of buffered readings.",
		Watch:   []string{"Backpressure 'busy' replies", "Tier 'delta' in C's history", "Partition rewrite in the event log", "Naive: acked but dropped"},
		PreWarmup: func(s *Sim) {
			switch s.Now {
			case 1 * DayTicks:
				s.byName["C"].Online = false
			case defaultWarmup - 360:
				s.byName["E"].Online = false
				s.byName["F"].Online = false
			}
		},
		Steps: []Step{
			{At: 0, Do: func(s *Sim) { s.setOnline("C", true); s.setOnline("E", true); s.setOnline("F", true) },
				Title: "Three stations reconnect at once",
				Text:  "C kept measuring through a 2-day outage and buffered ~288 readings. E and F were out for 6 hours (~36 each). They all start uploading now. Watch the backlog and the backpressure counter."},
			{At: 25, Title: "Bounded batches and backpressure",
				Text: "Each station sends at most 12 readings per request, with 2 requests in flight. When processing falls more than 60 readings behind, the leader replies 'busy' and the station backs off for 5 minutes. Memory stays bounded and nothing is dropped: the readings simply wait on the station."},
			{At: 95, Title: "Late data in old partitions",
				Text: "Readings from day 2 belong to partitions that were already compacted into immutable Parquet files. Instead of rewriting a file for every late reading, they go into a small delta file beside it (tier 'delta' in C's history). Once deltas pass 10% of a partition, it's rewritten once: look for 'Rewrote the partition' in the log."},
			{At: 160, Title: "Result",
				Text: "Two days of history arrived late, and all of it landed in the right place. In Naive mode, readings older than 6 hours are rejected AFTER the station was acknowledged: check the 'acked but dropped' counter."},
		},
	},
	{
		ID: "corrections", Case: "Case 4", Title: "Historical corrections", Focus: "D", Length: 70,
		Summary: "Station D corrects a reading from day 2, then sends conflicting and out-of-order corrections, and one for a reading that doesn't exist.",
		Watch:   []string{"D's history: rows flagged 'corrected' / 'conflict'", "Versions list for the corrected row", "Quarantine"},
		Setup: func(s *Sim) {
			r := s.recordNear("D", DayTicks+600)
			s.mark("ref", r.Seq)
			s.markF = r.Current().TempC
		},
		Steps: []Step{
			{At: 0, Do: func(s *Sim) { s.SendCorrection("D", s.marks["ref"], s.markF-0.7, 2) },
				Title: "A correction for last month's reading",
				Text:  "D was recalibrated and corrects a reading from day 2, 10:00. That reading already sits in a compacted Parquet partition. The correction is a new event that points at the original event ID."},
			{At: 12, Do: func(s *Sim) {
				s.SendCorrection("D", s.marks["ref"], s.markF-1.0, 3)
				s.SendCorrection("D", s.marks["ref"], s.markF-0.4, 3)
			},
				Title: "Stored as a new version, not an overwrite",
				Text:  "Click the corrected row in D's history: v1 is still there for audit, v2 is current. Next, two different corrections both claim to be v3 at the same moment."},
			{At: 25, Do: func(s *Sim) { s.SendCorrection("D", s.marks["ref"], s.markF-0.9, 5) },
				Title: "Conflict flagged",
				Text:  "Neither v3 silently wins: the row is flagged 'conflict' and v2 stays current until someone resolves it. Next, v5 is sent, then v4 a few minutes later, so v4 arrives last."},
			{At: 29, Do: func(s *Sim) { s.SendCorrection("D", s.marks["ref"], s.markF-0.2, 4) }},
			{At: 42, Do: func(s *Sim) { s.SendCorrection("D", 999999, 20, 2) },
				Title: "Highest version wins, not latest arrival",
				Text:  "v5 is current even though v4 arrived after it. Finally, a correction for a reading that doesn't exist (#999999)."},
			{At: 60, Title: "Result",
				Text: "The unknown-reference correction is quarantined, not applied. The full audit trail survives. In Naive mode each correction overwrites the value: v1 is gone, and v4 (the last to arrive) wins."},
		},
	},
	{
		ID: "crash", Case: "Case 5", Title: "Crash after accepting a reading", Focus: "A", Length: 90,
		Summary: "The leader crashes after appending but before fsync, then again right after sending 'success' with its disk destroyed.",
		Watch:   []string{"Lost acknowledged readings (must stay 0)", "Node logs: written vs fsynced", "Elections"},
		Steps: []Step{
			{At: 0, Do: func(s *Sim) { s.hookCrashAfterAppend = &crashHook{} },
				Title: "Crash before the disk flush",
				Text:  "The next time the leader appends a batch, it will crash before calling fsync. That entry exists only in the page cache. In Design mode no station has been told 'success' yet. In Naive mode (ack on receive), it has."},
			{At: 10, Do: func(s *Sim) { s.mark("first", s.findDown()) },
				Title: "Nothing promised, nothing lost",
				Text:  "The unsynced entry vanished with the crash, but no ack had been sent, so the station timed out and resent it to the new leader. Lost acknowledged readings: 0. Run this in Naive mode to see that counter go up."},
			{At: 25, Do: func(s *Sim) { s.restart(s.marks["first"], false) }},
			{At: 30, Do: func(s *Sim) { s.hookCrashAfterAck = &crashHook{loseDisk: true} },
				Title: "Crash right after 'success'",
				Text:  "Now the leader will crash immediately after sending an acknowledgement, and its disk is destroyed too. The ack was only sent after 2 machines had fsynced the entry."},
			{At: 45, Do: func(s *Sim) { s.mark("second", s.findDown()) },
				Title: "The promise held",
				Text:  "A follower became leader. Raft only elects a machine whose log is at least as up to date as a majority's, so it already had every acknowledged entry. Next, the destroyed machine is replaced and bootstraps from object storage."},
			{At: 50, Do: func(s *Sim) { s.restart(s.marks["second"], true) }},
			{At: 80, Title: "Result",
				Text: "Two leader crashes, one with total disk loss. Lost acknowledged readings: 0. 'Success' meant exactly what the system could prove would survive."},
		},
	},
	{
		ID: "no-quorum", Case: "Machines", Title: "Losing the majority", Focus: "E", Length: 90,
		Summary: "Two of three machines go down. Then the last one loses its disk. Writes must stop rather than make promises nobody can keep.",
		Watch:   []string{"'No quorum' refusals", "Station buffers growing", "Lost acknowledged readings"},
		Steps: []Step{
			{At: 0, Do: func(s *Sim) {
				for _, i := range s.followers(true) {
					s.crashNode(i, false)
				}
			},
				Title: "Two machines go down",
				Text:  "Only the leader is left. In Design mode it can't store anything on 2 machines, so it refuses writes with a retryable error. Stations keep buffering. History queries still work."},
			{At: 20, Title: "Writes refused, reads still served",
				Text: "See the 'no quorum' refusals and the growing station buffers. Next: the last machine's disk dies too."},
			{At: 25, Do: func(s *Sim) { s.crashLeader(true) }},
			{At: 40, Do: func(s *Sim) {
				for i, n := range s.Nodes {
					if !n.Up && len(n.Log) > 0 {
						s.restart(i, false)
					}
				}
			}},
			{At: 46, Title: "Two machines back",
				Text: "The two machines that crashed earlier come back with their disks, form a majority, and elect a leader. Stations flush their buffers. In Design mode nothing acknowledged was lost. In Naive mode the lone leader kept saying 'success' with a single copy, and all of it died with its disk."},
			{At: 55, Do: func(s *Sim) {
				for i, n := range s.Nodes {
					if !n.Up {
						s.restart(i, true)
					}
				}
			}},
			{At: 85, Title: "Result",
				Text: "Refusing writes for 40 minutes cost nothing: the stations held the data. Accepting them on one machine would have lost it."},
		},
	},
	{
		ID: "two-disks", Case: "Residual risk", Title: "The one way to lose data", Focus: "A", Length: 70,
		Summary: "Both machines holding the newest entries lose their disks before the next upload to object storage. This is the design's honest limit.",
		Watch:   []string{"Lost acknowledged readings", "Uploaded index vs commit index", "Try upload interval 5 vs 30"},
		Steps: []Step{
			{At: 0, Do: func(s *Sim) { i := s.followers(true)[0]; s.mark("x", i); s.crashNode(i, false) },
				Title: "Where the design can still lose data",
				Text:  "The archive survives any one machine. It can only lose acknowledged data if both machines holding the newest entries lose their disks before the next upload to object storage. One follower has stopped; the other two keep committing."},
			{At: 25, Do: func(s *Sim) {
				f := s.followers(true)
				s.mark("y", f[0])
				s.crashLeader(true)
				s.crashNode(f[0], true)
			},
				Title: "Both copies destroyed",
				Text:  "The leader and the remaining follower just lost their disks together. Entries committed since the last upload existed only there."},
			{At: 35, Do: func(s *Sim) { s.restart(s.marks["x"], false) }},
			{At: 40, Do: func(s *Sim) { s.restart(s.marks["y"], true) }},
			{At: 50, Title: "What was lost, and why",
				Text: "Everything acknowledged after the last upload is gone. This is why the copies sit in different zones (so a correlated failure is rare) and why segments are uploaded every few minutes. Restart with 'upload every' set to 5 and compare the loss."},
		},
	},
	{
		ID: "timestamps", Case: "Case 1 edge cases", Title: "Bad clocks and missing timestamps", Focus: "E", Length: 90,
		Summary: "Station E's clock runs fast, then sends readings with no timestamp, then jumps backwards.",
		Watch:   []string{"Flags in E's history: future-ts, missing-ts, clock-jump", "Flagged rows counter"},
		Steps: []Step{
			{At: 0, Do: func(s *Sim) { s.Do(Action{Type: "skew", Station: "E", Value: 120}) },
				Title: "E's clock runs 2 hours fast",
				Text:  "E's readings now claim to be from the future. The archive keeps the timestamp exactly as sent but flags it 'future-ts', rather than silently 'fixing' it."},
			{At: 25, Do: func(s *Sim) { s.Do(Action{Type: "skew", Station: "E", Value: 0}); s.byName["E"].MissingTS = 3 },
				Title: "Readings with no timestamp",
				Text:  "The next 3 readings arrive with no event time. They're stored at their ingestion time and flagged 'missing-ts' (estimated). The sequence number still keeps them in order."},
			{At: 55, Do: func(s *Sim) { s.Do(Action{Type: "skew", Station: "E", Value: -90}) },
				Title: "The clock jumps back 90 minutes",
				Text:  "A reading with a higher sequence number now claims an earlier time than the one before it. That's flagged 'clock-jump'."},
			{At: 80, Do: func(s *Sim) { s.Do(Action{Type: "skew", Station: "E", Value: 0}) },
				Title: "Result",
				Text:  "Every suspicious timestamp is kept as sent and visibly flagged. Nothing was silently corrected, so the bad data can be traced and fixed later."},
		},
	},
	{
		ID: "schema", Case: "Formats", Title: "Changing data formats", Focus: "G", Length: 80,
		Summary: "Station G switches to Fahrenheit (schema v2). Station H sends a schema the archive doesn't know yet (v3, Kelvin).",
		Watch:   []string{"G's history stays in °C", "Quarantine count", "Replay after deploying the v3 converter"},
		Steps: []Step{
			{At: 0, Do: func(s *Sim) { s.Do(Action{Type: "schema", Station: "G", Value: 2}) },
				Title: "G gets new firmware",
				Text:  "G now sends temp_f and rh instead of temp_c and humidity. Every event carries its schema version, so the normaliser converts it. G's history continues in °C without a seam."},
			{At: 20, Do: func(s *Sim) { s.Do(Action{Type: "schema", Station: "H", Value: 3}) },
				Title: "H sends a format nobody knows yet",
				Text:  "H's v3 readings (Kelvin) have no converter. They're still acknowledged and stored in the log, but quarantined instead of being dropped or blocking everyone else."},
			{At: 50, Do: func(s *Sim) { s.AddConverter(3) },
				Title: "Deploy the v3 converter",
				Text:  "A converter for v3 was just deployed. The quarantined readings are replayed into H's history in the right place. Nothing was lost and ingestion never stopped."},
			{At: 75, Title: "Result",
				Text: "Formats changed twice while the archive kept serving. The raw payload is always kept, so a fix later means replay, not data loss."},
		},
	},
	{
		ID: "similar", Case: "Search", Title: "Finding similar stations", Focus: "A", Length: 30,
		Summary: "Rank stations by how they behave: level, swing and the shape of the day. Station L went silent 30 hours ago.",
		Watch:   []string{"Similarity panel", "Daily profile chart", "L excluded for low coverage"},
		PreWarmup: func(s *Sim) {
			if s.Now == defaultWarmup-1800 {
				s.byName["L"].Stopped = true
			}
		},
		Steps: []Step{
			{At: 0, Title: "Find stations that behave like A",
				Text: "Each station gets a 31-number vector over the last 2 days: mean, spread, min, max, daily swing, humidity, coverage, and the 24-hour shape of its day. Features are normalised and compared with cosine similarity. A's twin should be G (also coastal). Pick other stations to compare."},
			{At: 10, Title: "Silent stations are excluded, not guessed",
				Text: "L stopped reporting 30 hours ago and is marked silent. Its window has under 70% coverage, so it's excluded rather than compared on half a picture."},
			{At: 20, Title: "Why no vector database",
				Text: "Here 12 stations × 31 numbers is ~1.5 KB. At my design scale (50,000 stations × 32 floats) it's ~6.4 MB and a full scan is ~1.6M multiply-adds: milliseconds. A vector index only pays off at millions of stations."},
		},
	},
	{
		ID: "sandbox", Case: "Sandbox", Title: "Free play and chaos", Focus: "A", Length: 0,
		Summary: "No script. Crash machines, cut stations off, drop acks, correct readings, or switch on chaos mode and leave it running.",
		Watch:   []string{"Lost acknowledged readings", "Duplicates stored", "Everything else"},
	},
}

func findScenario(id string) *Scenario {
	for _, sc := range Scenarios {
		if sc.ID == id {
			return sc
		}
	}
	return nil
}

func (s *Sim) findDown() int {
	for i, n := range s.Nodes {
		if !n.Up {
			return i
		}
	}
	return -1
}

var _ = math.Abs
