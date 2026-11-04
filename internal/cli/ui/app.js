"use strict";

// The page polls /api/state (this machine and the mesh) and, for another
// machine that the user selected, /api/nodes/{id}, with the token from the
// address that gst ui printed. Everything shown is built with DOM text
// nodes: names and destinations come from other machines of the mesh.

const token = location.hash.slice(1);
const SVG = "http://www.w3.org/2000/svg";
const POLL_MS = 3000;
const NODE_POLL_MS = 5000;
const SELECTED_KEY = "gst.machine";

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

// Machines that share a name are told apart by their id prefix.
function displayNames(nodes) {
  const count = {};
  for (const n of nodes) count[n.name] = (count[n.name] || 0) + 1;
  const names = {};
  for (const n of nodes) names[n.id] = count[n.name] > 1 ? `${n.name} (${shortID(n.id)})` : n.name;
  return names;
}

// ---- links ----

function renderLinks(links) {
  show("links", links.map((l) => {
    const status = [pill(l.state)];
    if (l.error && l.state !== "running") status.push(el("span", { class: "err" }, l.error));
    const peer = l.peer && l.peer.id
      ? [l.peer.name, el("span", { class: "sub mono" }, shortID(l.peer.id))]
      : [el("span", { class: "sub" }, "-")];
    return [el("strong", null, l.name), status, l.destination, peer];
  }));
}

// ---- machines ----

// routeText says whether n is online, as gst status does, and the
// machines that connections to it pass through.
function routeText(n, topo, names) {
  if (n.id === topo.self) return "this machine";
  if (!n.reachable || !n.path || n.path.length < 2) return "offline";
  if (n.path.length === 2) return "online";
  return "online via " + n.path.slice(1, -1).map((id) => names[id] || shortID(id)).join(", ");
}

function renderNodes(topo, names) {
  const nodes = [...topo.nodes].sort((a, b) => (a.id === topo.self ? -1 : b.id === topo.self ? 1 : names[a.id].localeCompare(names[b.id])));
  $("nodes").tBodies[0].replaceChildren(...nodes.map((n) => {
    const picked = n.id === selected;
    const pick = el("button", { type: "button", class: "pick", "aria-pressed": String(picked) }, names[n.id]);
    const row = el("tr", { class: "pickable" + (picked ? " picked" : "") },
      el("td", null, pick),
      el("td", null, el("span", { class: "mono" }, shortID(n.id))),
      el("td", null, n.software || "-"),
      el("td", null, pill(n.exit ? "yes" : "no", n.exit ? "yes" : "off")),
      el("td", null, n.reachable ? routeText(n, topo, names) : el("span", { class: "err-inline" }, "offline")));
    row.addEventListener("click", () => select(n.id));
    return row;
  }));
}

// The graph puts each machine in a column by its distance from this one,
// following links in either direction; machines without a path to it come
// last. Links are straight lines labeled with their names.
function renderGraph(topo, names) {
  const map = $("map");
  const W = 150, H = 44, COL = 230, ROW = 86, PAD = 24;

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

  const center = (a) => ({ x: a.x + W / 2, y: a.y + H / 2 });

  // clip returns where the line from the center of box a towards point b
  // leaves a.
  const clip = (a, b) => {
    const c = center(a);
    const dx = b.x - c.x, dy = b.y - c.y;
    if (dx === 0 && dy === 0) return c;
    const s = Math.min(dx ? W / 2 / Math.abs(dx) : Infinity, dy ? H / 2 / Math.abs(dy) : Infinity);
    return { x: c.x + dx * s, y: c.y + dy * s };
  };

  // curve is the line from box a to box b, bent sideways by bend.
  const curve = (a, b, bend) => {
    const ca = center(a), cb = center(b);
    const dx = cb.x - ca.x, dy = cb.y - ca.y;
    const len = Math.hypot(dx, dy) || 1;
    const c = { x: (ca.x + cb.x) / 2 - (dy / len) * bend, y: (ca.y + cb.y) / 2 + (dx / len) * bend };
    return { p: clip(a, c), c, q: clip(b, c) };
  };

  const pointAt = ({ p, c, q }, t) => ({
    x: (1 - t) ** 2 * p.x + 2 * t * (1 - t) * c.x + t ** 2 * q.x,
    y: (1 - t) ** 2 * p.y + 2 * t * (1 - t) * c.y + t ** 2 * q.y,
  });

  const samples = (k) => Array.from({ length: 41 }, (_, i) => pointAt(k, i / 40));

  // crosses reports whether curve k passes over a box other than its ends'.
  const crosses = (k, a, b) => Object.values(pos).some((box) => box !== a && box !== b &&
    samples(k).some(({ x, y }) => x > box.x - 8 && x < box.x + W + 8 && y > box.y - 8 && y < box.y + H + 8));

  // route bends the line from a to b away from base until it passes no
  // other box, so that a line never seems to end at a machine it passes.
  // It tries the right of the graph first, or its bottom for a level line,
  // where there is room.
  const route = (a, b, base) => {
    const ca = center(a), cb = center(b);
    const out = cb.y > ca.y || (cb.y === ca.y && cb.x < ca.x) ? -1 : 1;
    for (let i = 0; i <= 16; i++) {
      const k = curve(a, b, base + out * (i % 2 ? 1 : -1) * Math.ceil(i / 2) * 36);
      if (!crosses(k, a, b)) return k;
    }
    return curve(a, b, base);
  };

  // Several links between the same two machines are bent apart.
  const seen = {};
  const edgeLayer = svg("g");
  const labelLayer = svg("g");
  let left = 0, top = 0, right = width, bottom = height;
  for (const e of edges) {
    const key = [e.from, e.to].sort().join("\0");
    const n = (seen[key] = (seen[key] || 0) + 1) - 1;
    const k = route(pos[e.from], pos[e.to], (n % 2 ? -1 : 1) * Math.ceil(n / 2) * 26 * (e.from < e.to ? 1 : -1));
    edgeLayer.append(svg("path", { d: `M${k.p.x},${k.p.y} Q${k.c.x},${k.c.y} ${k.q.x},${k.q.y}`, class: "edge", "marker-end": "url(#arrow)" },
      svg("title", null, `${names[e.from]} → ${names[e.to]}: link ${e.link}`)));
    const mid = pointAt(k, 0.5);
    labelLayer.append(svg("text", { x: mid.x, y: mid.y - 4, "text-anchor": "middle", class: "edge-label" }, e.link));
    // A line bent around a box may leave the columns; the graph grows to hold it.
    for (const { x, y } of samples(k)) {
      left = Math.min(left, x - PAD);
      right = Math.max(right, x + PAD);
      top = Math.min(top, y - PAD);
      bottom = Math.max(bottom, y + PAD);
    }
    right = Math.max(right, mid.x + 4 * e.link.length + PAD);
    left = Math.min(left, mid.x - 4 * e.link.length - PAD);
  }

  const nodeLayer = svg("g");
  for (const n of topo.nodes) {
    const p = pos[n.id];
    const classes = ["node"];
    if (n.id === topo.self) classes.push("self");
    if (n.id === selected) classes.push("picked");
    if (!n.reachable) classes.push("unreachable");
    const kind = n.id === topo.self ? "this machine" : shortID(n.id) + (n.exit ? " · exit" : "");
    const g = svg("g", { class: classes.join(" "), transform: `translate(${p.x},${p.y})` },
      svg("title", null, `${names[n.id]}\n${n.id}${n.software ? "\n" + n.software : ""}${n.reachable ? "" : "\noffline"}`),
      svg("rect", { class: "box", width: W, height: H, rx: 8 }),
      svg("text", { class: "name", x: 12, y: 19 }, truncate(names[n.id], 18)),
      svg("text", { class: "kind", x: 12, y: 35 }, kind));
    g.addEventListener("click", () => select(n.id));
    nodeLayer.append(g);
  }

  map.setAttribute("viewBox", `${left} ${top} ${right - left} ${bottom - top}`);
  map.style.maxWidth = right - left + "px";
  // On narrow screens the graph scrolls rather than shrinking past legibility.
  map.style.minWidth = Math.min(right - left, 520) + "px";
  map.replaceChildren(defs, edgeLayer, nodeLayer, labelLayer);
}

function truncate(s, n) {
  return s.length > n ? s.slice(0, n - 1) + "…" : s;
}


// ---- machine detail ----

// A machine's detail is its status, as its daemon reports it: this
// machine's comes with /api/state, another's from /api/nodes/{id}.

function listenState(listen, error) {
  return !listen ? "off" : error ? "failed" : "listening";
}

function detailFromStatus(st) {
  return {
    proxy: { listen: st.proxy.listen, state: listenState(st.proxy.listen, st.proxy.error), error: st.proxy.error },
    links: st.links || [],
    maps: (st.maps || []).map((m) => ({ listen: m.listen, machine: m.machine, target: m.target, state: listenState(m.listen, m.error), error: m.error })),
    routes: st.routes || [],
  };
}

function renderProxy(proxy, isSelf) {
  const state = $("proxy-state");
  const usage = $("proxy-usage");
  usage.hidden = true;
  if (!proxy.listen) {
    state.replaceChildren(pill("off"), ...(isSelf ? [" ", el("span", { class: "muted" }, "Turn it on with "), el("code", null, "gst proxy 127.0.0.1:1080")] : []));
    return;
  }
  const children = [el("code", null, proxy.listen), " "];
  if (proxy.state === "failed") {
    children.push(pill("failed", "down"), " ", el("span", { class: "err-inline" }, proxy.error || ""));
  } else {
    children.push(pill("listening", "running"));
  }
  state.replaceChildren(...children);
  if (!isSelf) return;

  // The usage lines name an address that clients can connect to.
  const i = proxy.listen.lastIndexOf(":");
  let host = proxy.listen.slice(0, i), port = proxy.listen.slice(i + 1);
  if (host === "0.0.0.0" || host === "" || host === "::" || host === "[::]") host = "127.0.0.1";
  const addr = `${host}:${port}`;
  $("use-env").textContent = `ALL_PROXY=socks5h://${addr}`;
  usage.hidden = false;
}

function renderMaps(maps) {
  show("maps", maps.map((m) => {
    const status = [pill(m.state, m.state === "listening" ? "running" : "down")];
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

// renderDetail shows the selected machine: its header from what this
// machine knows of it, and its tables once its status is known.
function renderDetail(state, names) {
  const topo = state.topology;
  const isSelf = isSelfSelected(state);
  const known = topo && topo.nodes.find((n) => n.id === selected);
  const info = isSelf && state.node ? state.node : known;
  const detail = $("detail");
  detail.classList.toggle("self", isSelf);

  const view = nodeView && nodeView.id === selected ? nodeView : null;
  const status = isSelf ? state.status : view && view.status;
  const node = status && status.node && status.node.id ? status.node : info;
  $("m-name").textContent = node ? (names && names[node.id]) || node.name : "this machine";
  $("m-id").textContent = node ? shortID(node.id) : "";
  const software = isSelf ? state.software : (status && status.software) || (node && node.software) || "";
  $("m-software").textContent = software;
  const exit = $("m-exit");
  exit.hidden = !node;
  exit.className = "pill " + (node && node.exit ? "yes" : "off");
  exit.textContent = node && node.exit ? "exit" : "not an exit";
  $("m-self").hidden = !isSelf;

  let d = null;
  let problem = "";
  if (isSelf) {
    if (state.status) d = detailFromStatus(state.status);
    else problem = state.down ? "gst is down on this machine; run gst up." : "The gst daemon is not running; run gst up.";
  } else if (!view) {
    problem = "Loading…";
  } else if (view.error) {
    problem = view.error;
  } else {
    d = detailFromStatus(view.status);
  }
  $("m-problem").textContent = problem;
  $("m-problem").className = (isSelf ? !d : view && view.error) ? "problem err-inline" : "problem muted";
  $("m-problem").hidden = !problem;
  $("m-body").hidden = !d;
  if (!d) return;
  renderLinks(d.links);
  renderProxy(d.proxy, isSelf);
  renderMaps(d.maps);
  renderRoutes(d.routes, topo);
}

// ---- selection ----

let selected = ""; // the id of the machine shown in detail
let lastState = null;
let nodeView = null; // {id, status} or {id, error}: the last answer for a machine other than this one
let nodeInflight = false;
let nodeTimer = 0;

try {
  selected = localStorage.getItem(SELECTED_KEY) || "";
} catch (_) {}

// selfID is this machine's id as far as the page knows; empty when it has
// none yet.
function selfID(state) {
  return (state.topology && state.topology.self) || (state.node && state.node.id) || "";
}

// isSelfSelected is true when the selection is this machine, or when the
// page knows no other machines.
function isSelfSelected(state) {
  return !state.topology || state.topology.nodes.length === 0 || selected === selfID(state);
}

function select(id) {
  if (id === selected) return;
  selected = id;
  try {
    localStorage.setItem(SELECTED_KEY, id);
  } catch (_) {}
  nodeView = null;
  if (lastState) draw(lastState);
  refreshNode();
}

// refreshNode fetches the selected machine's status unless it is this
// machine, whose status comes with /api/state, or is known to be
// unreachable. Requests do not overlap: a request that finishes for a
// machine no longer selected is followed by one for the selected machine.
async function refreshNode() {
  clearTimeout(nodeTimer);
  nodeTimer = 0;
  const state = lastState;
  if (!state || !state.topology || isSelfSelected(state)) return;
  const id = selected;
  const known = state.topology.nodes.find((n) => n.id === id);
  if (known && !known.reachable) {
    const msg = "Offline: no running link leads to this machine from here.";
    if (!nodeView || nodeView.id !== id || nodeView.error !== msg) {
      nodeView = { id, error: msg };
      draw(state);
    }
    return;
  }
  if (nodeInflight) return;
  nodeInflight = true;
  try {
    const res = await fetch(`api/nodes/${encodeURIComponent(id)}`, { headers: { Authorization: "Bearer " + token }, cache: "no-store" });
    let view;
    if (res.ok) {
      view = { id, status: await res.json() };
    } else {
      view = { id, error: (await res.text()).trim() || res.statusText };
    }
    if (id === selected) nodeView = view;
  } catch (err) {
    if (id === selected) nodeView = { id, error: err instanceof TypeError ? "gst ui is not running." : String(err.message || err) };
  } finally {
    nodeInflight = false;
  }
  if (id !== selected) {
    refreshNode();
    return;
  }
  if (lastState) draw(lastState);
  if (!document.hidden) nodeTimer = setTimeout(refreshNode, NODE_POLL_MS);
}

// ---- page ----

function render(state) {
  lastState = state;
  // A remembered machine that the mesh no longer lists gives way to this one.
  const topo = state.topology;
  if (topo && topo.nodes.length > 0 && !topo.nodes.some((n) => n.id === selected)) selected = topo.self;
  if (!topo && selected !== selfID(state)) selected = selfID(state);

  const self = state.node;
  document.title = self ? `gst · ${self.name}` : "gst";
  $("node").textContent = self ? `${self.name} · ${shortID(self.id)}` : "this machine";
  const exit = $("exit");
  exit.hidden = !self;
  exit.className = "pill " + (self && self.exit ? "yes" : "off");
  exit.textContent = self && self.exit ? "exit" : "not an exit";

  const daemon = $("daemon");
  if (state.status) {
    daemon.className = "pill running";
    daemon.textContent = "daemon running";
  } else {
    daemon.className = "pill down";
    daemon.textContent = (state.down ? "down" : "daemon not running") + " — run gst up";
  }
  $("updated").textContent = `${state.software} · updated ${new Date().toLocaleTimeString()}`;

  showAlert(...(state.errors || []));

  draw(state);
  if (!nodeInflight && !nodeTimer) refreshNode();
}

// draw shows state and the selected machine's status.
function draw(state) {
  const topo = state.topology;
  const showTopo = !!topo && topo.nodes.length > 0;
  $("nodes-section").hidden = !showTopo;
  let names;
  if (showTopo) {
    names = displayNames(topo.nodes);
    renderGraph(topo, names);
    renderNodes(topo, names);
  }
  renderDetail(state, names);
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
  if (!document.hidden) {
    refresh();
    refreshNode();
  }
});
refresh();
