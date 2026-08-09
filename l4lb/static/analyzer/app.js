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
  timeZoomAnchorRatio: 0.5,
  positionScaleBytes: 16 * 1048576,
  positionScaleIndex: 2,
  positionScrollLeft: 0,
  positionRestoreAnchorBytes: null,
  positionZoomAnchorBytes: null,
  positionZoomAnchorRatio: 0.5,
  traceYScroll: {},
  filter: "",
  sortBy: "requests",
  autoRefresh: true,
  refreshIntervalMs: 1000,
  lastStatus: "demo data",
};

const $ = (id) => document.getElementById(id);
let syncingTimeScroll = false;
let syncingPositionScroll = false;
let syncingTraceYScroll = false;
let refreshTimer = null;
let loadInFlight = false;

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
  const flat = flatten(state.aggregate);
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
  renderMetrics(flat);
  renderOverview(rows, flat);
  renderCharts(selected, reqs, origins);
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

function renderMetrics(flat) {
  const totalReqs = flat.requests.length;
  const totalOrigins = flat.origins.length;
  const bytes = sampleBytes(state.aggregate);
  const clientBytes = bytes.client;
  const originBytes = bytes.origin;
  const recentReqs = state.samples.slice(-12).map((sample) => flatten(sample).requests.length);
  const recentOrigins = state.samples.slice(-12).map((sample) => flatten(sample).origins.length);
  const throughput = throughputSamples(state.samples);
  const latestThroughput = throughput[throughput.length - 1] || { origin: 0, client: 0, intervalMs: 0 };
  const resultCounts = ["HIT", "COLLAPSED", "MISS", "SWR", "SIE"].map((name) => ({
    name,
    value: flat.requests.filter((trace) => trace.result === name).length,
  }));
  $("metrics").innerHTML = [
    metricCard("REQUESTS", totalReqs.toLocaleString(), `${hitRatio(flat.requests).toFixed(1)}% hit`, recentReqs),
    metricCard("ORIGIN FETCH", totalOrigins.toLocaleString(), formatBytes(originBytes), recentOrigins),
    metricCard("ORIGIN THROUGHPUT", formatRate(latestThroughput.origin), throughputDetail(latestThroughput), throughput.map((sample) => sample.origin)),
    metricCard("CLIENT THROUGHPUT", formatRate(latestThroughput.client), throughputDetail(latestThroughput), throughput.map((sample) => sample.client)),
    donutCard(resultCounts),
    popTable(flat),
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

function popTable(flat) {
  const rows = (state.aggregate.pops || []).map((pop) => {
    const reqs = flat.requests.filter((trace) => trace.popId === pop.id);
    const bytes = popSampleBytes(state.aggregate, pop.id);
    return `<div class="pop-row"><span>${escapeHtml(pop.id)}</span><b>${formatBytes(bytes.origin)}</b><b>${formatBytes(bytes.client)}</b><b>${hitRatio(reqs).toFixed(1)}%</b></div>`;
  }).join("");
  return `<section class="panel pop-table"><div class="metric-title">STATUS (by PoP)</div><div class="pop-row"><span>PoP</span><span>Origin Data</span><span>Client Data</span><span>Hit</span></div>${rows}</section>`;
}

function renderOverview(rows, flat) {
  if (state.overview === "pop") {
    renderPopOverview(flat);
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

function renderPopOverview(flat) {
  $("overviewHead").className = "overview-head pop-overview-grid";
  $("overviewHead").innerHTML = "<span>PoP</span><span>Status</span><span>Requests</span><span>Origin Fetch</span><span>Client Throughput</span><span>Origin Throughput</span><span>Hit Ratio</span>";
  const pops = state.aggregate.pops || [];
  const summaryRows = pops.map((pop) => {
    const reqs = flat.requests.filter((trace) => trace.popId === pop.id);
    const origins = flat.origins.filter((trace) => trace.popId === pop.id);
    const status = pop.error ? pop.error : pop.snapshot && pop.snapshot.enabled ? "RECORDING" : "STOPPED";
    const history = popHistory(pop.id);
    const latest = history.throughput[history.throughput.length - 1] || { client: 0, origin: 0 };
    return `<button class="overview-row pop-overview-grid" data-pop="${escapeAttr(pop.id)}">
      <span>${escapeHtml(pop.id)}</span><span>${escapeHtml(status)}</span><span>${reqs.length}</span><span>${origins.length}</span>
      <span>${formatRate(latest.client)}</span><span>${formatRate(latest.origin)}</span><span>${hitRatio(reqs).toFixed(1)}%</span>
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
  const requests = samples.map((sample) => flatten(sample).requests.filter((trace) => trace.popId === popId).length);
  const throughput = [];
  for (let i = 1; i < samples.length; i++) {
    const prev = popSampleBytes(samples[i - 1], popId);
    const curr = popSampleBytes(samples[i], popId);
    const intervalMs = parseTimestampMs(samples[i].capturedAt || "") - parseTimestampMs(samples[i - 1].capturedAt || "");
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

function renderCharts(selected, reqs, origins) {
  document.querySelector(".timeline-panel").classList.toggle("hidden", state.overview !== "uri");
  if (state.overview !== "uri") {
    return;
  }

  rememberActiveChartScroll();
  $("timelineTitle").innerHTML = `CHUNK POSITION VIEW<small>${selected ? escapeHtml(selected.uri) : "-"}</small>`;
  renderPositionScaleControl();
  const positionReqs = positionTraces(reqs);
  const positionOrigins = positionTraces(origins);
  const max = Math.max(1, ...positionReqs.concat(positionOrigins).map((trace) => {
    const base = trace.chunkStart === null ? Number(trace.start || 0) : Number(trace.chunkStart || 0);
    return base + Number(trace.contentLength || trace.producedBytes || Math.max(1, trace.end - trace.start + 1));
  }));
  ensureSelectedRequest(reqs, origins);
  $("timelineBody").innerHTML = positionView(positionOrigins, positionReqs, max);
  renderLinkedTimeView(origins, reqs);
  restorePositionScroll();
  restoreTimeScroll();
  restoreTraceYScroll();
}

function positionTraces(traces) {
  return traces.filter((trace) => !isHeadTrace(trace));
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
  return reqs.map((request) => ({
    request,
    origin: originForRequestTrace(request, origins),
  }));
}

function originForRequestTrace(request, origins) {
  if ((request.result || "") === "HIT") return null;
  const producerRequestId = Number(request.producerRequestId || request.requestId || 0);
  const producerCacheKey = request.producerCacheKey || request.cacheKey || "";
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
  const scrollers = [...document.querySelectorAll(".position-scroll")];
  if (!scrollers.length) return;
  const restoreLeft = state.positionRestoreAnchorBytes !== null
    ? Math.max(0, positionWidthPx(state.positionRestoreAnchorBytes) - scrollers[0].clientWidth * state.positionZoomAnchorRatio)
    : state.positionScrollLeft;
  for (const scroller of scrollers) {
    scroller.scrollLeft = restoreLeft;
  }
  if (state.positionRestoreAnchorBytes !== null) {
    state.positionScrollLeft = restoreLeft;
  }
  state.positionRestoreAnchorBytes = null;
  for (const scroller of scrollers) {
    scroller.onscroll = () => {
      if (syncingPositionScroll) return;
      syncingPositionScroll = true;
      for (const other of scrollers) {
        if (other !== scroller) {
          other.scrollLeft = scroller.scrollLeft;
        }
      }
      state.positionScrollLeft = scroller.scrollLeft;
      syncingPositionScroll = false;
    };
  }
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
          }
        }
        syncingTraceYScroll = false;
      });
    }
  }
}

function restoreTimeScroll() {
  const scrollers = [...document.querySelectorAll(".time-scroll")];
  if (!scrollers.length) return;
  const restoreLeft = state.timeRestoreAnchorMs !== null
    ? Math.max(0, timeWidthPx(state.timeRestoreAnchorMs) - scrollers[0].clientWidth * state.timeZoomAnchorRatio)
    : state.timeScrollLeft;
  for (const scroller of scrollers) {
    scroller.scrollLeft = restoreLeft;
    refreshTimeTicks(scroller);
  }
  if (state.timeRestoreAnchorMs !== null) {
    state.timeScrollLeft = restoreLeft;
  }
  state.timeRestoreAnchorMs = null;
  for (const scroller of scrollers) {
    scroller.onscroll = () => {
      if (syncingTimeScroll) return;
      syncingTimeScroll = true;
      for (const other of scrollers) {
        if (other !== scroller) {
          other.scrollLeft = scroller.scrollLeft;
          refreshTimeTicks(other);
        }
      }
      state.timeScrollLeft = scroller.scrollLeft;
      refreshTimeTicks(scroller);
      syncingTimeScroll = false;
    };
  }
}

function positionView(origins, reqs, maxBytes) {
  const widthPx = Math.max(1600, Math.ceil(maxBytes / state.positionScaleBytes) * positionGridWidthPx);
  const rows = positionRows(reqs, origins);
  const trackStyle = `--position-width:${widthPx}px;--track-width:${widthPx}px;--grid-width:${positionGridWidthPx}px`;
  return `<div class="position-view-stack">
    ${positionSection("ORIGIN FETCH", "position-origin", positionLabels(rows), positionOriginBars(rows, widthPx), maxBytes, widthPx, trackStyle)}
    ${positionSection("CLIENT REQUESTS", "position-request", positionLabels(rows), positionRequestBars(rows, widthPx), maxBytes, widthPx, trackStyle)}
  </div>`;
}

function positionSection(title, scrollGroup, labels, bars, maxBytes, widthPx, trackStyle) {
  return `<div class="trace-section position-trace-section">
    <div class="trace-section-head">
      <div class="position-labels"><div class="position-axis-spacer"></div><h3>${escapeHtml(title)}</h3></div>
      <div class="position-scroll trace-axis-scroll">
        <div class="position-track" style="${trackStyle}">
          ${positionMarker(widthPx)}
          ${positionAxis(maxBytes)}
        </div>
      </div>
    </div>
    <div class="trace-section-body">
      <div class="position-labels trace-y-scroll trace-label-scroll" data-y-scroll="${escapeAttr(scrollGroup)}">${labels}</div>
      <div class="position-scroll trace-y-scroll trace-chart-scroll" data-y-scroll="${escapeAttr(scrollGroup)}">
        <div class="position-track" style="${trackStyle}">
          ${positionMarker(widthPx)}
          ${positionGrid(maxBytes, "time-grid body-grid")}
          <div class="chart">${bars}</div>
        </div>
      </div>
    </div>
  </div>`;
}

function positionRows(reqs, origins) {
  const rows = new Map();
  for (const request of reqs) {
    const key = requestKey(request);
    if (!rows.has(key)) {
      rows.set(key, { popId: request.popId, requestId: request.requestId, requests: [], origins: [] });
    }
    const row = rows.get(key);
    row.requests.push(request);
    const origin = originForRequestTrace(request, origins);
    if (origin && !row.origins.some((item) => traceKey(item) === traceKey(origin))) {
      row.origins.push(origin);
    }
  }
  return [...rows.values()].map((row) => ({
    ...row,
    requests: row.requests.sort(compareTimeViewTrace),
    origins: row.origins.sort(compareTimeViewTrace),
  })).sort((a, b) => a.requestId - b.requestId || a.popId.localeCompare(b.popId));
}

function positionLabels(rows) {
  if (!rows.length) return `<div class="time-label-row empty">No trace</div>`;
  return rows.map((row) => {
    return `<button class="time-label-row" data-request-row="${escapeAttr(`${row.popId}:${row.requestId}`)}">#${row.requestId} (${escapeHtml(row.popId)})</button>`;
  }).join("");
}

function positionOriginBars(rows, widthPx) {
  if (!rows.length) return `<div class="empty position-bar-row" style="--position-width:${widthPx}px"></div>`;
  return rows.map((row) => {
    const segments = row.origins.map((trace) => positionSegment(trace, false)).join("");
    return `<button class="position-bar-row" data-request-row="${escapeAttr(`${row.popId}:${row.requestId}`)}" style="--position-width:${widthPx}px"><i class="bar-bg">${segments}</i></button>`;
  }).join("");
}

function positionRequestBars(rows, widthPx) {
  if (!rows.length) return `<div class="empty position-bar-row" style="--position-width:${widthPx}px"></div>`;
  return rows.map((row) => {
    const segments = row.requests.map((trace) => positionSegment(trace, true)).join("");
    return `<button class="position-bar-row" data-request-row="${escapeAttr(`${row.popId}:${row.requestId}`)}" style="--position-width:${widthPx}px"><i class="bar-bg">${segments}</i></button>`;
  }).join("");
}

function positionSegment(trace, showResult) {
  const start = trace.chunkStart === null || trace.chunkStart === undefined ? Number(trace.start || 0) : Number(trace.chunkStart || 0);
  const widthBytes = Number(trace.contentLength || trace.producedBytes || Math.max(1, trace.end - trace.start + 1));
  const result = showResult ? trace.result || "UNKNOWN" : "UNKNOWN";
  const label = showResult ? result : "FETCH";
  const segmentWidth = Math.max(1, positionWidthPx(widthBytes));
  const chunkLabel = segmentWidth >= 24 ? chunkIdLabel(trace) : "";
  return `<b class="segment result-${escapeAttr(result)}" title="${escapeAttr(`${label}: ${formatBytes(start)} - ${formatBytes(start + widthBytes)}`)}" style="left:${positionWidthPx(start)}px;width:${segmentWidth}px">${escapeHtml(chunkLabel)}</b>`;
}

function chunkIdLabel(trace) {
  if (isHeadTrace(trace)) return "HEAD";
  if (trace.chunkStart === null || trace.chunkStart === undefined) return "";
  const start = Number(trace.chunkStart);
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
  const ticks = Math.ceil(maxBytes / state.positionScaleBytes);
  const labelStep = positionTickStep(maxBytes);
  let html = `<div class="position-axis"><i class="bar-bg">`;
  for (let i = 0; i <= ticks; i += labelStep) {
    html += `<b class="${i === 0 ? "edge-start" : ""}" style="left:${i * positionGridWidthPx}px">${escapeHtml(formatBytes(state.positionScaleBytes * i))}</b>`;
  }
  if (ticks % labelStep !== 0) {
    html += `<b class="edge-end" style="left:${positionWidthPx(maxBytes)}px">${escapeHtml(formatBytes(maxBytes))}</b>`;
  }
  return `${html}</i></div>`;
}

function positionGrid(maxBytes, className = "time-grid") {
  const ticks = Math.ceil(maxBytes / state.positionScaleBytes);
  const labelStep = positionTickStep(maxBytes);
  let html = `<div class="${className}">`;
  for (let i = 0; i <= ticks; i += labelStep) {
    html += `<i style="left:${i * positionGridWidthPx}px"></i>`;
  }
  if (ticks % labelStep !== 0) {
    html += `<i style="left:${positionWidthPx(maxBytes)}px"></i>`;
  }
  return `${html}</div>`;
}

function positionTickStep(maxBytes) {
  return Math.max(1, Math.ceil(Math.ceil(maxBytes / state.positionScaleBytes) / 600));
}

function positionWidthPx(bytes) {
  return bytes / state.positionScaleBytes * positionGridWidthPx;
}

function timeView(rows, start, end, range) {
  const widthPx = Math.max(1600, Math.ceil(range / state.timeScaleMs) * timeGridWidthPx);
  const marker = timeMarker(widthPx);
  const trackStyle = `--time-width:${widthPx}px;--track-width:${widthPx}px;--grid-width:${timeGridWidthPx}px`;
  return `<div class="time-client-pinned">
    <div class="time-labels">
      <div class="time-axis-spacer"></div>
      <h3>CLIENT REQUESTS</h3>
      ${timeRequestLabels(rows)}
    </div>
    <div class="time-scroll">
      <div class="time-track" data-range="${range}" style="${trackStyle}">
        ${marker}
        ${timeAxis(range)}
        ${timeGrid(range)}
        <div class="time-section-spacer"></div>
        <div class="chart">${timeRequestBars(rows, start, end, widthPx)}</div>
      </div>
    </div>
  </div>
  ${timeOriginSection(rows, start, end, range, widthPx, marker, trackStyle)}
  ${throughputView(start, end, range, widthPx, marker, trackStyle)}`;
}

function timeOriginSection(rows, start, end, range, widthPx, marker, trackStyle) {
  return `<div class="trace-section time-origin-section">
    <div class="trace-section-head">
      <div class="time-labels"><div class="time-axis-spacer"></div><h3>ORIGIN FETCH</h3></div>
      <div class="time-scroll trace-axis-scroll">
        <div class="time-track" data-range="${range}" style="${trackStyle}">
          ${marker}
          ${timeAxis(range)}
        </div>
      </div>
    </div>
    <div class="trace-section-body">
      <div class="time-labels trace-y-scroll trace-label-scroll" data-y-scroll="time-origin">${timeOriginLabels(rows)}</div>
      <div class="time-scroll trace-y-scroll trace-chart-scroll" data-y-scroll="time-origin">
        <div class="time-track" data-range="${range}" style="${trackStyle}">
          ${marker}
          ${timeGrid(range, "time-grid body-grid")}
          <div class="chart">${timeOriginBars(rows, start, end, widthPx)}</div>
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
    <div class="time-scroll">
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
  for (let i = 1; i < samples.length; i++) {
    const prevAt = parseTimestampMs(samples[i - 1].capturedAt || "");
    const currAt = parseTimestampMs(samples[i].capturedAt || "");
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
  const flat = flatten(sample);
  const requests = flat.requests.filter((trace) => requestKey(trace) === state.selectedRequestKey);
  const origins = [];
  for (const request of requests) {
    const origin = originForRequestTrace(request, flat.origins);
    if (origin && !origins.some((item) => traceKey(item) === traceKey(origin))) {
      origins.push(origin);
    }
  }
  return {
    client: sumBytes(requests),
    origin: sumBytes(origins),
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
    return `<button class="time-label-row" data-trace="${escapeAttr(traceKey(trace))}">${escapeHtml(traceChunkLabel(trace))}</button>`;
  }).join("");
}

function timeOriginLabels(rows) {
  const originRows = rows.filter((row) => row.origin);
  return timeRowLabels(originRows);
}

function timeRequestLabels(rows) {
  if (!rows.length) return `<div class="time-label-row empty">No trace</div>`;
  return `<button class="time-label-row request-label" data-request-row="${escapeAttr(requestKey(rows[0].request))}">req</button>`;
}

function timeOriginBars(rows, start, end, widthPx) {
  const originRows = rows.filter((row) => row.origin);
  if (!originRows.length) return `<div class="empty time-bar-row" style="--time-width:${widthPx}px"></div>`;
  return originRows.map((row) => {
    const segments = timeSegments(row.origin, start, end, false);
    return `<button class="time-bar-row" data-trace="${escapeAttr(traceKey(row.request))}" style="--time-width:${widthPx}px"><i class="bar-bg">${segments}</i></button>`;
  }).join("");
}

function timeRequestBars(rows, start, end, widthPx) {
  if (!rows.length) return `<div class="empty time-bar-row" style="--time-width:${widthPx}px"></div>`;
  const segments = rows.map((row) => timeSegments(row.request, start, end, true)).join("");
  return `<button class="time-bar-row request-row" data-request-row="${escapeAttr(requestKey(rows[0].request))}" style="--time-width:${widthPx}px"><i class="bar-bg">${segments}</i></button>`;
}

function timeSegments(trace, start, end, showResult, layerClass = "") {
  const s = parseTimestampMs(trace.startTime || "");
  const h = parseTimestampMs(trace.headerTime || "");
  const e = parseTimestampMs(trace.endTime || "");
  const left = Number.isFinite(s) ? timeLeftPx(s - start) : 0;
  const headerWidth = Number.isFinite(h) && Number.isFinite(s) ? Math.max(1, timeWidthPx(h - s)) : 2;
  const includeHeaderWait = !showResult;
  const bodyEnd = Number.isFinite(e) ? e : end;
  const result = showResult ? trace.result || "UNKNOWN" : "UNKNOWN";
  const bodyLabel = showResult ? result : "FETCH";
  const segmentTimeStart = Number.isFinite(h) ? h : s;
  const bodyStart = timeLeftPx(segmentTimeStart - start);
  const bodyWidth = Number.isFinite(bodyEnd) ? Math.max(1, timeWidthPx(bodyEnd - segmentTimeStart)) : 4;
  const chunkLabel = traceChunkLabel(trace);
  const layer = layerClass ? ` ${layerClass}` : "";
  const header = includeHeaderWait ? `<b class="segment wait-segment${layer}" title="${escapeAttr(relativeRangeTitle("HEADER_WAIT", s, h, start))}" style="left:${left}px;width:${headerWidth}px"></b>` : "";
  return `${header}<b class="segment result-${escapeAttr(result)}${layer}" title="${escapeAttr(relativeRangeTitle(bodyLabel, segmentTimeStart, bodyEnd, start))}" style="left:${bodyStart}px;width:${bodyWidth}px">${escapeHtml(chunkLabel)}</b>`;
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
  $("timeScaleSlider").max = String(timeScaleValuesMs.length - 1);
  $("timeScaleSlider").value = String(timeScaleValuesMs.length - 1 - state.timeScaleIndex);
  $("timeScaleValue").textContent = `${formatScaleMs(state.timeScaleMs)} / grid`;
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
  const start = trace.chunkStart === null || trace.chunkStart === undefined ? Number(trace.start || 0) : Number(trace.chunkStart || 0);
  return Number.isFinite(start) ? start : 0;
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
  const track = scroller.querySelector(".time-track");
  if (!track) return;
  const range = Number(track.dataset.range || 0);
  const axis = track.querySelector(".time-axis .bar-bg");
  const grid = track.querySelector(".time-grid");
  if (!Number.isFinite(range) || range <= 0) return;
  if (axis) axis.innerHTML = timeAxisTickHtml(range, scroller);
  if (grid) grid.innerHTML = timeGridTickHtml(range, scroller);
}

function timeAxisTickHtml(range, scroller) {
  const bounds = visibleTimeTickBounds(range, scroller);
  let html = "";
  for (let i = bounds.start; i <= bounds.end; i += bounds.step) {
    const left = i * timeGridWidthPx;
    const label = formatScaleMs(Math.min(range, state.timeScaleMs * i));
    html += `<b class="${i === 0 ? "edge-start" : ""}" style="left:${left}px">${escapeHtml(label)}</b>`;
  }
  const endLeft = timeWidthPx(range);
  if (endLeft >= bounds.leftPx && endLeft <= bounds.rightPx && bounds.ticks % bounds.step !== 0) {
    html += `<b class="edge-end" style="left:${endLeft}px">${escapeHtml(formatScaleMs(range))}</b>`;
  }
  return html;
}

function timeGridTickHtml(range, scroller) {
  const bounds = visibleTimeTickBounds(range, scroller);
  let html = "";
  for (let i = bounds.start; i <= bounds.end; i += bounds.step) {
    html += `<i style="left:${i * timeGridWidthPx}px"></i>`;
  }
  const endLeft = timeWidthPx(range);
  if (endLeft >= bounds.leftPx && endLeft <= bounds.rightPx && bounds.ticks % bounds.step !== 0) {
    html += `<i style="left:${endLeft}px"></i>`;
  }
  return html;
}

function visibleTimeTickBounds(range, scroller) {
  const ticks = Math.ceil(range / state.timeScaleMs);
  const step = Math.max(1, Math.ceil(92 / timeGridWidthPx));
  const scrollLeft = scroller ? scroller.scrollLeft : state.timeScrollLeft;
  const width = scroller && scroller.clientWidth > 0 ? scroller.clientWidth : 1600;
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
  render();
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
  const x = Math.max(0, event.clientX - rect.left);
  state.timeZoomAnchorRatio = rect.width > 0 ? x / rect.width : 0.5;
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
  const scroller = document.querySelector(".position-scroll");
  if (!scroller) return;
  const rect = scroller.getBoundingClientRect();
  const x = Math.max(0, event.clientX - rect.left);
  state.positionZoomAnchorRatio = rect.width > 0 ? x / rect.width : 0.5;
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
  refreshTimer = window.setTimeout(async () => {
    refreshTimer = null;
    await load();
    scheduleAutoRefresh();
  }, state.refreshIntervalMs);
}

render();
load().finally(scheduleAutoRefresh);
