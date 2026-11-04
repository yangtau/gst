"use strict";

// The page polls /api/state with the token from the address that gst ui
// printed. Everything shown is built with DOM text nodes: names and
// destinations come from other machines of the mesh.

const token = location.hash.slice(1);
const SVG = "http://www.w3.org/2000/svg";
const POLL_MS = 3000;

const $ = (id) => document.getElementById(id);

function el(tag, attrs, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v !== undefined && v !== null && v !== false) e.setAttribute(k, v);
  }
  for (const c of children) {
    if (c === null || c === undefined) continue;
    e.append(c instanceof Node ? c : String(c));
  }
  return e;
}

function svg(tag, attrs, ...children) {
  const e = document.createElementNS(SVG, tag);
  for (const [k, v] of Object.entries(attrs || {})) e.setAttribute(k, v);
  for (const c of children) e.append(c instanceof Node ? c : String(c));
  return e;
}

function pill(text, cls) {
  return el("span", { class: "pill " + (cls || text.replace(/ /g, "-")) }, text);
}

function shortID(id) {
  return id.slice(0, 8);
}

function fill(table, rows) {
  table.tBodies[0].replaceChildren(...rows.map((cells) => el("tr", null, ...cells.map((c) => el("td", null, ...[].concat(c))))));
}

// show fills a table, or hides it for the empty-state paragraph.
function show(name, rows) {
  fill($(name), rows);
  $(name).closest(".table-wrap").hidden = rows.length === 0;
  $(name + "-empty").hidden = rows.length !== 0;
}

function showAlert(...texts) {
  const a = $("alert");
  a.replaceChildren(...texts.filter(Boolean).map((t) => el("p", null, t)));
  a.hidden = a.childNodes.length === 0;
}

function showNotice(text) {
  const n = $("notice");
  n.textContent = text || "";
  n.hidden = !text;
}

// Machines that share a name are told apart by their id prefix.
function displayNames(nodes) {
  const count = {};
  for (const n of nodes) count[n.name] = (count[n.name] || 0) + 1;
  const names = {};
  for (const n of nodes) names[n.id] = count[n.name] > 1 ? `${n.name} (${shortID(n.id)})` : n.name;
  return names;
}

// ---- links ----

let busy = false;
let noticeTimer;
let alertSticky = 0; // until then, the alert shows an action's error
const ACTIONS = { start: ["Start", "Starting"], stop: ["Stop", "Stopping"], restart: ["Restart", "Restarting"] };

// actions returns the buttons that apply to a link, as gst link start,
// stop, and restart do.
function actions(l) {
  const names = l.status === "stopped" ? ["start"] : ["restart", "stop"];
  const box = el("span", { class: "actions" });
  for (const action of names) {
    const b = el("button", { type: "button", class: action === "stop" ? "quiet" : "" }, ACTIONS[action][0]);
    b.disabled = busy;
    b.addEventListener("click", () => act(l.name, action));
    box.append(b);
  }
  return box;
}

async function act(name, action) {
  if (busy) return;
  busy = true;
  clearTimeout(noticeTimer);
  for (const b of document.querySelectorAll(".actions button")) b.disabled = true;
  showNotice(`${ACTIONS[action][1]} ${name}…`);
  try {
    const res = await fetch(`api/links/${encodeURIComponent(name)}/${action}`, {
      method: "POST",
      headers: { Authorization: "Bearer " + token },
    });
    const body = await res.json().catch(() => ({ error: res.statusText }));
    if (res.ok) {
      showNotice(body.message || "Done.");
      noticeTimer = setTimeout(() => showNotice(""), 6000);
    } else {
      showNotice("");
      showAlert((body.error || res.statusText) + (body.hint ? ` — ${body.hint}` : ""));
      alertSticky = Date.now() + 8000;
    }
  } catch (err) {
    showNotice("");
    showAlert(String(err.message || err));
    alertSticky = Date.now() + 8000;
  } finally {
    busy = false;
    refresh();
  }
}

function renderLinks(links) {
  show("links", links.map((l) => {
    const status = [pill(l.status)];
    if (l.error && l.status !== "running") status.push(el("span", { class: "err" }, l.error));
    const peer = l.peer && l.peer.id
      ? [l.peer.name, el("span", { class: "sub mono" }, shortID(l.peer.id))]
      : [el("span", { class: "sub" }, "-")];
    return [el("strong", null, l.name), status, l.destination, peer, actions(l)];
  }));
}

// ---- machines ----

// routeText is how this machine reaches n: the machines along its path
// after this one.
function routeText(n, topo, names) {
  if (n.id === topo.self) return "this machine";
  if (!n.reachable || !n.path || n.path.length < 2) return "unreachable";
  return n.path.slice(1).map((id) => names[id] || shortID(id)).join(" → ");
}

function renderNodes(topo, names) {
  const nodes = [...topo.nodes].sort((a, b) => (a.id === topo.self ? -1 : b.id === topo.self ? 1 : names[a.id].localeCompare(names[b.id])));
  fill($("nodes"), nodes.map((n) => [
    el("strong", null, names[n.id]),
    el("span", { class: "mono" }, shortID(n.id)),
    n.software || "-",
    pill(n.exit ? "yes" : "no", n.exit ? "yes" : "off"),
    n.reachable ? routeText(n, topo, names) : el("span", { class: "err-inline" }, "unreachable"),
  ]));
}

// The graph puts each machine in a column by its distance from this one,
// following links in either direction; machines without a path to it come
// last. Links are straight lines labeled with their names.
function renderGraph(topo, names) {
  const map = $("map");
  const W = 150, H = 44, COL = 230, ROW = 70, PAD = 24;

  const adj = {};
  for (const n of topo.nodes) adj[n.id] = [];
  const edges = topo.edges.filter((e) => adj[e.from] && adj[e.to] && e.from !== e.to);
  for (const e of edges) {
    adj[e.from].push(e.to);
    adj[e.to].push(e.from);
  }
  const layer = { [topo.self]: 0 };
  const queue = [topo.self];
  while (queue.length) {
    const id = queue.shift();
    for (const next of adj[id] || []) {
      if (!(next in layer)) {
        layer[next] = layer[id] + 1;
        queue.push(next);
      }
    }
  }
  const maxLayer = Math.max(0, ...Object.values(layer));
  const columns = [];
  for (const n of topo.nodes) {
    const l = n.id in layer ? layer[n.id] : maxLayer + 1;
    (columns[l] ||= []).push(n);
  }
  const rows = Math.max(1, ...columns.filter(Boolean).map((c) => c.length));
  const height = PAD * 2 + (rows - 1) * ROW + H;
  const pos = {};
  let x = PAD;
  for (const col of columns) {
    if (!col) continue;
    col.sort((a, b) => names[a.id].localeCompare(names[b.id]));
    const top = PAD + ((rows - col.length) * ROW) / 2;
    col.forEach((n, i) => (pos[n.id] = { x, y: top + i * ROW }));
    x += COL;
  }
  const width = x - COL + W + PAD;

  const defs = svg("defs", null,
    svg("marker", { id: "arrow", viewBox: "0 0 10 10", refX: "9", refY: "5", markerWidth: "7", markerHeight: "7", orient: "auto" },
      svg("path", { d: "M0,0 L10,5 L0,10 z", class: "arrow" })));

  // clip returns where the line from the center of box a towards b leaves a.
  const clip = (a, b) => {
    const cx = a.x + W / 2, cy = a.y + H / 2;
    const dx = b.x + W / 2 - cx, dy = b.y + H / 2 - cy;
    if (dx === 0 && dy === 0) return { x: cx, y: cy };
    const s = Math.min(dx ? W / 2 / Math.abs(dx) : Infinity, dy ? H / 2 / Math.abs(dy) : Infinity);
    return { x: cx + dx * s, y: cy + dy * s };
  };

  // Several links between the same two machines are bent apart.
  const seen = {};
  const edgeLayer = svg("g");
  const labelLayer = svg("g");
  for (const e of edges) {
    const key = [e.from, e.to].sort().join("\0");
    const k = (seen[key] = (seen[key] || 0) + 1) - 1;
    const a = pos[e.from], b = pos[e.to];
    let p = clip(a, b), q = clip(b, a);
    const len = Math.hypot(q.x - p.x, q.y - p.y) || 1;
    const bend = (k % 2 ? -1 : 1) * Math.ceil(k / 2) * 26 * (e.from < e.to ? 1 : -1);
    const mx = (p.x + q.x) / 2 + (-(q.y - p.y) / len) * bend;
    const my = (p.y + q.y) / 2 + ((q.x - p.x) / len) * bend;
    edgeLayer.append(svg("path", { d: `M${p.x},${p.y} Q${mx},${my} ${q.x},${q.y}`, class: "edge", "marker-end": "url(#arrow)" },
      svg("title", null, `${names[e.from]} → ${names[e.to]}: link ${e.link}`)));
    // The curve's midpoint is halfway to the control point.
    labelLayer.append(svg("text", { x: (p.x + q.x) / 4 + mx / 2, y: (p.y + q.y) / 4 + my / 2 - 4, "text-anchor": "middle", class: "edge-label" }, e.link));
  }

  const nodeLayer = svg("g");
  for (const n of topo.nodes) {
    const p = pos[n.id];
    const classes = ["node"];
    if (n.id === topo.self) classes.push("self");
    if (!n.reachable) classes.push("unreachable");
    const kind = n.id === topo.self ? "this machine" : shortID(n.id) + (n.exit ? " · exit" : "");
    nodeLayer.append(svg("g", { class: classes.join(" "), transform: `translate(${p.x},${p.y})` },
      svg("title", null, `${names[n.id]}\n${n.id}${n.software ? "\n" + n.software : ""}${n.reachable ? "" : "\nunreachable"}`),
      svg("rect", { class: "box", width: W, height: H, rx: 8 }),
      svg("text", { class: "name", x: 12, y: 19 }, truncate(names[n.id], 18)),
      svg("text", { class: "kind", x: 12, y: 35 }, kind)));
  }

  map.setAttribute("viewBox", `0 0 ${width} ${height}`);
  map.style.maxWidth = width + "px";
  // On narrow screens the graph scrolls rather than shrinking past legibility.
  map.style.minWidth = Math.min(width, 520) + "px";
  map.replaceChildren(defs, edgeLayer, nodeLayer, labelLayer);
}

function truncate(s, n) {
  return s.length > n ? s.slice(0, n - 1) + "…" : s;
}

// ---- proxy, maps, routes ----

function renderProxy(proxy) {
  const state = $("proxy-state");
  const usage = $("proxy-usage");
  if (!proxy.address) {
    state.replaceChildren(pill("off"), " ", el("span", { class: "muted" }, "Turn it on with "), el("code", null, "gst proxy 127.0.0.1:1080"));
    usage.hidden = true;
    return;
  }
  const children = [el("code", null, proxy.address), " "];
  if (proxy.status === "failed") {
    children.push(pill("failed", "down"), " ", el("span", { class: "err-inline" }, proxy.error || ""));
  } else if (proxy.status === "listening") {
    children.push(pill("listening", "running"));
  } else {
    children.push(pill("gst is not running", "down"));
  }
  state.replaceChildren(...children);

  // The usage lines name an address that clients can connect to.
  const i = proxy.address.lastIndexOf(":");
  let host = proxy.address.slice(0, i), port = proxy.address.slice(i + 1);
  if (host === "0.0.0.0" || host === "" || host === "::" || host === "[::]") host = "127.0.0.1";
  const addr = `${host}:${port}`;
  $("use-env").textContent = `ALL_PROXY=socks5h://${addr}`;
  $("use-pac").textContent = `http://${addr}/proxy.pac`;
  $("use-ssh").textContent = "Host *.gst\n  ProxyCommand gst connect %h %p";
  usage.hidden = false;
}

function renderMaps(maps) {
  show("maps", maps.map((m) => {
    const status = [pill(m.status === "listening" ? "listening" : m.status === "failed" ? "failed" : "not running", m.status === "listening" ? "running" : "down")];
    if (m.error) status.push(el("span", { class: "err" }, m.error));
    return [el("span", { class: "mono" }, m.listen), el("span", { class: "mono" }, `${m.machine}:${m.target}`), status];
  }));
}

// viaNote says what the mesh shows about the machine a route goes through.
function viaNote(via, topo) {
  if (!topo) return null;
  const found = topo.nodes.filter((n) => n.name === via || n.id.startsWith(via));
  if (found.length === 0) return "not in the mesh";
  if (!found.some((n) => n.exit)) return "not an exit";
  return null;
}

function renderRoutes(routes, topo) {
  show("routes", routes.map((r) => {
    const note = viaNote(r.via, topo);
    return [el("span", { class: "mono" }, r.match), [r.via, note ? el("span", { class: "err" }, note) : null]];
  }));
}

// ---- page ----

function render(state) {
  const self = state.node;
  document.title = self ? `gst · ${self.name}` : "gst";
  $("node").textContent = self ? `${self.name} · ${shortID(self.id)}` : "this machine";
  const exit = $("exit");
  exit.hidden = !self;
  exit.className = "pill " + (self && self.exit ? "yes" : "off");
  exit.textContent = self && self.exit ? "exit" : "not an exit";

  const daemon = $("daemon");
  if (state.daemon) {
    daemon.className = "pill running";
    daemon.textContent = "daemon running";
  } else {
    daemon.className = "pill down";
    daemon.textContent = (state.down ? "down" : "daemon not running") + " — run gst up";
  }
  $("updated").textContent = `${state.software} · updated ${new Date().toLocaleTimeString()}`;

  if (Date.now() > alertSticky) showAlert(...(state.errors || []));

  renderLinks(state.links);
  const topo = state.topology;
  const showTopo = !!topo && topo.nodes.length > 0;
  $("nodes-section").hidden = !showTopo;
  if (showTopo) {
    const names = displayNames(topo.nodes);
    renderGraph(topo, names);
    renderNodes(topo, names);
  }
  renderProxy(state.proxy);
  renderMaps(state.maps);
  renderRoutes(state.routes, topo);
}

let timer;

async function refresh() {
  clearTimeout(timer);
  try {
    const res = await fetch("api/state", { headers: { Authorization: "Bearer " + token }, cache: "no-store" });
    if (res.status === 401) {
      showAlert("This address lacks the secret. Open the address that gst ui printed, including the part after #.");
      return;
    }
    if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
    render(await res.json());
  } catch (err) {
    showAlert(err instanceof TypeError ? "gst ui is not running. Start it again with gst ui." : String(err.message || err));
  }
  if (!document.hidden) timer = setTimeout(refresh, POLL_MS);
}

document.addEventListener("visibilitychange", () => {
  if (!document.hidden) refresh();
});
refresh();
