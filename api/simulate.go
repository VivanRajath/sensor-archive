// Package handler is a Vercel Go function: it runs one scenario headlessly on
// the server and returns the outcome as JSON.
//
//	GET /api/simulate?scenario=crash&mode=naive&seed=42&upload=30
package handler

import (
	"encoding/json"
	"net/http"
	"strconv"

	"archivesim/sim"
)

type result struct {
	Scenario   string          `json:"scenario"`
	Mode       string          `json:"mode"`
	Seed       int64           `json:"seed"`
	Ticks      int             `json:"ticks"`
	Invariants sim.Invariants  `json:"invariants"`
	Stats      sim.Stats       `json:"stats"`
	Pauses     []string        `json:"pauses"`
	KeyEvents  []sim.Event     `json:"keyEvents"`
	Scenarios  []*sim.Scenario `json:"scenarios,omitempty"`
}

var routine = map[string]bool{"append": true, "ack": true, "commit": true, "fsync": true, "upload": true, "retry": true}

// Handler runs the scenario to its end (plus 60 ticks) and reports what happened.
func Handler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := q.Get("scenario")
	if id == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result{Scenarios: sim.Scenarios})
		return
	}
	mode := q.Get("mode")
	cfg := sim.DesignConfig()
	if mode == "naive" {
		cfg = sim.NaiveConfig()
	} else {
		mode = "design"
	}
	if u, err := strconv.Atoi(q.Get("upload")); err == nil && u > 0 && u <= 240 {
		cfg.UploadEvery = u
	}
	seed, err := strconv.ParseInt(q.Get("seed"), 10, 64)
	if err != nil {
		seed = 42
	}
	s, err := sim.New(cfg, id, seed)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	n := 0
	for _, sc := range sim.Scenarios {
		if sc.ID == id {
			n = sc.Length
		}
	}
	if n == 0 {
		n = 600
		s.Do(sim.Action{Type: "chaos", Value: 1})
	}
	res := result{Scenario: id, Mode: mode, Seed: seed}
	for s.Now-s.T0 < n+60 {
		s.Step(n + 60)
		if s.Checkpoint != nil {
			res.Pauses = append(res.Pauses, s.Checkpoint.Title)
		}
	}
	res.Ticks = s.Now - s.T0
	res.Invariants = s.Invariants()
	res.Stats = s.Stats
	for _, e := range s.Events {
		if !routine[e.Kind] {
			res.KeyEvents = append(res.KeyEvents, e)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	json.NewEncoder(w).Encode(res)
}
