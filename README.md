# Sensor Archive Simulator

An interactive simulation of the fault-tolerant sensor archive from my design write-up. The engine is written in Go and runs in the browser as WebAssembly. A Go serverless function on Vercel runs the same engine headlessly.

Every edge case from the write-up is a guided scenario that pauses at key moments and explains what to look at. Each one can be re-run in **Naive** mode, with the safeguards switched off, to see exactly what breaks.

| Scenario | What happens | Naive mode shows |
| --- | --- | --- |
| Case 1 · Out-of-order data | Station A's readings overtake each other on a jittery network | History in arrival order |
| Case 2 · Duplicate data | Acks to B are lost, B retries, including after a leader crash | Duplicate rows stored |
| Case 3 · Delayed data | C was offline 2 days, E and F 6 hours; all reconnect at once | Late readings dropped after being acked |
| Case 4 · Historical corrections | Corrections, conflicting versions, out-of-order versions, unknown references | Values overwritten, last arrival wins |
| Case 5 · Crash after accepting | Leader crashes before fsync, then right after an ack with its disk destroyed | Acknowledged readings lost |
| Losing the majority | Two of three machines go down, then the last one loses its disk | Acknowledged readings lost |
| The one way to lose data | Both copies lost before the next upload to object storage | (the design's honest limit; try upload every 5 vs 30) |
| Bad clocks | Clock 2 h fast, missing timestamps, clock jumping back | |
| Changing formats | Fahrenheit firmware, an unknown schema quarantined then replayed | |
| Similar stations | 31-number feature vectors, Euclidean distance; a silent station is excluded | |
| Sandbox | Free play, plus a chaos mode that keeps breaking things | |

## How the model works

- **One tick = one simulated minute.** The world starts with 3 days of history from 12 stations (6 climate pairs).
- **Stations** buffer readings until acknowledged, send bounded batches (12 readings, 2 requests in flight), retry after 8 minutes of silence, and back off on "busy" or "no quorum".
- **Cluster:** 3 nodes in 3 zones. The leader appends to its log (page cache), fsyncs once per tick (group commit), and followers copy and fsync. An entry is committed when it's on disk on 2 of 3 machines; only then is the station acknowledged. Raft-style elections pick the most up-to-date log.
- **Crashes** drop anything not yet fsynced. "Destroy machine" also wipes the disk. Replacements bootstrap from object storage.
- **Processing** applies committed entries into a store with three tiers: recent (local), Parquet partitions (day × station bucket) and delta files for late data, merged into the partition once they pass 10%.
- **Correctness checks** run continuously: acknowledged data that no longer exists anywhere, duplicate rows, acked-then-dropped readings, misordered history, overwritten values.

## Project layout

```
sim/            the engine (pure Go, deterministic, tested)
wasm/           browser bridge (syscall/js)
api/            Vercel Go function: GET /api/simulate?scenario=crash&mode=naive
web/src/        page markup, styles and UI script
cmd/simcheck/   runs every scenario in both modes and prints the outcome
dist/           built site (index.html + main.wasm), what Vercel serves
build.sh        tests, compiles to WebAssembly, assembles dist/
```

## Run locally

Requires Go 1.22+ and Python 3.

```bash
./build.sh                       # tests + build into dist/
cd dist && python3 -m http.server 8080
# open http://localhost:8080/#crash
go run ./cmd/simcheck            # every scenario, design vs naive, in the terminal
```

## Deploy to Vercel

`dist/` is committed already built, so Vercel only serves files; it doesn't need to compile WebAssembly.

**From GitHub:** push this folder to a repository, then in Vercel choose *Add New → Project*, import the repository and deploy. `vercel.json` sets `dist` as the output folder and the WebAssembly content type. The Go function in `api/` is detected and built automatically.

**From the command line:**

```bash
npm i -g vercel
vercel          # first time: link or create the project
vercel --prod
```

After changing any Go or web source, run `./build.sh` again and redeploy.

Links can open a specific scenario: `https://<your-site>/#delayed`.
