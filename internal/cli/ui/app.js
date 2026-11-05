"use strict";

// The page polls /api/state (this machine and the mesh) and /api/nodes/{id}
// (every other machine it can reach, so that selecting one is instant), with
// the token from the address that gst ui printed. Everything shown is built
// with DOM text nodes: names and destinations come from other machines of
// the mesh.

const token = location.hash.slice(1);
const SVG = "http://www.w3.org/2000/svg";
const POLL_MS = 3000;
const MACHINE_POLL_MS = 5000;
const MACHINE_REQUESTS = 4; // at a time
const SELECTED_KEY = "gst.machine";

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
let selected = "";
let drawn = ""; // what the page shows, to leave it alone when nothing changed

try {
  selected = localStorage.getItem(SELECTED_KEY) || "";
} catch (_) {}

const topology = () => (state && state.topology) || null;
const selfID = () => (topology() && topology().self) || (state && state.node && state.node.id) || "";

// machines lists this machine first, as the mesh knows it or, without a
// daemon, as its configuration does.
function machines() {
  const topo = topology();
  if (topo && topo.nodes.length > 0) return topo.nodes;
  if (state && state.node) return [{ id: state.node.id, name: state.node.name, exit: state.node.exit, reachable: false }];
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

// linkText shows a link as the machines at its ends, as gst status does,
// with the link's own name when it differs from the machine it leads to.
function linkText(from, to, name) {
  if (!name) return `${from}->${to} (direct)`;
  return !to || to === name ? `${from}->${name}` : `${from}->${to} (${name})`;
}

// addresses shows the ends of a direct link, as the machine that opened it
// sees them.
const addresses = (e) => (e.local ? `${e.local} → ${e.remote}` : "");

// directEdges lists the direct links of the mesh.
function directEdges() {
  const topo = topology();
  return topo ? topo.edges.filter((e) => e.direct) : [];
}

// problems names what does not work on a machine, as gst status does.
function problems(st) {
  if (!st) return [];
  const found = [];
  for (const l of st.links || []) if (l.state !== "running") found.push(`link ${linkText(st.node.name, l.peer && l.peer.name, l.name)} is ${l.state}`);
  if (st.proxy && st.proxy.error) found.push(`proxy ${st.proxy.listen} failed`);
  for (const m of st.maps || []) if (m.error) found.push(`map ${m.listen} failed`);
  return found;
}

// findMachine finds the machine that a map or a route names, as far as the
// page can tell: by its name, the name of a link to it, or an id prefix.
function findMachine(ref) {
  const nodes = machines();
  const name = ref.replace(/\.gst$/, "");
  const byName = nodes.find((n) => n.name === name || n.id === name || (name.length >= 4 && n.id.startsWith(name)));
  if (byName) return byName;
  const topo = topology();
  const edge = topo && topo.edges.find((e) => e.link === name);
  return edge ? nodes.find((n) => n.id === edge.to) : undefined;
}

// via names the machines on the way to n, after this one and before n.
function via(n, names) {
  return n.path && n.path.length > 2 ? n.path.slice(1, -1).map((id) => names[id] || shortID(id)).join(", ") : "";
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
const listenMark = (l) => (l.error ? mark("bad", "failed") : mark("ok", "listening"));

// jump names a machine and selects it when clicked, if the mesh knows it.
function jump(ref, cls, text) {
  const n = findMachine(ref);
  if (!n) return el("span", { class: "cell " + (cls || "") }, text || ref);
  const b = el("button", { type: "button", class: "jump cell " + (cls || ""), title: `Show ${n.name}` }, text || ref);
  b.addEventListener("click", () => select(n.id));
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
  return el("section", { class: "group" },
    el("h2", null, title, note),
    el("ul", null, rows.length > 0 ? rows : empty && el("li", { class: "row row-empty" }, empty)));
}

const count = (n, bad) => el("span", { class: bad ? "bad" : "" }, n);

// ---- the selected machine ----

function linksGroup(st, isSelf) {
  const links = st.links || [];
  const down = links.filter((l) => l.state !== "running").length;
  const rows = links.map((l) => {
    const running = l.state === "running";
    const end = running
      ? el("span", { class: "row-end", title: `running since ${new Date(l.since).toLocaleString()}${l.reconnects ? `, ${plural(l.reconnects, "reconnect")}` : ""}` }, age(l.since))
      : el("span", { class: "row-end " + (l.state === "down" ? "bad" : "warn") }, l.state);
    return el("li", { class: "row" },
      linkMark(l),
      l.peer && l.peer.id
        ? jump(l.peer.name, "row-name", linkText(st.node.name, l.peer.name, l.name))
        : el("span", { class: "cell row-name" }, linkText(st.node.name, "", l.name)),
      el("span", { class: "cell mono sub row-wide", title: l.destination }, l.destination),
      end,
      !running && l.error ? el("p", { class: "row-error" }, l.error) : null);
  });
  return group("Links", count(down ? `${links.length - down} of ${links.length} running` : links.length, down), rows,
    isSelf && ["Add one with ", el("code", null, "gst add <name> [destination]")]);
}

// linkedFrom lists the links that other machines opened to this one,
// which only the mesh knows.
function linkedFromGroup(id, names) {
  const topo = topology();
  const edges = topo ? topo.edges.filter((e) => e.to === id && !e.direct) : [];
  if (edges.length === 0) return null;
  const rows = edges.map((e) => el("li", { class: "row" },
    mark("ok", "running"),
    jump(names[e.from] || shortID(e.from), "row-name", linkText(names[e.from] || shortID(e.from), names[e.to], e.link))));
  return group("Linked from", count(edges.length), rows);
}

// directGroup lists the direct links of this machine, whichever end
// opened them.
function directGroup(id, names) {
  const edges = directEdges().filter((e) => e.from === id || e.to === id);
  if (edges.length === 0) return null;
  const rows = edges.map((e) => {
    const other = e.from === id ? e.to : e.from;
    return el("li", { class: "row" },
      mark("ok", "running"),
      jump(names[other] || shortID(other), "row-name", linkText(names[e.from] || shortID(e.from), names[e.to] || shortID(e.to), "")),
      el("span", { class: "cell mono sub row-wide", title: addresses(e) }, addresses(e)));
  });
  return group("Direct links", count(edges.length), rows);
}

// proxyEnv is the line that gst proxy prints, for an address that clients
// can connect to.
function proxyEnv(listen) {
  const i = listen.lastIndexOf(":");
  let host = listen.slice(0, i);
  if (host === "0.0.0.0" || host === "" || host === "::" || host === "[::]") host = "127.0.0.1";
  const a = `${host}:${listen.slice(i + 1)}`;
  return `export ALL_PROXY=socks5h://${a} http_proxy=http://${a} https_proxy=http://${a}`;
}

function proxyGroup(st, isSelf) {
  const p = st.proxy || {};
  if (!p.listen) {
    return group("Proxy", count("off"), [], isSelf && ["Turn it on with ", el("code", null, "gst proxy 127.0.0.1:1080")]);
  }
  const env = proxyEnv(p.listen);
  const row = el("li", { class: "row" },
    listenMark(p),
    el("span", { class: "cell mono" }, p.listen),
    isSelf ? el("span", { class: "cell mono faint row-wide", title: env }, env) : el("span", { class: "row-wide" }),
    p.error ? el("span", { class: "row-end bad" }, "failed") : isSelf ? el("span", { class: "row-end" }, copyButton(env, "Copy")) : el("span"),
    p.error ? el("p", { class: "row-error" }, p.error) : null);
  return group("Proxy", null, [row]);
}

function mapsGroup(st, isSelf) {
  const maps = st.maps || [];
  const failed = maps.filter((m) => m.error).length;
  const rows = maps.map((m) => el("li", { class: "row" },
    listenMark(m),
    el("span", { class: "cell mono", title: m.listen }, m.listen),
    el("span", { class: "cell mono sub", title: m.target }, m.target),
    jump(m.machine),
    m.error ? el("span", { class: "row-end bad" }, "failed") : el("span"),
    m.error ? el("p", { class: "row-error" }, m.error) : null));
  return group("Maps", count(failed ? `${maps.length - failed} of ${maps.length} listening` : maps.length, failed), rows,
    isSelf && ["Add one with ", el("code", null, "gst map add <listen> <machine>:<target>")]);
}

// routeNote says what the mesh shows about the machine a route goes through.
function routeNote(via) {
  if (!topology()) return "";
  const n = findMachine(via);
  if (!n) return "not in the mesh";
  return n.exit ? "" : "not an exit";
}

function routesGroup(st, isSelf) {
  const routes = st.routes || [];
  const rows = routes.map((r) => {
    const note = routeNote(r.via);
    return el("li", { class: "row" },
      note ? mark("off", note) : mark("ok", "goes out through " + r.via),
      el("span", { class: "cell mono", title: r.match }, r.match),
      el("span"),
      jump(r.via),
      note ? el("span", { class: "row-end warn" }, note) : el("span"));
  });
  return group("Routes", count(routes.length), rows,
    isSelf && ["Add one with ", el("code", null, "gst route add <match> <machine>")]);
}

function blank(title, ...text) {
  return el("div", { class: "blank" }, el("strong", null, title), el("p", null, text));
}

function drawHead(n, names, st) {
  const isSelf = n && n.id === selfID();
  const found = problems(st);
  $("head").replaceChildren(...[
    el("h1", { class: "truncate" }, n ? names[n.id] : "This machine"),
    isSelf && el("span", { class: "tag" }, "this machine"),
    n && n.exit && el("span", { class: "tag" }, "exit"),
    found.length > 0 && el("span", { class: "head-problems", title: found.join("\n") }, plural(found.length, "problem")),
  ].filter(Boolean));
}

function drawDetail(n, names) {
  const detail = $("detail");
  const isSelf = !n || n.id === selfID();
  const st = n ? statusOf(n.id) : state.status;
  if (isSelf && !st) {
    const title = state.down ? "gst is down on this machine" : "The gst daemon is not running";
    detail.replaceChildren(blank(title, "Start it with ", el("code", null, "gst up"), "."));
    return;
  }
  if (!st) {
    const answer = answers.get(n.id);
    if (!n.reachable) {
      detail.replaceChildren(blank(`${names[n.id]} is offline`, "No running link leads to it from this machine. Its last report stays in the mesh until it expires."));
    } else if (answer && answer.error) {
      detail.replaceChildren(blank(`${names[n.id]} did not answer`, answer.error));
    } else {
      detail.replaceChildren(blank("Asking…", `Waiting for ${names[n.id]} to answer through the mesh.`));
    }
    return;
  }
  detail.replaceChildren(...[
    linksGroup(st, isSelf),
    n && directGroup(n.id, names),
    n && linkedFromGroup(n.id, names),
    proxyGroup(st, isSelf),
    mapsGroup(st, isSelf),
    routesGroup(st, isSelf),
  ].filter(Boolean));
}

// ---- the machines ----

function nodeMark(n) {
  if (n.id === selfID()) return state.status ? mark("ok", "this machine, running") : mark("off", "this machine, not running");
  return n.reachable ? mark("ok", "online") : mark("off", "offline");
}

function drawMachines(nodes, names) {
  const nav = $("machines");
  const focused = nav.contains(document.activeElement) ? document.activeElement.dataset.id : "";
  nav.replaceChildren(...nodes.map((n) => {
    const found = problems(statusOf(n.id)).length;
    const route = via(n, names);
    const b = el("button", { type: "button", class: "machine", "data-id": n.id, "aria-current": n.id === selected ? "true" : "false" },
      nodeMark(n),
      el("span", { class: "truncate machine-name" }, names[n.id]),
      el("span", { class: "machine-meta" },
        found ? el("span", { class: "count", title: plural(found, "problem") }, found) : null,
        route ? el("span", { class: "truncate", title: "via " + route }, "via " + route) : null,
        !n.reachable && n.id !== selfID() ? el("span", null, "offline") : null,
        n.direct ? el("span", { class: "tag" }, "direct") : null,
        n.exit ? el("span", { class: "tag" }, "exit") : null));
    b.addEventListener("click", () => select(n.id));
    return b;
  }));
  if (focused) {
    const b = nav.querySelector(`[data-id="${CSS.escape(focused)}"]`);
    if (b) b.focus();
  }
}

function drawSummary(nodes) {
  const online = nodes.filter((n) => n.reachable || n.id === selfID()).length;
  const found = nodes.reduce((sum, n) => sum + problems(statusOf(n.id)).length, 0);
  const text = !state.status ? "not running" : online === nodes.length ? `${online} online` : `${online} of ${nodes.length} online`;
  $("summary").replaceChildren(text, ...(found ? [", ", el("span", { class: "bad" }, plural(found, "problem"))] : []));
}

// ---- the mesh ----

// The machines sit on a circle, so that a line between two of them never
// passes a third, however the mesh grows. Lines are links and point at the
// machine that was linked to.
function drawRing(nodes, names) {
  const ring = $("ring");
  const topo = topology();
  const alone = !topo || nodes.length < 2;
  ring.toggleAttribute("hidden", alone);
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
  for (const e of topo.edges) {
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
    const on = e.from === selected || e.to === selected ? " on" : "";
    links.push(svg("path", { d: `M${p.x},${p.y} Q${c.x},${c.y} ${q.x},${q.y}`, class: "ring-link" + (e.direct ? " direct" : "") + on },
      svg("title", null, [linkText(names[e.from], names[e.to], e.link), addresses(e)].filter(Boolean).join("\n"))));
    // The arrowhead, along the line's last stretch.
    const angle = Math.atan2(q.y - c.y, q.x - c.x);
    const at = (d, turn) => `${q.x + d * Math.cos(angle + turn)},${q.y + d * Math.sin(angle + turn)}`;
    tips.push(svg("path", { d: `M${at(3, 0)} L${at(5, 2.5)} L${at(5, -2.5)} Z`, class: "ring-tip" + on }));
  }

  const dots = nodes.map((n) => {
    const p = pos[n.id];
    const online = n.reachable || n.id === selfID();
    const side = p.cos > 0.25 ? "start" : p.cos < -0.25 ? "end" : "middle";
    const dx = side === "start" ? 12 : side === "end" ? -12 : 0;
    const dy = side === "middle" ? (p.sin < 0 ? -12 : 20) : 4;
    const name = names[n.id].length > 12 ? names[n.id].slice(0, 11) + "…" : names[n.id];
    const g = svg("g", { class: `ring-node ${online ? "online" : "offline"}${n.id === selected ? " picked" : ""}` },
      svg("title", null, `${names[n.id]}${online ? "" : ", offline"}`),
      svg("circle", { class: "hit", cx: p.x, cy: p.y, r: 14 }),
      ...(n.id === selected ? [svg("circle", { class: "halo", cx: p.x, cy: p.y, r: 8.5 })] : []),
      svg("circle", { class: "dot", cx: p.x, cy: p.y, r: 4.5 }),
      svg("text", { x: p.x + dx, y: p.y + dy, "text-anchor": side }, name));
    g.addEventListener("click", () => select(n.id));
    return g;
  });

  ring.setAttribute("viewBox", `0 0 ${2 * cx} ${2 * cy}`);
  ring.replaceChildren(...links, ...tips, ...dots);
}

// drawDirects lists every direct link of the mesh, beside the ring that
// draws them dashed.
function drawDirects(names) {
  const box = $("directs");
  const edges = directEdges();
  box.toggleAttribute("hidden", edges.length === 0);
  box.replaceChildren(el("h2", null, "Direct links"), ...edges.map((e) => el("p", null,
    el("span", { class: "direct-name" }, `${names[e.from] || shortID(e.from)} → ${names[e.to] || shortID(e.to)}`),
    el("span", { class: "mono sub direct-addr" }, addresses(e)))));
}

function drawProps(n, names, st) {
  const props = $("props");
  if (!n) {
    props.replaceChildren();
    return;
  }
  const isSelf = n.id === selfID();
  const route = n.path && n.path.length > 1 ? n.path.map((id) => names[id] || shortID(id)).join(" → ") : "";
  const software = (st && st.software) || n.software;
  const row = (term, value) => (value ? [el("dt", null, term), el("dd", null, value)] : []);
  props.replaceChildren(
    ...row("Status", isSelf ? (state.status ? "Running" : "Not running") : n.reachable ? "Online" : "Offline"),
    ...row("Path", route),
    ...row("Exit", n.exit ? "Yes, connects out for others" : "No"),
    ...row("Version", software),
    ...row("ID", el("span", null, el("span", { class: "mono" }, shortID(n.id)), " ", copyButton(n.id, "Copy"))));
}

// ---- page ----

function select(id) {
  if (id === selected) return;
  selected = id;
  try {
    localStorage.setItem(SELECTED_KEY, id);
  } catch (_) {}
  draw();
}

function move(step) {
  const nodes = machines();
  const i = nodes.findIndex((n) => n.id === selected);
  const next = nodes[Math.min(nodes.length - 1, Math.max(0, i + step))];
  if (!next) return;
  select(next.id);
  const b = $("machines").querySelector(`[data-id="${CSS.escape(next.id)}"]`);
  if (b) b.scrollIntoView({ block: "nearest" });
}

document.addEventListener("keydown", (e) => {
  if (e.metaKey || e.ctrlKey || e.altKey) return;
  if (e.key === "j" || e.key === "ArrowDown") move(1);
  else if (e.key === "k" || e.key === "ArrowUp") move(-1);
  else return;
  e.preventDefault();
});

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
  // A remembered machine that the mesh no longer lists gives way to this one.
  if (!nodes.some((n) => n.id === selected)) selected = selfID();
  const key = JSON.stringify([state, [...answers], selected, Math.floor(Date.now() / 60000)]);
  if (key === drawn) return;
  drawn = key;

  const names = displayNames(nodes);
  const n = nodes.find((m) => m.id === selected);
  const st = n ? statusOf(n.id) : state.status;
  document.title = state.node ? `gst: ${state.node.name}` : "gst";
  $("software").textContent = state.software;
  drawSummary(nodes);
  drawMachines(nodes, names);
  drawHead(n, names, st);
  drawDetail(n, names);
  drawRing(nodes, names);
  drawDirects(names);
  drawProps(n, names, st);
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

// refreshMachines asks every machine that it can reach, the selected one
// first, a few at a time.
async function refreshMachines() {
  clearTimeout(machineTimer);
  if (asking) return;
  const self = selfID();
  const reachable = machines().filter((n) => n.reachable && n.id !== self).map((n) => n.id);
  for (const id of answers.keys()) if (!reachable.includes(id)) answers.delete(id);
  const queue = reachable.sort((a, b) => (b === selected) - (a === selected));
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
