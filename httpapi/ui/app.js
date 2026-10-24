// Ops UI for sg-property-engine: jobs, run history, table preview.
// Plain JS, no build step; served from the binary at /ui/. Every call goes
// to the same JSON API anyone can curl, via paths relative to /ui/ so the
// page keeps working behind a reverse proxy that mounts sgpe under a prefix.
"use strict";

// ── helpers ──────────────────────────────────────────────

// h builds an element. Children that are strings become text nodes, so
// values from the database are never parsed as HTML.
function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else if (k === "style") el.style.cssText = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  el.append(...nodes(children));
  return el;
}

// nodes flattens nested child lists and drops null/false, so views can
// write `cond && h(...)` and arrays of row groups freely.
function nodes(children) {
  return children.flat(Infinity)
    .filter((c) => c != null && c !== false)
    .map((c) => (c instanceof Node ? c : String(c)));
}

// fill replaces el's children, with the same rules as h().
const fill = (el, ...children) => el.replaceChildren(...nodes(children));

async function api(path, opts) {
  const res = await fetch(path, opts);
  let body = null;
  try { body = await res.json(); } catch { /* non-JSON error page */ }
  if (!res.ok) {
    const err = new Error((body && body.error) || `${res.status} ${res.statusText}`);
    err.status = res.status;
    err.code = body && body.code;
    throw err;
  }
  return body;
}

const fmtInt = (n) => (n == null ? "—" : Number(n).toLocaleString("en-SG"));

function fmtTime(s) {
  if (!s) return "—";
  const d = new Date(s);
  if (Number.isNaN(d.getTime())) return String(s);
  return d.toLocaleString("en-SG", { dateStyle: "medium", timeStyle: "short" });
}

function fmtAgo(s) {
  if (!s) return "";
  const sec = Math.round((Date.now() - new Date(s).getTime()) / 1000);
  if (Number.isNaN(sec) || sec < 0) return "";
  if (sec < 60) return "just now";
  if (sec < 3600) return `${Math.floor(sec / 60)} min ago`;
  if (sec < 86400) return `${Math.floor(sec / 3600)} h ago`;
  return `${Math.floor(sec / 86400)} d ago`;
}

function fmtDuration(start, end) {
  if (!start) return "—";
  const s = new Date(start).getTime();
  const e = end ? new Date(end).getTime() : Date.now();
  if (Number.isNaN(s) || Number.isNaN(e) || e < s) return "—";
  const sec = Math.round((e - s) / 1000);
  if (sec < 60) return `${sec}s`;
  if (sec < 3600) return `${Math.floor(sec / 60)}m ${sec % 60}s`;
  return `${Math.floor(sec / 3600)}h ${Math.floor((sec % 3600) / 60)}m`;
}

function fmtBytes(n) {
  if (n == null) return "—";
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let v = n / 1024, i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(1)} ${units[i]}`;
}

function fmtCell(v) {
  if (v == null) return "NULL";
  if (typeof v === "object") return JSON.stringify(v);
  return String(v);
}

const pill = (status) => h("span", { class: `pill ${status || ""}` }, status || "never run");

function toast(msg, tone) {
  const t = h("div", { class: `toast ${tone || ""}` }, msg);
  document.getElementById("toasts").append(t);
  setTimeout(() => t.remove(), tone === "error" ? 9000 : 5000);
}

function pageHead(title, sub, ...actions) {
  return h("div", { class: "page-head" },
    h("div", null,
      h("div", { class: "kicker" }, "Data engine"),
      h("h1", null, title),
      sub && h("div", { class: "sub" }, sub)),
    actions.length ? h("div", { class: "actions" }, actions) : null);
}

const banner = (msg) => h("div", { class: "banner" }, msg);

// ── health ───────────────────────────────────────────────

async function pollHealth() {
  const el = document.getElementById("health");
  try {
    await api("../healthz");
    el.className = "health ok";
    el.textContent = "db ok";
  } catch (e) {
    el.className = "health down";
    el.textContent = "db down";
    el.title = e.message;
  }
}

// ── jobs ─────────────────────────────────────────────────

// Poll quickly only while something is running, so an idle tab left open
// does not fill the request log.
const POLL_BUSY_MS = 2000;
const POLL_IDLE_MS = 10000;
// A Backfill click arms the button; a second click within this window runs it.
const CONFIRM_MS = 3000;

function jobsView(root) {
  let timer = null;
  let stopped = false;
  let armed = null;          // source whose Backfill button awaits a second click
  let pending = new Set();   // sources with a trigger request in flight
  const watching = new Set(); // sources this tab triggered; toast when they finish
  let last = { runs: {}, known: [] };
  let err = null;

  const body = h("div");
  root.append(
    pageHead("Jobs",
      "One row per ingest source. Incremental is safe to re-run; backfill re-ingests the source's full window.",
      h("button", { class: "btn", onclick: () => refresh() }, "Refresh")),
    body,
    h("p", { class: "note" },
      "Schedules come from the ", h("code", null, "cron:"), " section of the config; ",
      "an empty schedule leaves a source manual-only. A run that finds another one holding the ",
      "source's lock is recorded as ", h("code", null, "skipped"), "."));

  async function trigger(source, mode) {
    pending.add(source);
    render();
    try {
      const q = new URLSearchParams({ source, mode });
      await api(`../sync/run?${q}`, { method: "POST" });
      watching.add(source);
      toast(`Started ${mode} for ${source}`, "success");
    } catch (e) {
      toast(e.status === 409 ? `${source} is already running` : `Couldn't start ${source}: ${e.message}`, "error");
    } finally {
      pending.delete(source);
      refresh();
    }
  }

  function onBackfill(source) {
    if (armed !== source) {
      armed = source;
      render();
      setTimeout(() => { if (armed === source) { armed = null; render(); } }, CONFIRM_MS);
      return;
    }
    armed = null;
    trigger(source, "backfill");
  }

  function render() {
    const rows = last.known.map((src) => {
      const r = last.runs[src];
      const running = r && r.status === "running";
      const busy = running || pending.has(src);
      const stage = r && r.stage;
      return [
        h("tr", null,
          h("td", { class: "mono strong" }, src),
          h("td", null, pill(r && r.status)),
          h("td", null,
            h("div", { class: "stage", title: stage || "" }, stage || (r ? r.mode : "—")),
            running && h("div", { class: "bar-track" },
              h("div", { class: "bar-fill", style: `width:${Math.max(0, Math.min(100, r.progress || 0))}%` }))),
          h("td", { class: "num hide-sm" }, r ? fmtInt(r.rows_upserted) : "—"),
          h("td", { class: "dim right hide-sm", title: r ? fmtTime(r.finished_at || r.started_at) : "" },
            r ? (running ? `started ${fmtAgo(r.started_at)}` : fmtAgo(r.finished_at || r.started_at)) : "—"),
          h("td", null, h("div", { class: "btn-row" },
            h("button", { class: "btn", disabled: busy, title: "Incremental run", onclick: () => trigger(src, "incremental") }, "Incremental"),
            h("button", {
              class: armed === src ? "btn warn" : "btn", disabled: busy,
              title: "Re-ingest the full window", onclick: () => onBackfill(src),
            }, armed === src ? "Confirm" : "Backfill")))),
        r && r.status === "error" && r.error
          ? h("tr", { class: "err-row" }, h("td", { colspan: 6 }, h("pre", { class: "errtext" }, r.error)))
          : null,
      ];
    });
    fill(body, 
      err ? banner(err) : null,
      h("div", { class: "card" },
        h("table", { class: "grid" },
          h("thead", null, h("tr", null,
            h("th", null, "Source"), h("th", null, "Status"), h("th", null, "Stage"),
            h("th", { class: "right hide-sm" }, "Rows upserted"), h("th", { class: "right hide-sm" }, "Last run"),
            h("th", { class: "right" }, "Run"))),
          h("tbody", null, rows.length ? rows
            : h("tr", null, h("td", { colspan: 6, class: "empty" }, err ? "No data." : "Loading…"))))));
  }

  async function refresh() {
    clearTimeout(timer);
    let busy = false;
    try {
      const data = await api("../sync/status");
      const runs = {};
      for (const r of data.sources || []) runs[r.source] = r;
      const known = new Set(data.known || []);
      Object.keys(runs).forEach((s) => known.add(s));
      last = { runs, known: Array.from(known).sort() };
      err = null;
      for (const src of Array.from(watching)) {
        const r = runs[src];
        if (!r || r.status === "running") continue;
        watching.delete(src);
        if (r.status === "success") toast(`${src} finished · ${fmtInt(r.rows_upserted)} rows upserted`, "success");
        else if (r.status === "skipped") toast(`${src} skipped: another run held the lock`);
        else toast(`${src} ${r.status}${r.error ? `: ${r.error}` : ""}`, "error");
      }
      busy = Object.values(runs).some((r) => r.status === "running") || watching.size > 0;
    } catch (e) {
      err = `Couldn't load status: ${e.message}`;
    }
    if (stopped) return;
    render();
    // A hidden tab stops polling; visibilitychange calls wake() on return.
    if (!document.hidden) timer = setTimeout(refresh, busy ? POLL_BUSY_MS : POLL_IDLE_MS);
  }

  render();
  refresh();
  return {
    stop() { stopped = true; clearTimeout(timer); },
    wake() { refresh(); },
  };
}

// ── runs ─────────────────────────────────────────────────

const RUNS_PAGE = 50;

function runsView(root, params) {
  let source = params.get("source") || "";
  let offset = 0;
  let sources = [];
  let stopped = false;

  const select = h("select", {
    class: "btn", "aria-label": "Filter by source",
    onchange: (e) => {
      source = e.target.value;
      offset = 0;
      history.replaceState(null, "", source ? `#/runs?source=${encodeURIComponent(source)}` : "#/runs");
      load();
    },
  });
  const body = h("div");
  root.append(
    pageHead("Run history", "Every run the scheduler has recorded, newest first. Error rows show the message underneath.", select),
    body);

  function fillSelect() {
    fill(select, 
      h("option", { value: "" }, "All sources"),
      ...sources.map((s) => h("option", { value: s, selected: s === source }, s)));
  }

  async function load() {
    fill(body, h("div", { class: "card" }, h("div", { class: "empty" }, "Loading…")));
    try {
      const q = new URLSearchParams({ limit: RUNS_PAGE, offset });
      if (source) q.set("source", source);
      const data = await api(`../sync/runs?${q}`);
      if (!stopped) render(data);
    } catch (e) {
      if (!stopped) fill(body, banner(`Couldn't load runs: ${e.message}`));
    }
  }

  function render(data) {
    const runs = data.rows || [];
    const rows = runs.map((r) => [
      h("tr", null,
        h("td", { class: "mono dim" }, `#${r.id}`),
        h("td", { class: "mono strong" }, r.source),
        h("td", { class: "dim" }, r.mode),
        h("td", null, pill(r.status)),
        h("td", { class: "dim" }, fmtTime(r.started_at)),
        h("td", { class: "num" }, fmtDuration(r.started_at, r.finished_at)),
        h("td", { class: "num hide-sm" }, fmtInt(r.rows_in)),
        h("td", { class: "num" }, fmtInt(r.rows_upserted))),
      r.error ? h("tr", { class: "err-row" }, h("td", { colspan: 8 }, h("pre", { class: "errtext" }, r.error))) : null,
    ]);
    const total = data.total || 0;
    fill(body, 
      h("div", { class: "card" },
        h("table", { class: "grid" },
          h("thead", null, h("tr", null,
            h("th", null, "Run"), h("th", null, "Source"), h("th", null, "Mode"), h("th", null, "Status"),
            h("th", null, "Started"), h("th", { class: "right" }, "Took"),
            h("th", { class: "right hide-sm" }, "Rows in"), h("th", { class: "right" }, "Upserted"))),
          h("tbody", null, rows.length ? rows
            : h("tr", null, h("td", { colspan: 8, class: "empty" }, "No runs yet."))))),
      total > RUNS_PAGE ? h("div", { class: "pager" },
        `${fmtInt(offset + 1)}–${fmtInt(Math.min(offset + runs.length, total))} of ${fmtInt(total)}`,
        h("button", { class: "btn", disabled: offset === 0, onclick: () => { offset = Math.max(0, offset - RUNS_PAGE); load(); } }, "Newer"),
        h("button", { class: "btn", disabled: offset + RUNS_PAGE >= total, onclick: () => { offset += RUNS_PAGE; load(); } }, "Older"))
        : null);
  }

  // The filter lists every source the scheduler knows, not just the ones on
  // the current page, so picking one never hides the others.
  api("../sync/status").then((d) => {
    const all = new Set(d.known || []);
    (d.sources || []).forEach((r) => all.add(r.source));
    sources = Array.from(all).sort();
    if (!stopped) fillSelect();
  }).catch(() => { /* the run list still works without the filter */ });

  fillSelect();
  load();
  return { stop() { stopped = true; }, wake() {} };
}

// ── tables ───────────────────────────────────────────────

const PREVIEW_PAGE = 100;

function tablesView(root) {
  let stopped = false;
  const body = h("div");
  root.append(
    pageHead("Tables", "Row count, size on disk (with indexes) and last write for each table. Preview shows raw rows."),
    body);

  async function load() {
    try {
      const data = await api("../db/tables");
      if (stopped) return;
      const rows = (data.tables || []).map((t) => h("tr", null,
        h("td", { class: "mono strong" }, t.table),
        h("td", { class: "num" }, fmtInt(t.row_count)),
        h("td", { class: "num hide-sm" }, fmtBytes(t.size_bytes)),
        h("td", { class: "dim right hide-sm", title: fmtTime(t.last_update) }, t.last_update ? fmtAgo(t.last_update) : "—"),
        h("td", { class: "right" }, h("button", { class: "btn", disabled: !t.row_count, onclick: () => openPreview(t.table) }, "Preview"))));
      fill(body, h("div", { class: "card" },
        h("table", { class: "grid" },
          h("thead", null, h("tr", null,
            h("th", null, "Table"), h("th", { class: "right" }, "Rows"), h("th", { class: "right hide-sm" }, "Size"),
            h("th", { class: "right hide-sm" }, "Last write"), h("th", null, ""))),
          h("tbody", null, rows))));
    } catch (e) {
      if (!stopped) fill(body, banner(`Couldn't load tables: ${e.message}`));
    }
  }

  fill(body, h("div", { class: "card" }, h("div", { class: "empty" }, "Loading…")));
  load();
  return { stop() { stopped = true; closePreview(); }, wake() { load(); } };
}

let closePreview = () => {};

function openPreview(table) {
  closePreview();
  let offset = 0;
  const content = h("div", { class: "body" }, h("div", { class: "empty" }, "Loading…"));
  const foot = h("footer");
  const close = h("button", { class: "btn", onclick: () => closePreview() }, "Close");
  const scrim = h("div", { class: "scrim", onclick: (e) => { if (e.target === scrim) closePreview(); } },
    h("div", { class: "drawer", role: "dialog", "aria-modal": "true", "aria-label": `Preview of ${table}` },
      h("header", null,
        h("div", null, h("div", { class: "kicker" }, "Preview"), h("div", { class: "title" }, table)),
        close),
      content, foot));
  const onKey = (e) => { if (e.key === "Escape") closePreview(); };
  document.addEventListener("keydown", onKey);
  document.body.append(scrim);
  close.focus();
  closePreview = () => {
    scrim.remove();
    document.removeEventListener("keydown", onKey);
    closePreview = () => {};
  };

  async function load() {
    try {
      const q = new URLSearchParams({ limit: PREVIEW_PAGE, offset });
      const p = await api(`../db/tables/${encodeURIComponent(table)}/rows?${q}`);
      const cols = p.columns || [];
      fill(content, p.rows.length
        ? h("table", { class: "grid" },
          h("thead", null, h("tr", null, cols.map((c) => h("th", null, c)))),
          h("tbody", null, p.rows.map((r) => h("tr", null, cols.map((c) => {
            const text = fmtCell(r[c]);
            return h("td", { class: r[c] == null ? "null" : null, title: text.length > 40 ? text.slice(0, 2000) : null }, text);
          })))))
        : h("div", { class: "empty" }, "Empty."));
      fill(foot, 
        h("span", null, p.total ? `Rows ${fmtInt(offset + 1)}–${fmtInt(offset + p.rows.length)} of ${fmtInt(p.total)}` : ""),
        h("div", { class: "btn-row" },
          h("button", { class: "btn", disabled: offset === 0, onclick: () => { offset = Math.max(0, offset - PREVIEW_PAGE); load(); } }, "Previous"),
          h("button", { class: "btn", disabled: offset + PREVIEW_PAGE >= p.total, onclick: () => { offset += PREVIEW_PAGE; load(); } }, "Next")));
    } catch (e) {
      fill(content, banner(e.message));
    }
  }
  load();
}

// ── router ───────────────────────────────────────────────

const views = { jobs: jobsView, runs: runsView, tables: tablesView };
let current = null;

function route() {
  const [path, query] = (location.hash.replace(/^#\/?/, "") || "jobs").split("?");
  const name = views[path] ? path : "jobs";
  if (current) current.stop();
  const root = document.getElementById("view");
  fill(root);
  current = views[name](root, new URLSearchParams(query || ""));
  document.querySelectorAll("nav a").forEach((a) => {
    if (a.dataset.view === name) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  });
  document.title = `${name[0].toUpperCase()}${name.slice(1)} · sgpe ops`;
}

window.addEventListener("hashchange", route);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden && current) current.wake();
});
route();
pollHealth();
setInterval(pollHealth, 30000);
