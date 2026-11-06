package sim

import "sort"

// NodeView is a node as the UI sees it.
type NodeView struct {
	Name     string `json:"name"`
	Zone     string `json:"zone"`
	Up       bool   `json:"up"`
	Role     string `json:"role"`
	LogLen   int    `json:"logLen"`
	Durable  int    `json:"durable"`
	Unsynced int    `json:"unsynced"`
	LastTerm int    `json:"lastTerm"`
	Tail     []LogCell `json:"tail"`
}

// LogCell is one of the last few log entries, for drawing.
type LogCell struct {
	Index     int    `json:"index"`
	Station   string `json:"station"`
	N         int    `json:"n"`
	Durable   bool   `json:"durable"`
	Committed bool   `json:"committed"`
	Uploaded  bool   `json:"uploaded"`
}

// StationView is a station as the UI sees it.
type StationView struct {
	Name      string `json:"name"`
	Climate   string `json:"climate"`
	Online    bool   `json:"online"`
	Stopped   bool   `json:"stopped"`
	Silent    bool   `json:"silent"`
	Buffer    int    `json:"buffer"`
	Inflight  int    `json:"inflight"`
	Schema    int    `json:"schema"`
	Skew      int    `json:"skew"`
	Jitter    int    `json:"jitter"`
	AcksLost  bool   `json:"acksLost"`
	Stored    int    `json:"stored"`
	NextSeq   int    `json:"nextSeq"`
	LastSeen  int    `json:"lastSeen"`
}

// State is everything the UI draws each frame.
type State struct {
	Now        int           `json:"now"`
	Rel        int           `json:"rel"`
	Clock      string        `json:"clock"`
	Scenario   *Scenario     `json:"scenario"`
	Config     Config        `json:"config"`
	Term       int           `json:"term"`
	Leader     string        `json:"leader"`
	Commit     int           `json:"commit"`
	Applied    int           `json:"applied"`
	Uploaded   int           `json:"uploaded"`
	Backlog    int           `json:"backlog"`
	InNetwork  int           `json:"inNetwork"`
	Nodes      []NodeView    `json:"nodes"`
	Stations   []StationView `json:"stations"`
	Inv        Invariants    `json:"inv"`
	Stats      Stats         `json:"stats"`
	Partitions []Partition   `json:"partitions"`
	Quarantine []QItem       `json:"quarantine"`
	Events     []Event       `json:"events"`
	Checkpoint *Checkpoint   `json:"checkpoint"`
	Chaos      bool          `json:"chaos"`
	Done       bool          `json:"done"`
	StoredRows int           `json:"storedRows"`
}

// Snapshot builds the UI state and hands over new events.
func (s *Sim) Snapshot() State {
	st := State{Now: s.Now, Rel: s.rel(), Clock: clock(s.Now), Scenario: s.scenario, Config: s.Cfg,
		Term: s.Term, Commit: s.Commit, Applied: s.Applied, Uploaded: s.Uploaded, Backlog: s.backlog(),
		InNetwork: len(s.net), Inv: s.Invariants(), Stats: s.Stats, Events: s.NewEvents(),
		Checkpoint: s.Checkpoint, Chaos: s.Chaos, StoredRows: len(s.Store.Rows)}
	if s.Leader >= 0 {
		st.Leader = s.Nodes[s.Leader].Name
	}
	st.Done = s.scenario.Length > 0 && s.rel() >= s.scenario.Length
	for i, n := range s.Nodes {
		role := "follower"
		if !n.Up {
			role = "down"
		} else if i == s.Leader {
			role = "leader"
		}
		v := NodeView{Name: n.Name, Zone: n.Zone, Up: n.Up, Role: role, LogLen: len(n.Log),
			Durable: n.Durable, Unsynced: len(n.Log) - n.Durable, LastTerm: n.lastTerm()}
		from := max(0, len(n.Log)-14)
		for _, e := range n.Log[from:] {
			v.Tail = append(v.Tail, LogCell{Index: e.Index, Station: e.Batch.Station, N: len(e.Batch.Readings),
				Durable: e.Index <= n.Durable, Committed: e.Index <= s.Commit, Uploaded: e.Index <= s.Uploaded})
		}
		st.Nodes = append(st.Nodes, v)
	}
	counts := map[string]int{}
	for _, r := range s.Store.Rows {
		counts[r.Station]++
	}
	for _, x := range s.Stations {
		st.Stations = append(st.Stations, StationView{Name: x.Name, Climate: x.Climate, Online: x.Online,
			Stopped: x.Stopped, Silent: x.Silent, Buffer: len(x.buffer), Inflight: len(x.inflight),
			Schema: x.Schema, Skew: x.ClockSkew, Jitter: x.Jitter, AcksLost: s.Now < x.AckDropUntil,
			Stored: counts[x.Name], NextSeq: x.NextSeq, LastSeen: x.LastSeen})
	}
	for _, p := range s.Store.Partitions {
		st.Partitions = append(st.Partitions, *p)
	}
	sort.Slice(st.Partitions, func(i, j int) bool {
		if st.Partitions[i].Day != st.Partitions[j].Day {
			return st.Partitions[i].Day < st.Partitions[j].Day
		}
		return st.Partitions[i].Bucket < st.Partitions[j].Bucket
	})
	q := s.Store.Quarantine
	if len(q) > 30 {
		q = q[len(q)-30:]
	}
	st.Quarantine = q
	return st
}
