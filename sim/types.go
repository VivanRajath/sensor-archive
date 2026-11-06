// Package sim is a deterministic, step-by-step simulation of a fault-tolerant
// sensor archive: stations, an unreliable network, a 3-node replicated log,
// background processing into tiered storage, and similarity search.
//
// One tick = one simulated minute. Every behaviour that the design document
// argues for can be switched to a "naive" alternative so users can watch what
// breaks.
package sim

import "fmt"

// AckPolicy decides when the cluster tells a station "your data is safe".
type AckPolicy string

const (
	AckOnReceive  AckPolicy = "on-receive"  // naive: ack as soon as the request arrives
	AckLocalFsync AckPolicy = "local-fsync" // naive: ack after the leader's own disk flush
	AckQuorum     AckPolicy = "quorum"      // design: ack after fsync on a majority (2 of 3)
)

// Config holds the switches that separate the design from naive alternatives.
type Config struct {
	AckPolicy             AckPolicy `json:"ackPolicy"`
	Dedup                 bool      `json:"dedup"`
	EventTimeOrder        bool      `json:"eventTimeOrder"`
	AppendOnlyCorrections bool      `json:"appendOnlyCorrections"`
	AcceptLate            bool      `json:"acceptLate"`
	UploadEvery           int       `json:"uploadEvery"` // ticks between log-segment uploads to object storage
}

// DesignConfig is the architecture from the write-up.
func DesignConfig() Config {
	return Config{AckPolicy: AckQuorum, Dedup: true, EventTimeOrder: true,
		AppendOnlyCorrections: true, AcceptLate: true, UploadEvery: 30}
}

// NaiveConfig turns every safeguard off.
func NaiveConfig() Config {
	return Config{AckPolicy: AckOnReceive, Dedup: false, EventTimeOrder: false,
		AppendOnlyCorrections: false, AcceptLate: false, UploadEvery: 30}
}

// Reading is one event sent by a station: a measurement or a correction.
type Reading struct {
	Station   string             `json:"station"`
	Seq       int                `json:"seq"`
	EventTime int                `json:"eventTime"` // -1 = missing timestamp
	Schema    int                `json:"schema"`
	Fields    map[string]float64 `json:"fields"`
	Kind      string             `json:"kind"` // "reading" | "correction"
	RefSeq    int                `json:"refSeq,omitempty"`
	Version   int                `json:"version,omitempty"`
}

// Key is the event ID: station + persistent sequence number.
func (r Reading) Key() string { return fmt.Sprintf("%s#%d", r.Station, r.Seq) }

// Batch is one request from a station.
type Batch struct {
	ID       int       `json:"id"`
	Station  string    `json:"station"`
	Readings []Reading `json:"readings"`
	SentAt   int       `json:"sentAt"`
}

func (b Batch) seqs() []int {
	out := make([]int, len(b.Readings))
	for i, r := range b.Readings {
		out[i] = r.Seq
	}
	return out
}

// Entry is one record in the replicated write-ahead log.
type Entry struct {
	Index      int   `json:"index"` // 1-based
	Term       int   `json:"term"`
	Batch      Batch `json:"batch"`
	AppendedAt int   `json:"appendedAt"`
}

// Node is one cheap cloud machine running the log.
type Node struct {
	ID      int     `json:"id"`
	Name    string  `json:"name"`
	Zone    string  `json:"zone"`
	Up      bool    `json:"up"`
	Log     []Entry `json:"-"`
	Durable int     `json:"durable"` // entries fsynced to disk; the rest live only in the page cache
}

func (n *Node) lastTerm() int {
	if len(n.Log) == 0 {
		return 0
	}
	return n.Log[len(n.Log)-1].Term
}

// Message travels over the simulated network.
type Message struct {
	DeliverAt int
	Kind      string // "batch" (station→cluster), "ack", "nack" (cluster→station)
	Station   string
	Batch     Batch
	BatchID   int
	Seqs      []int
	Reason    string
}

type inflight struct {
	seqs   []int
	sentAt int
}

// Station is a sensor station with a local buffer of unacknowledged readings.
type Station struct {
	Name     string  `json:"name"`
	Climate  string  `json:"climate"`
	base     float64 // mean °C
	amp      float64 // diurnal swing °C
	phase    float64 // hours offset of the daily peak
	noise    float64
	humBase  float64
	Interval int `json:"interval"`

	NextSeq      int  `json:"nextSeq"`
	Online       bool `json:"online"`
	Stopped      bool `json:"stopped"` // gone for good
	Schema       int  `json:"schema"`
	ClockSkew    int  `json:"clockSkew"`
	Jitter       int  `json:"jitter"`
	BatchMax     int  `json:"batchMax"`
	Window       int  `json:"window"`
	AckDropUntil int  `json:"-"`
	MissingTS    int  `json:"-"` // next N readings have no timestamp

	buffer       []Reading
	inflight     map[int]inflight
	nextGen      int
	backoffUntil int
	lastSent     *Batch
	acked        map[int]bool // seqs this station has been told are safe

	LastSeen int  `json:"lastSeen"` // last tick the archive received anything
	Silent   bool `json:"silent"`
}

// Event is one line of narration shown to the user.
type Event struct {
	ID      int    `json:"id"`
	T       int    `json:"t"`
	Kind    string `json:"kind"`
	Level   string `json:"level"` // info | good | warn | bad
	Station string `json:"station,omitempty"`
	Node    string `json:"node,omitempty"`
	Msg     string `json:"msg"`
}
