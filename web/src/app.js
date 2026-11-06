(() => {
  "use strict";
  const $ = (id) => document.getElementById(id);
  const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const ROUTINE = new Set(["append", "ack", "commit", "fsync", "upload"]);
  const MODES = {
    design: { ackPolicy: "quorum", dedup: true, eventTimeOrder: true, appendOnlyCorrections: true, acceptLate: true },
    naive: { ackPolicy: "on-receive", dedup: false, eventTimeOrder: false, appendOnlyCorrections: false, acceptLate: false },
  };

  let A = null;           // the Go engine
  let scenarios = [];
  let S = null;           // latest state from the engine
  let mode = "design";
  let selected = "A";
  let playing = false;
  let acc = 0, lastT = 0, lastSlow = 0;
  let logAll = false;
  let events = [];
  let lastCp = null;
  let openVersions = null;
  let stick = true; // keep the history table scrolled to its newest row
  let invAll = false; // show every correctness check, not just the ones that fired
  let tab = "sim";

  // ---------- boot ----------
  async function boot() {
    try {
      const go = new Go();
      let inst;
      try {
        inst = await WebAssembly.instantiateStreaming(fetch("main.wasm"), go.importObject);
      } catch (e) {
        const buf = await (await fetch("main.wasm")).arrayBuffer();
        inst = await WebAssembly.instantiate(buf, go.importObject);
      }
      go.run(inst.instance);
      A = window.ArchiveSim;
      if (!A) throw new Error("engine did not start");
      scenarios = JSON.parse(A.scenarios());
      $("scenario").innerHTML = scenarios.map((s) => `<option value="${s.id}">${esc(s.case)} · ${esc(s.title)}</option>`).join("");
      const h = location.hash.slice(1);
      if (scenarios.some((s) => s.id === h)) $("scenario").value = h;
      wire();
      $("boot").hidden = true;
      $("app").hidden = false;
      load();
      requestAnimationFrame(frame);
      window.dispatchEvent(new Event("archive-ready"));
    } catch (e) {
      $("boot").classList.add("err");
      $("boot").textContent = "The simulation engine couldn't start in this browser (" + e.message + "). Try the deployed site or a recent Chrome, Firefox or Safari.";
    }
  }

  function config() {
    let c;
    if (mode === "custom") {
      c = { ackPolicy: $("t-ack").value, dedup: $("t-dedup").checked, eventTimeOrder: $("t-order").checked,
        appendOnlyCorrections: $("t-corr").checked, acceptLate: $("t-late").checked };
    } else c = { ...MODES[mode] };
    c.uploadEvery = +$("upload").value;
    return c;
  }

  function load() {
    const id = $("scenario").value;
    try { history.replaceState(null, "", "#" + id); } catch (e) { /* ignore */ }
    playing = false;
    events = [];
    lastCp = null;
    openVersions = null;
    $("log").innerHTML = "";
    const sc = scenarios.find((s) => s.id === id);
    selected = sc.focus || "A";
    stick = true;
    $("h-view").value = { corrections: "flagged", timestamps: "flagged", delayed: "late" }[id] || "latest";
    apply(JSON.parse(A.load(id, JSON.stringify(config()), 42)));
    if (sc.length > 0) apply(JSON.parse(A.step(1)));
    slow();
  }

  // ---------- engine calls ----------
  function step(n) { apply(JSON.parse(A.step(n))); }
  function act(a) { apply(JSON.parse(A.act(JSON.stringify(a)))); slow(); }

  function apply(st) {
    if (!st || st.error) return;
    S = st;
    if (st.events && st.events.length) addEvents(st.events);
    if (st.checkpoint) { lastCp = st.checkpoint; lastCp.fresh = true; setPlaying(false); }
    render();
  }

  function setPlaying(p) {
    playing = p;
    $("play").textContent = p ? "Pause" : (lastCp && lastCp.fresh ? "Continue" : "Play");
    if (p && lastCp) lastCp.fresh = false;
    acc = 0;
  }

  function frame(t) {
    const dt = Math.min(250, t - (lastT || t));
    lastT = t;
    if (playing && A) {
      acc += dt * (+$("speed").value) / 1000;
      const n = Math.floor(acc);
      if (n > 0) { acc -= n; step(n); }
      if (t - lastSlow > 700) slow();
    }
    requestAnimationFrame(frame);
  }

  // History and similarity are heavier: refresh them less often.
  function slow() {
    lastSlow = performance.now();
    renderHistory();
    if (tab === "sim") renderSimilar();
  }

  function setTab(t) {
    tab = t;
    for (const k of ["sim", "log", "store"]) {
      $("tab-" + k).setAttribute("aria-selected", String(k === t));
      $("pane-" + k).hidden = k !== t;
    }
    if (t === "sim") renderSimilar();
  }

  // ---------- wiring ----------
  function wire() {
    $("scenario").addEventListener("change", load);
    $("restart").addEventListener("click", load);
    $("upload").addEventListener("change", load);
    for (const m of ["design", "naive", "custom"]) {
      $("mode-" + m).addEventListener("click", () => {
        mode = m;
        for (const k of ["design", "naive", "custom"]) $("mode-" + k).setAttribute("aria-pressed", String(k === m));
        $("custom").hidden = m !== "custom";
        load();
      });
    }
    for (const id of ["t-ack", "t-dedup", "t-order", "t-corr", "t-late"]) $(id).addEventListener("change", load);
    $("play").addEventListener("click", () => setPlaying(!playing));
    $("step1").addEventListener("click", () => { setPlaying(false); step(1); slow(); });
    $("step10").addEventListener("click", () => { setPlaying(false); step(10); slow(); });
    $("tonext").addEventListener("click", () => {
      setPlaying(false);
      const sc = S.scenario;
      const limit = sc.length > 0 ? Math.max(1, sc.length + 30 - S.rel) : 60;
      if (lastCp) lastCp.fresh = false;
      step(limit);
      slow();
    });
    $("arm-append").addEventListener("click", () => act({ type: "crashAfterAppend" }));
    $("arm-ack").addEventListener("click", () => act({ type: "crashAfterAck" }));
    $("nodes").addEventListener("click", (e) => {
      const b = e.target.closest("button[data-act]");
      if (b) act({ type: b.dataset.act, node: b.dataset.node });
    });
    $("stations").addEventListener("click", (e) => {
      const b = e.target.closest("button[data-st]");
      if (!b) return;
      selected = b.dataset.st;
      openVersions = null;
      stick = true;
      render();
      slow();
    });
    $("a-online").addEventListener("click", () => {
      const st = S.stations.find((x) => x.name === selected);
      act({ type: st.online ? "offline" : "online", station: selected });
    });
    $("a-drop").addEventListener("click", () => act({ type: "dropAcks", station: selected, value: 20 }));
    $("a-resend").addEventListener("click", () => act({ type: "resend", station: selected }));
    $("a-jitter").addEventListener("change", (e) => act({ type: "jitter", station: selected, value: +e.target.value }));
    $("a-skew").addEventListener("change", (e) => act({ type: "skew", station: selected, value: +e.target.value }));
    $("a-schema").addEventListener("change", (e) => act({ type: "schema", station: selected, value: +e.target.value }));
    $("a-conv").addEventListener("click", () => act({ type: "converter", value: 3 }));
    $("corr").addEventListener("submit", (e) => {
      e.preventDefault();
      const seq = +$("c-seq").value, val = +$("c-val").value;
      if (seq > 0 && Number.isFinite(val)) act({ type: "correct", station: selected, seq, value: val });
    });
    $("h-body").addEventListener("click", (e) => {
      const tr = e.target.closest("tr[data-seq]");
      if (!tr) return;
      $("c-seq").value = tr.dataset.seq;
      $("c-val").value = tr.dataset.val;
      $("corr-box").open = true;
      openVersions = openVersions === tr.dataset.key ? null : tr.dataset.key;
      renderHistory();
    });
    $("h-view").addEventListener("change", () => { stick = true; renderHistory(); });
    $("h-wrap").addEventListener("scroll", () => {
      const w = $("h-wrap");
      stick = w.scrollTop + w.clientHeight >= w.scrollHeight - 30;
    });
    for (const k of ["sim", "log", "store"]) $("tab-" + k).addEventListener("click", () => setTab(k));
    $("inv").addEventListener("click", (e) => {
      if (e.target.closest("#inv-toggle")) { invAll = !invAll; renderInv(); }
    });
    // Close the settings popover when clicking elsewhere.
    document.addEventListener("click", (e) => {
      const m = document.querySelector(".menu[open]");
      if (m && !m.contains(e.target)) m.open = false;
    });
    $("lf-key").addEventListener("click", () => setLog(false));
    $("lf-all").addEventListener("click", () => setLog(true));
    $("cp").addEventListener("click", (e) => {
      if (e.target.id === "cp-go") setPlaying(true);
    });
    window.addEventListener("hashchange", () => {
      const h = location.hash.slice(1);
      if (h !== $("scenario").value && scenarios.some((s) => s.id === h)) { $("scenario").value = h; load(); }
    });
    window.addEventListener("resize", () => { renderHistory(); if (tab === "sim") renderSimilar(); });
  }

  function setLog(all) {
    logAll = all;
    $("lf-key").setAttribute("aria-pressed", String(!all));
    $("lf-all").setAttribute("aria-pressed", String(all));
    $("log").innerHTML = "";
    const show = events.filter((e) => all || !ROUTINE.has(e.kind)).slice(-300);
    $("log").insertAdjacentHTML("afterbegin", show.reverse().map(evHTML).join(""));
  }

  // ---------- rendering ----------
  const relTime = (t) => "T+" + (t - S.now + S.rel);
  const clockOf = (t) => {
    if (t < 0) return "—";
    const d = Math.floor(t / 1440), m = t % 1440;
    return `D${d + 1} ${String(Math.floor(m / 60)).padStart(2, "0")}:${String(m % 60).padStart(2, "0")}`;
  };

  function render() {
    if (!S) return;
    $("clock").textContent = S.clock;
    $("rel").textContent = `T+${S.rel} min`;
    const L = S.scenario.length;
    $("prog").style.width = L > 0 ? Math.min(100, (S.rel / L) * 100) + "%" : "100%";
    renderScenario();
    renderCheckpoint();
    renderInv();
    renderCluster();
    renderStations();
    renderDetail();
    renderStorage();
  }

  function renderScenario() {
    const sc = S.scenario;
    const html = `<span class="case">${esc(sc.case)}</span>
      <h2 class="scn-title" style="text-transform:none;letter-spacing:0;color:var(--ink)">${esc(sc.title)}</h2>
      <p>${esc(sc.summary)}</p>
      <details class="inline-more"><summary>What to watch</summary><ul class="watch">${sc.watch.map((w) => `<li>${esc(w)}</li>`).join("")}</ul></details>
      ${sc.id === "sandbox" ? `<button class="btn sm" id="chaos-btn">${S.chaos ? "Turn chaos off" : "Turn chaos on"}</button>` : ""}`;
    const el = $("scn");
    if (el.dataset.k !== html) {
      el.innerHTML = html;
      el.dataset.k = html;
      const cb = $("chaos-btn");
      if (cb) cb.addEventListener("click", () => { act({ type: "chaos", value: S.chaos ? 0 : 1 }); setPlaying(true); });
    }
  }

  function renderCheckpoint() {
    const el = $("cp");
    if (!lastCp) {
      el.hidden = S.scenario.length > 0;
      if (!el.hidden) return;
      el.hidden = false;
      el.innerHTML = `<h3>Free play</h3><p>Press Play, then break things: crash machines, cut stations off, lose acks or correct readings. Turn chaos on to let the simulator do it for you, and watch the correctness panel below.</p>`;
      return;
    }
    el.hidden = false;
    const done = S.done ? `<p class="sub">Scenario complete. Switch mode and press Restart to compare.</p>` : "";
    const html = `<span class="cp-n">Pause ${lastCp.index} of ${lastCp.total} · T+${lastCp.at}</span>
      <h3>${esc(lastCp.title)}</h3><p>${esc(lastCp.text)}</p>${done}
      ${S.done || playing ? "" : `<div class="cp-actions"><button class="btn primary sm" id="cp-go">Continue</button></div>`}`;
    if (el.dataset.k !== html) { el.innerHTML = html; el.dataset.k = html; }
  }

  function renderInv() {
    const i = S.inv, s = S.stats;
    const lost = i.lostAcked;
    const checks = [
      ["Duplicate rows stored", i.dupRows, "is-bad"],
      ["Acknowledged, then dropped", i.ackedDropped, "is-bad", "late data rejected after the ack"],
      ["Rows out of order in history", i.misordered, "is-bad"],
      ["Values overwritten", i.overwrites, "is-bad", "original lost"],
      ["Conflicting corrections", i.conflicts, "is-warn"],
      ["Quarantined events", i.quarantined, "is-warn"],
      ["Rows with suspect timestamps", i.flagged, "is-warn"],
      ["Late arrivals placed by event time", i.outOfOrder, "is-good"],
      ["Retries recognised", i.dupsIgnored + i.fastPathDups, "is-good"],
      ["“Busy” replies (backpressure)", s.nackBusy, "is-warn"],
      ["Writes refused, retry later", s.nackNoQuorum, "is-warn", "no leader, or fewer than 2 machines up"],
      ["Leader elections", s.elections, ""],
    ];
    // Only list checks that have fired; the zeros fold into one line.
    const shown = invAll ? checks : checks.filter((c) => c[1]);
    const hidden = checks.length - shown.length;
    const rows = shown.map(([label, v, cls, hint]) =>
      `<dt>${label}${hint ? `<small>${hint}</small>` : ""}</dt><dd class="${v ? cls : ""}">${v}</dd>`).join("");
    const toggle = invAll ? `<button class="all-clear" id="inv-toggle">Hide zero counts ▴</button>`
      : hidden ? `<button class="all-clear" id="inv-toggle">✓ ${hidden} other check${hidden > 1 ? "s" : ""} at zero · show ▾</button>` : "";
    const html = `<div class="panel-head"><h2>Correctness</h2><span class="sub">${S.inv.acked.toLocaleString()} acked</span></div>
      <div class="hero-metric ${lost ? "bad" : ""}"><span class="num">${lost}</span>
        <b>Acknowledged readings lost</b><span>${lost ? "Stations were told 'success' for data that no longer exists." : "Every 'success' sent is still backed by data."}</span></div>
      ${rows ? `<dl class="metrics">${rows}</dl>` : ""}${toggle}`;
    const el = $("inv");
    if (el.dataset.k !== html) { el.innerHTML = html; el.dataset.k = html; }
  }

  function renderCluster() {
    $("term").textContent = `term ${S.term}`;
    const pct = Math.min(100, (S.backlog / 60) * 100);
    $("cmeta").innerHTML = [
      ["leader", S.leader || "none"], ["committed", S.commit], ["processed", S.applied],
      ["uploaded", S.uploaded], ["in flight", S.inNetwork],
    ].map(([k, v]) => `<span>${k} <b>${esc(v)}</b></span>`).join("") +
      `<span class="mini" title="Processing backlog: the leader replies 'busy' above 60">backlog <span class="meter ${S.backlog > 45 ? "hot" : ""}"><i style="width:${pct}%"></i></span><b>${S.backlog}</b></span>`;
    const html = S.nodes.map((n) => {
      const cells = (n.tail || []).map((c) => {
        let cls = "cell " + (!c.durable ? "cache" : c.committed ? "committed" : "disk");
        if (c.uploaded) cls += " uploaded";
        return `<span class="${cls}" title="entry ${c.index}: ${c.n} readings from ${c.station}"><small>${c.index % 1000}</small>${esc(c.station)}${c.n > 1 ? `<sub>${c.n}</sub>` : ""}</span>`;
      }).join("");
      const tape = n.logLen === 0 ? `<div class="tape-empty">${n.up ? "empty log" : "disk destroyed: nothing left on this machine"}</div>` : `<div class="tape">${cells}</div>`;
      // Only offer the actions that make sense for the machine's current state.
      const btn = (act, label, cls = "") => `<button class="btn sm ${cls}" data-act="${act}" data-node="${n.name}">${label}</button>`;
      const actions = n.up ? btn("crash", "Crash", "") + btn("destroy", "Destroy", "danger")
        : (n.logLen > 0 ? btn("restart", "Restart") : "") + btn("replace", "Replace");
      return `<div class="node ${n.up ? "" : "down"}">
        <div class="node-head"><h3>${esc(n.name)}</h3><span class="pill ${n.role}">${n.role}</span>
          <span class="node-stats">${esc(n.zone)} · log ${n.logLen} · disk ${n.durable}${n.unsynced ? ` · <b style="color:var(--warn)">${n.unsynced} in cache</b>` : ""}</span>
          <div class="node-actions">${actions}</div></div>
        ${tape}</div>`;
    }).join("");
    const el = $("nodes");
    if (el.dataset.k !== html) { el.innerHTML = html; el.dataset.k = html; }
  }

  function renderStations() {
    const html = S.stations.map((s) => {
      let dot = "", st = s.climate;
      if (s.stopped) { dot = "silent"; st = "stopped"; }
      else if (!s.online) { dot = "off"; st = "offline"; }
      else if (s.silent) { dot = "silent"; st = "silent"; }
      else if (s.acksLost || s.jitter > 1 || s.skew || s.schema !== 1) { dot = "warn"; }
      return `<button class="stn" data-st="${s.name}" aria-pressed="${s.name === selected}">
        <span class="stn-top"><i class="dot ${dot}"></i><b>${s.name}</b><span class="buf">${s.buffer || ""}</span></span>
        <span>${esc(st)}</span></button>`;
    }).join("");
    const el = $("stations");
    if (el.dataset.k !== html) { el.innerHTML = html; el.dataset.k = html; }
  }

  function renderDetail() {
    const s = S.stations.find((x) => x.name === selected);
    $("d-title").textContent = `Station ${s.name} · ${s.climate}`;
    const bits = [
      s.stopped ? "stopped for good" : s.online ? "online" : "offline",
      `${s.buffer} waiting on station`, `${s.inflight} requests in flight`, `${s.stored} rows stored`,
      `last heard ${clockOf(s.lastSeen)}`,
    ];
    if (s.acksLost) bits.push("acks being lost");
    $("d-sub").textContent = bits.join(" · ");
    $("a-online").textContent = s.online ? "Go offline" : "Come back online";
    $("a-online").disabled = s.stopped;
    if (document.activeElement !== $("a-jitter")) $("a-jitter").value = String(s.jitter > 5 ? 9 : s.jitter > 1 ? 5 : 1);
    if (document.activeElement !== $("a-skew")) $("a-skew").value = ["0", "120", "-90"].includes(String(s.skew)) ? String(s.skew) : "0";
    if (document.activeElement !== $("a-schema")) $("a-schema").value = String(s.schema);
  }

  function renderHistory() {
    if (!A || !S) return;
    const h = JSON.parse(A.history(selected, 0));
    const all = h.rows || [];
    const view = $("h-view").value;
    const pick = view === "flagged" ? all.filter((r) => (r.flags && r.flags.length) || r.dup || r.versions.length > 1)
      : view === "late" ? all.filter((r) => r.late > 30) : all;
    const src = Object.entries(h.sources).map(([k, v]) => `${k} ${v}`).join(", ");
    $("h-meta").innerHTML = `<span>Ordered by <b>${esc(h.orderedBy)}</b></span><span>${h.total} rows</span><span>merged from ${esc(src || "—")}</span>${h.misordered ? `<span style="color:var(--bad)">${h.misordered} rows out of order</span>` : ""}`;
    const rows = pick.slice(-80);
    let prev = -Infinity;
    const html = rows.map((r) => {
      const moved = r.eventTime < prev; prev = Math.max(prev, r.eventTime);
      const flags = (r.flags || []).map((f) => `<span class="tag ${f === "conflict" ? "bad" : f === "corrected" ? "good" : "warn"}">${esc(f)}</span>`).join("") + (r.dup ? `<span class="tag bad">duplicate</span>` : "");
      let tr = `<tr class="click ${r.dup ? "dup" : moved ? "moved" : ""}" data-seq="${r.seq}" data-val="${r.tempC}" data-key="${esc(r.key)}">
        <td class="num">${r.seq}</td><td class="mono">${clockOf(r.eventTime)}</td><td class="mono">${clockOf(r.ingestTime)}</td>
        <td class="num">${r.late > 0 ? r.late + "m" : r.late < 0 ? r.late + "m" : "0"}</td><td class="num">${r.tempC.toFixed(1)}</td>
        <td class="num">v${r.version}${r.versions.length > 1 ? `<small>/${r.versions.length}</small>` : ""}</td>
        <td class="tier-${r.tier}">${r.tier}</td><td>${flags}</td></tr>`;
      if (openVersions === r.key) {
        tr += `<tr class="versions"><td colspan="8">${r.versions.map((v) => `v${v.version}: ${v.tempC.toFixed(1)}°C from ${esc(v.eventKey)} at ${clockOf(v.appliedAt)}${v.conflict ? " <span class='tag bad'>conflict</span>" : ""}`).join("<br>")}</td></tr>`;
      }
      return tr;
    }).join("");
    const wrap = $("h-wrap");
    $("h-body").innerHTML = html;
    if (stick) wrap.scrollTop = wrap.scrollHeight;
    if (!rows.length) $("h-body").innerHTML = `<tr><td colspan="8" class="sub">No rows match this view for station ${selected} yet.</td></tr>`;
    drawHistory(all.slice(-80));
  }

  function tok(name) { return getComputedStyle(document.documentElement).getPropertyValue(name).trim(); }

  function setupCanvas(c) {
    const dpr = window.devicePixelRatio || 1;
    const w = c.clientWidth, h = c.clientHeight;
    c.width = Math.round(w * dpr); c.height = Math.round(h * dpr);
    const g = c.getContext("2d");
    g.setTransform(dpr, 0, 0, dpr, 0, 0);
    g.clearRect(0, 0, w, h);
    g.font = `11px ${tok("--f-mono")}`;
    return [g, w, h];
  }

  function drawHistory(rows) {
    const [g, w, h] = setupCanvas($("h-chart"));
    if (!rows.length) return;
    const pad = { l: 40, r: 10, t: 10, b: 20 };
    const xs = rows.map((r) => r.eventTime), ys = rows.map((r) => r.tempC);
    let x0 = Math.min(...xs), x1 = Math.max(...xs), y0 = Math.min(...ys), y1 = Math.max(...ys);
    if (x1 === x0) x1 = x0 + 1;
    const yp = Math.max(0.5, (y1 - y0) * 0.1); y0 -= yp; y1 += yp;
    const X = (v) => pad.l + ((v - x0) / (x1 - x0)) * (w - pad.l - pad.r);
    const Y = (v) => pad.t + (1 - (v - y0) / (y1 - y0)) * (h - pad.t - pad.b);
    g.strokeStyle = tok("--line"); g.fillStyle = tok("--ink-3"); g.lineWidth = 1;
    for (let k = 0; k <= 3; k++) {
      const v = y0 + ((y1 - y0) * k) / 3, y = Y(v);
      g.beginPath(); g.moveTo(pad.l, y); g.lineTo(w - pad.r, y); g.stroke();
      g.fillText(v.toFixed(1) + "°", 2, y + 4);
    }
    g.fillText(clockOf(x0), pad.l, h - 4);
    const r1 = clockOf(x1); g.fillText(r1, w - pad.r - g.measureText(r1).width, h - 4);
    // Line drawn in stored order: in event-time order it runs left to right;
    // in arrival order it doubles back wherever a reading arrived late.
    g.strokeStyle = tok("--accent"); g.lineWidth = 1.5; g.beginPath();
    rows.forEach((r, i) => (i ? g.lineTo(X(r.eventTime), Y(r.tempC)) : g.moveTo(X(r.eventTime), Y(r.tempC))));
    g.stroke();
    for (const r of rows) {
      const f = r.flags || [];
      let col = null;
      if (r.dup || f.includes("conflict")) col = tok("--bad");
      else if (f.includes("corrected")) col = tok("--good");
      else if (f.length || r.tier === "delta") col = tok("--warn");
      if (!col) continue;
      g.fillStyle = col; g.beginPath(); g.arc(X(r.eventTime), Y(r.tempC), 3.5, 0, Math.PI * 2); g.fill();
    }
  }

  function renderSimilar() {
    if (!A || !S) return;
    const r = JSON.parse(A.similar(selected));
    $("s-target").textContent = selected;
    $("s-sub").textContent = r.dims ? `${r.dims} numbers per station · ${r.ops} multiply-adds` : "";
    const ms = r.matches || [];
    $("matches").innerHTML = r.targetVec ? ms.map((m) => `<div class="match ${m.excluded ? "ex" : ""}" title="${m.excluded ? esc(m.reason) : "distance " + m.distance}">
        <b>${m.station}</b><span>${esc(m.climate)}</span><span class="bar"><i style="width:${m.excluded ? 0 : Math.round(m.score * 100)}%"></i></span>
        <span class="num">${m.excluded ? "skip" : m.score.toFixed(2)}</span></div>`).join("")
      : `<span class="sub">Station ${selected} has under 70% data coverage in the last 2 days, so it isn't compared.</span>`;
    const ex = r.targetVec ? ms.filter((m) => m.excluded).map((m) => `${m.station} skipped: ${m.reason}`) : [];
    $("s-note").textContent = "Average temperature for each hour of the day over the last 2 days." + (ex.length ? " " + ex.join("; ") + "." : "");
    drawProfiles(r, ms);
  }

  function drawProfiles(r, ms) {
    const [g, w, h] = setupCanvas($("s-chart"));
    const P = r.profiles || {};
    const target = P[selected];
    const best = ms.find((m) => !m.excluded);
    const names = Object.keys(P);
    if (!names.length) return;
    let y0 = Infinity, y1 = -Infinity;
    for (const n of names) for (const v of P[n]) { y0 = Math.min(y0, v); y1 = Math.max(y1, v); }
    const pad = { l: 36, r: 46, t: 8, b: 18 };
    const X = (i) => pad.l + (i / 23) * (w - pad.l - pad.r);
    const Y = (v) => pad.t + (1 - (v - y0) / (y1 - y0 || 1)) * (h - pad.t - pad.b);
    g.fillStyle = tok("--ink-3"); g.strokeStyle = tok("--line"); g.lineWidth = 1;
    for (const v of [y0, (y0 + y1) / 2, y1]) { g.beginPath(); g.moveTo(pad.l, Y(v)); g.lineTo(w - pad.r, Y(v)); g.stroke(); g.fillText(Math.round(v) + "°", 2, Y(v) + 4); }
    for (const hr of [0, 6, 12, 18]) g.fillText(String(hr).padStart(2, "0") + "h", X(hr) - 8, h - 4);
    const line = (vals, col, wdt, label) => {
      g.strokeStyle = col; g.lineWidth = wdt; g.beginPath();
      vals.forEach((v, i) => (i ? g.lineTo(X(i), Y(v)) : g.moveTo(X(i), Y(v)))); g.stroke();
      if (label) { g.fillStyle = col; g.fillText(label, w - pad.r + 6, Y(vals[23]) + 4); }
    };
    for (const n of names) if (n !== selected && (!best || n !== best.station)) line(P[n], tok("--hatch"), 1, null);
    if (best && P[best.station]) line(P[best.station], tok("--good"), 2, best.station);
    if (target) line(target, tok("--accent"), 2.5, selected);
  }

  function renderStorage() {
    const parts = S.partitions || [];
    const days = [...new Set(parts.map((p) => p.day))].sort((a, b) => a - b);
    let html = `<span></span>${[0, 1, 2, 3].map((b) => `<span class="h">b${b}</span>`).join("")}`;
    for (const d of days) {
      html += `<span class="d">day ${d + 1}</span>`;
      for (let b = 0; b < 4; b++) {
        const p = parts.find((x) => x.day === d && x.bucket === b);
        if (!p) { html += `<span></span>`; continue; }
        html += `<span class="part ${p.sealed ? "sealed" : ""}" title="${p.sealed ? "sealed Parquet" : "still recent"}${p.rewrites ? ", rewritten " + p.rewrites + "×" : ""}">${p.sealed ? p.rows : "live"}${p.delta ? ` <span class="delta">+${p.delta}</span>` : ""}${p.rewrites ? "↻" : ""}</span>`;
      }
    }
    const el = $("parts");
    if (el.dataset.k !== html) { el.innerHTML = html; el.dataset.k = html; }
    const q = S.quarantine || [];
    $("q-n").textContent = q.length ? `${S.inv.quarantined} held` : "empty";
    $("q-badge").hidden = !S.inv.quarantined;
    $("q-badge").textContent = S.inv.quarantined;
    const qh = q.slice().reverse().slice(0, 8).map((x) => `<li><span class="mono">${esc(x.reading.station)}#${x.reading.seq}</span> · ${esc(x.reason)}</li>`).join("");
    if ($("qlist").dataset.k !== qh) { $("qlist").innerHTML = qh; $("qlist").dataset.k = qh; }
  }

  function evHTML(e) {
    const cls = e.kind === "checkpoint" ? "checkpoint" : e.level;
    return `<div class="ev ${cls}"><time>${relTime(e.t)}</time><div><span class="k">${esc(e.kind)}</span>${esc(e.msg)}</div></div>`;
  }

  function addEvents(list) {
    events.push(...list);
    if (events.length > 3000) events = events.slice(-3000);
    const show = list.filter((e) => logAll || !ROUTINE.has(e.kind));
    if (!show.length) return;
    const log = $("log");
    log.insertAdjacentHTML("afterbegin", show.slice(-200).reverse().map(evHTML).join(""));
    while (log.childElementCount > 400) log.lastElementChild.remove();
  }

  // ---------- guided tour ----------
  // Dims the page, spotlights one area at a time and explains what it shows
  // and what to look out for. Opens on a first visit; "? Guide" reopens it.
  const sw = (c) => `<i style="background:var(${c})"></i>`;
  const TOUR = [
    { title: "Welcome to the archive",
      text: "This page simulates an archive that collects temperature readings from 12 weather stations and keeps them on 3 cheap machines. Things go wrong on purpose: crashes, lost replies, late data, bad clocks, corrections. The question is always the same: does the archive still tell the truth?",
      look: ["Everything is computed live by the Go engine in your browser, so nothing is pre-recorded and every run reacts to what you click."] },
    { sel: "#scenario", title: "1 · Pick a story",
      text: "Each case is a scripted failure with a few built-in pauses that explain what's going on. Start with Case 1. The Sandbox is free play, with a chaos switch that breaks things at random.",
      look: ["Each case has its own 'What to watch' list in the left column."] },
    { sel: "#mode-seg", title: "2 · My design vs Naive",
      text: "My design has every safeguard switched on. Naive switches them all off: it says 'success' the moment data arrives, stores retries twice, sorts by arrival time and overwrites old values. Custom lets you flip each safeguard yourself.",
      look: ["Run a case in My design, then switch to Naive and watch the same events. The scoreboard tells two very different stories."] },
    { sel: ".transport", title: "3 · Driving the clock",
      text: "One tick is one simulated minute. Play runs continuously, +1 and +10 step forward, and Next pause jumps to the next explanation. The bar shows how far through the scenario you are.",
      look: ["The simulation stops itself at every important moment, so you won't miss it. Use +1 min around those moments to see each step."] },
    { sel: "#cp", title: "4 · The pause card",
      text: "Whenever the simulation stops, this card says what's about to happen and why it matters. Read it, press Continue, and watch the parts of the screen it mentions.",
      look: ["The text often tells you which station or counter to watch next."] },
    { sel: "#inv", title: "5 · The scoreboard",
      text: "This is the most important panel. The big number counts readings a station was told were saved but that no longer exist. It should always be 0. Below it, only the checks that have actually fired are listed.",
      look: [
        "<b style='color:var(--bad);display:inline'>Red</b>: the archive broke a promise (duplicates, out-of-order history, overwritten values, acked data dropped).",
        "<b style='color:var(--warn);display:inline'>Amber</b>: the system pushed back (busy, retry later) or set data aside. This is healthy.",
        "<b style='color:var(--good);display:inline'>Green</b>: a safeguard caught something, such as a retry being recognised.",
      ] },
    { sel: "#cluster", title: "6 · The replicated log",
      text: "Every batch of readings is appended to a log copied onto 3 machines. One is the leader and the others follow. Each square is one log entry, labelled with the station it came from.",
      keys: `<span>${sw("--warn-soft")}dashed: only in memory, a crash loses it</span><span>${sw("--panel-2")}on this disk</span><span>${sw("--accent-soft")}on 2 of 3 disks: safe</span><span>${sw("--good")}dot: also in object storage</span>`,
      look: ["Press Crash on the leader: another machine is elected and nothing that was committed disappears.", "Crash two machines: writes are refused (not lost) until a majority is back."] },
    { sel: "#stations", title: "7 · The stations",
      text: "Each tile is a weather station. The number next to its name is how many readings are waiting on the station itself, not yet accepted by the archive. Click a tile to inspect that station below.",
      keys: `<span>${sw("--good")}healthy</span><span>${sw("--warn")}something odd (delay, clock, lost acks)</span><span>${sw("--bad")}offline</span><span>${sw("--ink-3")}silent</span>`,
      look: ["When a station goes offline its number grows. When it comes back, everything it buffered should arrive without gaps or duplicates."] },
    { sel: "#detail .detail-head", title: "8 · Break a station",
      text: "Take the station offline, make the archive's 'success' replies get lost (so the station retries data that was already saved), or resend its last batch. 'More station faults' adds network delay, a wrong clock and a unit change (°C → °F → Kelvin).",
      look: ["After a retry, 'Retries recognised' should go up, not 'Duplicate rows stored'."] },
    { sel: "#h-chart", title: "9 · The station's history",
      text: "This line is the selected station's temperature, drawn in the order the archive stored it. Stored by measurement time, it runs smoothly left to right. Stored by arrival time, it doubles back wherever a reading arrived late.",
      keys: `<span>${sw("--bad")}duplicate / conflict</span><span>${sw("--good")}corrected</span><span>${sw("--warn")}flagged or in a patch file</span>`,
      look: ["A zig-zag that runs backwards means history is out of order. Compare Case 1 in both modes."] },
    { sel: "#h-wrap", title: "10 · Row by row",
      text: "Late is how many minutes after measurement the reading arrived. Ver counts corrections to that reading. Tier shows where the row lives: recent (fresh data), parquet (sealed daily files) or delta (a patch added to an already sealed file).",
      look: ["Red rows are duplicates and amber rows are out of order. In My design you shouldn't see either.", "Click any row to see every version of it and prefill the correction form."] },
    { sel: "#extras", title: "11 · More evidence",
      text: "Similar stations compares each station's daily temperature curve to find look-alikes. Event log narrates what happened (switch to Everything for low-level detail). Storage shows the sealed files and the quarantine.",
      look: ["A badge on Storage means readings were quarantined: kept safely but set aside, such as a new unit format with no converter yet. They're replayed later, not dropped."] },
    { sel: "#tour-btn", title: "You're ready",
      text: "Suggested first run: choose Case 1, press Play and read each pause. Then switch to Naive and run it again. Reopen this guide any time from this button.",
      look: [] },
  ];
  let tourI = -1, tourEls = null;

  function tourStart() {
    setPlaying(false);
    document.querySelectorAll(".menu[open]").forEach((m) => (m.open = false));
    if (!tourEls) {
      const block = document.createElement("div"); block.className = "tour-block";
      const hole = document.createElement("div"); hole.className = "tour-hole";
      const card = document.createElement("div"); card.className = "tour-card";
      card.setAttribute("role", "dialog"); card.setAttribute("aria-modal", "true"); card.setAttribute("aria-label", "Guided tour");
      card.addEventListener("click", (e) => {
        const b = e.target.closest("button[data-t]");
        if (!b) return;
        if (b.dataset.t === "next") tourGo(tourI + 1, 1);
        else if (b.dataset.t === "back") tourGo(tourI - 1, -1);
        else tourEnd();
      });
      tourEls = { block, hole, card };
    }
    document.body.append(tourEls.block, tourEls.hole, tourEls.card);
    tourGo(0, 1);
  }

  function tourEnd() {
    if (!tourEls) return;
    tourEls.block.remove(); tourEls.hole.remove(); tourEls.card.remove();
    tourI = -1;
    try { localStorage.setItem("archive-sim-tour", "1"); } catch (e) { /* storage unavailable */ }
    $("tour-btn").focus();
  }

  const tourTarget = (s) => {
    if (!s.sel) return null;
    const el = document.querySelector(s.sel);
    if (!el || el.closest("[hidden]") || !el.getClientRects().length) return null;
    return el;
  };

  // dir lets us skip steps whose target isn't on screen (e.g. no pause card in Sandbox).
  function tourGo(i, dir) {
    while (i >= 0 && i < TOUR.length && TOUR[i].sel && !tourTarget(TOUR[i])) i += dir;
    if (i < 0) i = 0;
    if (i >= TOUR.length) return tourEnd();
    tourI = i;
    const s = TOUR[i], last = i === TOUR.length - 1;
    const look = s.look && s.look.length
      ? `<div class="tour-look"><b>Look out for</b>${s.look.length > 1 ? `<ul>${s.look.map((l) => `<li>${l}</li>`).join("")}</ul>` : s.look[0]}</div>` : "";
    tourEls.card.innerHTML = `<span class="t-step">Guide · ${i + 1} of ${TOUR.length}</span>
      <h3>${s.title}</h3><p>${s.text}</p>${s.keys ? `<div class="tour-keys">${s.keys}</div>` : ""}${look}
      <div class="tour-nav"><div class="tour-dots">${TOUR.map((_, k) => `<span class="${k === i ? "on" : ""}"></span>`).join("")}</div>
        ${i > 0 ? `<button class="btn sm" data-t="back">Back</button>` : `<button class="btn sm ghost" data-t="skip">Skip</button>`}
        <button class="btn sm primary" data-t="next">${last ? "Finish" : i === 0 ? "Show me around" : "Next"}</button></div>`;
    const el = tourTarget(s);
    if (el) el.scrollIntoView({ block: "center" });
    tourPlace();
    tourEls.card.querySelector('[data-t="next"]').focus({ preventScroll: true });
  }

  function tourPlace() {
    if (tourI < 0) return;
    const { hole, card } = tourEls;
    const el = tourTarget(TOUR[tourI]);
    const vw = window.innerWidth, vh = window.innerHeight, M = 16, P = 6;
    if (!el) {
      hole.classList.add("center");
      card.style.left = Math.max(M, (vw - card.offsetWidth) / 2) + "px";
      card.style.top = Math.max(M, (vh - card.offsetHeight) / 2) + "px";
      return;
    }
    hole.classList.remove("center");
    const r = el.getBoundingClientRect();
    const top = Math.max(r.top - P, 0), bot = Math.min(r.bottom + P, vh);
    Object.assign(hole.style, { top: top + "px", left: (r.left - P) + "px", width: (r.width + P * 2) + "px", height: Math.max(0, bot - top) + "px" });
    const cw = card.offsetWidth, ch = card.offsetHeight;
    let x, y;
    if (vh - bot >= ch + M) { y = bot + 10; x = r.left; }                  // below
    else if (top >= ch + M) { y = top - ch - 10; x = r.left; }             // above
    else if (vw - r.right >= cw + M * 2) { x = r.right + 14; y = r.top; }  // right
    else if (r.left >= cw + M * 2) { x = r.left - cw - 14; y = r.top; }    // left
    else { x = vw - cw - M; y = vh - ch - M; }                             // overlap, bottom corner
    card.style.left = Math.min(Math.max(M, x), vw - cw - M) + "px";
    card.style.top = Math.min(Math.max(M, y), vh - ch - M) + "px";
  }

  $("tour-btn").addEventListener("click", tourStart);
  window.addEventListener("resize", tourPlace);
  window.addEventListener("scroll", tourPlace, true);
  document.addEventListener("keydown", (e) => {
    if (tourI < 0) return;
    if (e.key === "Escape") tourEnd();
    else if (e.key === "ArrowRight") tourGo(tourI + 1, 1);
    else if (e.key === "ArrowLeft" && tourI > 0) tourGo(tourI - 1, -1);
  });
  // First visit: open the guide once the engine is up.
  window.addEventListener("archive-ready", () => {
    let seen = false;
    try { seen = localStorage.getItem("archive-sim-tour") === "1"; } catch (e) { /* storage unavailable */ }
    if (!seen) setTimeout(tourStart, 400);
  }, { once: true });

  boot();
})();
