package sim

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
)

const (
	RetryTimeout   = 8  // ticks a station waits for an ack before resending
	NackBackoff    = 5  // ticks a station waits after "unavailable"/"busy"
	ApplyRate      = 10 // readings the background workers process per tick
	MaxBacklog     = 60 // unprocessed readings before the leader pushes back
	ElectionDelay  = 2  // ticks without a leader after it disappears
	SilentFactor   = 3  // a station is "silent" after 3× its interval without data
	MaxEvents      = 4000
	defaultWarmup  = 3 * DayTicks
)

// Sim is the whole simulated world.
type Sim struct {
	Now   int
	T0    int // tick when the scenario proper starts (after warm-up)
	Cfg   Config
	rng   *rand.Rand
	Nodes []*Node

	Leader     int // index into Nodes, -1 = none
	Term       int
	electionAt int
	Commit     int
	Applied    int
	Uploaded   int
	ObjectLog  []Entry
	applyPos   int // readings already applied inside entry Applied+1

	net      []*Message
	Stations []*Station
	byName   map[string]*Station
	Store    *Store

	converters map[int]bool

	// What the cluster has promised: event key → tick the ack was sent.
	Acked map[string]int
	// Acks waiting for the commit rule: log index → batch.
	pending map[int]Batch

	batchSeq int
	quiet    bool

	Events   []Event
	eventID  int
	lastSent int

	hookCrashAfterAppend *crashHook
	hookCrashAfterAck    *crashHook

	Stats Stats

	scenario    *Scenario
	actionIdx   int
	checkIdx    int
	Checkpoint  *Checkpoint
	Chaos       bool
	chaosNext   int
	LostEver    map[string]bool
	marks       map[string]int
	markF       float64
}

type crashHook struct{ loseDisk bool }

// Stats are counters shown in the UI.
type Stats struct {
	BatchesSent     int `json:"batchesSent"`
	Retries         int `json:"retries"`
	AcksSent        int `json:"acksSent"`
	AcksDropped     int `json:"acksDropped"`
	NackNoQuorum    int `json:"nackNoQuorum"`
	NackBusy        int `json:"nackBusy"`
	FastPathDups    int `json:"fastPathDups"`
	Fsyncs          int `json:"fsyncs"`
	Elections       int `json:"elections"`
	Uploads         int `json:"uploads"`
	LostUnsynced    int `json:"lostUnsynced"`
	ReadsServed     int `json:"readsServed"`
	MaxBacklog      int `json:"maxBacklog"`
}

// New builds a world, runs the warm-up, and loads the scenario.
func New(cfg Config, scenarioID string, seed int64) (*Sim, error) {
	sc := findScenario(scenarioID)
	if sc == nil {
		return nil, fmt.Errorf("unknown scenario %q", scenarioID)
	}
	if cfg.UploadEvery <= 0 {
		cfg.UploadEvery = 30
	}
	s := &Sim{
		Cfg: cfg, rng: rand.New(rand.NewSource(seed)), Leader: 0, Term: 1,
		Store: newStore(), Acked: map[string]int{}, pending: map[int]Batch{},
		converters: map[int]bool{1: true, 2: true}, byName: map[string]*Station{},
		LostEver: map[string]bool{},
	}
	zones := []string{"zone-a", "zone-b", "zone-c"}
	for i := 0; i < 3; i++ {
		s.Nodes = append(s.Nodes, &Node{ID: i, Name: fmt.Sprintf("node-%d", i+1), Zone: zones[i], Up: true})
	}
	for _, c := range climates {
		st := &Station{Name: c.name, Climate: c.label, base: c.base, amp: c.amp, phase: c.phase,
			noise: c.noise, humBase: c.hum, Interval: 10, Online: true, Schema: 1, Jitter: 1,
			BatchMax: 12, Window: 2, inflight: map[int]inflight{}, acked: map[int]bool{}}
		st.nextGen = int(c.name[0]-'A') % 10
		s.Stations = append(s.Stations, st)
		s.byName[st.Name] = st
	}
	s.scenario = sc
	warm := defaultWarmup
	s.quiet = true
	for s.Now < warm {
		if sc.PreWarmup != nil {
			sc.PreWarmup(s)
		}
		s.tick()
	}
	s.quiet = false
	s.T0 = s.Now
	s.Events = nil
	s.lastSent = 0
	s.emit("scenario", "info", "", "", fmt.Sprintf("Scenario \"%s\" starts. The archive already holds %d readings from 3 simulated days.", sc.Title, len(s.Store.Rows)))
	if sc.Setup != nil {
		sc.Setup(s)
	}
	return s, nil
}

// Step advances up to n ticks, stopping early at a scenario checkpoint.
func (s *Sim) Step(n int) {
	s.Checkpoint = nil
	for i := 0; i < n; i++ {
		s.tick()
		if s.Checkpoint != nil {
			return
		}
	}
}

func (s *Sim) rel() int { return s.Now - s.T0 }

func (s *Sim) tick() {
	s.Now++
	if !s.quiet {
		s.runScenario()
	}
	if s.Chaos && !s.quiet {
		s.chaos()
	}
	leader := s.leaderNode()
	startLen := 0
	if leader != nil {
		startLen = len(leader.Log)
	}

	// 1. Deliver network messages due now.
	s.deliver()

	// 2. Followers copy what the leader had at the start of the tick, then fsync.
	leader = s.leaderNode()
	if leader != nil {
		for _, f := range s.Nodes {
			if f == leader || !f.Up {
				continue
			}
			s.catchUp(f, leader, startLen)
			f.Durable = len(f.Log)
		}
	}

	// 3. Group commit on the leader: one fsync covers everything appended this tick.
	leader = s.leaderNode()
	if leader != nil && leader.Durable < len(leader.Log) {
		n := len(leader.Log) - leader.Durable
		leader.Durable = len(leader.Log)
		s.Stats.Fsyncs++
		if n > 1 {
			s.emitQ("fsync", "info", "", leader.Name, fmt.Sprintf("Group commit: %s flushed %d log entries to disk with one fsync.", leader.Name, n))
		}
		if s.Cfg.AckPolicy == AckLocalFsync {
			s.ackUpTo(leader.Durable)
		}
	}

	// 4. Commit rule and acks.
	if s.leaderNode() != nil {
		s.advanceCommit()
	}

	// 5. Elections.
	if s.Leader < 0 && s.Now >= s.electionAt {
		s.elect()
	}

	// 6. Background processing.
	s.applyCommitted()

	// 7. Stations measure and send.
	for _, st := range s.Stations {
		s.stationTick(st)
	}

	// 8. Periodic jobs.
	if s.Now%s.Cfg.UploadEvery == 0 {
		s.upload()
	}
	if s.Now%CompactEvery == 0 {
		s.compact()
	}
	s.detectSilence()
	if bl := s.backlog(); bl > s.Stats.MaxBacklog {
		s.Stats.MaxBacklog = bl
	}
}

// ---------- cluster ----------

func (s *Sim) leaderNode() *Node {
	if s.Leader < 0 {
		return nil
	}
	return s.Nodes[s.Leader]
}

func (s *Sim) upCount() int {
	n := 0
	for _, nd := range s.Nodes {
		if nd.Up {
			n++
		}
	}
	return n
}

// catchUp makes f's log a prefix-consistent copy of the leader's first `upto` entries.
func (s *Sim) catchUp(f, leader *Node, upto int) {
	// Truncate any suffix that disagrees with the leader (uncommitted leftovers).
	i := 0
	for i < len(f.Log) && i < len(leader.Log) && f.Log[i].Term == leader.Log[i].Term && f.Log[i].Batch.ID == leader.Log[i].Batch.ID {
		i++
	}
	if i < len(f.Log) {
		dropped := len(f.Log) - i
		f.Log = f.Log[:i]
		if f.Durable > i {
			f.Durable = i
		}
		s.emitQ("replicate", "info", "", f.Name, fmt.Sprintf("%s discarded %d uncommitted entries that the new leader doesn't have.", f.Name, dropped))
	}
	if upto > len(leader.Log) {
		upto = len(leader.Log)
	}
	if len(f.Log) < upto {
		f.Log = append(f.Log, leader.Log[len(f.Log):upto]...)
	}
}

func (s *Sim) advanceCommit() {
	leader := s.leaderNode()
	var c int
	if s.Cfg.AckPolicy == AckQuorum {
		var ds []int
		for _, n := range s.Nodes {
			if n.Up {
				d := n.Durable
				if d > len(leader.Log) {
					d = len(leader.Log)
				}
				ds = append(ds, d)
			}
		}
		sort.Sort(sort.Reverse(sort.IntSlice(ds)))
		if len(ds) >= 2 {
			c = ds[1] // largest index durable on at least 2 of 3 machines
		}
	} else {
		c = leader.Durable // naive: one machine's word is enough
	}
	if c > s.Commit {
		from := s.Commit + 1
		s.Commit = c
		if s.Cfg.AckPolicy == AckQuorum {
			s.emitQ("commit", "info", "", "", fmt.Sprintf("Entries %d–%d are on disk on 2 of 3 machines: committed.", from, c))
			s.ackUpTo(c)
		}
	}
}

func (s *Sim) ackUpTo(idx int) {
	var ids []int
	for i := range s.pending {
		if i <= idx {
			ids = append(ids, i)
		}
	}
	sort.Ints(ids)
	for _, i := range ids {
		if s.Leader < 0 {
			return
		}
		b := s.pending[i]
		delete(s.pending, i)
		s.sendAck(b.Station, b.ID, b.seqs(), fmt.Sprintf("entry %d", i))
	}
}

func diskText(lose bool) string {
	if lose {
		return ", and its disk is gone with it"
	}
	return ""
}

func (s *Sim) sendAck(station string, batchID int, seqs []int, why string) {
	for _, q := range seqs {
		key := fmt.Sprintf("%s#%d", station, q)
		if _, ok := s.Acked[key]; !ok {
			s.Acked[key] = s.Now
		}
	}
	s.Stats.AcksSent++
	st := s.byName[station]
	if st != nil && s.Now < st.AckDropUntil {
		s.Stats.AcksDropped++
		s.emit("network", "warn", station, "", fmt.Sprintf("Ack for batch #%d (seq %s) lost on the way back to %s. The data IS safe, but the station doesn't know.", batchID, seqRange(seqs), station))
		return
	}
	s.send(&Message{Kind: "ack", Station: station, BatchID: batchID, Seqs: seqs, Reason: why})
	s.emitQ("ack", "good", station, "", fmt.Sprintf("Acked batch #%d from %s (seq %s) after %s.", batchID, station, seqRange(seqs), ackWhy(s.Cfg.AckPolicy)))
	s.fireAckHook(why)
}

func (s *Sim) fireAckHook(why string) {
	if why == "already stored" || s.hookCrashAfterAck == nil || s.Leader < 0 {
		return
	}
	h := s.hookCrashAfterAck
	s.hookCrashAfterAck = nil
	ld := s.leaderNode()
	s.emit("fault", "bad", "", ld.Name, fmt.Sprintf("FAULT: %s crashes right after sending that acknowledgement%s.", ld.Name, diskText(h.loseDisk)))
	s.crash(s.Leader, h.loseDisk)
}

func ackWhy(p AckPolicy) string {
	switch p {
	case AckOnReceive:
		return "merely receiving it (NAIVE)"
	case AckLocalFsync:
		return "one disk flush on one machine (NAIVE)"
	}
	return "fsync on 2 of 3 machines"
}

func (s *Sim) crash(i int, loseDisk bool) {
	n := s.Nodes[i]
	if !n.Up {
		return
	}
	unsynced := len(n.Log) - n.Durable
	ackedLost := 0
	for _, e := range n.Log[n.Durable:] {
		for _, r := range e.Batch.Readings {
			if _, ok := s.Acked[r.Key()]; ok {
				ackedLost++
			}
		}
	}
	n.Up = false
	n.Log = n.Log[:n.Durable]
	what := "process crash: entries already fsynced survive on its disk"
	if loseDisk {
		n.Log = nil
		n.Durable = 0
		what = "machine disappeared with its local disk"
	}
	if unsynced > 0 {
		s.Stats.LostUnsynced += unsynced
		lvl := "info"
		extra := " None had been acknowledged, so stations will simply retry."
		if ackedLost > 0 {
			lvl = "bad"
			extra = fmt.Sprintf(" %d of those readings had ALREADY been acknowledged.", ackedLost)
		}
		s.emit("crash", lvl, "", n.Name, fmt.Sprintf("%s crashed (%s). %d entries were only in the page cache (written, not fsynced) and are gone.%s", n.Name, what, unsynced, extra))
	} else {
		s.emit("crash", "warn", "", n.Name, fmt.Sprintf("%s crashed (%s).", n.Name, what))
	}
	if s.Leader == i {
		s.Leader = -1
		s.electionAt = s.Now + ElectionDelay
		s.pending = map[int]Batch{} // unacked requests: stations will retry
	}
	s.checkLoss()
}

func (s *Sim) restart(i int, replace bool) {
	n := s.Nodes[i]
	if n.Up && !replace {
		return
	}
	if replace {
		n.Up = false
		n.Log = nil
		n.Durable = 0
	}
	// Bootstrap anything older than the last upload from object storage.
	if len(n.Log) < s.Uploaded {
		n.Log = append(n.Log, s.ObjectLog[len(n.Log):s.Uploaded]...)
		n.Durable = len(n.Log)
	}
	n.Up = true
	if replace {
		s.emit("recover", "good", "", n.Name, fmt.Sprintf("Replacement %s started in %s and bootstrapped %d entries from object storage. It will copy the rest from the leader.", n.Name, n.Zone, len(n.Log)))
	} else {
		s.emit("recover", "good", "", n.Name, fmt.Sprintf("%s restarted with %d entries on its disk and rejoins the cluster.", n.Name, len(n.Log)))
	}
	if s.Leader < 0 && s.electionAt < s.Now {
		s.electionAt = s.Now + 1
	}
}

func (s *Sim) elect() {
	need := 2
	if s.Cfg.AckPolicy != AckQuorum {
		need = 1 // naive: no quorum rule
	}
	if s.upCount() < need {
		s.electionAt = s.Now + 1
		return
	}
	best := -1
	for i, n := range s.Nodes {
		if !n.Up {
			continue
		}
		if best < 0 || n.lastTerm() > s.Nodes[best].lastTerm() ||
			(n.lastTerm() == s.Nodes[best].lastTerm() && len(n.Log) > len(s.Nodes[best].Log)) {
			best = i
		}
	}
	s.Term++
	s.Leader = best
	s.Stats.Elections++
	ld := s.Nodes[best]
	ld.Durable = len(ld.Log)
	s.emit("election", "info", "", ld.Name, fmt.Sprintf("%s elected leader for term %d. Raft picks the machine with the most up-to-date log (%d entries), so every committed entry is kept.", ld.Name, s.Term, len(ld.Log)))
	if s.Commit > len(ld.Log) {
		lost := s.Commit - len(ld.Log)
		s.emit("loss", "bad", "", "", fmt.Sprintf("DATA LOSS: %d committed log entries existed only on machines that are gone, and hadn't reached object storage yet.", lost))
		s.Commit = len(ld.Log)
	}
	if s.Applied > s.Commit {
		s.rebuildStore()
	}
	s.checkLoss()
}

func (s *Sim) rebuildStore() {
	ld := s.leaderNode()
	s.Store = newStore()
	q := s.quiet
	s.quiet = true
	for _, e := range ld.Log[:s.Commit] {
		s.apply(e)
	}
	s.quiet = q
	s.Applied = s.Commit
	s.applyPos = 0
	s.compact()
	s.emit("recover", "warn", "", ld.Name, fmt.Sprintf("Rebuilt the queryable store from the surviving log (%d entries).", s.Commit))
}

func (s *Sim) applyCommitted() {
	ld := s.leaderNode()
	if ld == nil {
		return
	}
	budget := ApplyRate
	for budget > 0 && s.Applied < s.Commit {
		e := ld.Log[s.Applied]
		rs := e.Batch.Readings
		for s.applyPos < len(rs) && budget > 0 {
			s.applyReading(rs[s.applyPos], e.AppendedAt)
			s.applyPos++
			budget--
		}
		if s.applyPos >= len(rs) {
			s.Applied++
			s.applyPos = 0
		}
	}
}

func (s *Sim) backlog() int {
	ld := s.leaderNode()
	if ld == nil {
		return 0
	}
	n := 0
	for i := s.Applied; i < len(ld.Log); i++ {
		n += len(ld.Log[i].Batch.Readings)
	}
	return n - s.applyPos
}

func (s *Sim) upload() {
	ld := s.leaderNode()
	if ld == nil || s.Commit <= s.Uploaded {
		return
	}
	from := s.Uploaded + 1
	s.ObjectLog = append(s.ObjectLog, ld.Log[s.Uploaded:s.Commit]...)
	s.Uploaded = s.Commit
	s.Stats.Uploads++
	s.emitQ("upload", "info", "", "", fmt.Sprintf("Uploaded sealed log segment (entries %d–%d) to object storage. Local disks are now just a cache for these.", from, s.Uploaded))
}

// ---------- network & stations ----------

func (s *Sim) send(m *Message) {
	delay := 1
	if st := s.byName[m.Station]; st != nil && st.Jitter > 0 {
		delay += s.rng.Intn(st.Jitter + 1)
	}
	m.DeliverAt = s.Now + delay
	s.net = append(s.net, m)
}

func (s *Sim) deliver() {
	var keep []*Message
	var due []*Message
	for _, m := range s.net {
		if m.DeliverAt <= s.Now {
			due = append(due, m)
		} else {
			keep = append(keep, m)
		}
	}
	s.net = keep
	for _, m := range due {
		switch m.Kind {
		case "batch":
			s.receiveBatch(m.Batch)
		case "ack":
			s.stationAck(m)
		case "nack":
			s.stationNack(m)
		}
	}
}

func (s *Sim) nack(b Batch, reason string) {
	s.send(&Message{Kind: "nack", Station: b.Station, BatchID: b.ID, Reason: reason})
}

func (s *Sim) receiveBatch(b Batch) {
	ld := s.leaderNode()
	if ld == nil {
		s.Stats.NackNoQuorum++
		s.nack(b, "no leader")
		s.emitQ("reject", "warn", b.Station, "", fmt.Sprintf("Batch #%d from %s refused: no leader right now. The station keeps the data and retries.", b.ID, b.Station))
		return
	}
	if s.Cfg.AckPolicy == AckQuorum && s.upCount() < 2 {
		s.Stats.NackNoQuorum++
		s.nack(b, "no quorum")
		s.emitQ("reject", "warn", b.Station, "", fmt.Sprintf("Batch #%d from %s refused with a retryable error: only %d of 3 machines are up, so it can't be stored twice. Reads still work.", b.ID, b.Station, s.upCount()))
		return
	}
	if s.backlog() > MaxBacklog {
		s.Stats.NackBusy++
		s.nack(b, "busy")
		s.emitQ("backpressure", "warn", b.Station, "", fmt.Sprintf("Backpressure: %d readings waiting to be processed, so batch #%d from %s is told to slow down and retry. Memory stays bounded.", s.backlog(), b.ID, b.Station))
		return
	}
	// Fast path: readings already in the store are retries.
	keep := b.Readings
	if s.Cfg.Dedup {
		keep = nil
		for _, r := range b.Readings {
			stored := s.Store.Records[r.Key()] != nil || s.Store.appliedCorr[r.Key()]
			if stored {
				s.Stats.FastPathDups++
			} else {
				keep = append(keep, r)
			}
		}
		if len(keep) == 0 {
			s.emitQ("dedup", "good", b.Station, "", fmt.Sprintf("Batch #%d from %s is a retry of readings already stored. Acknowledged again without writing anything.", b.ID, b.Station))
			s.sendAck(b.Station, b.ID, b.seqs(), "already stored")
			return
		}
	}
	nb := b
	nb.Readings = keep
	e := Entry{Index: len(ld.Log) + 1, Term: s.Term, Batch: nb, AppendedAt: s.Now}
	ld.Log = append(ld.Log, e)
	s.emitQ("append", "info", b.Station, ld.Name, fmt.Sprintf("%s appended batch #%d from %s (%d readings) as log entry %d. In the page cache, not yet on disk.", ld.Name, b.ID, b.Station, len(nb.Readings), e.Index))
	if s.Cfg.AckPolicy == AckOnReceive {
		s.sendAck(b.Station, b.ID, b.seqs(), "receive")
	} else {
		s.pending[e.Index] = b
	}
	if s.hookCrashAfterAppend != nil && s.Leader >= 0 {
		h := s.hookCrashAfterAppend
		s.hookCrashAfterAppend = nil
		s.emit("fault", "bad", "", ld.Name, fmt.Sprintf("FAULT: %s crashes right after appending entry %d, before any fsync%s.", ld.Name, e.Index, diskText(h.loseDisk)))
		s.crash(s.Leader, h.loseDisk)
	}
}

func (s *Sim) stationAck(m *Message) {
	st := s.byName[m.Station]
	delete(st.inflight, m.BatchID)
	done := map[int]bool{}
	for _, q := range m.Seqs {
		done[q] = true
		st.acked[q] = true
	}
	var keep []Reading
	for _, r := range st.buffer {
		if !done[r.Seq] {
			keep = append(keep, r)
		}
	}
	st.buffer = keep
}

func (s *Sim) stationNack(m *Message) {
	st := s.byName[m.Station]
	delete(st.inflight, m.BatchID)
	st.backoffUntil = s.Now + NackBackoff
}

func (s *Sim) measure(st *Station, t int) Reading {
	hour := float64(t%DayTicks) / 60.0
	temp := st.base + st.amp*math.Sin(2*math.Pi*(hour-st.phase+6)/24) + st.noise*s.rng.NormFloat64()
	hum := math.Max(5, math.Min(100, st.humBase-0.8*(temp-st.base)+2*s.rng.NormFloat64()))
	ev := t + st.ClockSkew
	if st.MissingTS > 0 {
		ev = -1
		st.MissingTS--
	}
	st.NextSeq++
	r := Reading{Station: st.Name, Seq: st.NextSeq, EventTime: ev, Schema: st.Schema, Kind: "reading"}
	r.Fields = encode(st.Schema, temp, hum)
	return r
}

func encode(schema int, tempC, hum float64) map[string]float64 {
	switch schema {
	case 2:
		return map[string]float64{"temp_f": tempC*9/5 + 32, "rh": hum}
	case 3:
		return map[string]float64{"temp_k": tempC + 273.15, "rh_pct": hum}
	}
	return map[string]float64{"temp_c": tempC, "humidity": hum}
}

func (s *Sim) stationTick(st *Station) {
	if st.Stopped {
		return
	}
	for st.nextGen <= s.Now {
		st.buffer = append(st.buffer, s.measure(st, st.nextGen))
		st.nextGen += st.Interval
	}
	if !st.Online || s.Now < st.backoffUntil {
		return
	}
	// Retries: batches without an answer go back into the sendable pool.
	for id, f := range st.inflight {
		if s.Now-f.sentAt >= max(RetryTimeout, 2*st.Jitter+4) {
			delete(st.inflight, id)
			s.Stats.Retries++
			s.emitQ("retry", "info", st.Name, "", fmt.Sprintf("%s heard nothing for batch #%d after %d min, so it will resend the same readings (same event IDs).", st.Name, id, s.Now-f.sentAt))
		}
	}
	busy := map[int]bool{}
	for _, f := range st.inflight {
		for _, q := range f.seqs {
			busy[q] = true
		}
	}
	for len(st.inflight) < st.Window {
		var b []Reading
		for _, r := range st.buffer {
			if !busy[r.Seq] {
				b = append(b, r)
				busy[r.Seq] = true
				if len(b) >= st.BatchMax {
					break
				}
			}
		}
		if len(b) == 0 {
			return
		}
		s.batchSeq++
		batch := Batch{ID: s.batchSeq, Station: st.Name, Readings: b, SentAt: s.Now}
		st.inflight[batch.ID] = inflight{seqs: batch.seqs(), sentAt: s.Now}
		cp := batch
		st.lastSent = &cp
		s.Stats.BatchesSent++
		s.send(&Message{Kind: "batch", Station: st.Name, Batch: batch})
	}
}

func (s *Sim) detectSilence() {
	for _, st := range s.Stations {
		silent := s.Now-st.LastSeen > SilentFactor*st.Interval+RetryTimeout
		if silent != st.Silent {
			st.Silent = silent
			if silent {
				s.emit("silence", "warn", st.Name, "", fmt.Sprintf("%s has sent nothing for %d min (expected every %d). Marked silent; the gap is stored as missing, not filled in.", st.Name, s.Now-st.LastSeen, st.Interval))
			} else {
				s.emit("silence", "good", st.Name, "", fmt.Sprintf("%s is reporting again.", st.Name))
			}
		}
	}
}

func (s *Sim) stationByName(n string) *Station { return s.byName[n] }

// ---------- invariants ----------

// survivingKeys is every event key that still exists on some disk or in object storage.
func (s *Sim) survivingKeys() map[string]bool {
	out := map[string]bool{}
	add := func(es []Entry) {
		for _, e := range es {
			for _, r := range e.Batch.Readings {
				out[r.Key()] = true
			}
		}
	}
	for _, n := range s.Nodes {
		add(n.Log[:min(n.Durable, len(n.Log))])
		if n.Up {
			add(n.Log) // page-cache entries count while the machine is alive
		}
	}
	add(s.ObjectLog)
	for k := range s.Store.Records {
		out[k] = true // already applied into tiered storage
	}
	for k := range s.Store.appliedCorr {
		out[k] = true
	}
	return out
}

func (s *Sim) checkLoss() {
	alive := s.survivingKeys()
	newly := 0
	for k := range s.Acked {
		if !alive[k] && !s.LostEver[k] {
			s.LostEver[k] = true
			newly++
		}
	}
	if newly > 0 {
		s.emit("loss", "bad", "", "", fmt.Sprintf("INVARIANT BROKEN: %d acknowledged readings no longer exist anywhere. The stations already deleted their copies.", newly))
	}
}

// Invariants summarise correctness for the UI.
type Invariants struct {
	Acked        int `json:"acked"`
	LostAcked    int `json:"lostAcked"`
	AckedDropped int `json:"ackedDropped"` // acked + processed, but missing from history (naive late rejection)
	DupRows      int `json:"dupRows"`
	DupsIgnored  int `json:"dupsIgnored"`
	FastPathDups int `json:"fastPathDups"`
	Conflicts    int `json:"conflicts"`
	Overwrites   int `json:"overwrites"`
	Quarantined  int `json:"quarantined"`
	OutOfOrder   int `json:"outOfOrder"`
	Misordered   int `json:"misordered"`
	Flagged      int `json:"flagged"`
}

func (s *Sim) Invariants() Invariants {
	st := s.Store
	inv := Invariants{Acked: len(s.Acked), LostAcked: len(s.LostEver), DupRows: st.DupsStored,
		DupsIgnored: st.DupsIgnored, FastPathDups: s.Stats.FastPathDups, Conflicts: st.Conflicts,
		Overwrites: st.Overwrites, Quarantined: len(st.Quarantine), OutOfOrder: st.OutOfOrder}
	for k := range s.Acked {
		if st.rejectedKeys[k] && st.Records[k] == nil {
			inv.AckedDropped++
		}
	}
	for _, r := range st.Rows {
		if r.hasFlag("future-ts") || r.hasFlag("missing-ts") || r.hasFlag("clock-jump") {
			inv.Flagged++
		}
	}
	if !s.Cfg.EventTimeOrder {
		for _, stn := range s.Stations {
			inv.Misordered += s.History(stn.Name, 0).Misorder
		}
	}
	return inv
}

// ---------- events ----------

func (s *Sim) emit(kind, level, station, node, msg string) {
	if s.quiet {
		return
	}
	s.eventID++
	s.Events = append(s.Events, Event{ID: s.eventID, T: s.Now, Kind: kind, Level: level, Station: station, Node: node, Msg: msg})
	if len(s.Events) > MaxEvents {
		s.Events = s.Events[len(s.Events)-MaxEvents:]
	}
}

// emitQ is for high-frequency, routine events.
func (s *Sim) emitQ(kind, level, station, node, msg string) { s.emit(kind, level, station, node, msg) }

// NewEvents returns events not yet handed to the UI.
func (s *Sim) NewEvents() []Event {
	var out []Event
	for _, e := range s.Events {
		if e.ID > s.lastSent {
			out = append(out, e)
		}
	}
	if len(out) > 0 {
		s.lastSent = out[len(out)-1].ID
	}
	return out
}

func clock(t int) string {
	if t < 0 {
		return "—"
	}
	d := t / DayTicks
	m := t % DayTicks
	return fmt.Sprintf("D%d %02d:%02d", d+1, m/60, m%60)
}

func seqRange(q []int) string {
	if len(q) == 0 {
		return "-"
	}
	lo, hi := q[0], q[0]
	for _, v := range q {
		lo = min(lo, v)
		hi = max(hi, v)
	}
	if lo == hi {
		return fmt.Sprint(lo)
	}
	return fmt.Sprintf("%d–%d", lo, hi)
}
