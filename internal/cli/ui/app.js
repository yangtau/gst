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
  return el("span", { class: "pill " + (cls || text) }, text);
}

function shortID(id) {
  return id.includes(":") ? id : id.slice(0, 8);
}

function since(t) {
  const d = new Date(t);
  if (isNaN(d) || d.getFullYear() < 2000) return "";
  const s = Math.round((Date.now() - d) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return Math.floor(s / 60) + "m ago";
  if (s < 86400) return Math.floor(s / 3600) + "h ago";
  return d.toLocaleString();
}

function fill(tbody, rows) {
  tbody.replaceChildren(...rows.map((cells) => el("tr", null, ...cells.map((c) => el("td", null, ...[].concat(c))))));
}

function showAlert(text) {
  const a = $("alert");
  a.textContent = text || "";
  a.hidden = !text;
}

// Display names: gst nodes that share a name get their id prefix.
function displayNames(nodes) {
  const count = {};
  for (const n of nodes) if (n.gst) count[n.name] = (count[n.name] || 0) + 1;
  const names = {};
  for (const n of nodes) names[n.id] = n.gst && count[n.name] > 1 ? `${n.name} (${shortID(n.id)})` : n.name;
  return names;
}

function edgeClass(e) {
  if (e.linked) return "linked";
  return e.state === "running" ? "running" : "down";
}

function renderTunnels(tunnels) {
  fill($("tunnels").tBodies[0], tunnels.map((t) => {
    const status = [pill(t.status)];
    if (t.error && t.status !== "running") status.push(el("span", { class: "err" }, t.error));
    const dest = [t.destination];
    if (t.destination.startsWith("mesh:")) dest.push(el("span", { class: "sub" }, "through the mesh"));
    else if (t.no_mesh) dest.push(el("span", { class: "sub" }, "no mesh"));
    const fwds = t.forwards.length ? t.forwards.map((f) => el("span", { class: "fwd" }, f.spec)) : [el("span", { class: "sub" }, "none")];
    return [el("strong", null, t.name), status, dest, fwds, actions(t)];
  }));
  $("tunnels").hidden = tunnels.length === 0;
  $("tunnels-empty").hidden = tunnels.length !== 0;
}

// actions returns the buttons that change a tunnel, as gst start, stop,
// and restart do.
function actions(t) {
  const names = t.status === "stopped" ? ["start"] : ["restart", "stop"];
  const box = el("span", { class: "actions" });
  for (const action of names) {
    const b = el("button", { type: "button", class: action === "stop" ? "quiet" : "" }, ACTIONS[action][0]);
    b.disabled = busy;
    b.addEventListener("click", () => act(t.name, action));
    box.append(b);
  }
  return box;
}

let busy = false;
let noticeTimer;
const ACTIONS = { start: ["Start", "Starting"], stop: ["Stop", "Stopping"], restart: ["Restart", "Restarting"] };

async function act(name, action) {
  if (busy) return;
  busy = true;
  clearTimeout(noticeTimer);
  for (const b of document.querySelectorAll(".actions button")) b.disabled = true;
  showNotice(`${ACTIONS[action][1]} ${name}…`);
  try {
    const res = await fetch(`api/tunnels/${encodeURIComponent(name)}/${action}`, {
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
  } finally {
    busy = false;
    refresh();
  }
}

function showNotice(text) {
  const n = $("notice");
  n.textContent = text || "";
  n.hidden = !text;
}

function renderPeers(peers) {
  fill($("peers").tBodies[0], peers.map((l) => {
    const state = [pill(l.state)];
    if (l.error) state.push(el("span", { class: "err" }, l.error));
    const peer = l.peer && l.peer.id ? [l.peer.name, el("span", { class: "sub mono" }, shortID(l.peer.id))] : ["-"];
    return [l.direction, l.tunnel || "-", state, peer, since(l.since)];
  }));
  $("peers").hidden = peers.length === 0;
  $("peers-empty").hidden = peers.length !== 0;
}

function renderNodes(topo, names) {
  const rows = topo.nodes.map((n) => {
    const name = [el("strong", null, names[n.id])];
    if (n.id === topo.self) name.push(el("span", { class: "sub" }, "this machine"));
    return [
      name,
      n.gst ? "gst" : "SSH server",
      n.id.startsWith("tunnel:") ? el("span", { class: "sub" }, "not connected yet") : el("span", { class: "mono" }, shortID(n.id)),
      pill(n.reachable ? "yes" : "no"),
      n.software || "-",
    ];
  });
  fill($("nodes").tBodies[0], rows);
}

// The map places each machine in a column by its distance from this one,
// following tunnels in either direction; machines without a path to it
// come last.
function renderMap(topo, names) {
  const map = $("map");
  const W = 150, H = 46, COL = 240, ROW = 74, PAD = 24;

  const adj = {};
  for (const n of topo.nodes) adj[n.id] = [];
  for (const e of topo.edges) {
    if (adj[e.from] && adj[e.to]) {
      adj[e.from].push(e.to);
      adj[e.to].push(e.from);
    }
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
  const pos = {};
  const rows = Math.max(1, ...columns.filter(Boolean).map((c) => c.length));
  const height = PAD * 2 + rows * ROW - (ROW - H);
  columns.forEach((col, l) => {
    if (!col) return;
    col.sort((a, b) => names[a.id].localeCompare(names[b.id]));
    const top = PAD + ((rows - col.length) * ROW) / 2;
    col.forEach((n, i) => (pos[n.id] = { x: PAD + l * COL, y: top + i * ROW }));
  });
  const width = PAD * 2 + (columns.length - 1) * COL + W + 60;

  const defs = svg("defs");
  for (const cls of ["linked", "running", "down"]) {
    defs.append(svg("marker", { id: "arrow-" + cls, viewBox: "0 0 10 10", refX: "9", refY: "5", markerWidth: "7", markerHeight: "7", orient: "auto-start-reverse" },
      svg("path", { d: "M0,0 L10,5 L0,10 z", class: "arrow-" + cls })));
  }

  // Each edge leaves and enters a node at its own point along the node's
  // side, ordered by where the other end is, so that arrows do not meet.
  const edges = topo.edges.filter((e) => pos[e.from] && pos[e.to] && e.from !== e.to);
  const ports = {};
  const port = (id, side, edge, other) => (ports[id + side] ||= []).push({ edge, other });
  const sideOf = (a, b, out) => (a.x === b.x ? "R" : (a.x < b.x) === out ? "R" : "L");
  for (const e of edges) {
    const a = pos[e.from], b = pos[e.to];
    port(e.from, sideOf(a, b, true), e, b);
    port(e.to, sideOf(b, a, true), e, a);
  }
  const at = {};
  for (const [key, list] of Object.entries(ports)) {
    list.sort((p, q) => p.other.y - q.other.y || p.other.x - q.other.x);
    list.forEach((p, i) => {
      const id = key.slice(0, -1), side = key.slice(-1), n = pos[id];
      at[(p.edge.from === id ? "from" : "to") + "\0" + edges.indexOf(p.edge)] = {
        x: side === "R" ? n.x + W : n.x,
        y: n.y + (H * (i + 1)) / (list.length + 1),
        dir: side === "R" ? 1 : -1,
      };
    });
  }

  const edgeLayer = svg("g");
  const labelLayer = svg("g");
  edges.forEach((e, i) => {
    const p = at["from\0" + i], q = at["to\0" + i];
    const x2 = q.x + q.dir * 2;
    const reach = Math.max(40, Math.abs(x2 - p.x) / 2);
    const d = `M${p.x},${p.y} C${p.x + p.dir * reach},${p.y} ${x2 + q.dir * reach},${q.y} ${x2},${q.y}`;
    // The curve's midpoint, for the label.
    const mx = (p.x + x2) / 2 + ((p.dir + q.dir) * reach * 3) / 8;
    const my = (p.y + q.y) / 2;
    const cls = edgeClass(e);
    const title = `${names[e.from]} → ${names[e.to]}: tunnel ${e.tunnel} to ${e.destination}, ${e.state}${e.linked ? ", peer link up" : ""}`;
    edgeLayer.append(svg("path", { d, class: "edge " + cls, "marker-end": `url(#arrow-${cls})` }, svg("title", null, title)));
    labelLayer.append(svg("text", { x: mx, y: my - 4, "text-anchor": "middle", class: "edge-label" }, e.tunnel));
  });

  const nodeLayer = svg("g");
  for (const n of topo.nodes) {
    const p = pos[n.id];
    const classes = ["node", n.gst ? "gst" : "ssh"];
    if (n.id === topo.self) classes.push("self");
    if (!n.reachable) classes.push("unreachable");
    const kind = n.id === topo.self ? "this machine" : n.gst ? "gst " + shortID(n.id) : "SSH server";
    const title = `${names[n.id]}\n${n.gst ? "gst node " + n.id : "SSH server without gst"}\n${n.reachable ? "reachable" : "unreachable"}${n.software ? "\n" + n.software : ""}`;
    nodeLayer.append(svg("g", { class: classes.join(" "), transform: `translate(${p.x},${p.y})` },
      svg("title", null, title),
      svg("rect", { class: "box", width: W, height: H, rx: 8 }),
      svg("text", { class: "name", x: 12, y: 20 }, truncate(names[n.id], 18)),
      svg("text", { class: "kind", x: 12, y: 36 }, kind)));
  }

  map.setAttribute("viewBox", `0 0 ${width} ${height}`);
  map.style.maxWidth = width + "px";
  // On narrow screens the map scrolls rather than shrinking past legibility.
  map.style.minWidth = Math.min(width, 560) + "px";
  map.replaceChildren(defs, edgeLayer, nodeLayer, labelLayer);
}

function truncate(s, n) {
  return s.length > n ? s.slice(0, n - 1) + "…" : s;
}

function render(state) {
  document.title = state.node ? `gst · ${state.node.name}` : "gst";
  $("node").textContent = state.node ? `${state.node.name} · ${shortID(state.node.id)}` : "this machine";
  const daemon = $("daemon");
  daemon.className = "pill " + (state.daemon ? "running" : "down");
  daemon.textContent = state.daemon ? "daemon running" : "daemon not running";
  $("updated").textContent = state.software + " · updated " + new Date().toLocaleTimeString();

  const notes = [];
  if (!state.daemon && state.tunnels.some((t) => t.status !== "stopped")) notes.push("The gst daemon does not answer; started tunnels show as down. Start it with gst start or gst mesh enable.");
  for (const e of state.errors || []) notes.push(e);
  if (Date.now() > alertSticky) showAlert(notes.join(" "));

  renderTunnels(state.tunnels);

  const meshOn = !!state.node;
  $("mesh-off").hidden = meshOn;
  $("peers-section").hidden = !meshOn;
  renderPeers(state.peers);
  const topo = state.topology;
  const showTopo = meshOn && topo && topo.nodes.length > 0;
  $("mesh-section").hidden = !showTopo;
  $("nodes-section").hidden = !showTopo;
  if (showTopo) {
    const names = displayNames(topo.nodes);
    renderMap(topo, names);
    renderNodes(topo, names);
  }
}

let timer;
let alertSticky = 0; // until then, the alert shows an action's error

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
