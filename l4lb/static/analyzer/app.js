"use strict";

const resultColors = {
  HIT: "#55cf6c",
  COLLAPSED: "#f0cb3e",
  MISS: "#ef6a62",
  SWR: "#ad79d2",
  SIE: "#2bb8c5",
  UNKNOWN: "#318ce7",
};

const timeGridWidthPx = 120;
const timeScaleValuesMs = [60000, 30000, 15000, 10000, 5000, 1000, 500, 100, 50, 10, 5, 1, 0.5, 0.1, 0.01, 0.001, 0.0001];
const throughputChartHeightPx = 150;
const positionGridWidthPx = 120;
const positionScaleValuesBytes = [64 * 1048576, 32 * 1048576, 16 * 1048576, 8 * 1048576, 4 * 1048576, 1048576, 512 * 1024, 256 * 1024, 128 * 1024, 64 * 1024, 16 * 1024];
const traceLabelColumnPx = 112;
const traceRowHeightPx = 31;
const traceBarHeightPx = 22;

const state = {
  aggregate: demoAggregate(),
  samples: [],
  selectedPop: "ALL",
  selectedUri: "/video/big.mp4",
  selectedRequestKey: "",
  overview: "uri",
  timeScaleMs: 1000,
  timeScaleIndex: 5,
  timeScrollLeft: 0,
  timeRestoreAnchorMs: null,
  timeZoomAnchorMs: null,
  timeZoomAnchorOffsetPx: null,
  timeZoomAnchorRatio: 0.5,
  timeViewMode: "simple",
  positionScaleBytes: 16 * 1048576,
  positionScaleIndex: 2,
  positionScrollLeft: 0,
  positionRestoreAnchorBytes: null,
  positionZoomAnchorBytes: null,
  positionZoomAnchorOffsetPx: null,
  positionZoomAnchorRatio: 0.5,
  traceYScroll: {},
  filter: "",
  sortBy: "requests",
  autoRefresh: true,
  refreshIntervalMs: 1000,
  lastStatus: "demo data",
};

const $ = (id) => document.getElementById(id);
let syncingTraceYScroll = false;
let refreshTimer = null;
let deferredRenderTimer = null;
let chartScrollIdleTimer = null;
let renderPendingAfterChartScroll = false;
let chartScrollActiveUntil = 0;
let loadInFlight = false;
let nextCanvasChartId = 1;
const canvasCharts = new Map();
const selectedBytesCache = new WeakMap();
let lastFlattenAggregate = null;
let lastFlattenResult = null;

function parseCacheKey(cacheKey) {
  const parts = String(cacheKey || "").split("\u0000");
  const method = parts[0] || "GET";
  const uri = parts[2] || parts[1] || cacheKey || "/";
  const rawChunk = parts.length > 3 ? Number(parts[3]) : NaN;
  return { method, uri, chunkStart: Number.isFinite(rawChunk) ? rawChunk : null };
}

function flatten(aggregate) {
  const requests = [];
  const origins = [];
  for (const pop of aggregate.pops || []) {
    const keys = (pop.snapshot && pop.snapshot.keys) || {};
    for (const [cacheKey, traces] of Object.entries(keys)) {
      const parsed = parseCacheKey(cacheKey);
      for (const trace of traces.requests || []) {
        requests.push({ ...trace, ...parsed, cacheKey, popId: pop.id, popURL: pop.url, kind: "request" });
      }
      for (const trace of traces.origins || []) {
        origins.push({ ...trace, ...parsed, cacheKey, popId: pop.id, popURL: pop.url, kind: "origin" });
      }
    }
  }
  return { requests, origins };
}

function flattenCached(aggregate) {
  if (!aggregate || typeof aggregate !== "object") return flatten(aggregate);
  if (aggregate === lastFlattenAggregate && lastFlattenResult) {
    return lastFlattenResult;
  }
  lastFlattenAggregate = aggregate;
  lastFlattenResult = flatten(aggregate);
  return lastFlattenResult;
}

function groupByUri(requests, origins) {
  const rows = new Map();
  const ensure = (uri) => {
    if (!rows.has(uri)) rows.set(uri, { uri, pops: new Set(), requests: [], origins: [] });
    return rows.get(uri);
  };
  for (const request of requests) {
    const row = ensure(request.uri);
    row.requests.push(request);
    row.pops.add(request.popId);
  }
  for (const origin of origins) {
    const row = ensure(origin.uri);
    row.origins.push(origin);
    row.pops.add(origin.popId);
  }
  return [...rows.values()].map((row) => ({ ...row, pops: [...row.pops] }));
}

function sumBytes(traces) {
  return traces.reduce((sum, trace) => sum + Math.max(0, Number(trace.producedBytes || 0)), 0);
}

function requestIdCount(traces) {
  return new Set(traces.map(requestKey)).size;
}

function completionMs(trace) {
  if (!trace.startTime || !trace.endTime) return null;
  const ms = parseTimestampMs(trace.endTime) - parseTimestampMs(trace.startTime);
  return Number.isFinite(ms) ? Math.max(0, ms) : null;
}

function formatBytes(bytes) {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let value = bytes;
  let idx = 0;
  while (value >= 1024 && idx < units.length - 1) {
    value /= 1024;
    idx += 1;
  }
  return `${value.toFixed(idx === 0 ? 0 : 1)} ${units[idx]}`;
}

function formatRate(bytesPerSec) {
  if (!Number.isFinite(bytesPerSec) || bytesPerSec <= 0) return "0 bps";
  const units = ["bps", "Kbps", "Mbps", "Gbps"];
  let value = bytesPerSec * 8;
  let idx = 0;
  while (value >= 1000 && idx < units.length - 1) {
    value /= 1000;
    idx += 1;
  }
  return `${value.toFixed(idx === 0 ? 0 : 2)} ${units[idx]}`;
}

function formatMs(start, end) {
  if (!start || !end) return "-";
  const delta = parseTimestampMs(end) - parseTimestampMs(start);
  if (!Number.isFinite(delta)) return "-";
  return formatRelativeMs(Math.max(0, delta));
}

function parseTimestampMs(value) {
  if (!value) return NaN;
  const text = String(value);
  const match = text.match(/^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)?$/);
  if (!match) return Date.parse(text);
  const base = Date.parse(`${match[1]}${match[3] || "Z"}`);
  if (!Number.isFinite(base)) return NaN;
  if (!match[2]) return base;
  return base + Number(`0.${match[2]}`) * 1000;
}

function traceKey(trace) {
  return `${trace.kind}:${trace.popId}:${trace.requestId}:${trace.cacheKey}`;
}

function visibleRequests(row) {
  return (row ? row.requests : []).filter((trace) => state.selectedPop === "ALL" || trace.popId === state.selectedPop);
}

function visibleOrigins(row) {
  return (row ? row.origins : []).filter((trace) => state.selectedPop === "ALL" || trace.popId === state.selectedPop);
}

function render() {
  const flat = flattenCached(state.aggregate);
  const popIndex = indexByPop(flat);
  let rows = groupByUri(flat.requests, flat.origins).filter((row) => row.uri.includes(state.filter));
  rows = sortRows(rows);
  if (rows.length && !rows.some((row) => row.uri === state.selectedUri)) {
    state.selectedUri = rows[0].uri;
  }
  const selected = rows.find((row) => row.uri === state.selectedUri) || rows[0] || null;
  const reqs = visibleRequests(selected);
  const origins = visibleOrigins(selected);
  renderTop();
  renderSidebar(flat);
  renderMetrics(flat, popIndex);
  renderOverview(rows, flat, popIndex);
  renderCharts(selected, reqs, origins);
}

function indexByPop(flat) {
  const byPop = new Map();
  const ensure = (popId) => {
    if (!byPop.has(popId)) byPop.set(popId, { requests: [], origins: [] });
    return byPop.get(popId);
  };
  for (const request of flat.requests) {
    ensure(request.popId).requests.push(request);
  }
  for (const origin of flat.origins) {
    ensure(origin.popId).origins.push(origin);
  }
  return byPop;
}

function renderWhenChartScrollIdle() {
  if (Date.now() >= chartScrollActiveUntil) {
    render();
    return;
  }
  renderPendingAfterChartScroll = true;
  scheduleDeferredRender();
}

function markChartScrollActive() {
  chartScrollActiveUntil = Date.now() + 320;
  scheduleChartScrollIdleWork();
  if (renderPendingAfterChartScroll) {
    scheduleDeferredRender();
  }
}

function scheduleChartScrollIdleWork() {
  if (chartScrollIdleTimer !== null) {
    window.clearTimeout(chartScrollIdleTimer);
  }
  const delay = Math.max(0, chartScrollActiveUntil - Date.now());
  chartScrollIdleTimer = window.setTimeout(() => {
    chartScrollIdleTimer = null;
    if (Date.now() < chartScrollActiveUntil) {
      scheduleChartScrollIdleWork();
      return;
    }
    refreshIdleChartScrollViews();
  }, delay);
}

function refreshIdleChartScrollViews() {
  const positionScroller = document.querySelector(".position-scroll");
  if (positionScroller) {
    refreshPositionTicks(positionScroller);
    requestCanvasDrawInScroller(positionScroller);
  }
  const timeScroller = document.querySelector(".time-scroll");
  if (timeScroller) {
    refreshTimeTicks(timeScroller);
    requestCanvasDrawInScroller(timeScroller);
  }
}

function scheduleDeferredRender() {
  if (deferredRenderTimer !== null) {
    window.clearTimeout(deferredRenderTimer);
  }
  const delay = Math.max(0, chartScrollActiveUntil - Date.now());
  deferredRenderTimer = window.setTimeout(() => {
    deferredRenderTimer = null;
    if (Date.now() < chartScrollActiveUntil) {
      scheduleDeferredRender();
      return;
    }
    if (!renderPendingAfterChartScroll) return;
    renderPendingAfterChartScroll = false;
    render();
  }, delay);
}

function sortRows(rows) {
  return [...rows].sort((a, b) => {
    if (state.sortBy === "origin") return b.origins.length - a.origins.length;
    if (state.sortBy === "bytes") return sumBytes(b.requests) - sumBytes(a.requests);
    if (state.sortBy === "hit") return hitRatio(b.requests) - hitRatio(a.requests);
    return b.requests.length - a.requests.length;
  });
}

function hitRatio(requests) {
  return requests.length ? (requests.filter((trace) => trace.result === "HIT").length / requests.length) * 100 : 0;
}

function resultCountMap(requests) {
  const counts = new Map();
  for (const trace of requests) {
    const result = trace.result || "UNKNOWN";
    counts.set(result, (counts.get(result) || 0) + 1);
  }
  return counts;
}

function renderTop() {
  $("sessionTime").textContent = `Captured: ${state.aggregate.capturedAt ? new Date(state.aggregate.capturedAt).toLocaleString() : "-"}`;
  const status = analysisStatus(state.aggregate);
  $("statusPill").textContent = state.lastStatus || status.label;
  $("statusPill").classList.toggle("warn", Boolean(state.lastStatus) || status.warn);
  document.querySelectorAll("[data-overview]").forEach((el) => {
    el.classList.toggle("active", el.dataset.overview === state.overview);
  });
}

function analysisStatus(aggregate) {
  const pops = aggregate.pops || [];
  const snapshots = pops.filter((pop) => pop.snapshot);
  if (!pops.length) return { label: "NO POPS", warn: true };
  if (pops.some((pop) => pop.error)) return { label: "ERROR", warn: true };
  if (!snapshots.length) return { label: "NO SNAPSHOT", warn: true };

  const enabled = snapshots.filter((pop) => pop.snapshot && pop.snapshot.enabled).length;
  if (enabled === snapshots.length) return { label: "RECORDING", warn: false };
  if (enabled === 0) return { label: "STOPPED", warn: true };
  return { label: "PARTIAL", warn: true };
}

function renderSidebar(flat) {
  const pops = state.aggregate.pops || [];
  const bytes = sampleBytes(state.aggregate);
  $("popButtons").innerHTML = [
    `<button class="${state.selectedPop === "ALL" ? "active" : ""}" data-pop="ALL">All PoPs</button>`,
    ...pops.map((pop) => `<button class="${state.selectedPop === pop.id ? "active" : ""}" data-pop="${escapeHtml(pop.id)}">${escapeHtml(pop.id)}</button>`),
  ].join("");
  $("sessionInfo").innerHTML = [
    ["PoPs", String(pops.length)],
    ["Requests", flat.requests.length.toLocaleString()],
    ["Origin Fetch", flat.origins.length.toLocaleString()],
    ["Origin Data", formatBytes(bytes.origin)],
    ["Client Data", formatBytes(bytes.client)],
  ].map(([k, v]) => `<dt>${k}</dt><dd>${v}</dd>`).join("");
}

function renderMetrics(flat, popIndex) {
  const totalReqs = flat.requests.length;
  const totalOrigins = flat.origins.length;
  const bytes = sampleBytes(state.aggregate);
  const clientBytes = bytes.client;
  const originBytes = bytes.origin;
  const recentCounts = state.samples.slice(-12).map((sample) => sampleTraceCounts(sample));
  const recentReqs = recentCounts.map((counts) => counts.requests);
  const recentOrigins = recentCounts.map((counts) => counts.origins);
  const throughput = throughputSamples(state.samples);
  const latestThroughput = throughput[throughput.length - 1] || { origin: 0, client: 0, intervalMs: 0 };
  const counts = resultCountMap(flat.requests);
  const resultCounts = ["HIT", "COLLAPSED", "MISS", "SWR", "SIE"].map((name) => ({
    name,
    value: counts.get(name) || 0,
  }));
  $("metrics").innerHTML = [
    metricCard("REQUESTS", totalReqs.toLocaleString(), `${hitRatio(flat.requests).toFixed(1)}% hit`, recentReqs),
    metricCard("ORIGIN FETCH", totalOrigins.toLocaleString(), formatBytes(originBytes), recentOrigins),
    metricCard("ORIGIN THROUGHPUT", formatRate(latestThroughput.origin), throughputDetail(latestThroughput), throughput.map((sample) => sample.origin)),
    metricCard("CLIENT THROUGHPUT", formatRate(latestThroughput.client), throughputDetail(latestThroughput), throughput.map((sample) => sample.client)),
    donutCard(resultCounts),
    popTable(popIndex),
  ].join("");
}

function throughputSamples(samples) {
  const values = [];
  const recent = samples.slice(-13);
  for (let i = 1; i < recent.length; i++) {
    const prev = sampleBytes(recent[i - 1]);
    const curr = sampleBytes(recent[i]);
    const intervalMs = parseTimestampMs(recent[i].capturedAt || "") - parseTimestampMs(recent[i - 1].capturedAt || "");
    if (!Number.isFinite(intervalMs) || intervalMs <= 0) {
      values.push({ origin: 0, client: 0, intervalMs: 0 });
      continue;
    }
    values.push({
      origin: Math.max(0, curr.origin - prev.origin) / (intervalMs / 1000),
      client: Math.max(0, curr.client - prev.client) / (intervalMs / 1000),
      intervalMs,
    });
  }
  return values;
}

function sampleTraceCounts(sample, popId = "") {
  let requests = 0;
  let origins = 0;
  for (const pop of sample && sample.pops || []) {
    if (popId && pop.id !== popId) continue;
    const keys = (pop.snapshot && pop.snapshot.keys) || {};
    for (const traces of Object.values(keys)) {
      requests += (traces.requests || []).length;
      origins += (traces.origins || []).length;
    }
  }
  return { requests, origins };
}

function sampleBytes(sample) {
  return (sample && sample.pops || []).reduce((sum, pop) => {
    const bytes = popSampleBytes(sample, pop.id);
    sum.client += bytes.client;
    sum.origin += bytes.origin;
    return sum;
  }, { origin: 0, client: 0 });
}

function throughputDetail(sample) {
  if (!sample.intervalMs) return "waiting for next sample";
  return `delta over ${(sample.intervalMs / 1000).toFixed(2)}s`;
}

function metricCard(title, value, detail, values) {
  return `<section class="metric"><div class="metric-title">${title}</div><div class="metric-value">${value}</div><div class="metric-detail">${detail}</div>${spark(values)}</section>`;
}

function spark(values) {
  const safe = values.length ? values : [0, 1, 0.4, 0.8, 0.2];
  const max = Math.max(...safe, 1);
  const points = safe.map((value, idx) => {
    const x = (idx / Math.max(1, safe.length - 1)) * 100;
    const y = 30 - (value / max) * 24;
    return `${x},${y}`;
  }).join(" ");
  return `<svg class="spark" viewBox="0 0 100 32" preserveAspectRatio="none"><path class="line" d="M${points.replaceAll(" ", " L")}"></path></svg>`;
}

function donutCard(items) {
  const total = items.reduce((sum, item) => sum + item.value, 0) || 1;
  let cursor = 0;
  const stops = items.map((item) => {
    const start = cursor;
    cursor += item.value / total * 360;
    return `${resultColors[item.name]} ${start}deg ${cursor}deg`;
  }).join(", ");
  return `<section class="panel donut-panel"><div class="metric-title">CACHE RESULT</div><div class="donut" style="background: conic-gradient(${stops})"></div><div class="legend-list">${items.map((item) => `<span><i style="background:${resultColors[item.name]}"></i>${item.name} ${item.value}</span>`).join("")}</div></section>`;
}

function popTable(popIndex) {
  const rows = (state.aggregate.pops || []).map((pop) => {
    const indexed = popIndex.get(pop.id) || { requests: [] };
    const bytes = popSampleBytes(state.aggregate, pop.id);
    return `<div class="pop-row"><span>${escapeHtml(pop.id)}</span><b>${formatBytes(bytes.origin)}</b><b>${formatBytes(bytes.client)}</b><b>${hitRatio(indexed.requests).toFixed(1)}%</b></div>`;
  }).join("");
  return `<section class="panel pop-table"><div class="metric-title">STATUS (by PoP)</div><div class="pop-row"><span>PoP</span><span>Origin Data</span><span>Client Data</span><span>Hit</span></div>${rows}</section>`;
}

function renderOverview(rows, flat, popIndex) {
  if (state.overview === "pop") {
    renderPopOverview(popIndex);
    return;
  }
  renderUriOverview(rows);
}

function renderUriOverview(rows) {
  $("overviewHead").className = "overview-head uri-overview-grid";
  $("overviewHead").innerHTML = "<span>URI</span><span>PoPs</span><span>Requests</span><span>Origin Fetch</span><span>Origin Data</span><span>Client Data</span><span>Hit Ratio</span><span>Avg Completion</span>";
  $("overviewRows").innerHTML = rows.map((row) => {
    const avgValues = row.requests.map(completionMs).filter((value) => value !== null);
    const avg = avgValues.length ? `${(avgValues.reduce((a, b) => a + b, 0) / avgValues.length).toFixed(0)} ms` : "-";
    const selected = row.uri === state.selectedUri ? " selected" : "";
    return `<button class="overview-row uri-overview-grid${selected}" data-uri="${escapeAttr(row.uri)}">
      <span>${escapeHtml(row.uri)}</span>
      <span>${row.pops.map((pop) => `<i class="badge">${escapeHtml(pop)}</i>`).join("")}</span>
      <span>${requestIdCount(row.requests)}</span><span>${row.origins.length}</span>
      <span>${formatBytes(sumBytes(row.origins))}</span><span>${formatBytes(sumBytes(row.requests))}</span>
      <span>${hitRatio(row.requests).toFixed(1)}%</span><span>${avg}</span>
    </button>`;
  }).join("");
}

function renderPopOverview(popIndex) {
  $("overviewHead").className = "overview-head pop-overview-grid";
  $("overviewHead").innerHTML = "<span>PoP</span><span>Status</span><span>Requests</span><span>Origin Fetch</span><span>Client Throughput</span><span>Origin Throughput</span><span>Hit Ratio</span>";
  const pops = state.aggregate.pops || [];
  const summaryRows = pops.map((pop) => {
    const indexed = popIndex.get(pop.id) || { requests: [], origins: [] };
    const status = pop.error ? pop.error : pop.snapshot && pop.snapshot.enabled ? "RECORDING" : "STOPPED";
    const history = popHistory(pop.id);
    const latest = history.throughput[history.throughput.length - 1] || { client: 0, origin: 0 };
    return `<button class="overview-row pop-overview-grid" data-pop="${escapeAttr(pop.id)}">
      <span>${escapeHtml(pop.id)}</span><span>${escapeHtml(status)}</span><span>${indexed.requests.length}</span><span>${indexed.origins.length}</span>
      <span>${formatRate(latest.client)}</span><span>${formatRate(latest.origin)}</span><span>${hitRatio(indexed.requests).toFixed(1)}%</span>
    </button>`;
  }).join("");
  $("overviewRows").innerHTML = summaryRows + popThroughputHistoryTable(pops);
}

function popThroughputHistoryTable(pops) {
  const rows = popThroughputHistoryRows(pops);
  const gridColumns = `132px 88px repeat(${pops.length * 2 + 2}, 140px)`;
  const popHeads = pops.map((pop) => `<span>${escapeHtml(pop.id)} Client</span><span>${escapeHtml(pop.id)} Origin</span>`).join("");
  const body = rows.length ? rows.map((row) => `<div class="throughput-history-row" style="grid-template-columns:${gridColumns}">
    <span>${escapeHtml(row.time)}</span><span>${escapeHtml(row.window)}</span>
    ${pops.map((pop) => {
      const value = row.pops.get(pop.id) || { client: 0, origin: 0 };
      return `<span>${formatRate(value.client)}</span><span>${formatRate(value.origin)}</span>`;
    }).join("")}
    <span>${formatRate(row.clientTotal)}</span><span>${formatRate(row.originTotal)}</span>
  </div>`).join("") : `<div class="throughput-history-empty">Waiting for the next snapshot</div>`;
  return `<div class="pop-throughput-history">
    <div class="throughput-history-title">THROUGHPUT HISTORY</div>
    <div class="throughput-history-head" style="grid-template-columns:${gridColumns}"><span>Captured</span><span>Window</span>${popHeads}<span>Client Total</span><span>Origin Total</span></div>
    ${body}
  </div>`;
}

function popThroughputHistoryRows(pops) {
  const samples = state.samples;
  const rows = [];
  for (let i = samples.length - 1; i >= 1; i--) {
    const prevAt = parseTimestampMs(samples[i - 1].capturedAt || "");
    const currAt = parseTimestampMs(samples[i].capturedAt || "");
    const intervalMs = currAt - prevAt;
    if (!Number.isFinite(intervalMs) || intervalMs <= 0) continue;
    const row = {
      time: new Date(currAt).toLocaleTimeString(),
      window: formatRelativeMs(intervalMs),
      pops: new Map(),
      clientTotal: 0,
      originTotal: 0,
    };
    for (const pop of pops) {
      const prev = popSampleBytes(samples[i - 1], pop.id);
      const curr = popSampleBytes(samples[i], pop.id);
      const client = Math.max(0, curr.client - prev.client) / (intervalMs / 1000);
      const origin = Math.max(0, curr.origin - prev.origin) / (intervalMs / 1000);
      row.pops.set(pop.id, { client, origin });
      row.clientTotal += client;
      row.originTotal += origin;
    }
    rows.push(row);
  }
  return rows;
}

function popHistory(popId) {
  const samples = state.samples.slice(-24);
  const requests = samples.map((sample) => sampleTraceCounts(sample, popId).requests);
  const throughput = [];
  for (let i = 1; i < samples.length; i++) {
    const prev = popSampleBytes(samples[i - 1], popId);
    const curr = popSampleBytes(samples[i], popId);
    const intervalMs = sampleCapturedAtMs(samples[i], popId) - sampleCapturedAtMs(samples[i - 1], popId);
    if (!Number.isFinite(intervalMs) || intervalMs <= 0) {
      throughput.push({ client: 0, origin: 0 });
      continue;
    }
    throughput.push({
      client: Math.max(0, curr.client - prev.client) / (intervalMs / 1000),
      origin: Math.max(0, curr.origin - prev.origin) / (intervalMs / 1000),
    });
  }
  return { requests, throughput };
}

function popSampleBytes(sample, popId) {
  const pop = (sample.pops || []).find((item) => item.id === popId);
  const snapshot = pop && pop.snapshot || {};
  return {
    client: Number(snapshot.requestBytes || 0),
    origin: Number(snapshot.originBytes || 0),
  };
}

function sampleCapturedAtMs(sample, popId = "") {
  if (popId) {
    const pop = (sample && sample.pops || []).find((item) => item.id === popId);
    const popCapturedAt = parseTimestampMs(pop && pop.snapshot && pop.snapshot.capturedAt || "");
    if (Number.isFinite(popCapturedAt)) return popCapturedAt;
  }
  return parseTimestampMs(sample && sample.capturedAt || "");
}

function renderCharts(selected, reqs, origins) {
  document.querySelector(".timeline-panel").classList.toggle("hidden", state.overview !== "uri");
  if (state.overview !== "uri") {
    return;
  }

  rememberActiveChartScroll();
  canvasCharts.clear();
  nextCanvasChartId = 1;
  $("timelineTitle").innerHTML = `CHUNK POSITION VIEW<small>${selected ? escapeHtml(selected.uri) : "-"}</small>`;
  renderPositionScaleControl();
  const positionReqs = positionTraces(reqs);
  const positionOrigins = positionTraces(origins);
  const max = maxTraceEndBytes(positionReqs, positionOrigins);
  ensureSelectedRequest(reqs, origins);
  $("timelineBody").innerHTML = positionView(positionOrigins, positionReqs, max);
  renderLinkedTimeView(origins, reqs);
  restorePositionScroll();
  restoreTimeScroll();
  restoreTraceYScroll();
  initCanvasCharts();
}

function positionTraces(traces) {
  return traces.filter((trace) => !isHeadTrace(trace));
}

function maxTraceEndBytes(reqs, origins) {
  let max = 1;
  for (const trace of reqs) {
    max = Math.max(max, traceEndBytes(trace));
  }
  for (const trace of origins) {
    max = Math.max(max, traceEndBytes(trace));
  }
  return max;
}

function traceEndBytes(trace) {
  const start = traceStartBytes(trace);
  const end = Number(trace.end);
  if (Number.isFinite(start) && Number.isFinite(end) && end >= start) {
    return end + 1;
  }
  return start + traceWidthBytes(trace);
}

function renderLinkedTimeView(origins, reqs) {
  const panel = $("linkedTimePanel");
  if (!state.selectedRequestKey) {
    panel.classList.add("hidden");
    $("linkedTimeBody").innerHTML = "";
    return;
  }
  const start = captureStartTime();
  const end = captureEndTime();
  const range = Math.max(1, end - start);
  const timeReqs = filterSelectedRequest(reqs);
  const timeRows = buildTimeRequestRows(timeReqs, origins);
  const label = selectedRequestLabel(reqs.concat(origins));
  panel.classList.remove("hidden");
  $("linkedTimeTitle").innerHTML = `TIME VIEW<small>${escapeHtml(label)}</small>`;
  renderTimeScaleControl();
  $("linkedTimeBody").innerHTML = timeView(timeRows, start, end, range);
}

function buildTimeRequestRows(reqs, origins) {
  const originLookup = buildOriginLookup(origins);
  return reqs.map((request) => ({
    request,
    origin: originForRequestTrace(request, origins, originLookup),
  }));
}

function buildOriginLookup(origins) {
  const exact = new Map();
  const byCacheKey = new Map();
  for (const origin of origins) {
    const cacheKey = origin.cacheKey || "";
    const popId = origin.popId || "";
    const requestId = Number(origin.requestId || 0);
    if (requestId) {
      exact.set(originLookupKey(popId, requestId, cacheKey), origin);
    }
    const cacheOnlyKey = originLookupKey(popId, 0, cacheKey);
    if (!byCacheKey.has(cacheOnlyKey)) {
      byCacheKey.set(cacheOnlyKey, origin);
    }
  }
  return { exact, byCacheKey };
}

function originLookupKey(popId, requestId, cacheKey) {
  return `${popId}\u0000${requestId || 0}\u0000${cacheKey || ""}`;
}

function originForRequestTrace(request, origins, lookup = null) {
  if ((request.result || "") === "HIT") return null;
  const producerRequestId = Number(request.producerRequestId || request.requestId || 0);
  const producerCacheKey = request.producerCacheKey || request.cacheKey || "";
  if (lookup) {
    const exact = producerRequestId ? lookup.exact.get(originLookupKey(request.popId, producerRequestId, producerCacheKey)) : null;
    if (exact) return exact;
    if (producerRequestId) return null;
    return lookup.byCacheKey.get(originLookupKey(request.popId, 0, producerCacheKey)) || null;
  }
  return origins.find((origin) => {
    if (origin.popId !== request.popId) return false;
    if (producerRequestId && Number(origin.requestId || 0) !== producerRequestId) return false;
    return origin.cacheKey === producerCacheKey;
  }) || null;
}

function rememberActiveChartScroll() {
  const timeScroller = document.querySelector(".time-scroll");
  if (timeScroller) state.timeScrollLeft = timeScroller.scrollLeft;
  const positionScroller = document.querySelector(".position-scroll");
  if (positionScroller) state.positionScrollLeft = positionScroller.scrollLeft;
  rememberTraceYScroll();
}

function restorePositionScroll() {
  const scroller = document.querySelector(".position-scroll");
  if (!scroller) return;
  const positionAnchorOffsetPx = state.positionZoomAnchorOffsetPx !== null
    ? state.positionZoomAnchorOffsetPx
    : Math.max(1, scroller.clientWidth - traceLabelColumnPx) * state.positionZoomAnchorRatio;
  const restoreLeft = state.positionRestoreAnchorBytes !== null
    ? Math.max(0, positionWidthPx(state.positionRestoreAnchorBytes) - positionAnchorOffsetPx)
    : state.positionScrollLeft;
  scroller.scrollLeft = restoreLeft;
  refreshPositionTicks(scroller);
  if (state.positionRestoreAnchorBytes !== null) {
    state.positionScrollLeft = restoreLeft;
  }
  state.positionRestoreAnchorBytes = null;
  state.positionZoomAnchorOffsetPx = null;
  scroller.onscroll = () => {
    markChartScrollActive();
    state.positionScrollLeft = scroller.scrollLeft;
  };
}

function rememberTraceYScroll() {
  for (const scroller of document.querySelectorAll("[data-y-scroll]")) {
    state.traceYScroll[scroller.dataset.yScroll] = scroller.scrollTop;
  }
}

function restoreTraceYScroll() {
  const scrollersByGroup = new Map();
  for (const scroller of document.querySelectorAll("[data-y-scroll]")) {
    const group = scroller.dataset.yScroll;
    if (!scrollersByGroup.has(group)) {
      scrollersByGroup.set(group, []);
    }
    scrollersByGroup.get(group).push(scroller);
  }

  for (const [group, scrollers] of scrollersByGroup) {
    const restoreTop = state.traceYScroll[group] || 0;
    for (const scroller of scrollers) {
      scroller.scrollTop = restoreTop;
      scroller.addEventListener("scroll", () => {
        if (syncingTraceYScroll) return;
        syncingTraceYScroll = true;
        state.traceYScroll[group] = scroller.scrollTop;
        for (const other of scrollers) {
          if (other !== scroller) {
            other.scrollTop = scroller.scrollTop;
            requestCanvasDrawInScroller(other);
          }
        }
        requestCanvasDrawInScroller(scroller);
        syncingTraceYScroll = false;
      });
    }
  }
}

function requestCanvasDrawInScroller(scroller) {
  if (!scroller.querySelectorAll) return;
  for (const canvas of scroller.querySelectorAll("canvas")) {
    requestCanvasDraw(canvas);
  }
}

function restoreTimeScroll() {
  const scroller = document.querySelector(".time-scroll");
  if (!scroller) return;
  const timeAnchorOffsetPx = state.timeZoomAnchorOffsetPx !== null
    ? state.timeZoomAnchorOffsetPx
    : Math.max(1, scroller.clientWidth - traceLabelColumnPx) * state.timeZoomAnchorRatio;
  const restoreLeft = state.timeRestoreAnchorMs !== null
    ? Math.max(0, timeWidthPx(state.timeRestoreAnchorMs) - timeAnchorOffsetPx)
    : state.timeScrollLeft;
  scroller.scrollLeft = restoreLeft;
  refreshTimeTicks(scroller);
  if (state.timeRestoreAnchorMs !== null) {
    state.timeScrollLeft = restoreLeft;
  }
  state.timeRestoreAnchorMs = null;
  state.timeZoomAnchorOffsetPx = null;
  scroller.onscroll = () => {
    markChartScrollActive();
    state.timeScrollLeft = scroller.scrollLeft;
  };
}

function registerCanvasChart(config) {
  const id = `canvas-chart-${nextCanvasChartId++}`;
  canvasCharts.set(id, config);
  return id;
}

function canvasChartHeight(rowCount) {
  return Math.max(traceRowHeightPx, rowCount * traceRowHeightPx);
}

function initCanvasCharts() {
  for (const scroller of document.querySelectorAll("[data-canvas-chart]")) {
    const canvas = scroller.querySelector("canvas");
    if (!canvas) continue;
    requestCanvasDraw(canvas);
    scroller.addEventListener("scroll", () => requestCanvasDraw(canvas));
  }
}

function requestCanvasDraw(canvas) {
  if (canvas._drawQueued) return;
  canvas._drawQueued = true;
  window.requestAnimationFrame(() => {
    canvas._drawQueued = false;
    drawCanvasChart(canvas);
  });
}

function drawCanvasChart(canvas) {
  const scroller = canvas.closest("[data-canvas-chart]");
  const config = scroller && canvasCharts.get(scroller.dataset.canvasChart);
  if (!scroller || !config) return;

  if (config.kind === "position-origin" || config.kind === "position-request") {
    drawPositionCanvasChart(canvas, scroller, config);
    return;
  }
  drawTimeCanvasChart(canvas, scroller, config);
}

function prepareCanvas(canvas, width, height) {
  const dpr = window.devicePixelRatio || 1;
  const pixelWidth = Math.ceil(width * dpr);
  const pixelHeight = Math.ceil(height * dpr);
  if (canvas.width !== pixelWidth || canvas.height !== pixelHeight) {
    canvas.width = pixelWidth;
    canvas.height = pixelHeight;
  }
  canvas.style.width = `${width}px`;
  canvas.style.height = `${height}px`;
}

function drawPositionCanvasChart(canvas, scroller, config) {
  const horizontalScroller = canvas.closest(".position-scroll");
  if (!horizontalScroller) return;
  const viewportWidth = Math.max(1, horizontalScroller.clientWidth - traceLabelColumnPx);
  const viewportHeight = Math.max(1, scroller.clientHeight);
  const windowRect = positionCanvasWindow(canvas, scroller, horizontalScroller, config, viewportWidth, viewportHeight);

  canvas.style.position = "absolute";
  canvas.style.left = `${windowRect.left}px`;
  canvas.style.top = `${windowRect.top}px`;
  prepareCanvas(canvas, windowRect.width, windowRect.height);
  if (!windowRect.needsDraw) return;

  const ctx = canvas.getContext("2d");
  const dpr = window.devicePixelRatio || 1;
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, windowRect.width, windowRect.height);
  ctx.font = "800 10px Inter, ui-sans-serif, system-ui, sans-serif";
  ctx.textAlign = "center";
  ctx.textBaseline = "middle";

  drawPositionCanvas(ctx, config, windowRect);
}

function drawTimeCanvasChart(canvas, scroller, config) {
  const horizontalScroller = canvas.closest(".time-scroll");
  if (!horizontalScroller) return;
  const viewportWidth = Math.max(1, horizontalScroller.clientWidth - traceLabelColumnPx);
  const viewportHeight = Math.max(1, scroller.clientHeight);
  const contentHeight = timeCanvasContentHeight(config);
  const windowRect = horizontalCanvasWindow(canvas, scroller, horizontalScroller, config.widthPx, contentHeight, viewportWidth, viewportHeight);

  canvas.style.position = "absolute";
  canvas.style.left = `${windowRect.left}px`;
  canvas.style.top = `${windowRect.top}px`;
  prepareCanvas(canvas, windowRect.width, windowRect.height);
  if (!windowRect.needsDraw) return;

  const ctx = canvas.getContext("2d");
  const dpr = window.devicePixelRatio || 1;
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, windowRect.width, windowRect.height);
  ctx.font = "800 10px Inter, ui-sans-serif, system-ui, sans-serif";
  ctx.textAlign = "center";
  ctx.textBaseline = "middle";

  drawTimeCanvas(ctx, config, windowRect);
}

function timeCanvasContentHeight(config) {
  if (config.kind === "time-request" || config.mode === "simple") return canvasChartHeight(1);
  return canvasChartHeight(config.rows.length);
}

function horizontalCanvasWindow(canvas, scroller, horizontalScroller, contentWidth, contentHeight, viewportWidth, viewportHeight) {
  const drawWidth = Math.min(contentWidth, Math.max(viewportWidth, viewportWidth * 7));
  const drawHeight = Math.min(contentHeight, Math.max(viewportHeight, viewportHeight * 3));
  const scrollLeft = horizontalScroller.scrollLeft;
  const scrollTop = scroller.scrollTop;
  const currentLeft = Number(canvas.dataset.windowLeft);
  const currentTop = Number(canvas.dataset.windowTop);
  const currentWidth = Number(canvas.dataset.windowWidth);
  const currentHeight = Number(canvas.dataset.windowHeight);
  const valid = Number.isFinite(currentLeft)
    && Number.isFinite(currentTop)
    && currentWidth === drawWidth
    && currentHeight === drawHeight;

  let left = valid ? currentLeft : 0;
  let top = valid ? currentTop : 0;
  const horizontalCovered = valid
    && scrollLeft >= left
    && scrollLeft + viewportWidth <= left + drawWidth;
  const verticalCovered = valid
    && scrollTop >= top
    && scrollTop + viewportHeight <= top + drawHeight;

  if (!horizontalCovered) {
    left = Math.min(Math.max(0, scrollLeft - viewportWidth), Math.max(0, contentWidth - drawWidth));
  }
  if (!verticalCovered) {
    top = Math.min(Math.max(0, scrollTop - viewportHeight), Math.max(0, contentHeight - drawHeight));
  }

  const needsDraw = !valid || left !== currentLeft || top !== currentTop;
  canvas.dataset.windowLeft = String(left);
  canvas.dataset.windowTop = String(top);
  canvas.dataset.windowWidth = String(drawWidth);
  canvas.dataset.windowHeight = String(drawHeight);
  return { left, top, width: drawWidth, height: drawHeight, needsDraw };
}

function positionCanvasWindow(canvas, scroller, horizontalScroller, config, viewportWidth, viewportHeight) {
  return horizontalCanvasWindow(canvas, scroller, horizontalScroller, config.widthPx, canvasChartHeight(config.rows.length), viewportWidth, viewportHeight);
}

function drawPositionCanvas(ctx, config, windowRect) {
  const firstRow = Math.max(0, Math.floor(windowRect.top / traceRowHeightPx) - 1);
  const lastRow = Math.min(config.rows.length - 1, Math.ceil((windowRect.top + windowRect.height) / traceRowHeightPx) + 1);
  const visibleLeft = windowRect.left - 4;
  const visibleRight = windowRect.left + windowRect.width + 4;

  for (let rowIndex = firstRow; rowIndex <= lastRow; rowIndex++) {
    const row = config.rows[rowIndex];
    if (!row) continue;
    const traces = config.kind === "position-request" ? row.requests : row.origins;
    const y = rowIndex * traceRowHeightPx - windowRect.top;
    drawCanvasBarBackground(ctx, 0, y, windowRect.width);
    for (const trace of traces) {
      const start = positionWidthPx(positionTraceStartBytes(trace));
      if (start > visibleRight) break;
      const width = Math.max(0.5, positionWidthPx(traceWidthBytes(trace)));
      if (start + width < visibleLeft) continue;
      const result = config.kind === "position-request" ? trace.result || "UNKNOWN" : "UNKNOWN";
      const label = width >= 24 ? chunkIdLabel(trace) : "";
      drawCanvasSegment(ctx, start - windowRect.left, y, width, result, label);
    }
  }
}

function positionTraceStartBytes(trace) {
  return traceStartBytes(trace);
}

function traceStartBytes(trace) {
  const traceStart = Number(trace.start);
  if (Number.isFinite(traceStart) && traceStart >= 0) return traceStart;
  const start = Number(trace.chunkStart);
  return Number.isFinite(start) ? start : 0;
}

function traceWidthBytes(trace) {
  const start = traceStartBytes(trace);
  const end = Number(trace.end);
  const rangeWidth = Number.isFinite(start) && Number.isFinite(end) && end >= start ? end - start + 1 : 0;
  const width = Number(rangeWidth || trace.contentLength || trace.producedBytes || 1);
  return Number.isFinite(width) && width > 0 ? width : 1;
}

function drawTimeCanvas(ctx, config, windowRect) {
  if (config.mode === "simple") {
    drawSimpleTimeCanvas(ctx, config, windowRect);
    return;
  }

  const scrollLeft = windowRect.left;
  const scrollTop = windowRect.top;
  const width = windowRect.width;
  const rows = config.kind === "time-request" ? [{ request: null }] : config.rows;
  const firstRow = Math.max(0, Math.floor(scrollTop / traceRowHeightPx) - 1);
  const lastRow = Math.min(rows.length - 1, Math.ceil((scrollTop + windowRect.height) / traceRowHeightPx) + 1);
  const visibleLeft = scrollLeft - 4;
  const visibleRight = scrollLeft + width + 4;

  for (let rowIndex = firstRow; rowIndex <= lastRow; rowIndex++) {
    const y = rowIndex * traceRowHeightPx - scrollTop;
    drawCanvasBarBackground(ctx, 0, y, width);
    if (config.kind === "time-request") {
      for (const row of config.rows) {
        drawTimeTraceSegments(ctx, row.request, config, visibleLeft, visibleRight, scrollLeft, y);
      }
    } else {
      drawTimeTraceSegments(ctx, rows[rowIndex].origin, config, visibleLeft, visibleRight, scrollLeft, y);
    }
  }
}

function drawTimeTraceSegments(ctx, trace, config, visibleLeft, visibleRight, scrollLeft, y) {
  for (const segment of timeCanvasSegments(trace, config)) {
    if (segment.left + segment.width < visibleLeft || segment.left > visibleRight) continue;
    drawCanvasSegment(ctx, segment.left - scrollLeft, y, segment.width, segment.result, segment.label, segment.wait);
  }
}

function drawSimpleTimeCanvas(ctx, config, windowRect) {
  const scrollLeft = windowRect.left;
  const width = windowRect.width;
  const visibleLeft = scrollLeft - 4;
  const visibleRight = scrollLeft + width + 4;
  drawCanvasBarBackground(ctx, 0, 0, width);

  const segment = simpleTimeCanvasSegment(config.rows, config);
  if (!segment || segment.left + segment.width < visibleLeft || segment.left > visibleRight) return;
  drawCanvasSegment(ctx, segment.left - scrollLeft, 0, segment.width, segment.result, "", false, segment.color);
}

function simpleTimeCanvasSegment(rows, config) {
  let start = Infinity;
  let end = -Infinity;
  for (const row of rows) {
    const trace = config.kind === "time-request" ? row.request : row.origin;
    if (!trace) continue;
    const traceStart = parseTimestampMs(trace.startTime || "");
    const traceEnd = parseTimestampMs(trace.endTime || "");
    if (!Number.isFinite(traceStart) || !Number.isFinite(traceEnd) || traceEnd < traceStart) continue;
    start = Math.min(start, traceStart);
    end = Math.max(end, traceEnd);
  }
  if (!Number.isFinite(start) || !Number.isFinite(end)) return null;
  const left = timeLeftPx(start - config.start);
  const width = Math.max(0.5, timeWidthPx(end - start));
  return {
    left,
    width,
    result: "UNKNOWN",
    color: config.kind === "time-request" ? resultColors.HIT : resultColors.UNKNOWN,
  };
}

function timeCanvasSegments(trace, config) {
  if (!trace) return [];
  const s = parseTimestampMs(trace.startTime || "");
  const h = parseTimestampMs(trace.headerTime || "");
  const e = parseTimestampMs(trace.endTime || "");
  const bodyEnd = Number.isFinite(e) ? e : config.end;
  const includeHeaderWait = config.kind !== "time-request";
  const result = config.kind === "time-request" ? trace.result || "UNKNOWN" : "UNKNOWN";
  const segmentTimeStart = Number.isFinite(h) ? h : s;
  const segments = [];

  if (includeHeaderWait && Number.isFinite(s) && Number.isFinite(h) && h > s) {
    segments.push({
      left: timeLeftPx(s - config.start),
      width: Math.max(0.5, timeWidthPx(h - s)),
      result: "UNKNOWN",
      label: "",
      wait: true,
    });
  }
  if (Number.isFinite(segmentTimeStart) && Number.isFinite(bodyEnd) && bodyEnd >= segmentTimeStart) {
    const width = Math.max(0.5, timeWidthPx(bodyEnd - segmentTimeStart));
    segments.push({
      left: timeLeftPx(segmentTimeStart - config.start),
      width,
      result,
      label: width >= 24 ? traceChunkLabel(trace) : "",
      wait: false,
    });
  }
  return segments;
}

function drawCanvasBarBackground(ctx, x, y, width) {
  ctx.fillStyle = "#0b1318";
  ctx.fillRect(x, y, width, traceBarHeightPx);
}

function drawCanvasSegment(ctx, x, rowY, width, result, label, wait = false, color = "") {
  const clipWidth = ctx.canvas.clientWidth || width;
  const clippedX = Math.max(0, x);
  const clippedRight = Math.min(clipWidth, x + width);
  if (clippedRight <= clippedX) return;
  const y = rowY + 2;
  const height = traceBarHeightPx - 4;
  ctx.fillStyle = color || (wait ? "#8395aa" : resultColors[result] || resultColors.UNKNOWN);
  ctx.fillRect(clippedX, y, Math.max(0.5, clippedRight - clippedX), height);
  if (!label || width < 24) return;
  const labelX = x + width / 2;
  if (labelX < 0 || labelX > clipWidth) return;
  ctx.fillStyle = "#061014";
  ctx.fillText(label, labelX, y + height / 2);
}

function positionView(origins, reqs, maxBytes) {
  const widthPx = Math.max(1600, Math.ceil(maxBytes / state.positionScaleBytes) * positionGridWidthPx);
  const rows = positionRows(reqs, origins);
  const trackStyle = `--position-width:${widthPx}px;--track-width:${widthPx}px;--grid-width:${positionGridWidthPx}px`;
  return `<div class="position-scroll position-h-scroll">
    <div class="position-view-stack position-wide" style="--position-width:${widthPx}px">
      ${positionSection("ORIGIN FETCH", "position-origin", positionLabels(rows), maxBytes, widthPx, trackStyle, registerCanvasChart({ kind: "position-origin", rows, widthPx }))}
      ${positionSection("CLIENT REQUESTS", "position-request", positionLabels(rows), maxBytes, widthPx, trackStyle, registerCanvasChart({ kind: "position-request", rows, widthPx }))}
    </div>
  </div>`;
}

function positionSection(title, scrollGroup, labels, maxBytes, widthPx, trackStyle, chartId) {
  return `<div class="trace-section position-trace-section">
    <div class="trace-section-head">
      <div class="position-labels"><div class="position-axis-spacer"></div><h3>${escapeHtml(title)}</h3></div>
      <div class="position-axis-pane trace-axis-scroll">
        <div class="position-track" data-max-bytes="${escapeAttr(maxBytes)}" style="${trackStyle}">
          ${positionMarker(widthPx)}
          ${positionAxis(maxBytes)}
        </div>
      </div>
    </div>
    <div class="trace-section-body">
      <div class="position-labels trace-y-scroll trace-label-scroll" data-y-scroll="${escapeAttr(scrollGroup)}">${labels}</div>
      <div class="position-chart-pane trace-y-scroll trace-chart-scroll canvas-chart-scroll" data-y-scroll="${escapeAttr(scrollGroup)}" data-canvas-chart="${escapeAttr(chartId)}">
        <div class="position-track canvas-scroll-spacer" data-max-bytes="${escapeAttr(maxBytes)}" style="${trackStyle};height:${canvasChartHeight(canvasCharts.get(chartId).rows.length)}px">
          ${positionMarker(widthPx)}
          ${positionGrid(maxBytes, "time-grid body-grid")}
          <canvas class="trace-canvas"></canvas>
        </div>
      </div>
    </div>
  </div>`;
}

function positionRows(reqs, origins) {
  const rows = new Map();
  const originLookup = buildOriginLookup(origins);
  for (const request of reqs) {
    const key = requestKey(request);
    if (!rows.has(key)) {
      rows.set(key, { popId: request.popId, requestId: request.requestId, requests: [], origins: [], originKeys: new Set() });
    }
    const row = rows.get(key);
    row.requests.push(request);
    const origin = originForRequestTrace(request, origins, originLookup);
    const originKey = origin && traceKey(origin);
    if (origin && !row.originKeys.has(originKey)) {
      row.origins.push(origin);
      row.originKeys.add(originKey);
    }
  }
  return [...rows.values()].map((row) => ({
    popId: row.popId,
    requestId: row.requestId,
    requests: row.requests.sort(compareTimeViewTrace),
    origins: row.origins.sort(compareTimeViewTrace),
  })).sort((a, b) => b.requestId - a.requestId || b.popId.localeCompare(a.popId));
}

function positionLabels(rows) {
  if (!rows.length) return `<div class="time-label-row empty">No trace</div>`;
  return rows.map((row) => {
    return `<button class="time-label-row" data-request-row="${escapeAttr(`${row.popId}:${row.requestId}`)}">#${row.requestId} (${escapeHtml(row.popId)})</button>`;
  }).join("");
}

function chunkIdLabel(trace) {
  if (isHeadTrace(trace)) return "HEAD";
  const start = traceStartBytes(trace);
  if (!Number.isFinite(start) || start <= 0) return "c0";
  return `c${Math.floor(start / 1048576)}`;
}

function traceChunkLabel(trace) {
  return chunkIdLabel(trace) || "c0";
}

function isHeadTrace(trace) {
  return String(trace.method || "").toUpperCase() === "HEAD";
}

function positionMarker(widthPx) {
  if (state.positionZoomAnchorBytes === null) return "";
  const left = Math.min(widthPx, Math.max(0, positionWidthPx(state.positionZoomAnchorBytes)));
  return `<div class="time-marker" style="left:${left}px"><span>${escapeHtml(formatBytes(state.positionZoomAnchorBytes))}</span></div>`;
}

function positionAxis(maxBytes) {
  return `<div class="position-axis"><i class="bar-bg">${positionAxisTickHtml(maxBytes, null)}</i></div>`;
}

function positionGrid(maxBytes, className = "time-grid") {
  return `<div class="${className}">${positionGridTickHtml(maxBytes, null)}</div>`;
}

function positionTickStep(maxBytes) {
  return Math.max(1, Math.ceil(Math.ceil(maxBytes / state.positionScaleBytes) / 600));
}

function refreshPositionTicks(scroller) {
  for (const track of scroller.querySelectorAll(".position-track")) {
    const maxBytes = Number(track.dataset.maxBytes || 0);
    const axis = track.querySelector(".position-axis .bar-bg");
    const grid = track.querySelector(".time-grid");
    if (!Number.isFinite(maxBytes) || maxBytes <= 0) continue;
    if (axis) axis.innerHTML = positionAxisTickHtml(maxBytes, scroller);
    if (grid) grid.innerHTML = positionGridTickHtml(maxBytes, scroller);
  }
}

function positionAxisTickHtml(maxBytes, scroller) {
  let html = "";
  for (const tick of visiblePositionTicks(maxBytes, scroller)) {
    html += `<b class="${tick.className}" style="left:${tick.left}px">${escapeHtml(tick.label)}</b>`;
  }
  return html;
}

function positionGridTickHtml(maxBytes, scroller) {
  let html = "";
  for (const tick of visiblePositionTicks(maxBytes, scroller)) {
    html += `<i style="left:${tick.left}px"></i>`;
  }
  return html;
}

function visiblePositionTicks(maxBytes, scroller) {
  const bounds = visiblePositionTickBounds(maxBytes, scroller);
  const ticks = [];
  for (let i = bounds.start; i <= bounds.end; i += bounds.step) {
    ticks.push({
      left: i * positionGridWidthPx,
      label: formatBytes(Math.min(maxBytes, state.positionScaleBytes * i)),
      className: i === 0 ? "edge-start" : "",
    });
  }
  const endLeft = positionWidthPx(maxBytes);
  if (endLeft >= bounds.leftPx && endLeft <= bounds.rightPx && bounds.ticks % bounds.step !== 0) {
    ticks.push({
      left: endLeft,
      label: formatBytes(maxBytes),
      className: "edge-end",
    });
  }
  return ticks;
}

function visiblePositionTickBounds(maxBytes, scroller) {
  const ticks = Math.ceil(maxBytes / state.positionScaleBytes);
  const step = positionTickStep(maxBytes);
  const scrollLeft = scroller ? scroller.scrollLeft : state.positionScrollLeft;
  const width = scroller && scroller.clientWidth > 0 ? Math.max(1, scroller.clientWidth - traceLabelColumnPx) : 1600;
  const leftPx = Math.max(0, scrollLeft - width);
  const rightPx = Math.min(positionWidthPx(maxBytes), scrollLeft + width * 2);
  const start = Math.max(0, Math.floor(leftPx / positionGridWidthPx / step) * step);
  const end = Math.min(ticks, Math.ceil(rightPx / positionGridWidthPx / step) * step);
  return { ticks, step, start, end, leftPx, rightPx };
}

function positionWidthPx(bytes) {
  return bytes / state.positionScaleBytes * positionGridWidthPx;
}

function timeView(rows, start, end, range) {
  const widthPx = Math.max(1600, Math.ceil(range / state.timeScaleMs) * timeGridWidthPx);
  const marker = timeMarker(widthPx);
  const trackStyle = `--time-width:${widthPx}px;--track-width:${widthPx}px;--grid-width:${timeGridWidthPx}px`;
  const requestChartId = registerCanvasChart({ kind: "time-request", mode: state.timeViewMode, rows, start, end, widthPx });
  return `<div class="time-scroll time-h-scroll">
  <div class="time-view-stack time-wide" style="--time-width:${widthPx}px">
  <div class="time-client-pinned trace-section">
    <div class="trace-section-head">
      <div class="time-labels"><div class="time-axis-spacer"></div><h3>CLIENT REQUESTS</h3></div>
      <div class="time-axis-pane trace-axis-scroll">
        <div class="time-track" data-range="${range}" style="${trackStyle}">
          ${marker}
          ${timeAxis(range)}
        </div>
      </div>
    </div>
    <div class="trace-section-body">
      <div class="time-labels trace-label-scroll">${timeRequestLabels(rows)}</div>
      <div class="time-chart-pane canvas-chart-scroll" data-canvas-chart="${escapeAttr(requestChartId)}">
        <div class="time-track canvas-scroll-spacer" data-range="${range}" style="${trackStyle};height:${canvasChartHeight(1)}px">
          ${marker}
          ${timeGrid(range, "time-grid body-grid")}
          <canvas class="trace-canvas"></canvas>
        </div>
      </div>
    </div>
  </div>
  ${timeOriginSection(rows, start, end, range, widthPx, marker, trackStyle)}
  ${throughputView(start, end, range, widthPx, marker, trackStyle)}
  </div>
  </div>`;
}

function timeOriginSection(rows, start, end, range, widthPx, marker, trackStyle) {
  const originRows = rows.filter((row) => row.origin);
  const rowCount = state.timeViewMode === "simple" ? 1 : originRows.length;
  const chartId = registerCanvasChart({ kind: "time-origin", mode: state.timeViewMode, rows: originRows, start, end, widthPx });
  return `<div class="trace-section time-origin-section">
    <div class="trace-section-head">
      <div class="time-labels"><div class="time-axis-spacer"></div><h3>ORIGIN FETCH</h3></div>
      <div class="time-axis-pane trace-axis-scroll">
        <div class="time-track" data-range="${range}" style="${trackStyle}">
          ${marker}
          ${timeAxis(range)}
        </div>
      </div>
    </div>
    <div class="trace-section-body">
      <div class="time-labels trace-y-scroll trace-label-scroll" data-y-scroll="time-origin">${timeOriginLabelsForMode(rows)}</div>
      <div class="time-chart-pane trace-y-scroll trace-chart-scroll canvas-chart-scroll" data-y-scroll="time-origin" data-canvas-chart="${escapeAttr(chartId)}">
        <div class="time-track canvas-scroll-spacer" data-range="${range}" style="${trackStyle};height:${canvasChartHeight(rowCount)}px">
          ${marker}
          ${timeGrid(range, "time-grid body-grid")}
          <canvas class="trace-canvas"></canvas>
        </div>
      </div>
    </div>
  </div>`;
}

function throughputView(start, end, range, widthPx, marker, trackStyle) {
  const graph = throughputGraph(start, end, widthPx);
  return `<div class="time-throughput-view">
    <div class="time-labels throughput-labels">
      <div class="time-axis-spacer"></div>
      <h3>THROUGHPUT</h3>
      <div class="throughput-axis-labels" style="--throughput-height:${throughputChartHeightPx}px">
        <span>${escapeHtml(formatRate(graph.peak))}</span>
        <span>${escapeHtml(formatRate(graph.peak / 2))}</span>
        <span>0 bps</span>
      </div>
      <div class="throughput-label"><i class="throughput-key client"></i><span>Client</span></div>
      <div class="throughput-label"><i class="throughput-key origin"></i><span>Origin</span></div>
    </div>
    <div class="time-chart-pane">
      <div class="time-track" data-range="${range}" style="${trackStyle}">
        ${marker}
        ${timeAxis(range)}
        ${timeGrid(range)}
        <div class="time-section-spacer"></div>
        <div class="throughput-chart" style="--time-width:${widthPx}px;--throughput-height:${throughputChartHeightPx}px">${graph.svg}</div>
      </div>
    </div>
  </div>`;
}

function throughputGraph(start, end, widthPx) {
  const client = selectedRequestThroughputSeries("client", start, end);
  const origin = selectedRequestThroughputSeries("origin", start, end);
  const clientPeak = seriesPeak(client);
  const originPeak = seriesPeak(origin);
  const peak = Math.max(clientPeak, originPeak, 1);
  const height = throughputChartHeightPx;
  const clientPath = throughputPath(client, start, peak, height);
  const originPath = throughputPath(origin, start, peak, height);
  const svg = `<svg class="throughput-svg" width="${widthPx}" height="${height}" viewBox="0 0 ${widthPx} ${height}" preserveAspectRatio="none" aria-hidden="true">
    ${throughputYAxis(peak, height, widthPx)}
    <path class="throughput-line client" d="${escapeAttr(clientPath)}"></path>
    <path class="throughput-line origin" d="${escapeAttr(originPath)}"></path>
  </svg>`;
  return { svg, clientPeak, originPeak, peak };
}

function selectedRequestThroughputSeries(kind, start, end) {
  const points = [{ t: start, value: 0 }];
  const samples = state.samples;
  const popId = selectedRequestPopId();
  for (let i = 1; i < samples.length; i++) {
    const prevAt = sampleCapturedAtMs(samples[i - 1], popId);
    const currAt = sampleCapturedAtMs(samples[i], popId);
    const intervalMs = currAt - prevAt;
    if (!Number.isFinite(intervalMs) || intervalMs <= 0) continue;
    const windowStart = Math.max(start, prevAt);
    const windowEnd = Math.min(end, currAt);
    if (windowEnd <= start || windowStart >= end || windowEnd <= windowStart) continue;

    const prev = selectedRequestSampleBytes(samples[i - 1]);
    const curr = selectedRequestSampleBytes(samples[i]);
    const rate = Math.max(0, curr[kind] - prev[kind]) / (intervalMs / 1000);
    appendThroughputWindow(points, windowStart, windowEnd, rate);
  }
  if (end > points[points.length - 1].t) {
    points.push({ t: end, value: 0 });
  }
  return points;
}

function selectedRequestPopId() {
  return String(state.selectedRequestKey || "").split(":")[0] || "";
}

function appendThroughputWindow(points, start, end, value) {
  const last = points[points.length - 1];
  if (start > last.t) {
    points.push({ t: start, value: 0 });
  }
  points.push({ t: start, value });
  points.push({ t: end, value });
  points.push({ t: end, value: 0 });
}

function selectedRequestSampleBytes(sample) {
  if (!state.selectedRequestKey) return { client: 0, origin: 0 };
  let byRequest = selectedBytesCache.get(sample);
  if (!byRequest) {
    byRequest = new Map();
    selectedBytesCache.set(sample, byRequest);
  }
  const cached = byRequest.get(state.selectedRequestKey);
  if (cached) return cached;

  const bytes = selectedRequestSampleBytesRaw(sample, state.selectedRequestKey);
  byRequest.set(state.selectedRequestKey, bytes);
  return bytes;
}

function selectedRequestSampleBytesRaw(sample, selectedKey) {
  const [selectedPopId, selectedRequestIdText] = String(selectedKey).split(":");
  const selectedRequestId = Number(selectedRequestIdText || 0);
  const requests = [];
  const wantedOriginKeys = new Set();
  for (const pop of sample && sample.pops || []) {
    if (pop.id !== selectedPopId) continue;
    const keys = (pop.snapshot && pop.snapshot.keys) || {};
    for (const [cacheKey, traces] of Object.entries(keys)) {
      const parsed = parseCacheKey(cacheKey);
      for (const request of traces.requests || []) {
        if (Number(request.requestId || 0) !== selectedRequestId) continue;
        requests.push({ ...request, ...parsed, cacheKey, popId: pop.id, popURL: pop.url, kind: "request" });
        if ((request.result || "") !== "HIT") {
          wantedOriginKeys.add(request.producerCacheKey || cacheKey);
        }
      }
    }
    const origins = [];
    for (const [cacheKey, traces] of Object.entries(keys)) {
      if (!wantedOriginKeys.has(cacheKey)) continue;
      const parsed = parseCacheKey(cacheKey);
      for (const origin of traces.origins || []) {
        origins.push({ ...origin, ...parsed, cacheKey, popId: pop.id, popURL: pop.url, kind: "origin" });
      }
    }
    const originLookup = buildOriginLookup(origins);
    const originKeys = new Set();
    let originBytes = 0;
    for (const request of requests) {
      const origin = originForRequestTrace(request, origins, originLookup);
      const key = origin && traceKey(origin);
      if (!origin || originKeys.has(key)) continue;
      originBytes += Math.max(0, Number(origin.producedBytes || 0));
      originKeys.add(key);
    }
    return {
      client: sumBytes(requests),
      origin: originBytes,
    };
  }
  return {
    client: sumBytes(requests),
    origin: 0,
  };
}

function seriesPeak(points) {
  return points.reduce((max, point) => Math.max(max, point.value), 0);
}

function throughputPath(points, start, peak, height) {
  if (points.length < 2) return "";
  return points.map((point, index) => {
    const x = timeLeftPx(point.t - start);
    return `${index === 0 ? "M" : "L"}${x.toFixed(2)} ${throughputY(point.value, peak, height).toFixed(2)}`;
  }).join(" ");
}

function throughputYAxis(peak, height, widthPx) {
  return [peak, peak / 2, 0].map((value) => {
    const y = throughputY(value, peak, height).toFixed(2);
    return `<line class="throughput-y-grid-line" x1="0" y1="${y}" x2="${widthPx}" y2="${y}"></line>`;
  }).join("") + `<line class="throughput-y-axis-line" x1="0" y1="0" x2="0" y2="${height}"></line>`;
}

function throughputY(value, peak, height) {
  const padding = 7;
  const usableHeight = height - padding * 2;
  return padding + usableHeight - Math.max(0, value) / peak * usableHeight;
}

function timeMarker(widthPx) {
  if (state.timeZoomAnchorMs === null) return "";
  const left = Math.min(widthPx, Math.max(0, timeWidthPx(state.timeZoomAnchorMs)));
  return `<div class="time-marker" style="left:${left}px"><span>${escapeHtml(formatAnchorTime(state.timeZoomAnchorMs))}</span></div>`;
}

function timeRowLabels(rows) {
  if (!rows.length) return `<div class="time-label-row empty">No trace</div>`;
  return rows.map((row) => {
    const trace = row.request;
    return `<button class="time-label-row">${escapeHtml(traceChunkLabel(trace))}</button>`;
  }).join("");
}

function timeOriginLabels(rows) {
  const originRows = rows.filter((row) => row.origin);
  return timeRowLabels(originRows);
}

function timeOriginLabelsForMode(rows) {
  if (state.timeViewMode === "detail") return timeOriginLabels(rows);
  return rows.some((row) => row.origin) ? `<button class="time-label-row">origin</button>` : `<div class="time-label-row empty">No trace</div>`;
}

function timeRequestLabels(rows) {
  if (!rows.length) return `<div class="time-label-row empty">No trace</div>`;
  return `<button class="time-label-row request-label" data-request-row="${escapeAttr(requestKey(rows[0].request))}">req</button>`;
}

function captureStartTime() {
  const times = (state.aggregate.pops || [])
    .map((pop) => parseTimestampMs(pop.snapshot && pop.snapshot.startedAt || ""))
    .filter(Number.isFinite);
  if (times.length) return Math.min(...times);
  const capturedAt = parseTimestampMs(state.aggregate.capturedAt || "");
  return Number.isFinite(capturedAt) ? capturedAt : Date.now();
}

function captureEndTime() {
  const snapshots = (state.aggregate.pops || [])
    .map((pop) => pop.snapshot)
    .filter(Boolean);
  if (snapshots.length && snapshots.every((snapshot) => !snapshot.enabled && snapshot.stoppedAt)) {
    const stopped = snapshots.map((snapshot) => parseTimestampMs(snapshot.stoppedAt || "")).filter(Number.isFinite);
    if (stopped.length) return Math.max(...stopped);
  }
  return Date.now();
}

function renderTimeScaleControl() {
  $("timeScaleControl").classList.toggle("hidden", state.overview !== "uri" || !state.selectedRequestKey);
  $("timeModeControl").classList.toggle("hidden", state.overview !== "uri" || !state.selectedRequestKey);
  $("timeScaleSlider").max = String(timeScaleValuesMs.length - 1);
  $("timeScaleSlider").value = String(timeScaleValuesMs.length - 1 - state.timeScaleIndex);
  $("timeScaleValue").textContent = `${formatScaleMs(state.timeScaleMs)} / grid`;
  $("timeViewMode").value = state.timeViewMode;
}

function renderPositionScaleControl() {
  $("positionScaleControl").classList.toggle("hidden", state.overview !== "uri");
  $("positionScaleSlider").value = String(positionScaleValuesBytes.length - 1 - state.positionScaleIndex);
  $("positionScaleValue").textContent = `${formatBytes(state.positionScaleBytes)} / grid`;
}

function ensureSelectedRequest(reqs, origins) {
  const traces = reqs.concat(origins);
  const seen = new Set();
  for (const trace of traces) {
    const key = requestKey(trace);
    if (seen.has(key)) continue;
    seen.add(key);
  }
  if (!seen.has(state.selectedRequestKey)) {
    state.selectedRequestKey = seen.size ? [...seen][0] : "";
  }
}

function selectedRequestLabel(traces) {
  const trace = traces.find((item) => requestKey(item) === state.selectedRequestKey);
  if (!trace) return "";
  const kind = String(trace.method || "").toUpperCase() === "HEAD" ? " HEAD" : "";
  return `#${trace.requestId}${kind} (${trace.popId})`;
}

function filterSelectedRequest(traces) {
  if (!state.selectedRequestKey) return [];
  return traces.filter((trace) => requestKey(trace) === state.selectedRequestKey).sort(compareTimeViewTrace);
}

function requestKey(trace) {
  return `${trace.popId}:${trace.requestId}`;
}

function compareTimeViewTrace(a, b) {
  const rankDiff = traceOrderRank(a) - traceOrderRank(b);
  if (rankDiff !== 0) return rankDiff;
  const chunkDiff = traceChunkStart(a) - traceChunkStart(b);
  if (chunkDiff !== 0) return chunkDiff;
  const startDiff = parseTimestampMs(a.startTime || "") - parseTimestampMs(b.startTime || "");
  if (Number.isFinite(startDiff) && startDiff !== 0) return startDiff;
  return String(a.cacheKey || "").localeCompare(String(b.cacheKey || ""));
}

function traceOrderRank(trace) {
  return isHeadTrace(trace) ? 0 : 1;
}

function traceChunkStart(trace) {
  return traceStartBytes(trace);
}

function timeLeftPx(ms) {
  return Math.max(0, timeWidthPx(ms));
}

function timeWidthPx(ms) {
  return ms / state.timeScaleMs * timeGridWidthPx;
}

function timeAxis(range) {
  return `<div class="time-axis"><i class="bar-bg">${timeAxisTickHtml(range, null)}</i></div>`;
}

function timeGrid(range, className = "time-grid") {
  return `<div class="${className}">${timeGridTickHtml(range, null)}</div>`;
}

function refreshTimeTicks(scroller) {
  for (const track of scroller.querySelectorAll(".time-track")) {
    const range = Number(track.dataset.range || 0);
    const axis = track.querySelector(".time-axis .bar-bg");
    const grid = track.querySelector(".time-grid");
    if (!Number.isFinite(range) || range <= 0) continue;
    if (axis) axis.innerHTML = timeAxisTickHtml(range, scroller);
    if (grid) grid.innerHTML = timeGridTickHtml(range, scroller);
  }
}

function timeAxisTickHtml(range, scroller) {
  let html = "";
  for (const tick of visibleTimeTicks(range, scroller)) {
    html += `<b class="${tick.className}" style="left:${tick.left}px">${escapeHtml(tick.label)}</b>`;
  }
  return html;
}

function timeGridTickHtml(range, scroller) {
  let html = "";
  for (const tick of visibleTimeTicks(range, scroller)) {
    html += `<i style="left:${tick.left}px"></i>`;
  }
  return html;
}

function visibleTimeTicks(range, scroller) {
  const bounds = visibleTimeTickBounds(range, scroller);
  const ticks = [];
  for (let i = bounds.start; i <= bounds.end; i += bounds.step) {
    ticks.push({
      left: i * timeGridWidthPx,
      label: formatScaleMs(Math.min(range, state.timeScaleMs * i)),
      className: i === 0 ? "edge-start" : "",
    });
  }
  const endLeft = timeWidthPx(range);
  if (endLeft >= bounds.leftPx && endLeft <= bounds.rightPx && bounds.ticks % bounds.step !== 0) {
    ticks.push({
      left: endLeft,
      label: formatScaleMs(range),
      className: "edge-end",
    });
  }
  return ticks;
}

function visibleTimeTickBounds(range, scroller) {
  const ticks = Math.ceil(range / state.timeScaleMs);
  const step = Math.max(1, Math.ceil(92 / timeGridWidthPx));
  const scrollLeft = scroller ? scroller.scrollLeft : state.timeScrollLeft;
  const width = scroller && scroller.clientWidth > 0 ? Math.max(1, scroller.clientWidth - traceLabelColumnPx) : 1600;
  const leftPx = Math.max(0, scrollLeft - width);
  const rightPx = Math.min(timeWidthPx(range), scrollLeft + width * 2);
  const start = Math.max(0, Math.floor(leftPx / timeGridWidthPx / step) * step);
  const end = Math.min(ticks, Math.ceil(rightPx / timeGridWidthPx / step) * step);
  return { ticks, step, start, end, leftPx, rightPx };
}

function formatRelativeMs(ms) {
  if (ms < 0.001) return `${(ms * 1000000).toFixed(0)}ns`;
  if (ms < 1) return `${(ms * 1000).toFixed(ms < 0.1 ? 1 : 0)}us`;
  if (ms < 1000) return `${ms < 10 ? ms.toFixed(2) : ms < 100 ? ms.toFixed(1) : Math.round(ms)}ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`;
  const minutes = Math.floor(ms / 60000);
  const seconds = Math.floor((ms % 60000) / 1000);
  return `${minutes}m${String(seconds).padStart(2, "0")}s`;
}

function formatScaleMs(ms) {
  if (state.timeScaleMs < 0.001) return `${(ms * 1000000).toFixed(0)}ns`;
  if (state.timeScaleMs < 1) return `${(ms * 1000).toFixed(ms * 1000 < 10 ? 1 : 0)}us`;
  if (state.timeScaleMs < 1000) return `${ms.toFixed(ms < 10 ? 2 : ms < 100 ? 1 : 0)}ms`;
  return `${(ms / 1000).toFixed(ms < 10000 ? 2 : 1)}s`;
}

function formatAnchorTime(ms) {
  const step = state.timeScaleMs / 100;
  const rounded = Math.round(ms / step) * step;
  if (state.timeScaleMs < 0.001) return `${(rounded * 1000000).toFixed(0)}ns`;
  if (state.timeScaleMs < 1) {
    const value = rounded * 1000;
    const decimals = state.timeScaleMs <= 0.01 ? 2 : 1;
    return `${value.toFixed(decimals)}us`;
  }
  if (state.timeScaleMs < 1000) {
    const decimals = state.timeScaleMs <= 1 ? 3 : state.timeScaleMs <= 10 ? 2 : 1;
    return `${rounded.toFixed(decimals)}ms`;
  }
  return `${(rounded / 1000).toFixed(3)}s`;
}

function relativeRangeTitle(label, start, end, captureStart) {
  const startLabel = Number.isFinite(start) ? formatScaleMs(start - captureStart) : "-";
  const endLabel = Number.isFinite(end) ? formatScaleMs(end - captureStart) : "-";
  return `${label}: ${startLabel} - ${endLabel}`;
}

async function load() {
  if (loadInFlight) return;
  loadInFlight = true;
  try {
    const res = await fetch("/debug/analysis", { cache: "no-store" });
    if (!res.ok) throw new Error(res.statusText);
    const aggregate = await res.json();
    if (!aggregate || !Array.isArray(aggregate.pops)) throw new Error("unexpected aggregate response");
    state.aggregate = aggregate;
    state.samples = [...state.samples, aggregate];
    state.lastStatus = null;
    stopAutoRefreshIfStopped();
  } catch (err) {
    state.lastStatus = err instanceof Error ? err.message : String(err);
    if (!state.samples.length) state.samples = [state.aggregate];
  } finally {
    loadInFlight = false;
  }
  renderWhenChartScrollIdle();
}

function stopAutoRefreshIfStopped() {
  const status = analysisStatus(state.aggregate);
  if (status.label !== "STOPPED") {
    return;
  }
  state.autoRefresh = false;
  $("autoRefresh").checked = false;
  scheduleAutoRefresh();
}

async function post(path) {
  try {
    const res = await fetch(path, { method: "POST" });
    if (!res.ok) throw new Error(res.statusText);
    state.lastStatus = null;
    await load();
  } catch (err) {
    state.lastStatus = err instanceof Error ? err.message : String(err);
    render();
  }
}

function exportJSON() {
  const snapshots = state.samples.length ? state.samples : [state.aggregate];
  const payload = {
    version: 1,
    exportedAt: new Date().toISOString(),
    snapshots,
  };
  const blob = new Blob([JSON.stringify(payload, null, 2)], { type: "application/json" });
  const a = document.createElement("a");
  a.href = URL.createObjectURL(blob);
  a.download = `ncdn-analysis-${Date.now()}.json`;
  a.click();
  URL.revokeObjectURL(a.href);
}

async function importJSON(file) {
  if (!file) return;
  const payload = JSON.parse(await file.text());
  const snapshots = importedSnapshots(payload);
  state.samples = snapshots;
  state.aggregate = snapshots[snapshots.length - 1];
  state.autoRefresh = false;
  $("autoRefresh").checked = false;
  scheduleAutoRefresh();
  state.lastStatus = `imported ${snapshots.length} snapshot${snapshots.length === 1 ? "" : "s"}`;
  render();
}

function importedSnapshots(payload) {
  if (payload && Array.isArray(payload.snapshots)) {
    const snapshots = payload.snapshots.filter((snapshot) => snapshot && Array.isArray(snapshot.pops));
    if (snapshots.length) return snapshots;
  }
  if (payload && Array.isArray(payload.pops)) {
    return [payload];
  }
  throw new Error("unexpected aggregate response");
}

function escapeHtml(value) {
  return String(value).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function escapeAttr(value) {
  return escapeHtml(value);
}

function demoAggregate() {
  const now = Date.now();
  const key = (uri, chunk) => `GET\u0000origin\u0000${uri}${chunk === null ? "" : `\u0000${chunk}`}`;
  const mk = (requestId, cacheKey, result, startOffset, dur, bytes) => ({
    requestId,
    cacheKey,
    start: 0,
    end: bytes - 1,
    result,
    producedBytes: bytes,
    contentLength: bytes,
    statusCode: cacheKey.includes("\u0000") ? 206 : 200,
    startTime: new Date(now + startOffset).toISOString(),
    headerTime: new Date(now + startOffset + 34).toISOString(),
    endTime: new Date(now + startOffset + dur).toISOString(),
    state: "DONE",
  });
  return {
    capturedAt: new Date(now + 5000).toISOString(),
    pops: ["POP0", "POP1", "POP2"].map((id, popIndex) => {
      const keys = {
        [key("/video/big.mp4", 0)]: {
          requests: [
            mk(12 + popIndex, key("/video/big.mp4", 0), popIndex === 1 ? "MISS" : "HIT", 100 + popIndex * 140, 580, 1048576),
            mk(20 + popIndex, key("/video/big.mp4", 0), "COLLAPSED", 360 + popIndex * 100, 300, 1048576),
          ],
          origins: popIndex === 1 ? [mk(31, key("/video/big.mp4", 0), "MISS", 100, 820, 1048576)] : [],
        },
        [key("/video/big.mp4", 1048576)]: {
          requests: [mk(50 + popIndex, key("/video/big.mp4", 1048576), "HIT", 900 + popIndex * 160, 360, 1048576)],
          origins: popIndex === 0 ? [mk(61, key("/video/big.mp4", 1048576), "MISS", 760, 760, 1048576)] : [],
        },
        [key("/api/data", null)]: {
          requests: [mk(90 + popIndex, key("/api/data", null), "SWR", 1240 + popIndex * 120, 190, 32768)],
          origins: [],
        },
      };
      const traces = Object.values(keys);
      return {
        id,
        url: `http://pop${popIndex}:8889`,
        snapshot: {
          requestBytes: traces.reduce((sum, item) => sum + sumBytes(item.requests), 0),
          originBytes: traces.reduce((sum, item) => sum + sumBytes(item.origins), 0),
          enabled: true,
          startedAt: new Date(now).toISOString(),
          keys,
        },
      };
    }),
  };
}

document.addEventListener("click", (event) => {
  const canvasScroller = event.target.closest("[data-canvas-chart]");
  if (canvasScroller) {
    handleCanvasChartClick(event, canvasScroller);
    return;
  }
  const timeTrack = event.target.closest(".time-track");
  if (timeTrack) {
    setTimeZoomAnchor(event);
  }
  const positionTrack = event.target.closest(".position-track");
  if (positionTrack) {
    setPositionZoomAnchor(event);
  }
  const overviewButton = event.target.closest("[data-overview]");
  if (overviewButton) {
    state.overview = overviewButton.dataset.overview;
    render();
    return;
  }
  const popButton = event.target.closest("[data-pop]");
  if (popButton) {
    state.selectedPop = popButton.dataset.pop;
    render();
    return;
  }
  const uriRow = event.target.closest("[data-uri]");
  if (uriRow) {
    state.selectedUri = uriRow.dataset.uri;
    render();
    return;
  }
  const requestRow = event.target.closest("[data-request-row]");
  if (requestRow) {
    state.selectedRequestKey = requestRow.dataset.requestRow;
    render();
  }
});

function handleCanvasChartClick(event, scroller) {
  const config = canvasCharts.get(scroller.dataset.canvasChart);
  if (!config) return;
  if (config.kind === "position-origin" || config.kind === "position-request") {
    const point = positionCanvasPointFromEvent(event, scroller);
    if (!point) return;
    const { x, y, width, horizontalScroller } = point;
    state.positionZoomAnchorRatio = width > 0 ? x / width : 0.5;
    state.positionZoomAnchorOffsetPx = x;
    state.positionZoomAnchorBytes = Math.max(0, (horizontalScroller.scrollLeft + x) / positionGridWidthPx * state.positionScaleBytes);
    state.positionRestoreAnchorBytes = state.positionZoomAnchorBytes;
    const rowIndex = Math.floor((scroller.scrollTop + y) / traceRowHeightPx);
    const rowY = (scroller.scrollTop + y) - rowIndex * traceRowHeightPx;
    const row = config.rows[rowIndex];
    if (row && rowY >= 0 && rowY <= traceBarHeightPx) {
      state.selectedRequestKey = `${row.popId}:${row.requestId}`;
    }
    render();
    return;
  }

  const point = timeCanvasPointFromEvent(event, scroller);
  if (!point) return;
  const { x, width, horizontalScroller } = point;
  state.timeZoomAnchorRatio = width > 0 ? x / width : 0.5;
  state.timeZoomAnchorOffsetPx = x;
  state.timeZoomAnchorMs = Math.max(0, (horizontalScroller.scrollLeft + x) / timeGridWidthPx * state.timeScaleMs);
  state.timeRestoreAnchorMs = state.timeZoomAnchorMs;
  render();
}

function positionCanvasPointFromEvent(event, scroller) {
  const horizontalScroller = scroller.closest(".position-scroll");
  if (!horizontalScroller) return null;
  const horizontalRect = horizontalScroller.getBoundingClientRect();
  const scrollerRect = scroller.getBoundingClientRect();
  const left = horizontalRect.left + horizontalScroller.clientLeft + traceLabelColumnPx;
  const top = scrollerRect.top + scroller.clientTop;
  const width = Math.max(1, horizontalScroller.clientWidth - traceLabelColumnPx);
  const height = scroller.clientHeight;
  const x = event.clientX - left;
  const y = event.clientY - top;
  if (x < 0 || x > width || y < 0 || y > height) {
    return null;
  }
  return {
    x,
    y,
    width,
    height,
    horizontalScroller,
  };
}

function timeCanvasPointFromEvent(event, scroller) {
  const horizontalScroller = scroller.closest(".time-scroll");
  if (!horizontalScroller) return null;
  const horizontalRect = horizontalScroller.getBoundingClientRect();
  const scrollerRect = scroller.getBoundingClientRect();
  const left = horizontalRect.left + horizontalScroller.clientLeft + traceLabelColumnPx;
  const top = scrollerRect.top + scroller.clientTop;
  const width = Math.max(1, horizontalScroller.clientWidth - traceLabelColumnPx);
  const height = scroller.clientHeight;
  const x = event.clientX - left;
  const y = event.clientY - top;
  if (x < 0 || x > width || y < 0 || y > height) {
    return null;
  }
  return {
    x,
    y,
    width,
    height,
    horizontalScroller,
  };
}

$("uriFilter").addEventListener("input", (event) => {
  state.filter = event.target.value;
  render();
});

$("sortBy").addEventListener("change", (event) => {
  state.sortBy = event.target.value;
  render();
});

$("timeScaleSlider").addEventListener("input", (event) => {
  updateTimeScale(Number(event.target.value) || 0);
});

$("timeViewMode").addEventListener("change", (event) => {
  state.timeViewMode = event.target.value === "detail" ? "detail" : "simple";
  state.traceYScroll["time-origin"] = 0;
  render();
});

function updateTimeScale(sliderValue) {
  if (state.timeZoomAnchorMs === null) {
    state.timeRestoreAnchorMs = currentTimeScrollMs();
    state.timeZoomAnchorRatio = 0;
  } else {
    state.timeRestoreAnchorMs = state.timeZoomAnchorMs;
  }
  state.timeScaleIndex = timeScaleValuesMs.length - 1 - sliderValue;
  state.timeScaleMs = timeScaleValuesMs[state.timeScaleIndex] || 100;
  render();
}

$("positionScaleSlider").addEventListener("input", (event) => {
  if (state.positionZoomAnchorBytes === null) {
    state.positionRestoreAnchorBytes = currentPositionScrollBytes();
    state.positionZoomAnchorRatio = 0;
  } else {
    state.positionRestoreAnchorBytes = state.positionZoomAnchorBytes;
  }
  state.positionScaleIndex = positionScaleValuesBytes.length - 1 - (Number(event.target.value) || 0);
  state.positionScaleBytes = positionScaleValuesBytes[state.positionScaleIndex] || 1048576;
  render();
});

function currentTimeScrollMs() {
  const scroller = document.querySelector(".time-scroll");
  if (!scroller) return 0;
  return Math.max(0, scroller.scrollLeft / timeGridWidthPx * state.timeScaleMs);
}

function setTimeZoomAnchor(event) {
  const scroller = event.target.closest(".time-scroll") || document.querySelector(".time-scroll");
  if (!scroller) return;
  const rect = scroller.getBoundingClientRect();
  const width = Math.max(1, scroller.clientWidth - traceLabelColumnPx);
  const x = Math.max(0, event.clientX - rect.left - traceLabelColumnPx);
  state.timeZoomAnchorRatio = width > 0 ? x / width : 0.5;
  state.timeZoomAnchorOffsetPx = x;
  state.timeZoomAnchorMs = Math.max(0, (scroller.scrollLeft + x) / timeGridWidthPx * state.timeScaleMs);
  state.timeRestoreAnchorMs = state.timeZoomAnchorMs;
  render();
}

function currentPositionScrollBytes() {
  const scroller = document.querySelector(".position-scroll");
  if (!scroller) return 0;
  return Math.max(0, scroller.scrollLeft / positionGridWidthPx * state.positionScaleBytes);
}

function setPositionZoomAnchor(event) {
  const scroller = event.target.closest(".position-scroll") || document.querySelector(".position-scroll");
  if (!scroller) return;
  const rect = scroller.getBoundingClientRect();
  const width = Math.max(1, scroller.clientWidth - traceLabelColumnPx);
  const x = Math.max(0, event.clientX - rect.left - traceLabelColumnPx);
  state.positionZoomAnchorRatio = width > 0 ? x / width : 0.5;
  state.positionZoomAnchorOffsetPx = x;
  state.positionZoomAnchorBytes = Math.max(0, (scroller.scrollLeft + x) / positionGridWidthPx * state.positionScaleBytes);
  state.positionRestoreAnchorBytes = state.positionZoomAnchorBytes;
  render();
}

$("autoRefresh").addEventListener("change", (event) => {
  state.autoRefresh = event.target.checked;
  scheduleAutoRefresh();
});
$("refreshIntervalMs").addEventListener("input", (event) => {
  state.refreshIntervalMs = refreshIntervalMs(event.target.value);
  event.target.value = String(state.refreshIntervalMs);
  scheduleAutoRefresh();
});
$("refreshButton").addEventListener("click", load);
$("stopButton").addEventListener("click", () => post("/debug/analysis/stop"));
$("startButton").addEventListener("click", () => {
  const maxRequestIds = Math.max(0, Number($("maxRequestIds").value || 10000));
  state.samples = [];
  state.selectedRequestKey = "";
  state.autoRefresh = true;
  $("autoRefresh").checked = true;
  scheduleAutoRefresh();
  post(`/debug/analysis/start?maxRequestIds=${encodeURIComponent(maxRequestIds)}`);
});
$("exportButton").addEventListener("click", exportJSON);
$("importButton").addEventListener("click", () => $("importFile").click());
$("importFile").addEventListener("change", (event) => importJSON(event.target.files && event.target.files[0]).catch((err) => {
  state.lastStatus = err instanceof Error ? err.message : String(err);
  render();
}));

function refreshIntervalMs(value) {
  const ms = Number(value);
  if (!Number.isFinite(ms)) return 1000;
  return Math.max(100, Math.round(ms));
}

function scheduleAutoRefresh() {
  if (refreshTimer !== null) {
    window.clearTimeout(refreshTimer);
    refreshTimer = null;
  }
  if (!state.autoRefresh) return;
  refreshTimer = window.setTimeout(() => {
    refreshTimer = null;
    scheduleAutoRefresh();
    load();
  }, state.refreshIntervalMs);
}

render();
load().finally(scheduleAutoRefresh);
