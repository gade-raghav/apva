// Copyright 2026 Raghav Gade
// SPDX-License-Identifier: Apache-2.0
"use strict";

const $ = (id) => document.getElementById(id);
const css = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();
const STATUS_VAR = { ok: "--ok", over: "--over", under: "--under", hold: "--hold", "idle-gpu": "--idle", unknown: "--unknown" };
const ACTION_VAR = { ok: "--ok", downsize: "--over", upsize: "--under", "set-request": "--under", hold: "--hold",
  idle: "--idle", share: "--idle", "no-data": "--unknown" };

let data = null;
let selected = null;

const cores = (c) => (c >= 1 ? c.toFixed(2) : Math.round(c * 1000) + "m");
const mib = (b) => (b >= 1 << 30 ? (b / (1 << 30)).toFixed(1) + "Gi" : Math.round(b / (1 << 20)) + "Mi");
const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const OUTCOME_VAR = { applied: "--ok", "dry-run": "--hold", waiting: "--hold", delegated: "--hold", skipped: "--muted", failed: "--under" };
const ago = (t) => {
  const s = Math.max(0, (Date.now() - new Date(t).getTime()) / 1000);
  return s < 60 ? Math.round(s) + "s ago" : s < 3600 ? Math.round(s / 60) + "m ago" : Math.round(s / 3600) + "h ago";
};
const outcome = (o) => `<span class="badge" style="color:var(${OUTCOME_VAR[o] || "--muted"})">${esc(o)}</span>`;
const badge = (a) => `<span class="badge" style="color:var(${ACTION_VAR[a] || "--muted"})">${esc(a)}</span>`;

async function load() {
  try {
    const r = await fetch("api/v1/result");
    const body = await r.json();
    if (!r.ok) throw new Error(body.error || r.statusText);
    data = body;
    $("err").hidden = true;
    render();
  } catch (e) {
    $("err").textContent = "Could not load analysis: " + e.message;
    $("err").hidden = false;
  }
}

function render() {
  const s = data.summary;
  $("meta").textContent = `window ${data.window.replace(/(\d)h0m0s$/, "$1h").replace(/(\d)m0s$/, "$1m")} · updated ${new Date(data.generatedAt).toLocaleTimeString()}`;
  const tiles = [
    [s.workloads, "workloads analysed"],
    [s.downsize, "over-provisioned"],
    [s.upsize, "under-provisioned"],
    [s.held, "held (traffic rising)"],
    [cores(s.cpuSavingsCores), "CPU reclaimable"],
    [mib(s.memSavingsBytes), "memory reclaimable"],
    [s.gpuSavings, "idle GPUs"],
  ];
  const ar = data.autoResize || { enabled: false, events: [] };
  if (ar.enabled) tiles.push([s.autoResized || 0, ar.dryRun ? "would auto-resize" : "auto-resized"]);
  $("mode").hidden = false;
  $("mode").textContent = ar.enabled ? (ar.dryRun ? "auto-resize: dry-run" : "auto-resize: on") : "recommend-only";
  $("mode").style.color = `var(${ar.enabled ? (ar.dryRun ? "--hold" : "--ok") : "--muted"})`;
  $("mode").title = ar.enabled ? `acts on ${ar.minConfidence}+ confidence, cooldown ${ar.cooldown}` : "start with --auto-resize to apply recommendations automatically";
  $("arcol").hidden = !ar.enabled;
  $("activity-panel").hidden = !ar.enabled;
  const latestEv = {};
  for (const ev of ar.events || []) {
    const id = ev.workload.namespace + "/" + ev.workload.name;
    if (!latestEv[id]) latestEv[id] = ev;
  }
  const ng = data.nodeGroups || { enabled: false, events: [] };
  const feed = [
    ...(ar.events || []).map((ev) => ({ ...ev, who: ev.workload.namespace + "/" + ev.workload.name })),
    ...(ng.events || []).map((ev) => ({ ...ev, who: "node group " + ev.group })),
  ].sort((a, b) => new Date(b.time) - new Date(a.time));
  $("activity-title").textContent = ng.enabled
    ? `Auto-resize activity · ${ng.provider === "aws" ? "EKS" : ng.provider} ${ng.cluster}: ${(ng.groups || []).join(", ")}`
    : "Auto-resize activity";
  $("activity").innerHTML = feed.length
    ? feed.slice(0, 30).map((ev) => `<li><time datetime="${esc(ev.time)}">${ago(ev.time)}</time>
        ${outcome(ev.outcome)} <strong>${esc(ev.who)}</strong>
        <span class="muted">${esc(ev.reason)}</span></li>`).join("")
    : `<li class="muted">No resizes yet. APVA acts once a recommendation reaches ${esc(ar.minConfidence || "high")} confidence.</li>`;
  $("tiles").innerHTML = tiles.map(([v, l]) => `<div class="tile"><div class="v">${esc(v)}</div><div class="l">${esc(l)}</div></div>`).join("");
  $("warnings").innerHTML = (data.warnings || []).map((w) => `<li>${esc(w)}</li>`).join("");

  $("rows").innerHTML = data.recommendations.map((r) => {
    const id = r.workload.namespace + "/" + r.workload.name;
    const gpu = r.gpu ? `${r.gpu.requested} · p95 ${r.gpu.p95UtilPct.toFixed(0)}% ${badge(r.gpu.action)}` : "—";
    return `<tr data-id="${esc(id)}" class="${id === selected ? "sel" : ""}">
      <td>${esc(id)}</td><td>${r.replicas}</td>
      <td>${cores(r.cpu.current)} → ${cores(r.cpu.recommended)}</td><td>${badge(r.cpu.action)}</td>
      <td>${mib(r.memory.current)} → ${mib(r.memory.recommended)}</td><td>${badge(r.memory.action)}</td>
      <td>${gpu}</td><td>${esc(r.confidence)}</td>
      ${ar.enabled ? `<td>${latestEv[id] ? `${outcome(latestEv[id].outcome)} <span class="muted">${ago(latestEv[id].time)}</span>` : `<span class="muted">—</span>`}</td>` : ""}</tr>`;
  }).join("");
  for (const tr of $("rows").querySelectorAll("tr")) tr.onclick = () => select(tr.dataset.id);

  drawGraph(data.graph);
  if (selected) showDetail(selected);
}

function select(id) {
  selected = id;
  render();
}

function showDetail(id) {
  const r = data.recommendations.find((x) => x.workload.namespace + "/" + x.workload.name === id);
  const d = $("detail");
  if (!r) {
    d.innerHTML = `<h2>${esc(id)}</h2><p class="muted">No resource metrics for this workload (external caller or not scraped).</p>`;
    return;
  }
  const callers = (r.upstream || []).map((u) =>
    `<li>${esc(u.from.namespace + "/" + u.from.name)} — ${u.ratePerSec.toFixed(1)}/s, trend ${u.trend >= 1 ? "+" : ""}${((u.trend - 1) * 100).toFixed(0)}%</li>`).join("");
  d.innerHTML = `<h2>${esc(id)}</h2>
    <p class="muted">${r.replicas} pod(s) · confidence ${esc(r.confidence)}</p>
    <ul>
      <li>CPU: p95 ${cores(r.cpu.p95)}, request ${cores(r.cpu.current)} → ${cores(r.cpu.recommended)} ${badge(r.cpu.action)}</li>
      <li>Memory: p95 ${mib(r.memory.p95)}, request ${mib(r.memory.current)} → ${mib(r.memory.recommended)} ${badge(r.memory.action)}</li>
      ${r.gpu ? `<li>GPU: ${r.gpu.requested} requested, avg ${r.gpu.avgUtilPct.toFixed(1)}%, p95 ${r.gpu.p95UtilPct.toFixed(1)}% ${badge(r.gpu.action)}</li>` : ""}
    </ul>
    ${r.reasons && r.reasons.length ? `<strong>Why</strong><ul>${r.reasons.map((x) => `<li>${esc(x)}</li>`).join("")}</ul>` : ""}
    ${autoDetail(id)}
    ${callers ? `<strong>Called by</strong><ul>${callers}</ul>` : `<p class="muted">No observed callers.</p>`}`;
}

function autoDetail(id) {
  const ar = data.autoResize;
  if (!ar || !ar.enabled) return "";
  const evs = (ar.events || []).filter((ev) => ev.workload.namespace + "/" + ev.workload.name === id).slice(0, 5);
  if (!evs.length) return "";
  return `<strong>Auto-resize</strong><ul>${evs.map((ev) =>
    `<li>${outcome(ev.outcome)} ${esc(ev.reason)} <span class="muted">${ago(ev.time)}</span></li>`).join("")}</ul>`;
}

// Small force-directed layout; deterministic start so the picture is stable between refreshes.
function drawGraph(g) {
  const svg = $("graph");
  const W = svg.clientWidth || 700, H = svg.clientHeight || 440;
  const nodes = g.nodes.map((n, i) => {
    const a = (2 * Math.PI * i) / Math.max(1, g.nodes.length);
    return { ...n, x: W / 2 + Math.cos(a) * W * 0.3, y: H / 2 + Math.sin(a) * H * 0.3, vx: 0, vy: 0 };
  });
  const byId = Object.fromEntries(nodes.map((n) => [n.id, n]));
  const links = (g.links || []).filter((l) => byId[l.source] && byId[l.target]);
  for (let it = 0; it < 300; it++) {
    for (const a of nodes) for (const b of nodes) {
      if (a === b) continue;
      const dx = a.x - b.x, dy = a.y - b.y, d2 = Math.max(dx * dx + dy * dy, 25);
      const f = 5200 / d2;
      a.vx += (dx / Math.sqrt(d2)) * f; a.vy += (dy / Math.sqrt(d2)) * f;
    }
    for (const l of links) {
      const a = byId[l.source], b = byId[l.target];
      const dx = b.x - a.x, dy = b.y - a.y, d = Math.sqrt(dx * dx + dy * dy) || 1;
      const f = (d - 150) * 0.02;
      a.vx += (dx / d) * f; a.vy += (dy / d) * f; b.vx -= (dx / d) * f; b.vy -= (dy / d) * f;
    }
    for (const n of nodes) {
      n.vx += (W / 2 - n.x) * 0.003; n.vy += (H / 2 - n.y) * 0.003;
      n.x = Math.min(W - 60, Math.max(60, n.x + n.vx * 0.5));
      n.y = Math.min(H - 24, Math.max(24, n.y + n.vy * 0.5));
      n.vx *= 0.6; n.vy *= 0.6;
    }
  }
  const maxRate = Math.max(1, ...links.map((l) => l.ratePerSec));
  const muted = css("--muted"), line = css("--line"), text = css("--text"), hold = css("--hold");
  let out = `<defs><marker id="arr" viewBox="0 0 10 10" refX="10" refY="5" markerUnits="userSpaceOnUse" markerWidth="9" markerHeight="9" orient="auto-start-reverse">
    <path d="M0,0 L10,5 L0,10 z" fill="${muted}"/></marker></defs>`;
  for (const l of links) {
    const a = byId[l.source], b = byId[l.target];
    const dx = b.x - a.x, dy = b.y - a.y, d = Math.sqrt(dx * dx + dy * dy) || 1, r = 13;
    const w = 1 + 4 * (l.ratePerSec / maxRate);
    const rising = l.trend >= 1.25;
    out += `<line x1="${a.x + (dx / d) * r}" y1="${a.y + (dy / d) * r}" x2="${b.x - (dx / d) * (r + 3)}" y2="${b.y - (dy / d) * (r + 3)}"
      stroke="${rising ? hold : line}" stroke-width="${w}" marker-end="url(#arr)"><title>${esc(l.source)} → ${esc(l.target)}: ${l.ratePerSec.toFixed(1)}/s</title></line>`;
  }
  for (const n of nodes) {
    const color = css(STATUS_VAR[n.status] || "--unknown");
    const sel = n.id === selected;
    out += `<g data-id="${esc(n.id)}" style="cursor:pointer">
      <circle cx="${n.x}" cy="${n.y}" r="${sel ? 14 : 11}" fill="${color}" stroke="${sel ? text : "none"}" stroke-width="2"/>
      ${n.hasGpu ? `<text x="${n.x}" y="${n.y + 4}" text-anchor="middle" font-size="9" font-weight="700" fill="#111">GPU</text>` : ""}
      <text x="${n.x}" y="${n.y + 26}" text-anchor="middle" font-size="11" fill="${text}">${esc(n.id.split("/").pop())}</text>
      <title>${esc(n.id)} (${esc(n.status)})</title></g>`;
  }
  svg.innerHTML = out;
  for (const el of svg.querySelectorAll("g[data-id]")) el.onclick = () => select(el.dataset.id);
}

window.addEventListener("resize", () => data && drawGraph(data.graph));
load();
setInterval(load, 10000);
