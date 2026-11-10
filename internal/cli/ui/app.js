"use strict";

// The page polls /api/state (this machine and the mesh) and /api/nodes/{id}
// (every other machine it can reach, so that opening one is instant), with
// the token from the address that gst ui printed. Everything shown is built
// with DOM text nodes: names and destinations come from other machines of
// the mesh.

const token = location.hash.slice(1);
const SVG = "http://www.w3.org/2000/svg";
const POLL_MS = 3000;
const MACHINE_POLL_MS = 5000;
const MACHINE_REQUESTS = 4; // at a time
const OPEN_KEY = "gst.open";

const $ = (id) => document.getElementById(id);

function el(tag, attrs, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v !== undefined && v !== null && v !== false) e.setAttribute(k, v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
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

const shortID = (id) => id.slice(0, 8);
const plural = (n, word) => `${n} ${word}${n === 1 ? "" : "s"}`;

// ---- state ----

let state = null; // the last /api/state
const answers = new Map(); // machine id: {status} or {error}, for machines other than this one
let open = null; // ids of the machines shown open; null until the page knows this machine
let drawn = ""; // what the page shows, to leave it alone when nothing changed

try {
  const saved = JSON.parse(localStorage.getItem(OPEN_KEY));
  if (Array.isArray(saved)) open = new Set(saved);
} catch (_) {}

const topology = () => (state && state.topology) || null;
const selfID = () => (topology() && topology().self) || (state && state.node && state.node.id) || "";

// machines lists this machine first, as the mesh knows it or, without a
// daemon, as its configuration does.
function machines() {
  const topo = topology();
  if (topo && topo.nodes.length > 0) return topo.nodes;
  if (state && state.node) return [{ id: state.node.id, name: state.node.name, reachable: false }];
  return [];
}

// displayNames tells apart the machines that share a name by their ids.
function displayNames(nodes) {
  const count = {};
  for (const n of nodes) count[n.name] = (count[n.name] || 0) + 1;
  const names = {};
  for (const n of nodes) names[n.id] = count[n.name] > 1 ? `${n.name} (${shortID(n.id)})` : n.name;
  return names;
}

// statusOf returns what the daemon on machine id reports, if it answered.
function statusOf(id) {
  if (id === selfID()) return (state && state.status) || null;
  const answer = answers.get(id);
  return (answer && answer.status) || null;
}

// destinationText is a link's destination, with the machine at its other
// end when its name differs, as gst status does.
function destinationText(l) {
  return !l.peer || l.peer.name === l.destination ? l.destination : `${l.destination} (${l.peer.name})`;
}

// problems names what does not work on a machine, as gst status does.
function problems(st) {
  if (!st) return [];
  const found = [];
  for (const l of st.links || []) if (l.state !== "running") found.push(`link to ${destinationText(l)}: ${l.state}`);
  if (st.net && st.net.error) found.push(`network: ${st.net.error}`);
  for (const e of st.exposes || []) {
    if (e.state !== "running") found.push(`exposed ${e.protocol}/${e.port}: ${e.state}${e.error ? `: ${e.error}` : ""}`);
  }
  return found;
}

// findMachine finds the machine that a report names: by its name, its
// id, or an id prefix.
function findMachine(ref) {
  const name = ref.replace(/\.gst$/, "");
  return machines().find((n) => n.name === name || n.id === name || (name.length >= 4 && n.id.startsWith(name)));
}

// via names the machines on the way to n, after this one and before n.
function via(n, names) {
  return n.route && n.route.length > 2 ? n.route.slice(1, -1).map((id) => names[id] || shortID(id)).join(", ") : "";
}

// way says how packets go to another machine, as gst status does.
function way(n, names) {
  if (n.direct) return "direct";
  if (!n.reachable) return n.seen ? "offline, seen " + seenText(n.seen) : "offline";
  const route = via(n, names);
  return route ? "relay via " + route : "relay";
}

// seenText says when a machine was last online, as gst status does.
function seenText(seen) {
  const a = age(seen);
  return a === "now" ? "just now" : a + " ago";
}

function age(since) {
  if (!since) return "";
  const s = Math.max(0, (Date.now() - new Date(since).getTime()) / 1000);
  if (s < 60) return "now";
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  return `${Math.floor(s / 86400)}d`;
}

// ---- parts ----

// mark is a status icon. Healthy things show nothing else, so that the
// words on a page are the things that need attention.
function mark(kind, title) {
  const children = [svg("title", null, title)];
  if (kind === "ok") children.push(svg("circle", { class: "fill", cx: 7, cy: 7, r: 4.5 }));
  if (kind === "wait" || kind === "off") children.push(svg("circle", { cx: 7, cy: 7, r: 4.75 }));
  if (kind === "bad") children.push(svg("circle", { class: "fill", cx: 7, cy: 7, r: 6 }), svg("path", { d: "M5 5l4 4M9 5l-4 4" }));
  return svg("svg", { class: "mark " + kind, viewBox: "0 0 14 14", role: "img", "aria-label": title }, ...children);
}

const linkMark = (l) => mark({ running: "ok", connecting: "wait" }[l.state] || "bad", l.state);

// jump names a machine and shows it when clicked, if the page knows it.
function jump(ref, cls, text) {
  const n = findMachine(ref);
  if (!n) return el("span", { class: "cell " + (cls || "") }, text || ref);
  const b = el("button", { type: "button", class: "jump " + (cls || ""), title: `Show ${n.name}` }, text || ref);
  b.addEventListener("click", () => show(n.id));
  return el("span", { class: "cell" }, b);
}

function copyButton(text, label) {
  const b = el("button", { type: "button", class: "copy" }, label);
  b.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(text);
      b.textContent = "Copied";
    } catch (_) {
      b.textContent = "Not copied";
    }
    setTimeout(() => (b.textContent = label), 1500);
  });
  return b;
}

function group(title, note, rows, empty) {
  if (rows.length === 0 && !empty) return null;
  return el("section", { class: "group" },
    el("h3", null, title, " ", note),
    el("ul", null, rows.length > 0 ? rows : el("li", { class: "row-empty" }, empty)));
}

const count = (n, bad) => el("span", { class: bad ? "bad" : "faint" }, n);

// ---- a machine, open ----

// linksGroup lists the links of machine n, as `gst status -v` does: a
// line "from → to" each, made on "from". The links that n made come from
// its own report, with their destinations and states; the links that other
// machines made to n come from the network, which knows only running ones.
function linksGroup(n, st, names) {
  const isSelf = n.id === selfID();
  const name = (id) => jump(id, "", names[id] || shortID(id));
  const arrow = (from, to) => el("span", { class: "cell edge" }, from, el("span", { class: "faint" }, " → "), to);
  const mine = (st && st.links) || [];
  const rows = mine.map((l) => {
    const running = l.state === "running";
    const end = running
      ? el("span", { class: "row-end", title: `running since ${new Date(l.since).toLocaleString()}${l.reconnects ? `, ${plural(l.reconnects, "reconnect")}` : ""}` }, age(l.since))
      : el("span", { class: "row-end " + (l.state === "down" ? "bad" : "warn") }, l.state);
    const to = l.peer && l.peer.id ? name(l.peer.id) : el("span", { class: "cell mono" }, l.destination);
    return el("li", { class: "row" },
      linkMark(l),
      arrow(names[n.id], to),
      // The destination, when the arrow does not already say it.
      el("span", { class: "cell mono sub", title: "ssh destination" }, l.peer && l.peer.id && l.peer.name !== l.destination ? l.destination : ""),
      end,
      !running && l.error ? el("p", { class: "row-error" }, l.error) : null);
  });
  const topo = topology();
  for (const e of topo ? topo.links.filter((e) => e.to === n.id && e.from !== n.id) : []) {
    rows.push(el("li", { class: "row" },
      mark("ok", "running"),
      arrow(name(e.from), names[n.id]),
      el("span"),
      el("span")));
  }
  const down = mine.filter((l) => l.state !== "running").length;
  return group("Links", count(down ? `${rows.length - down} of ${rows.length} running` : rows.length || "", down), rows,
    isSelf && ["None yet. Add one with ", el("code", null, "gst add <destination>"), "."]);
}

function exposesGroup(st) {
  const exposes = st.exposes || [];
  const down = exposes.filter((e) => e.state !== "running").length;
  const rows = exposes.map((e) => el("li", { class: "row" },
    mark({ running: "ok", waiting: "wait", stopped: "off", failed: "bad" }[e.state], e.state),
    el("span", { class: "cell mono", title: e.address }, e.address || `${e.protocol}/${e.port}`),
    el("span", { class: "cell mono sub", title: e.target }, "→ " + e.target),
    el("span", { class: "row-end " + (e.state === "failed" ? "bad" : e.state === "waiting" ? "warn" : "") }, e.state === "running" ? e.protocol : e.state),
    e.error ? el("p", { class: "row-error" }, e.error) : null));
  return group("Exposed ports", count(down ? `${exposes.length - down} of ${exposes.length} running` : exposes.length || "", down), rows);
}

function note(title, ...text) {
  return el("p", { class: "note" }, el("strong", null, title), " ", ...text);
}

function props(n, names, st) {
  const isSelf = n.id === selfID();
  const route = !isSelf && n.route && n.route.length > 2 ? n.route.map((id) => names[id] || shortID(id)).join(" → ") : "";
  const software = (st && st.software) || n.software;
  const row = (term, value) => (value ? [el("dt", null, term), el("dd", null, value)] : []);
  return el("dl", { class: "props" },
    ...row("Name", el("span", { class: "mono" }, `${n.name}.gst`)),
    ...row("Route", route),
    ...row("Version", software),
    ...row("ID", el("span", null, el("span", { class: "mono" }, shortID(n.id)), copyButton(n.id, "Copy"))));
}

function machineBody(n, names) {
  const isSelf = n.id === selfID();
  const st = statusOf(n.id);
  // This machine's facts are at the top of the page.
  const parts = [isSelf ? null : props(n, names, st)];
  if (isSelf && !st) {
    parts.push(note(state.down ? "gst is down on this machine." : "gst is not running on this machine.", "Start it with ", el("code", null, "gst up"), "."));
  } else if (!st) {
    const answer = answers.get(n.id);
    if (!n.reachable) {
      parts.push(n.direct
        ? note("No link leads to it now,", "so gst ui cannot ask it. Packets still reach it directly.")
        : note("Offline.", "No link leads to it now."));
    } else if (answer && answer.error) {
      parts.push(note("It did not answer:", answer.error));
    } else {
      parts.push(note("Asking…", ""));
    }
  } else {
    const found = problems(st);
    if (found.length > 0) parts.push(el("ul", { class: "problems" }, found.map((p) => el("li", null, p))));
  }
  parts.push(linksGroup(n, st, names), st && exposesGroup(st));
  return el("div", { class: "machine-body" }, ...parts.filter(Boolean));
}

// ---- the machines ----

function nodeMark(n) {
  if (n.id === selfID()) return state.status ? mark("ok", "running") : mark("off", "not running");
  return n.reachable || n.direct ? mark("ok", "reachable") : mark("off", "offline");
}

const chevron = () => svg("svg", { class: "chevron", viewBox: "0 0 12 12", "aria-hidden": "true" }, svg("path", { d: "M4.5 2.5L8 6l-3.5 3.5" }));

function drawMachines(nodes, names) {
  const list = $("machines");
  const focused = list.contains(document.activeElement) ? document.activeElement.dataset.id : "";
  list.replaceChildren(...nodes.map((n) => {
    const isSelf = n.id === selfID();
    const isOpen = open.has(n.id);
    const found = problems(statusOf(n.id)).length;
    const how = isSelf ? "" : way(n, names);
    const address = n.net && n.net.address;
    const b = el("button", { type: "button", class: "machine-row", "data-id": n.id, "aria-expanded": String(isOpen) },
      nodeMark(n),
      el("span", { class: "cell machine-name" }, el("span", null, names[n.id]), isSelf && el("span", { class: "tag" }, "this machine")),
      el("span", { class: "cell mono sub ip" }, address || ""),
      el("span", { class: "cell way" + (how === "offline" ? " off" : ""), title: how }, how),
      el("span", { class: "end" }, found ? plural(found, "problem") : ""),
      chevron());
    b.addEventListener("click", () => toggle(n.id));
    return el("li", { class: "machine" + (isOpen ? " open" : ""), id: "m-" + n.id }, b, isOpen && machineBody(n, names));
  }));
  if (focused) {
    const b = list.querySelector(`[data-id="${CSS.escape(focused)}"]`);
    if (b) b.focus();
  }
  $("count").textContent = nodes.length > 1 ? String(nodes.length) : "";
}

function drawSummary(nodes) {
  const found = nodes.reduce((sum, n) => sum + problems(statusOf(n.id)).length, 0);
  const text = !state.status ? "gst is not running" : plural(nodes.length, "machine");
  $("summary").replaceChildren(text, ...(found ? [" · ", el("span", { class: "bad" }, plural(found, "problem"))] : []));
}

// drawSelf shows this machine at the top.
function drawSelf(nodes) {
  const self = nodes.find((n) => n.id === selfID());
  const st = state.status;
  const row = (term, value) => (value ? [el("dt", null, term), el("dd", null, value)] : []);
  if (!self) {
    $("self").replaceChildren(el("div", { class: "blank-lead" },
      el("strong", null, "gst is not set up on this machine"), "Start it with ", el("code", null, "gst up"), "."));
    return;
  }
  const address = (st && st.net && st.net.address) || (self.net && self.net.address);
  $("self").replaceChildren(
    el("div", { class: "lead" }, nodeMark(self), el("h1", { class: "cell" }, self.name)),
    ...row("Status", st ? "Running" : state.down ? "Down: start it with gst up" : "Not running: start it with gst up"),
    ...row("gst IP", address && el("span", { class: "mono" }, address)),
    ...row("Name", el("span", { class: "mono" }, `${self.name}.gst`)),
    ...row("Version", (st && st.software) || state.software),
    ...row("ID", el("span", null, el("span", { class: "mono" }, shortID(self.id)), copyButton(self.id, "Copy"))));
}

// ---- the map ----

// The machines sit on a circle, so that a line between two of them never
// passes a third, however the network grows. Lines are links and point at
// the machine that was linked to.
function drawRing(nodes, names) {
  const ring = $("ring");
  const topo = topology();
  const alone = !topo || nodes.length < 2;
  ring.toggleAttribute("hidden", alone);
  $("overview").classList.toggle("alone", alone);
  if (alone) return;

  const r = Math.max(82, nodes.length * 13);
  const padX = 88, padY = 28;
  const cx = r + padX, cy = r + padY;
  const pos = {};
  nodes.forEach((n, i) => {
    const a = -Math.PI / 2 + (2 * Math.PI * i) / nodes.length;
    pos[n.id] = { x: cx + r * Math.cos(a), y: cy + r * Math.sin(a), cos: Math.cos(a), sin: Math.sin(a) };
  });

  const seen = {};
  const links = [];
  const tips = [];
  for (const e of topo.links) {
    const a = pos[e.from], b = pos[e.to];
    if (!a || !b || a === b) continue;
    const key = [e.from, e.to].sort().join("\0");
    const k = (seen[key] = (seen[key] || 0) + 1) - 1;
    // Lines bow towards the center; repeated links bow further.
    const mx = (a.x + b.x) / 2, my = (a.y + b.y) / 2;
    const len = Math.hypot(b.x - a.x, b.y - a.y) || 1;
    const side = k * 16 * (e.from < e.to ? 1 : -1);
    const c = { x: mx + (cx - mx) * 0.3 - ((b.y - a.y) / len) * side, y: my + (cy - my) * 0.3 + ((b.x - a.x) / len) * side };
    const away = (p, d) => {
      const l = Math.hypot(c.x - p.x, c.y - p.y) || 1;
      return { x: p.x + ((c.x - p.x) / l) * d, y: p.y + ((c.y - p.y) / l) * d };
    };
    const p = away(a, 9), q = away(b, 12);
    const on = open.has(e.from) || open.has(e.to) ? " on" : "";
    links.push(svg("path", { d: `M${p.x},${p.y} Q${c.x},${c.y} ${q.x},${q.y}`, class: "ring-link" + on },
      svg("title", null, `${names[e.from]} -> ${names[e.to]}`)));
    // The arrowhead, along the line's last stretch.
    const angle = Math.atan2(q.y - c.y, q.x - c.x);
    const at = (d, turn) => `${q.x + d * Math.cos(angle + turn)},${q.y + d * Math.sin(angle + turn)}`;
    tips.push(svg("path", { d: `M${at(3, 0)} L${at(5, 2.5)} L${at(5, -2.5)} Z`, class: "ring-tip" + on }));
  }

  const dots = nodes.map((n) => {
    const p = pos[n.id];
    const side = p.cos > 0.25 ? "start" : p.cos < -0.25 ? "end" : "middle";
    const dx = side === "start" ? 12 : side === "end" ? -12 : 0;
    const dy = side === "middle" ? (p.sin < 0 ? -12 : 20) : 4;
    const name = names[n.id].length > 12 ? names[n.id].slice(0, 11) + "…" : names[n.id];
    const live = n.id === selfID() ? !!state.status : n.reachable || n.direct;
    const g = svg("g", { class: `ring-node${live ? "" : " off"}${open.has(n.id) ? " picked" : ""}` },
      svg("title", null, names[n.id]),
      svg("circle", { class: "hit", cx: p.x, cy: p.y, r: 14 }),
      ...(open.has(n.id) ? [svg("circle", { class: "halo", cx: p.x, cy: p.y, r: 8.5 })] : []),
      svg("circle", { class: "dot", cx: p.x, cy: p.y, r: 4.5 }),
      svg("text", { x: p.x + dx, y: p.y + dy, "text-anchor": side }, name));
    g.addEventListener("click", () => show(n.id));
    return g;
  });

  ring.setAttribute("viewBox", `0 0 ${2 * cx} ${2 * cy}`);
  ring.replaceChildren(...links, ...tips, ...dots);
}

// ---- page ----

function saveOpen() {
  try {
    localStorage.setItem(OPEN_KEY, JSON.stringify([...open]));
  } catch (_) {}
}

function toggle(id) {
  if (open.has(id)) open.delete(id);
  else open.add(id);
  saveOpen();
  draw();
  if (open.has(id)) refreshMachines();
}

// show opens a machine and scrolls to it.
function show(id) {
  if (!open.has(id)) toggle(id);
  const li = document.getElementById("m-" + id);
  if (li) li.scrollIntoView({ block: "nearest", behavior: "smooth" });
}

function showAlert(...texts) {
  const a = $("alert");
  a.replaceChildren(...texts.filter(Boolean).map((t) => el("p", null, t)));
  a.hidden = a.childNodes.length === 0;
}

// draw shows the state, unless nothing changed: redrawing would take away
// a text selection for nothing.
function draw() {
  if (!state) return;
  const nodes = machines();
  // On a first visit, this machine starts open.
  if (open === null) open = new Set(selfID() ? [selfID()] : []);
  const key = JSON.stringify([state, [...answers], [...open], Math.floor(Date.now() / 60000)]);
  if (key === drawn) return;
  drawn = key;

  const names = displayNames(nodes);
  document.title = state.node ? `gst: ${state.node.name}` : "gst";
  $("software").textContent = state.software;
  drawSummary(nodes);
  drawSelf(nodes);
  drawRing(nodes, names);
  drawMachines(nodes, names);
}

const authorized = { headers: { Authorization: "Bearer " + token }, cache: "no-store" };
let stateTimer = 0;

async function refresh() {
  clearTimeout(stateTimer);
  try {
    const res = await fetch("api/state", authorized);
    if (res.status === 401) {
      showAlert("This address lacks the secret. Open the address that gst ui printed, including the part after #.");
      return;
    }
    if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
    state = await res.json();
    showAlert(...(state.errors || []));
    draw();
  } catch (err) {
    showAlert(err instanceof TypeError ? "gst ui stopped. Start it again with gst ui." : String(err.message || err));
  }
  if (!document.hidden) stateTimer = setTimeout(refresh, POLL_MS);
}

// ask stores what a machine's daemon answers.
async function ask(id) {
  try {
    const res = await fetch(`api/nodes/${encodeURIComponent(id)}`, authorized);
    answers.set(id, res.ok ? { status: await res.json() } : { error: (await res.text()).trim() || res.statusText });
  } catch (err) {
    answers.set(id, { error: err instanceof TypeError ? "gst ui stopped." : String(err.message || err) });
  }
  draw();
}

let asking = false;
let machineTimer = 0;

// refreshMachines asks every machine that it can reach, the open ones
// first, a few at a time.
async function refreshMachines() {
  clearTimeout(machineTimer);
  if (asking) return;
  const self = selfID();
  const reachable = machines().filter((n) => n.reachable && n.id !== self).map((n) => n.id);
  for (const id of answers.keys()) if (!reachable.includes(id)) answers.delete(id);
  const queue = reachable.sort((a, b) => (open && open.has(b)) - (open && open.has(a)));
  asking = true;
  await Promise.all(Array.from({ length: MACHINE_REQUESTS }, async () => {
    while (queue.length) await ask(queue.shift());
  }));
  asking = false;
  draw();
  if (!document.hidden) machineTimer = setTimeout(refreshMachines, MACHINE_POLL_MS);
}

document.addEventListener("visibilitychange", () => {
  if (!document.hidden) {
    refresh();
    refreshMachines();
  }
});

refresh().then(refreshMachines);
