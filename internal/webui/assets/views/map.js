// The live map: what is connected to this gateway, and how much traffic
// leaves the network. Hosted models sit below a line, and every request to
// one is drawn crossing it.
//
// A canvas rather than SVG: hundreds of packets a minute as DOM nodes
// stutter. It draws only when something moved.
//
// Each packet is a real row of the request log, not an animated average.

import { api } from "../api.js";
import { orgNameOf } from "./orgs.js";
import { outcomeOf } from "../status.js";
import {
  h,
  compact,
  money,
  amount,
  ms,
  num,
  table,
  empty,
  RANGES,
  currentRange,
  rangePicker,
  go,
  liveControl,
  sectionHead,
  dash,
  rangeOf,
  followRequests,
} from "../ui.js";

// Fixed sizes rather than measured, so the map does not jump when a model is
// added.
const BOX = {
  // Heights must stay in step with rows().
  clientW: 176,
  clientH: 74,
  modelW: 190,
  modelH: 72,
  gatewayW: 200,
  gatewayH: 92,
  gapY: 12,
  pad: 18,
  // Room for the lines between columns to curve.
  gapX: 40,
};

// Below this the card scrolls sideways rather than overlap the boxes.
const MIN_WIDTH =
  BOX.pad * 2 + BOX.clientW + BOX.gatewayW + BOX.modelW + BOX.gapX * 2;

// Boxes per column before the quietest fold into one. The tables below the
// canvas still list every one.
const MAX_CLIENTS = 7;
const MAX_MODELS = 6;

// Packet speed in pixels per millisecond. The pause at the gateway shows the
// request going through it, not just past it.
const SPEED = 0.55;
const HOLD_MS = 110;
// Most packets drawn per stream batch, so a batch of 200 rows is not a wall.
// The rest are counted but not drawn.
const MAX_SPAWN = 48;
const MAX_PACKETS = 400;

export async function mapView(ctx) {
  const since = currentRange();
  ctx.setSubtitle(rangeOf(since, RANGES[0]).long);

  const data = await api.trafficMap(ctx.orgID, since);

  const scene = sceneFrom(data);
  const canvas = h("canvas", {
    class: "map-canvas",
    // For screen readers. The tables underneath hold the rest.
    role: "img",
    "aria-label": describe(scene),
  });
  const tip = h("div", { class: "map-tip", hidden: true });
  const holder = h("div", { class: "card map-holder" }, canvas, tip);

  const live = liveControl({
    key: "keera.map.live",
    label: "Follow the request log",
    titles: {
      live: "Live: requests cross the map as they happen. Click to pause.",
      connecting: "Reconnecting. Click to stop.",
      paused: "Paused. The numbers cover the selected period.",
      stopped:
        "The live stream stopped. Click to try again, or reload the page.",
    },
    words: { paused: "Paused", stopped: "Reconnect" },
  });
  const head = h(
    "div",
    { class: "map-head" },
    h(
      "div",
      { class: "map-title" },
      h("h2", {}, "What this gateway is connected to"),
      h(
        "div",
        { class: "hint" },
        "Boxes show who called in this period and where requests can go. " +
          "Packets are live requests.",
      ),
    ),
    h("div", { class: "spacer" }),
    live.el,
    rangePicker(ctx, since),
  );

  const painter = paint(ctx, canvas, tip, scene);
  ctx.onTeardown(painter.stop);

  watch(ctx, { since, scene, painter, live });
  refresh(ctx, { since, scene, painter });

  return h(
    "div",
    { class: "map-page" },
    head,
    holder,
    modelReport(ctx, scene),
    clientReport(scene),
  );
}

/** sceneFrom turns the report into boxes and lines. Models come from the
 *  catalogue, not the traffic: a quiet hosted model is still a way out of
 *  the network. Clients are whatever has called. */
function sceneFrom(data) {
  const clients = fold(
    (data.clients || []).map((c) => ({
      kind: "client",
      key: c.client,
      label: c.label || "Unidentified client",
      cell: cellOf(c),
      pulse: 0,
    })),
    MAX_CLIENTS,
    "client",
  );

  const models = (data.models || []).map((m) => ({
    kind: "model",
    key: m.alias,
    label: m.alias,
    cell: cellOf(m),
    pulse: 0,
    hosting: m.hosting,
    endpoint: m.endpoint || "",
    enabled: m.enabled,
    kindName: m.kind || "",
  }));
  const inside = fold(
    models.filter((m) => m.hosting !== "external"),
    MAX_MODELS,
    "model",
  );
  const outside = fold(
    models.filter((m) => m.hosting === "external"),
    MAX_MODELS,
    "model",
  );

  const gateway = {
    kind: "gateway",
    key: "gateway",
    label: "Keera Gateway",
    pulse: 0,
    sub: hostOf(data.gateway_url),
    cell: cellOf(data.gateway || {}),
  };

  const scene = {
    clients,
    inside,
    outside,
    gateway,
    models,
    total: gateway.cell,
    // So hovering a client lights only the models it reached.
    flows: (data.flows || []).map((f) => ({
      client: f.client,
      alias: f.alias,
    })),
    from: data.from,
    to: data.to,
    // Live request batches as { at, n }, oldest first, for the gateway's rate.
    beats: beatsFrom(data.last_minute),
    height: 0,
  };
  index(scene);
  return scene;
}

/** beatsFrom seeds the last minute from the report (per second, newest
 *  first). Without it the rate would take a minute to be right. */
function beatsFrom(counts) {
  const now = Date.now();
  const beats = [];
  for (let i = (counts || []).length - 1; i >= 0; i--) {
    if (counts[i] > 0) beats.push({ at: now - i * 1000 - 500, n: counts[i] });
  }
  return beats;
}

function cellOf(c) {
  return {
    requests: c.requests || 0,
    input_tokens: c.input_tokens || 0,
    output_tokens: c.output_tokens || 0,
    cost_micros: c.cost_micros || 0,
    refused: c.refused || 0,
    failed: c.failed || 0,
    ttft_median_ms: c.ttft_median_ms || 0,
  };
}

/** fold keeps the busiest and adds up the rest into one box. */
function fold(list, max, kind) {
  if (list.length <= max) return list;
  const kept = list.slice(0, max - 1);
  const rest = list.slice(max - 1);
  const cell = cellOf({});
  for (const n of rest) {
    cell.requests += n.cell.requests;
    cell.input_tokens += n.cell.input_tokens;
    cell.output_tokens += n.cell.output_tokens;
    cell.cost_micros += n.cell.cost_micros;
    cell.refused += n.cell.refused;
    cell.failed += n.cell.failed;
  }
  kept.push({
    kind,
    key: " rest",
    label: rest.length + " more",
    cell,
    rest,
    pulse: 0,
    hosting: rest[0] && rest[0].hosting,
    folded: true,
  });
  return kept;
}

/** index maps a live row's client and alias to their boxes. */
function index(scene) {
  scene.byClient = new Map();
  for (const c of scene.clients) {
    scene.byClient.set(c.key, c);
    for (const r of c.rest || []) scene.byClient.set(r.key, c);
  }
  scene.byModel = new Map();
  for (const m of [...scene.inside, ...scene.outside]) {
    scene.byModel.set(m.key, m);
    for (const r of m.rest || []) scene.byModel.set(r.key, m);
  }
}

function hostOf(url) {
  if (!url) return "";
  try {
    return new URL(url).host;
  } catch {
    return url.replace(/^https?:\/\//, "").replace(/\/.*$/, "");
  }
}

/** describe is the picture in one sentence, for screen readers. */
function describe(scene) {
  const folded = scene.outside.find((m) => m.folded);
  const out =
    scene.outside.filter((m) => !m.folded).length +
    (folded ? folded.rest.length : 0);
  return (
    scene.clients.length +
    " client types calling this gateway, " +
    scene.inside.length +
    " models inside your network and " +
    out +
    " at external providers. " +
    tokenShare(scene) +
    "% of tokens in this period left your network."
  );
}

/** tokenShare is the percentage of tokens that went outside the network. */
function tokenShare(scene) {
  let outside = 0;
  for (const m of scene.outside)
    outside += m.cell.input_tokens + m.cell.output_tokens;
  const total = scene.total.input_tokens + scene.total.output_tokens;
  if (!total) return 0;
  return Math.round((outside / total) * 100);
}

/** layout places every box and returns the canvas height. The customer's
 *  network is above, hosted providers below, and the boundary spans the full
 *  width so every line to a hosted model crosses it. */
function layout(scene, width) {
  const P = BOX.pad;
  const insideH = Math.max(
    stack(scene.clients, BOX.clientH),
    stack(scene.inside, BOX.modelH),
    BOX.gatewayH,
    96,
  );
  const insideTop = P + 30; // under the band's caption
  const outsideH = Math.max(stack(scene.outside, BOX.modelH), 52);
  const lineY = insideTop + insideH + 30;

  scene.geometry = {
    width,
    insideTop,
    insideH,
    lineY,
    outsideH,
    outsideTop: lineY + 40,
    clientX: P,
    modelX: width - P - BOX.modelW,
    // Centred, but on a narrow card never into the room the lines need.
    gatewayX: Math.min(
      Math.max(
        Math.round((width - BOX.gatewayW) / 2),
        P + BOX.clientW + BOX.gapX,
      ),
      width - P - BOX.modelW - BOX.gapX - BOX.gatewayW,
    ),
  };
  const g = scene.geometry;

  place(
    scene.clients,
    g.clientX,
    BOX.clientW,
    BOX.clientH,
    g.insideTop,
    insideH,
  );
  place(scene.inside, g.modelX, BOX.modelW, BOX.modelH, g.insideTop, insideH);
  place(
    scene.outside,
    g.modelX,
    BOX.modelW,
    BOX.modelH,
    g.outsideTop,
    outsideH,
  );

  scene.gateway.x = g.gatewayX;
  scene.gateway.w = BOX.gatewayW;
  scene.gateway.h = BOX.gatewayH;
  scene.gateway.y = Math.round(g.insideTop + (insideH - BOX.gatewayH) / 2);

  // With nothing hosted outside, the lower band is left out.
  scene.height = scene.outside.length
    ? g.outsideTop + outsideH + P + 6
    : g.insideTop + insideH + P + 6;
  return scene.height;
}

/** stack is the height of one column of boxes. */
function stack(list, hh) {
  return list.length ? list.length * (hh + BOX.gapY) - BOX.gapY : 0;
}

/** place stacks one column, centred in its band. */
function place(list, x, w, hh, top, bandH) {
  let y = Math.round(top + (bandH - stack(list, hh)) / 2);
  for (const n of list) {
    n.x = x;
    n.y = y;
    n.w = w;
    n.h = hh;
    y += hh + BOX.gapY;
  }
}

/** legOne and legTwo are the two halves of a request's path. Computed each
 *  time, not stored, so a relayout cannot send a packet to an old position. */
function legOne(node, gateway) {
  return curve(
    node.x + node.w,
    node.y + node.h / 2,
    gateway.x,
    gateway.y + gateway.h / 2,
  );
}

function legTwo(gateway, node) {
  return curve(
    gateway.x + gateway.w,
    gateway.y + gateway.h / 2,
    node.x,
    node.y + node.h / 2,
  );
}

function curve(x1, y1, x2, y2) {
  const dx = (x2 - x1) * 0.45;
  return { x1, y1, cx1: x1 + dx, cy1: y1, cx2: x2 - dx, cy2: y2, x2, y2 };
}

function at(c, t) {
  const u = 1 - t;
  const a = u * u * u,
    b = 3 * u * u * t,
    d = 3 * u * t * t,
    e = t * t * t;
  return {
    x: a * c.x1 + b * c.cx1 + d * c.cx2 + e * c.x2,
    y: a * c.y1 + b * c.cy1 + d * c.cy2 + e * c.y2,
  };
}

// Straight-line distance is close enough for a curve this shallow; it only
// sets how long a packet takes.
function span(c) {
  return Math.hypot(c.x2 - c.x1, c.y2 - c.y1);
}

/** paint owns the canvas: its size, palette, animation and pointer. */
function paint(ctx, canvas, tip, scene) {
  const g2 = canvas.getContext("2d");
  const packets = [];
  let colors = palette();
  let dirty = true;
  let painted = 0; // device pixels per drawing pixel
  let hover = null;
  let raf = 0;
  let last = 0;
  const still = window.matchMedia("(prefers-reduced-motion: reduce)");

  function resize() {
    const holder = canvas.parentElement;
    const width = Math.max(MIN_WIDTH, Math.floor(holder.clientWidth) - 2);
    const height = layout(scene, width);
    // Screen density times the panel's `html { zoom }`, which stretches a
    // canvas without telling it and blurs its labels. Measured over reported
    // width is that zoom.
    const stretch = holder.clientWidth
      ? holder.getBoundingClientRect().width / holder.clientWidth
      : 1;
    const scale = Math.min(
      4,
      Math.max(1, (window.devicePixelRatio || 1) * stretch),
    );
    // Resizing the canvas resizes the observed card, so skip when nothing
    // changed, or this loops.
    if (
      canvas.style.width === width + "px" &&
      canvas.style.height === height + "px" &&
      painted === scale
    ) {
      return;
    }
    painted = scale;
    canvas.style.width = width + "px";
    canvas.style.height = height + "px";
    canvas.width = Math.round(width * scale);
    canvas.height = Math.round(height * scale);
    g2.setTransform(scale, 0, 0, scale, 0, 0);
    // The first resize is the first moment the stylesheet can be read.
    colors = palette();
    dirty = true;
  }

  const observer = new ResizeObserver(resize);
  observer.observe(canvas.parentElement);

  // A canvas cannot use CSS custom properties, so reread the colours when
  // the theme changes, in the topbar or the operating system.
  const themeWatch = new MutationObserver(() => {
    colors = palette();
    dirty = true;
  });
  themeWatch.observe(document.documentElement, {
    attributes: true,
    attributeFilter: ["data-theme"],
  });
  const scheme = window.matchMedia("(prefers-color-scheme: dark)");
  const onScheme = () => {
    colors = palette();
    dirty = true;
  };
  scheme.addEventListener("change", onScheme);

  function frame(now) {
    raf = requestAnimationFrame(frame);
    const dt = Math.min(64, now - (last || now));
    last = now;
    if (advance(dt) || dirty) {
      dirty = false;
      render(g2, scene, colors, packets, hover);
    }
  }

  /** advance moves packets and fades pulses, and reports whether anything
   *  changed, so an idle map does not repaint. */
  function advance(dt) {
    let moved = false;
    for (const n of boxes(scene)) {
      if (n.pulse > 0) {
        n.pulse = Math.max(0, n.pulse - dt / 420);
        moved = true;
      }
    }
    for (let i = packets.length - 1; i >= 0; i--) {
      if (step(packets[i], dt)) packets.splice(i, 1);
      moved = true;
    }
    // The per-minute rate drops as beats age out, even with no new requests.
    const cut = Date.now() - 60000;
    while (scene.beats.length && scene.beats[0].at < cut) {
      scene.beats.shift();
      moved = true;
    }
    return moved;
  }

  function step(p, dt) {
    if (p.delay > 0) {
      p.delay -= dt;
      return false;
    }
    switch (p.phase) {
      case "out1": {
        p.t +=
          (dt * SPEED) / Math.max(60, span(legOne(p.client, scene.gateway)));
        if (p.t >= 1) {
          p.t = 0;
          scene.gateway.pulse = 1;
          p.phase = p.outcome === "refused" || !p.model ? "burst" : "hold";
          p.wait = HOLD_MS;
        }
        return false;
      }
      case "hold":
        p.wait -= dt;
        if (p.wait <= 0) p.phase = "out2";
        return false;
      case "burst":
        // A refusal never leaves the gateway, nor does a packet to a model
        // the map does not draw.
        p.wait -= dt * 2.2;
        return p.wait <= -260;
      case "out2": {
        p.t +=
          (dt * SPEED) / Math.max(60, span(legTwo(scene.gateway, p.model)));
        if (p.t >= 1) {
          p.t = 1;
          p.model.pulse = 1;
          p.phase = "back";
        }
        return false;
      }
      case "back": {
        // The answer, drawn thinner. Without it, a model that answers and one
        // that has gone quiet look the same.
        const c =
          p.leg === 1
            ? legTwo(scene.gateway, p.model)
            : legOne(p.client, scene.gateway);
        p.t -= (dt * SPEED * 1.15) / Math.max(60, span(c));
        if (p.t <= 0) {
          if (p.leg === 1) {
            p.leg = 0;
            p.t = 1;
          } else {
            p.client.pulse = 1;
            return true;
          }
        }
        return false;
      }
      default:
        return true;
    }
  }

  function send(client, model, outcome, delay) {
    dirty = true;
    if (still.matches) {
      // Reduced motion: light the boxes, but draw no packets.
      client.pulse = 1;
      scene.gateway.pulse = 1;
      if (outcome !== "refused" && model) model.pulse = 1;
      return;
    }
    if (packets.length >= MAX_PACKETS) return;
    packets.push({
      client,
      model,
      outcome,
      phase: "out1",
      t: 0,
      leg: 1,
      wait: 0,
      delay: delay || 0,
    });
  }

  /* A click opens a box's screen; hovering shows the numbers that do not
     fit on the box. */
  function locate(e) {
    // The pointer and the canvas's rectangle come in zoomed pixels, the
    // boxes in the canvas's own, so undo the panel's `html { zoom }`.
    const r = canvas.getBoundingClientRect();
    const zoom = r.width / canvas.clientWidth || 1;
    const x = (e.clientX - r.left) / zoom,
      y = (e.clientY - r.top) / zoom;
    for (const n of boxes(scene)) {
      if (x >= n.x && x <= n.x + n.w && y >= n.y && y <= n.y + n.h) return n;
    }
    return null;
  }

  const onMove = (e) => {
    const found = locate(e);
    if (found === hover) return;
    hover = found;
    dirty = true;
    canvas.style.cursor = found && target(found) ? "pointer" : "default";
    if (found) showTip(tip, canvas, found);
    else tip.hidden = true;
  };
  const onLeave = () => {
    hover = null;
    tip.hidden = true;
    dirty = true;
  };
  const onClick = (e) => {
    const found = locate(e);
    const path = found && target(found);
    if (path) ctx.navigate(path);
  };
  canvas.addEventListener("mousemove", onMove);
  canvas.addEventListener("mouseleave", onLeave);
  canvas.addEventListener("click", onClick);

  raf = requestAnimationFrame(frame);

  return {
    send,
    touch: () => {
      dirty = true;
    },
    // Packets in flight point at the replaced boxes, so drop them; the new
    // report already counts them.
    reset() {
      packets.length = 0;
      dirty = true;
      resize();
    },
    stop() {
      cancelAnimationFrame(raf);
      observer.disconnect();
      themeWatch.disconnect();
      scheme.removeEventListener("change", onScheme);
      canvas.removeEventListener("mousemove", onMove);
      canvas.removeEventListener("mouseleave", onLeave);
      canvas.removeEventListener("click", onClick);
    },
  };
}

/** target is where a box leads, or null for a folded box. */
function target(n) {
  if (n.kind === "model" && !n.folded)
    return "/models/" + encodeURIComponent(n.key);
  if (n.kind === "gateway") return "/requests";
  return null;
}

function boxes(scene) {
  return [...scene.clients, scene.gateway, ...scene.inside, ...scene.outside];
}

/** palette reads the root, not the canvas: the view is built before it is on
 *  the page, and a detached element resolves every property to "", which
 *  leaves black on black. The fallbacks give a wrong shade rather than that. */
function palette() {
  const cs = getComputedStyle(document.documentElement);
  const v = (name, fallback) => cs.getPropertyValue(name).trim() || fallback;
  return {
    text: v("--text", "#17171a"),
    muted: v("--muted", "#6b6a66"),
    faint: v("--faint", "#93918b"),
    border: v("--border", "#e4e2dd"),
    strong: v("--border-strong", "#d2cfc8"),
    surface: v("--surface", "#ffffff"),
    surface2: v("--surface-2", "#fafaf9"),
    accent: v("--accent", "#1f5bc8"),
    good: v("--good", "#1c7a4d"),
    warn: v("--warn", "#9a5b00"),
    warnSoft: v("--warn-soft", "#fbf0dd"),
    bad: v("--bad", "#a92a20"),
    // For clients: accent is the gateway's, good and warn the boundary's.
    series: v("--s4", "#7b3fa0"),
    font:
      (document.body && getComputedStyle(document.body).fontFamily) ||
      "sans-serif",
  };
}

function render(g, scene, c, packets, hover) {
  const geo = scene.geometry;
  if (!geo) return;
  g.clearRect(0, 0, geo.width, scene.height);

  bands(g, scene, c);
  lines(g, scene, c, hover);
  for (const p of packets) drawPacket(g, scene, p, c);
  for (const n of scene.clients) box(g, n, c, hover, clientFace(n, c));
  box(g, scene.gateway, c, hover, gatewayFace(scene, c));
  for (const n of [...scene.inside, ...scene.outside])
    box(g, n, c, hover, modelFace(n, c));
}

/** bands draws the two bands and the boundary between them. */
function bands(g, scene, c) {
  const geo = scene.geometry;

  g.textBaseline = "alphabetic";
  g.font = "600 11px " + c.font;
  g.fillStyle = c.muted;
  g.fillText("YOUR NETWORK", 6, 16);
  if (!scene.outside.length) return;

  // Tinted, so "not your network" reads before any word does.
  g.fillStyle = c.warnSoft;
  roundRect(
    g,
    2,
    geo.lineY + 12,
    geo.width - 4,
    scene.height - geo.lineY - 14,
    12,
  );
  g.fill();

  g.save();
  g.setLineDash([7, 6]);
  g.lineWidth = 1.5;
  g.strokeStyle = c.warn;
  g.beginPath();
  g.moveTo(4, geo.lineY);
  g.lineTo(geo.width - 4, geo.lineY);
  g.stroke();
  g.restore();

  g.fillStyle = c.warn;
  g.fillText("LEAVES YOUR NETWORK", 6, geo.lineY + 26);
}

/** lines draws one link per client and per model, weighted by traffic. One
 *  per pair would be truer but unreadable. */
function lines(g, scene, c, hover) {
  const max = Math.max(
    1,
    ...boxes(scene).map((n) => (n.kind === "gateway" ? 0 : n.cell.requests)),
  );
  for (const n of scene.clients) {
    link(g, legOne(n, scene.gateway), n, scene, c, hover, max, false);
  }
  for (const n of [...scene.inside, ...scene.outside]) {
    link(
      g,
      legTwo(scene.gateway, n),
      n,
      scene,
      c,
      hover,
      max,
      n.hosting === "external",
    );
  }
}

function link(g, path, node, scene, c, hover, max, external) {
  const lit = related(node, hover, scene);
  g.save();
  g.lineWidth = 0.8 + 2.6 * Math.sqrt(node.cell.requests / max || 0);
  g.strokeStyle = external ? c.warn : c.strong;
  g.globalAlpha = hover ? (lit ? 1 : 0.15) : external ? 0.5 : 0.4;
  g.beginPath();
  g.moveTo(path.x1, path.y1);
  g.bezierCurveTo(path.cx1, path.cy1, path.cx2, path.cy2, path.x2, path.y2);
  g.stroke();
  g.restore();
}

/** related decides what a hovered box lights up: itself, the gateway, and
 *  the boxes it exchanged traffic with. */
function related(node, hover, scene) {
  if (!hover) return true;
  if (hover === node || hover.kind === "gateway") return true;
  if (hover.kind === "client" && node.kind === "model") {
    return scene.flows.some(
      (f) =>
        scene.byClient.get(f.client) === hover &&
        scene.byModel.get(f.alias) === node,
    );
  }
  if (hover.kind === "model" && node.kind === "client") {
    return scene.flows.some(
      (f) =>
        scene.byModel.get(f.alias) === hover &&
        scene.byClient.get(f.client) === node,
    );
  }
  return false;
}

function box(g, n, c, hover, face) {
  // A model of unknown hosting sits above the line but is marked like an
  // outside one: this screen must never wrongly promise that traffic stays.
  const unsure = n.kind === "model" && n.hosting !== "internal";
  const dim = hover && hover !== n ? 0.55 : 1;
  g.save();
  g.globalAlpha = dim;

  if (n.pulse > 0) {
    g.save();
    g.globalAlpha = 0.35 * n.pulse * dim;
    g.fillStyle = face.tone;
    roundRect(g, n.x - 5, n.y - 5, n.w + 10, n.h + 10, 12);
    g.fill();
    g.restore();
  }

  g.fillStyle = n.kind === "gateway" ? c.surface2 : c.surface;
  g.strokeStyle = hover === n ? face.tone : unsure ? c.warn : c.border;
  g.lineWidth = hover === n || n.kind === "gateway" ? 1.6 : 1;
  roundRect(g, n.x, n.y, n.w, n.h, 10);
  g.fill();
  g.stroke();

  g.fillStyle = face.tone;
  g.globalAlpha = dim * 0.9;
  roundRect(g, n.x, n.y + 8, 3, n.h - 16, 2);
  g.fill();
  g.globalAlpha = dim;

  const left = n.x + 12;
  const room = n.w - 22;
  const r = rows(n);
  g.textBaseline = "alphabetic";
  g.font = "600 " + TEXT.title + "px " + c.font;
  g.fillStyle = c.text;
  g.fillText(fit(g, face.title, room), left, r.title);

  // A long number pushes the unit off rather than through it.
  g.font = "600 " + r.size + "px " + c.font;
  g.fillStyle = face.value === "-" ? c.faint : c.text;
  g.fillText(face.value, left, r.value);
  const used = g.measureText(face.value).width;
  g.font = TEXT.note + "px " + c.font;
  g.fillStyle = c.muted;
  g.fillText(fit(g, face.unit, room - used - 5), left + used + 5, r.value);

  g.fillStyle = c.faint;
  g.fillText(fit(g, face.note, room), left, r.note);
  g.restore();
}

const TEXT = { title: 13, value: 17, bigValue: 22, note: 11 };

/** rows is where a box's three lines of text sit, kept together so they
 *  cannot overlap on the short box. Measured up from the bottom edge, so a
 *  change in height keeps them in place. */
function rows(n) {
  const big = n.kind === "gateway";
  return {
    title: n.y + (big ? 22 : 21),
    value: n.y + n.h - (big ? 36 : 28),
    size: big ? TEXT.bigValue : TEXT.value,
    note: n.y + n.h - (big ? 14 : 10),
  };
}

/** Each kind of box shows a different number on purpose: a client goes wrong
 *  by calling too much, a model by going slow, the gateway by refusing. */
function clientFace(n, c) {
  return {
    title: n.label,
    value: compact(n.cell.requests),
    unit: "requests",
    note:
      money(n.cell.cost_micros) +
      " · " +
      compact(n.cell.input_tokens + n.cell.output_tokens) +
      " tokens",
    tone: c.series,
  };
}

function gatewayFace(scene, c) {
  return {
    title: scene.gateway.label,
    value: String(scene.beats.reduce((sum, b) => sum + b.n, 0)),
    unit: "req/min now",
    note:
      (scene.gateway.sub ? scene.gateway.sub + " · " : "") +
      compact(scene.total.requests) +
      " in period · " +
      compact(scene.total.refused) +
      " refused",
    tone: c.accent,
  };
}

/** hostingLabel says where a model runs without naming its host, which
 *  members are not sent. */
function hostingLabel(hosting) {
  if (hosting === "internal") return "your own network";
  if (hosting === "external") return "a hosted provider";
  return "";
}

function modelFace(n, c) {
  // Where it runs matters most here, so it replaces the cost.
  const where = n.endpoint || hostingLabel(n.hosting) || "not known";
  return {
    title: n.label,
    value: ms(n.cell.ttft_median_ms),
    unit: n.cell.ttft_median_ms ? "to first token" : "nothing in this period",
    note: where + " · " + compact(n.cell.requests) + " req",
    tone:
      n.hosting === "external"
        ? c.warn
        : n.hosting === "internal"
          ? c.good
          : c.faint,
  };
}

/** drawPacket draws one request in flight, coloured by its outcome: each row
 *  is already a finished request. */
function drawPacket(g, scene, p, c) {
  if (p.delay > 0) return;
  const colour =
    p.outcome === "refused"
      ? c.warn
      : p.outcome === "failed"
        ? c.bad
        : c.accent;
  if (p.phase === "burst") {
    const k = Math.max(0, 1 + p.wait / 260);
    const gw = scene.gateway;
    g.save();
    g.globalAlpha = k * 0.8;
    g.strokeStyle = colour;
    g.lineWidth = 2;
    g.beginPath();
    g.arc(gw.x + gw.w / 2, gw.y + gw.h / 2, 14 + (1 - k) * 26, 0, Math.PI * 2);
    g.stroke();
    g.restore();
    return;
  }
  const path =
    p.phase === "out1" || (p.phase === "back" && p.leg === 0)
      ? legOne(p.client, scene.gateway)
      : legTwo(scene.gateway, p.model);
  const t = Math.max(0, Math.min(1, p.phase === "hold" ? 1 : p.t));
  const back = p.phase === "back";

  // A short tail shows direction in a still frame.
  g.save();
  g.fillStyle = colour;
  for (let i = 3; i >= 1; i--) {
    const q = at(
      path,
      Math.max(0, Math.min(1, t + (back ? i * 0.035 : -i * 0.035))),
    );
    g.globalAlpha = 0.1 * (4 - i);
    g.beginPath();
    g.arc(q.x, q.y, back ? 1.6 : 2.1, 0, Math.PI * 2);
    g.fill();
  }
  const head = at(path, t);
  g.globalAlpha = 1;
  g.beginPath();
  g.arc(head.x, head.y, back ? 2.3 : 3.4, 0, Math.PI * 2);
  g.fill();
  g.restore();
}

// The context's own roundRect is recent, and the panel has no polyfills.
function roundRect(g, x, y, w, h, r) {
  const rad = Math.max(0, Math.min(r, w / 2, h / 2));
  g.beginPath();
  g.moveTo(x + rad, y);
  g.arcTo(x + w, y, x + w, y + h, rad);
  g.arcTo(x + w, y + h, x, y + h, rad);
  g.arcTo(x, y + h, x, y, rad);
  g.arcTo(x, y, x + w, y, rad);
  g.closePath();
}

/** fit shortens text with an ellipsis until it fits the width. */
function fit(g, text, max) {
  const s = String(text == null ? "" : text);
  if (max <= 0) return "";
  if (g.measureText(s).width <= max) return s;
  let lo = 0,
    hi = s.length;
  while (lo < hi) {
    const mid = Math.ceil((lo + hi) / 2);
    if (g.measureText(s.slice(0, mid) + "…").width <= max) lo = mid;
    else hi = mid - 1;
  }
  return s.slice(0, lo) + "…";
}

function showTip(tip, canvas, n) {
  const rows = [];
  if (n.kind === "model") {
    rows.push([
      "Runs at",
      n.endpoint || hostingLabel(n.hosting) || "no backend",
    ]);
    rows.push([
      "Prompts",
      n.hosting === "external"
        ? "leave your network"
        : n.hosting === "internal"
          ? "stay in your network"
          : "unknown",
    ]);
    if (n.kindName) rows.push(["Surface", n.kindName]);
    if (!n.folded && !n.enabled) rows.push(["Status", "disabled"]);
  }
  if (n.folded)
    rows.push(["Folded together", n.rest.map((r) => r.label).join(", ")]);
  rows.push(["Requests", num(n.cell.requests)]);
  rows.push(["Tokens", compact(n.cell.input_tokens + n.cell.output_tokens)]);
  rows.push(["Cost", money(n.cell.cost_micros)]);
  if (n.cell.ttft_median_ms)
    rows.push(["Median first token", ms(n.cell.ttft_median_ms)]);
  if (n.cell.refused) rows.push(["Refused", num(n.cell.refused)]);
  if (n.cell.failed) rows.push(["Failed", num(n.cell.failed)]);
  if (target(n)) rows.push(["", "Click to open"]);

  tip.replaceChildren(
    h("div", { class: "map-tip-title" }, n.label),
    h(
      "dl",
      {},
      rows.map(([k, v]) => h("div", {}, h("dt", {}, k), h("dd", {}, v))),
    ),
  );
  tip.hidden = false;

  // Flip left rather than hang off the card's right edge.
  const holder = canvas.parentElement;
  const right = n.x + n.w + 12;
  const flip = right + 236 > holder.clientWidth;
  tip.style.left = (flip ? Math.max(8, n.x - 248) : right) + "px";
  tip.style.top = Math.max(8, Math.min(n.y, canvas.clientHeight - 200)) + "px";
}

/** watch turns the live request log into packets. It is the stream the
 *  Requests screen reads, so a packet here is a row there. */
function watch(ctx, { since, scene, painter, live }) {
  const listen = { org_id: ctx.orgID, since, after: 0 };
  followRequests(ctx, live, () => api.requestStream(listen), {
    onRequests: (payload) => {
      if (payload.next_after) listen.after = payload.next_after;
      arrived(payload.data || []);
    },
  });

  function arrived(rows) {
    // Every row counts towards the rate, drawn or not.
    if (rows.length) scene.beats.push({ at: Date.now(), n: rows.length });
    // Oldest first and staggered, so a batch looks like many requests, not a
    // wall.
    const drawn = rows.slice(0, MAX_SPAWN).reverse();
    drawn.forEach((row, i) => {
      const client = clientNode(scene, row.client || "");
      const model = scene.byModel.get(row.alias || "");
      count(scene, client, model, row);
      // Only refused and failed look different; an interrupted request is
      // drawn as served.
      painter.send(client, model, outcomeOf(row), i * 22);
    });
    // Rows past the cap still count towards the numbers.
    for (const row of rows.slice(MAX_SPAWN)) {
      count(
        scene,
        clientNode(scene, row.client || ""),
        scene.byModel.get(row.alias || ""),
        row,
      );
    }
    painter.touch();
  }
}

// A refusal is any 4xx and a failure any 5xx, as on the dashboard and in the
// request log.
function refused(row) {
  return row.status >= 400 && row.status < 500;
}

/** count adds one live row to the boxes, so totals move with the packets
 *  instead of jumping at each refresh. Medians come with the refresh. */
function count(scene, client, model, row) {
  const cells = [client.cell, scene.total];
  if (model) cells.push(model.cell);
  for (const cell of cells) {
    cell.requests += 1;
    cell.input_tokens += row.input_tokens || 0;
    cell.output_tokens += row.output_tokens || 0;
    cell.cost_micros += row.cost_micros || 0;
    if (refused(row)) cell.refused += 1;
    if (row.status >= 500) cell.failed += 1;
  }
}

/** clientNode finds the box for a live row, or makes one: a new client's
 *  first request is what people watch this screen for. Models are not made
 *  up this way: an unknown alias is a typo or a deleted model, and a new
 *  model arrives with the next refresh. */
function clientNode(scene, key) {
  const found = scene.byClient.get(key);
  if (found) return found;
  const node = {
    kind: "client",
    key,
    label: key || "Unidentified client",
    cell: cellOf({}),
    pulse: 0,
    x: 0,
    y: 0,
    w: 0,
    h: 0,
  };
  scene.clients.push(node);
  scene.byClient.set(key, node);
  return node;
}

/** refresh reloads the report every half minute, for what the stream cannot
 *  update: medians, new models, and traffic leaving the window. */
function refresh(ctx, { since, scene, painter }) {
  const timer = setInterval(async () => {
    if (document.hidden) return;
    try {
      const data = await api.trafficMap(ctx.orgID, since);
      const fresh = sceneFrom(data);
      scene.clients = fresh.clients;
      scene.inside = fresh.inside;
      scene.outside = fresh.outside;
      scene.models = fresh.models;
      scene.flows = fresh.flows;
      scene.total = fresh.gateway.cell;
      scene.gateway.cell = fresh.gateway.cell;
      scene.gateway.sub = fresh.gateway.sub;
      index(scene);
      painter.reset();
    } catch {
      // Stale numbers are fine; the stream shows if the control plane is gone.
    }
  }, 30000);
  ctx.onTeardown(() => clearInterval(timer));
}

/** modelReport lists every model, not only those that fit on the canvas. */
function modelReport(ctx, scene) {
  const rows = scene.models;
  if (!rows.length) {
    return h(
      "div",
      {},
      sectionHead("Every model, and where it runs"),
      h(
        "div",
        { class: "card" },
        empty(
          `${orgNameOf(ctx)} has no models`,
          "Add one under Models to see it here.",
        ),
      ),
    );
  }
  return h(
    "div",
    {},
    sectionHead("Every model, and where it runs"),
    table(
      [
        {
          label: "Model",
          sortKey: (m) => m.label,
          cell: (m) =>
            h(
              "a",
              {
                class: "row-link",
                href: "/models/" + encodeURIComponent(m.key),
                onClick: go(ctx, "/models/" + encodeURIComponent(m.key)),
              },
              m.label,
            ),
        },
        {
          label: "Prompts",
          sortKey: (m) => m.hosting,
          cell: (m) =>
            m.hosting === "external"
              ? h("span", { class: "pill pill-warn" }, "Leave your network")
              : m.hosting === "internal"
                ? h("span", { class: "pill pill-good" }, "Stay in your network")
                : h("span", { class: "pill" }, "Unknown"),
        },
        {
          label: "Served by",
          cell: (m) =>
            m.endpoint
              ? h("span", { class: "mono muted" }, m.endpoint)
              : dash(),
        },
        {
          label: "Requests",
          num: true,
          shrink: true,
          sortDir: "desc",
          sortKey: (m) => m.cell.requests,
          cell: (m) => num(m.cell.requests),
        },
        {
          label: "First token",
          num: true,
          shrink: true,
          sortDir: "desc",
          sortKey: (m) => m.cell.ttft_median_ms || null,
          cell: (m) =>
            m.cell.ttft_median_ms ? ms(m.cell.ttft_median_ms) : dash(),
        },
        {
          label: "Cost (CHF)",
          num: true,
          shrink: true,
          sortDir: "desc",
          sortKey: (m) => m.cell.cost_micros,
          cell: (m) => amount(m.cell.cost_micros),
        },
      ],
      rows,
      { sortBy: "Requests", sortDir: "desc" },
    ),
  );
}

/** clientReport lists every client that called. */
function clientReport(scene) {
  const rows = [];
  for (const c of scene.clients) {
    if (c.rest) rows.push(...c.rest);
    else rows.push(c);
  }
  if (!rows.length) {
    return h(
      "div",
      {},
      sectionHead("Every client that called"),
      h(
        "div",
        { class: "card" },
        empty(
          "Nothing has called in this period",
          "Connect an editor under My clients. It shows here after its " +
            "first request.",
        ),
      ),
    );
  }
  return h(
    "div",
    {},
    sectionHead("Every client that called"),
    table(
      [
        { label: "Client", sortKey: (c) => c.label, cell: (c) => c.label },
        {
          label: "Requests",
          num: true,
          shrink: true,
          sortDir: "desc",
          sortKey: (c) => c.cell.requests,
          cell: (c) => num(c.cell.requests),
        },
        {
          label: "Tokens",
          num: true,
          shrink: true,
          sortDir: "desc",
          sortKey: (c) => c.cell.input_tokens + c.cell.output_tokens,
          cell: (c) => compact(c.cell.input_tokens + c.cell.output_tokens),
        },
        {
          label: "Refused",
          num: true,
          shrink: true,
          sortDir: "desc",
          sortKey: (c) => c.cell.refused,
          cell: (c) =>
            c.cell.refused
              ? h("span", { class: "map-warn" }, num(c.cell.refused))
              : h("span", { class: "faint" }, "0"),
        },
        {
          label: "Cost (CHF)",
          num: true,
          shrink: true,
          sortDir: "desc",
          sortKey: (c) => c.cell.cost_micros,
          cell: (c) => amount(c.cell.cost_micros),
        },
      ],
      rows,
      { sortBy: "Requests", sortDir: "desc" },
    ),
  );
}
