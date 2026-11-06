//go:build js && wasm

// Command wasm exposes the simulation engine to the browser.
package main

import (
	"encoding/json"
	"syscall/js"

	"archivesim/sim"
)

var cur *sim.Sim

func toJS(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return js.ValueOf(`{"error":"` + err.Error() + `"}`)
	}
	return js.ValueOf(string(b))
}

func main() {
	api := js.Global().Get("Object").New()

	// scenarios() → JSON list of guided scenarios
	api.Set("scenarios", js.FuncOf(func(this js.Value, a []js.Value) any {
		return toJS(sim.Scenarios)
	}))

	// load(id, configJSON, seed) → state JSON
	api.Set("load", js.FuncOf(func(this js.Value, a []js.Value) any {
		var cfg sim.Config
		if err := json.Unmarshal([]byte(a[1].String()), &cfg); err != nil {
			return toJS(map[string]string{"error": err.Error()})
		}
		s, err := sim.New(cfg, a[0].String(), int64(a[2].Int()))
		if err != nil {
			return toJS(map[string]string{"error": err.Error()})
		}
		cur = s
		return toJS(cur.Snapshot())
	}))

	// step(n) → state JSON (stops early at a checkpoint)
	api.Set("step", js.FuncOf(func(this js.Value, a []js.Value) any {
		if cur == nil {
			return js.Null()
		}
		cur.Step(a[0].Int())
		return toJS(cur.Snapshot())
	}))

	// act(actionJSON) → state JSON
	api.Set("act", js.FuncOf(func(this js.Value, a []js.Value) any {
		if cur == nil {
			return js.Null()
		}
		var ac sim.Action
		if err := json.Unmarshal([]byte(a[0].String()), &ac); err == nil {
			cur.Do(ac)
		}
		return toJS(cur.Snapshot())
	}))

	// history(station, limit) → history JSON
	api.Set("history", js.FuncOf(func(this js.Value, a []js.Value) any {
		if cur == nil {
			return js.Null()
		}
		return toJS(cur.History(a[0].String(), a[1].Int()))
	}))

	// similar(station) → similarity JSON
	api.Set("similar", js.FuncOf(func(this js.Value, a []js.Value) any {
		if cur == nil {
			return js.Null()
		}
		return toJS(cur.Similar(a[0].String()))
	}))

	js.Global().Set("ArchiveSim", api)
	js.Global().Call("dispatchEvent", js.Global().Get("Event").New("archivesim-ready"))
	select {}
}
