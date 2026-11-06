package sim

import (
	"fmt"
	"hash/fnv"
	"math"
	"sort"
)

const (
	RecentWindow   = 720  // ticks (12 simulated hours) kept in the local "recent" tier
	DayTicks       = 1440 // one partition per simulated day
	Buckets        = 4    // station-hash buckets per day
	CompactEvery   = 60
	LateLimitNaive = 360 // naive mode rejects readings more than 6 h old
)

// Version is one value of a reading; corrections add versions instead of overwriting.
type Version struct {
	Version   int     `json:"version"`
	TempC     float64 `json:"tempC"`
	Hum       float64 `json:"hum"`
	EventKey  string  `json:"eventKey"`
	AppliedAt int     `json:"appliedAt"`
	Conflict  bool    `json:"conflict,omitempty"`
}

// Record is one stored measurement.
type Record struct {
	Key        string    `json:"key"`
	Station    string    `json:"station"`
	Seq        int       `json:"seq"`
	EventTime  int       `json:"eventTime"`
	IngestTime int       `json:"ingestTime"`
	Order      int       `json:"order"` // arrival order at the store
	Versions   []Version `json:"versions"`
	Flags      []string  `json:"flags"`
	Tier       string    `json:"tier"` // recent | parquet | delta
	Partition  string    `json:"partition"`
	Dup        bool      `json:"dup"`
}

// Current returns the value a query should show.
func (r *Record) Current() Version {
	best := r.Versions[0]
	for _, v := range r.Versions[1:] {
		if !v.Conflict && v.Version > best.Version {
			best = v
		}
	}
	return best
}

func (r *Record) hasFlag(f string) bool {
	for _, x := range r.Flags {
		if x == f {
			return true
		}
	}
	return false
}

func (r *Record) flag(f string) {
	if !r.hasFlag(f) {
		r.Flags = append(r.Flags, f)
	}
}

// QItem is a quarantined event: kept, not dropped, for later inspection or replay.
type QItem struct {
	Reading Reading `json:"reading"`
	Reason  string  `json:"reason"`
	At      int     `json:"at"`
	Ingest  int     `json:"ingest"`
}

// Partition is one immutable Parquet file group (day × station bucket).
type Partition struct {
	ID       string `json:"id"`
	Day      int    `json:"day"`
	Bucket   int    `json:"bucket"`
	Rows     int    `json:"rows"`
	Delta    int    `json:"delta"`
	Rewrites int    `json:"rewrites"`
	Sealed   bool   `json:"sealed"`
}

// Store is the logical archive built from the committed log.
type Store struct {
	Records      map[string]*Record
	Rows         []*Record
	Quarantine   []QItem
	Partitions   map[string]*Partition
	appliedCorr  map[string]bool
	order        int
	DupsIgnored  int
	DupsStored   int
	Conflicts    int
	RejectedLate int
	Overwrites   int
	OutOfOrder   int
	maxEvent     map[string]int
	rejectedKeys map[string]bool
}

func newStore() *Store {
	return &Store{
		Records:      map[string]*Record{},
		Partitions:   map[string]*Partition{},
		appliedCorr:  map[string]bool{},
		maxEvent:     map[string]int{},
		rejectedKeys: map[string]bool{},
	}
}

func bucketOf(station string) int {
	h := fnv.New32a()
	h.Write([]byte(station))
	return int(h.Sum32() % Buckets)
}

func partitionID(eventTime int, station string) (string, int, int) {
	day := eventTime / DayTicks
	if eventTime < 0 {
		day = 0
	}
	b := bucketOf(station)
	return fmt.Sprintf("day=%d/bucket=%d", day, b), day, b
}

// normalise converts any supported schema into °C and %RH.
func normalise(r Reading, converters map[int]bool) (tempC, hum float64, ok bool) {
	if !converters[r.Schema] {
		return 0, 0, false
	}
	switch r.Schema {
	case 1:
		return r.Fields["temp_c"], r.Fields["humidity"], true
	case 2:
		return (r.Fields["temp_f"] - 32) * 5 / 9, r.Fields["rh"], true
	case 3:
		return r.Fields["temp_k"] - 273.15, r.Fields["rh_pct"], true
	}
	return 0, 0, false
}

// apply processes one committed log entry into the store.
func (s *Sim) apply(e Entry) {
	st := s.Store
	for _, r := range e.Batch.Readings {
		s.applyReading(r, e.AppendedAt)
	}
	_ = st
}

func (s *Sim) applyReading(r Reading, ingest int) {
	st := s.Store
	tempC, hum, ok := normalise(r, s.converters)
	if !ok {
		st.Quarantine = append(st.Quarantine, QItem{Reading: r, Reason: fmt.Sprintf("unknown schema v%d", r.Schema), At: s.Now, Ingest: ingest})
		s.emit("quarantine", "warn", r.Station, "", fmt.Sprintf("Quarantined %s: schema v%d has no converter yet. Raw payload kept, nothing dropped.", r.Key(), r.Schema))
		return
	}
	if r.Kind == "correction" {
		s.applyCorrection(r, tempC, hum, ingest)
		return
	}
	key := r.Key()
	existing := st.Records[key]
	if existing != nil && s.Cfg.Dedup {
		st.DupsIgnored++
		s.emit("dedup", "good", r.Station, "", fmt.Sprintf("Duplicate %s ignored: same event ID already stored (a retry, not a new reading).", key))
		return
	}

	eventTime := r.EventTime
	var flags []string
	if eventTime < 0 {
		eventTime = ingest
		flags = append(flags, "missing-ts")
		s.emit("validate", "warn", r.Station, "", fmt.Sprintf("%s arrived with no timestamp. Stored at its ingestion time and flagged 'missing-ts' (estimated), not silently trusted.", key))
	} else if eventTime > ingest+5 {
		flags = append(flags, "future-ts")
		s.emit("validate", "warn", r.Station, "", fmt.Sprintf("%s claims event time %s, after it was received (%s). Clock drift: kept as sent, flagged 'future-ts'.", key, clock(eventTime), clock(ingest)))
	} else if prev := st.Records[fmt.Sprintf("%s#%d", r.Station, r.Seq-1)]; prev != nil && eventTime < prev.EventTime-30 {
		flags = append(flags, "clock-jump")
		s.emit("validate", "warn", r.Station, "", fmt.Sprintf("%s is %d min earlier than the previous sequence number. The station's clock jumped back: flagged 'clock-jump'.", key, prev.EventTime-eventTime))
	}

	if !s.Cfg.AcceptLate && eventTime < ingest-LateLimitNaive {
		st.RejectedLate++
		st.rejectedKeys[key] = true
		s.emitQ("reject", "bad", r.Station, "", fmt.Sprintf("NAIVE: rejected %s because it is %d h old. The station was already told it was stored.", key, (ingest-eventTime)/60))
		return
	}

	if eventTime < st.maxEvent[r.Station] {
		st.OutOfOrder++
		s.emitQ("order", "info", r.Station, "", fmt.Sprintf("%s (event %s) arrived after a later reading (event %s). Placed by event time.", key, clock(eventTime), clock(st.maxEvent[r.Station])))
	} else {
		st.maxEvent[r.Station] = eventTime
	}

	pid, day, b := partitionID(eventTime, r.Station)
	p := st.Partitions[pid]
	if p == nil {
		p = &Partition{ID: pid, Day: day, Bucket: b}
		st.Partitions[pid] = p
	}
	tier := "recent"
	if p.Sealed {
		tier = "delta"
		p.Delta++
		s.emitQ("storage", "info", r.Station, "", fmt.Sprintf("%s belongs to sealed partition %s. Written to a small delta file beside it instead of rewriting the Parquet file.", key, pid))
	}

	st.order++
	rec := &Record{
		Key: key, Station: r.Station, Seq: r.Seq, EventTime: eventTime, IngestTime: ingest,
		Order: st.order, Flags: flags, Tier: tier, Partition: pid,
		Versions: []Version{{Version: 1, TempC: tempC, Hum: hum, EventKey: key, AppliedAt: s.Now}},
	}
	if existing != nil {
		rec.Dup = true
		rec.Key = fmt.Sprintf("%s(dup%d)", key, st.order)
		st.DupsStored++
		s.emit("dedup", "bad", r.Station, "", fmt.Sprintf("NAIVE: stored a second copy of %s. History now double-counts this reading.", key))
	} else {
		st.Records[key] = rec
	}
	st.Rows = append(st.Rows, rec)
	if s.stationByName(r.Station) != nil && ingest > s.stationByName(r.Station).LastSeen {
		s.stationByName(r.Station).LastSeen = ingest
	}
}

func (s *Sim) applyCorrection(r Reading, tempC, hum float64, ingest int) {
	st := s.Store
	key := r.Key()
	if st.appliedCorr[key] && s.Cfg.Dedup {
		st.DupsIgnored++
		s.emit("dedup", "good", r.Station, "", fmt.Sprintf("Duplicate correction %s ignored.", key))
		return
	}
	st.appliedCorr[key] = true
	target := st.Records[fmt.Sprintf("%s#%d", r.Station, r.RefSeq)]
	if target == nil {
		st.Quarantine = append(st.Quarantine, QItem{Reading: r, Reason: fmt.Sprintf("correction refers to unknown reading %s#%d", r.Station, r.RefSeq), At: s.Now, Ingest: ingest})
		s.emit("correction", "warn", r.Station, "", fmt.Sprintf("Correction %s points to %s#%d, which doesn't exist. Quarantined, not applied.", key, r.Station, r.RefSeq))
		return
	}
	old := target.Current()
	if !s.Cfg.AppendOnlyCorrections {
		target.Versions = []Version{{Version: r.Version, TempC: tempC, Hum: hum, EventKey: key, AppliedAt: s.Now}}
		st.Overwrites++
		s.emit("correction", "bad", r.Station, "", fmt.Sprintf("NAIVE: overwrote %s %.1f°C → %.1f°C (v%d). The original value and the history of the change are gone; last arrival wins.", target.Key, old.TempC, tempC, r.Version))
		return
	}
	for _, v := range target.Versions {
		if v.Version == r.Version {
			target.Versions = append(target.Versions, Version{Version: r.Version, TempC: tempC, Hum: hum, EventKey: key, AppliedAt: s.Now, Conflict: true})
			target.flag("conflict")
			st.Conflicts++
			s.emit("correction", "warn", r.Station, "", fmt.Sprintf("Two different corrections both claim v%d for %s (%.1f°C vs %.1f°C). Flagged as a conflict; neither silently wins.", r.Version, target.Key, v.TempC, tempC))
			return
		}
	}
	target.Versions = append(target.Versions, Version{Version: r.Version, TempC: tempC, Hum: hum, EventKey: key, AppliedAt: s.Now})
	target.flag("corrected")
	now := target.Current()
	if now.Version == r.Version {
		s.emit("correction", "good", r.Station, "", fmt.Sprintf("Correction v%d for %s: %.1f°C → %.1f°C. Stored as a new version; v1 (%.1f°C) kept for audit.", r.Version, target.Key, old.TempC, tempC, target.Versions[0].TempC))
	} else {
		s.emit("correction", "good", r.Station, "", fmt.Sprintf("Correction v%d for %s arrived after v%d. Kept in the audit trail, but v%d stays current: the highest version wins, not the latest arrival.", r.Version, target.Key, now.Version, now.Version))
	}
}

// compact seals old partitions into Parquet and merges large delta files.
func (s *Sim) compact() {
	st := s.Store
	cutoff := s.Now - RecentWindow
	sealedNow := map[string]int{}
	for _, rec := range st.Rows {
		p := st.Partitions[rec.Partition]
		if rec.Tier == "recent" && (p.Day+1)*DayTicks <= cutoff {
			rec.Tier = "parquet"
			p.Rows++
			sealedNow[p.ID]++
		}
	}
	ids := make([]string, 0, len(st.Partitions))
	for id := range st.Partitions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := st.Partitions[id]
		if !p.Sealed && (p.Day+1)*DayTicks <= cutoff {
			p.Sealed = true
			s.emitQ("storage", "info", "", "", fmt.Sprintf("Compacted %s into an immutable Parquet file (%d rows) and uploaded it to object storage.", id, sealedNow[id]))
		}
		if p.Delta >= 3 && float64(p.Delta) > 0.10*float64(p.Rows) {
			for _, rec := range st.Rows {
				if rec.Partition == id && rec.Tier == "delta" {
					rec.Tier = "parquet"
				}
			}
			s.emit("storage", "good", "", "", fmt.Sprintf("Delta files for %s reached %d rows (>10%% of %d). Rewrote the partition once, merging them in.", id, p.Delta, p.Rows))
			p.Rows += p.Delta
			p.Delta = 0
			p.Rewrites++
		}
	}
}

// HistRow is one row of a station history query.
type HistRow struct {
	Key        string    `json:"key"`
	Seq        int       `json:"seq"`
	EventTime  int       `json:"eventTime"`
	IngestTime int       `json:"ingestTime"`
	TempC      float64   `json:"tempC"`
	Hum        float64   `json:"hum"`
	Version    int       `json:"version"`
	Versions   []Version `json:"versions"`
	Tier       string    `json:"tier"`
	Partition  string    `json:"partition"`
	Flags      []string  `json:"flags"`
	Dup        bool      `json:"dup"`
	Late       int       `json:"late"` // minutes between event and ingestion
}

// HistoryResult is what the query service returns.
type HistoryResult struct {
	Station   string         `json:"station"`
	OrderedBy string         `json:"orderedBy"`
	Rows      []HistRow      `json:"rows"`
	Total     int            `json:"total"`
	Sources   map[string]int `json:"sources"`
	Misorder  int            `json:"misordered"` // adjacent rows whose event times go backwards
}

// History merges every tier for one station and returns the newest `limit` rows.
func (s *Sim) History(station string, limit int) HistoryResult {
	var rows []*Record
	for _, r := range s.Store.Rows {
		if r.Station == station {
			rows = append(rows, r)
		}
	}
	res := HistoryResult{Station: station, Sources: map[string]int{}}
	if s.Cfg.EventTimeOrder {
		res.OrderedBy = "event time"
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].EventTime != rows[j].EventTime {
				return rows[i].EventTime < rows[j].EventTime
			}
			return rows[i].Seq < rows[j].Seq
		})
	} else {
		res.OrderedBy = "arrival order"
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Order < rows[j].Order })
	}
	res.Total = len(rows)
	for i, r := range rows {
		res.Sources[r.Tier]++
		if i > 0 && r.EventTime < rows[i-1].EventTime {
			res.Misorder++
		}
	}
	start := 0
	if limit > 0 && len(rows) > limit {
		start = len(rows) - limit
	}
	for _, r := range rows[start:] {
		c := r.Current()
		res.Rows = append(res.Rows, HistRow{
			Key: r.Key, Seq: r.Seq, EventTime: r.EventTime, IngestTime: r.IngestTime,
			TempC: round1(c.TempC), Hum: round1(c.Hum), Version: c.Version, Versions: r.Versions,
			Tier: r.Tier, Partition: r.Partition, Flags: r.Flags, Dup: r.Dup, Late: r.IngestTime - r.EventTime,
		})
	}
	return res
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }
