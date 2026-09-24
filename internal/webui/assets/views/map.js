// The live map: the deployment drawn, with its traffic moving on it.
//
// Every other screen here is a table, because every other question is about
// time. This one answers the two questions about shape: what is connected to
// this gateway, and how much of what goes through it leaves the building.
//
// The second is why it is drawn at all. A model served from the cluster and
// one served by a hosted provider sit one row apart on the Models screen and
// are worlds apart in what they mean. On a table that difference is a URL
// somebody has to recognise; here it is a line across the middle of the
// picture, and every request that crosses it is drawn crossing it.
//
// A canvas rather than the inline SVG the dashboard's chart uses: a busy
// gateway puts hundreds of packets on this screen a minute, and hundreds of
// appearing and disappearing DOM nodes is a page that stutters. The canvas
// draws only when something has moved.
//
// The numbers on the boxes are the window's, from the same log every other
// screen reads. What moves between them is the request log as it is written,
// so a packet crossing the boundary is a request that happened seconds ago and
// not an animation of an average.

import { api } from "../api.js";
import {
  h,
  compact,
  money,
  ms,
  num,
  table,
  empty,
  RANGES,
  currentRange,
  rangePicker,
} from "../ui.js";

// How the boxes are sized. Fixed rather than measured: a map whose boxes are as
// wide as their longest name is a map that jumps every time a model is added.
const BOX = {
  // The heights are the three lines a box carries plus the room they need -
  // see rows() below, which is where they come from and where they have to
  // stay in step with.
  clientW: 176,
  clientH: 74,
  modelW: 190,
  modelH: 72,
  gatewayW: 200,
  gatewayH: 92,
  gapY: 12,
  pad: 18,
  // The room between one column and the next. It is not spare space: it is
  // where the lines are, and a curve with no room to bend in is a corner.
  gapX: 40,
};

// The narrowest the drawing is allowed to be - three columns and the two gaps
// the lines need. Below this the card scrolls sideways rather than the boxes
// being squeezed into each other, because a map whose gateway sits on top of
// its clients has stopped saying anything.
const MIN_WIDTH =
  BOX.pad * 2 + BOX.clientW + BOX.gatewayW + BOX.modelW + BOX.gapX * 2;

// How many boxes a column draws before the rest are folded into one. Past this
// the picture stops being readable, and the ones folded away are the quiet ones:
// the report behind the canvas still lists every one of them.
const MAX_CLIENTS = 7;
const MAX_MODELS = 6;

// A packet's speed in pixels per millisecond, and the pause at the gateway
// where it is authorised, budgeted and filtered. The pause is what makes the
// gateway look like something the request goes *through* rather than a corner
// the line happens to turn at.
const SPEED = 0.55;
const HOLD_MS = 110;
// The most packets one batch of the stream puts on the screen. A tick can carry
// two hundred rows; two hundred packets leaving at once reads as a wall rather
// than as traffic, so the rest are counted and not drawn.
const MAX_SPAWN = 48;
const MAX_PACKETS = 400;

export async function mapView(ctx) {
  const since = currentRange();
  const range = RANGES.find((r) => r.since === since) || RANGES[0];
  ctx.setSubtitle(range.long);

  const data = await api.trafficMap(ctx.orgID, since);
  const currency = data.currency || ctx.currency;

  const scene = sceneFrom(data);
  const canvas = h("canvas", {
    class: "map-canvas",
    // A canvas is a picture as far as anything but a mouse is concerned, so it
    // says what it is - and everything on it is repeated as text underneath,
    // where it can be read, searched and sorted.
    role: "img",
    "aria-label": describe(scene),
  });
  const tip = h("div", { class: "map-tip", hidden: true });
  const holder = h("div", { class: "card map-holder" }, canvas, tip);

  const live = liveControl();
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
        "Boxes show who called in this window and where requests can go. " +
          "Packets are live requests.",
      ),
    ),
    h("div", { class: "spacer" }),
    live.el,
    rangePicker(ctx, since),
  );

  const painter = paint(ctx, canvas, tip, scene, currency);
  ctx.onTeardown(painter.stop);

  watch(ctx, { since, scene, painter, live });
  refresh(ctx, { since, scene, painter });

  return h(
    "div",
    { class: "map-page" },
    head,
    holder,
    legend(),
    h(
      "div",
      { class: "hint" },
      "A request refused by a guardrail stops at the gateway and never " +
        "reaches a model. Unrecognised clients show as Unidentified; they " +
        "can name themselves with an X-Keera-Client header.",
    ),
    modelReport(ctx, scene, currency),
    clientReport(scene, currency),
  );
}

/* ------------------------------------------------------------------ scene */

/** sceneFrom turns the report into the boxes and lines the canvas draws.
 *
 *  The catalogue decides the models, not the traffic: a hosted model nobody has
 *  called this week is still a way out of the building, and leaving it off
 *  because it was quiet would be the one omission this screen cannot afford.
 *  The clients are the other way round - nothing registers a client, it just
 *  arrives with a key - so they are whatever has called. */
function sceneFrom(data) {
  const clients = fold(
    (data.clients || []).map((c) => ({
      kind: "client",
      key: c.key,
      label: c.label || "Unidentified",
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
    retired: !!m.retired,
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
    // Which client used which model, for the highlight: hovering a client
    // lights the models it actually reached rather than every line on the map.
    flows: (data.flows || []).map((f) => ({
      client: f.client,
      alias: f.alias,
    })),
    from: data.from,
    to: data.to,
    // When each live packet arrived, for the rate the gateway shows.
    beats: [],
    height: 0,
  };
  index(scene);
  return scene;
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

/** fold keeps the busiest and adds up the rest into one box, so that a
 *  deployment with forty models is still a picture. */
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

/** index builds the lookups the live stream needs: a row names a client and an
 *  alias, and both have to become a box in constant time. */
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

/** describe is the whole picture in one sentence, for whoever is not looking at
 *  it. */
function describe(scene) {
  const folded = scene.outside.find((m) => m.folded);
  const out =
    scene.outside.filter((m) => !m.folded).length +
    (folded ? folded.rest.length : 0);
  return (
    scene.clients.length +
    " client types calling this gateway, " +
    scene.inside.length +
    " models inside your infrastructure and " +
    out +
    " at external providers. " +
    tokenShare(scene) +
    "% of tokens in this window left your infrastructure."
  );
}

/** tokenShare is the number the whole screen exists for: how much of what went
 *  through this gateway went outside it. */
function tokenShare(scene) {
  let outside = 0;
  for (const m of scene.outside)
    outside += m.cell.input_tokens + m.cell.output_tokens;
  const total = scene.total.input_tokens + scene.total.output_tokens;
  if (!total) return 0;
  return Math.round((outside / total) * 100);
}

function outsideCost(scene) {
  let micros = 0;
  for (const m of scene.outside) micros += m.cell.cost_micros;
  return micros;
}

/* ----------------------------------------------------------------- layout */

/** layout places every box for a canvas this wide and returns how tall the
 *  drawing has to be.
 *
 *  Two bands, one above the other: everything inside the customer's network,
 *  and the providers outside it. The boundary runs between them across the
 *  whole width, so a line from the gateway to a hosted model has to cross it -
 *  which is the one thing this drawing is for. */
function layout(scene, width) {
  const P = BOX.pad;
  const stack = (list, hh) =>
    list.length ? list.length * (hh + BOX.gapY) - BOX.gapY : 0;

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
    // Centred, but never at the cost of the room the lines need: on a narrow
    // card the middle of the canvas is inside one of the columns.
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

  scene.height = g.outsideTop + outsideH + P + 6;
  return scene.height;
}

/** place stacks one column, centred in the band it belongs to. */
function place(list, x, w, hh, top, bandH) {
  const total = list.length ? list.length * (hh + BOX.gapY) - BOX.gapY : 0;
  let y = Math.round(top + (bandH - total) / 2);
  for (const n of list) {
    n.x = x;
    n.y = y;
    n.w = w;
    n.h = hh;
    y += hh + BOX.gapY;
  }
}

/** legOne and legTwo are the two halves of a request's path. They are computed
 *  from the boxes each time rather than stored, so a relayout cannot leave a
 *  packet travelling to where a box used to be. */
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

// Straight-line distance is close enough for a curve this shallow, and it is
// only ever used to decide how long a packet takes to cross it.
function span(c) {
  return Math.hypot(c.x2 - c.x1, c.y2 - c.y1);
}

/* ------------------------------------------------------------------ paint */

/** paint owns the canvas: its size, its palette, its animation and everything
 *  the pointer does to it. It hands back the three things the rest of the
 *  screen needs - a way to put a request on the map, a way to say the numbers
 *  moved, and a way to stop. */
function paint(ctx, canvas, tip, scene, currency) {
  const g2 = canvas.getContext("2d");
  const packets = [];
  let colors = palette();
  let dirty = true;
  let painted = 0; // the device pixels one of the drawing's own pixels covers
  let hover = null;
  let raf = 0;
  let last = 0;
  const still = window.matchMedia("(prefers-reduced-motion: reduce)");

  function resize() {
    const holder = canvas.parentElement;
    const width = Math.max(MIN_WIDTH, Math.floor(holder.clientWidth) - 2);
    const height = layout(scene, width);
    // How many device pixels one of this drawing's pixels is worth. The screen
    // density is half of it; the other half is the panel, which scales itself
    // with `html { zoom }` and stretches a canvas without telling it - a canvas
    // painted at 1:1 and then enlarged by a tenth is a canvas whose labels are
    // soft. The card's measured width against its reported width is how much
    // it is being enlarged by, and where nothing is enlarging it that ratio is
    // 1 and this changes nothing.
    const stretch = holder.clientWidth
      ? holder.getBoundingClientRect().width / holder.clientWidth
      : 1;
    const scale = Math.min(
      4,
      Math.max(1, (window.devicePixelRatio || 1) * stretch),
    );
    // The card is as tall as the canvas, so resizing the canvas resizes what is
    // being observed. Doing nothing when nothing changed is what keeps that
    // from being a loop the browser has to break for us.
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
    // The first of these runs once the view is on the page, which is the first
    // moment the stylesheet can be read at all.
    colors = palette();
    dirty = true;
  }

  const observer = new ResizeObserver(resize);
  observer.observe(canvas.parentElement);

  // The panel's colours are CSS custom properties and the canvas cannot use
  // them, so they are read out - once, and again whenever the theme moves
  // underneath: by the switcher in the topbar, or by the operating system for
  // anyone who left the panel on "system".
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
      render(g2, scene, colors, packets, hover, currency);
    }
  }

  /** advance moves every packet and fades every pulse, and reports whether
   *  anything actually changed - which is what keeps a map nobody is sending
   *  requests through from repainting sixty times a second. */
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
    // The gateway's headline is a rate, so it changes as packets age out of the
    // last minute whether or not new ones arrive.
    const cut = Date.now() - 60000;
    while (scene.beats.length && scene.beats[0] < cut) {
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
          p.phase = p.outcome === "refused" ? "burst" : "hold";
          p.wait = HOLD_MS;
        }
        return false;
      }
      case "hold":
        p.wait -= dt;
        if (p.wait <= 0) p.phase = "out2";
        return false;
      case "burst":
        // A refusal never leaves the gateway, so neither does its packet.
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
        // The answer, coming home. It is the same path drawn thinner, because
        // what a reader is being shown is a round trip and not a fire and
        // forget - and because a model that is answering and a model that has
        // gone quiet look identical without it.
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

  /** send puts one recorded request on the map. */
  function send(client, model, outcome, delay) {
    scene.beats.push(Date.now());
    dirty = true;
    if (still.matches) {
      // Whoever asked for less movement gets the boxes lighting up and the
      // numbers changing, and no packets at all.
      client.pulse = 1;
      scene.gateway.pulse = 1;
      if (outcome !== "refused") model.pulse = 1;
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

  /* The pointer. A box is worth opening - a model has its own screen - and
     worth explaining, which is a tooltip rather than more text on the canvas:
     the numbers that fit on a box are the ones worth reading at a glance, and
     the rest are for whoever asked. */
  function locate(e) {
    const r = canvas.getBoundingClientRect();
    const x = e.clientX - r.left,
      y = e.clientY - r.top;
    for (const n of boxes(scene)) {
      if (x >= n.x && x <= n.x + n.w && y >= n.y && y <= n.y + n.h) return n;
    }
    return null;
  }

  const onMove = (e) => {
    const found = locate(e);
    // Only when the box under the pointer changes: the tooltip is built out of
    // DOM, and a mouse crossing this screen fires this a hundred times.
    if (found === hover) return;
    hover = found;
    dirty = true;
    canvas.style.cursor = found && target(found) ? "pointer" : "default";
    if (found) showTip(tip, canvas, found, currency);
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
    // What the refresh calls when the boxes it is drawing have been replaced.
    // The packets in flight refer to the old ones, so they go: they are a
    // second and a half of animation, and the requests behind them are already
    // counted in the report that has just arrived.
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

/** target is where a box leads, or nothing for the boxes that lead nowhere. A
 *  folded box leads nowhere on purpose: it is several things. */
function target(n) {
  if (n.kind === "model" && !n.folded && !n.retired)
    return "/models/" + encodeURIComponent(n.key);
  if (n.kind === "gateway") return "/requests";
  return null;
}

function boxes(scene) {
  return [...scene.clients, scene.gateway, ...scene.inside, ...scene.outside];
}

/** palette reads the panel's colours out of the stylesheet, because a canvas
 *  cannot use a custom property and this screen must not be the one place with
 *  colours of its own.
 *
 *  It reads the root and not the canvas: a view is built before it is put on
 *  the page, and a detached element resolves every custom property to the
 *  empty string - which a canvas context silently ignores, leaving its default
 *  black on black.
 *
 *  Every colour still carries a fallback, because an empty string is one the
 *  canvas ignores rather than refuses. A missing property has to fail as the
 *  wrong shade rather than as an unreadable screen. */
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
    accent: v("--accent", "#2d4ea0"),
    good: v("--good", "#1c7a4d"),
    warn: v("--warn", "#9a5b00"),
    warnSoft: v("--warn-soft", "#fbf0dd"),
    bad: v("--bad", "#a92a20"),
    // The panel's fourth series colour, which is what a client is drawn in:
    // the accent belongs to the gateway, and the good and warning colours have
    // been spoken for by the boundary.
    series: v("--s4", "#7b3fa0"),
    font:
      (document.body && getComputedStyle(document.body).fontFamily) ||
      "sans-serif",
  };
}

/* ----------------------------------------------------------------- render */

function render(g, scene, c, packets, hover, currency) {
  const geo = scene.geometry;
  if (!geo) return;
  g.clearRect(0, 0, geo.width, scene.height);

  bands(g, scene, c, currency);
  lines(g, scene, c, hover);
  for (const p of packets) drawPacket(g, scene, p, c);
  for (const n of scene.clients)
    box(g, n, c, hover, clientFace(n, currency, c));
  box(g, scene.gateway, c, hover, gatewayFace(scene, c));
  for (const n of [...scene.inside, ...scene.outside])
    box(g, n, c, hover, modelFace(n, c));
}

/** bands draws the two halves of the world and the boundary between them. */
function bands(g, scene, c, currency) {
  const geo = scene.geometry;

  // Outside is tinted, because "this part of the picture is not yours" has to
  // be readable before a single word on it is.
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

  g.textBaseline = "alphabetic";
  g.font = "600 11px " + c.font;
  g.fillStyle = c.muted;
  g.fillText("YOUR INFRASTRUCTURE", 6, 16);
  g.fillStyle = c.warn;
  g.fillText("LEAVES YOUR INFRASTRUCTURE", 6, geo.lineY + 26);

  const note = scene.outside.length
    ? tokenShare(scene) +
      "% of tokens in this window, " +
      money(outsideCost(scene), currency) +
      " spent outside"
    : "Nothing leaves: every model is served from your own network";
  g.font = "12px " + c.font;
  g.textAlign = "right";
  g.fillStyle = scene.outside.length ? c.warn : c.muted;
  g.fillText(fit(g, note, geo.width - 240), geo.width - 6, geo.lineY + 26);
  g.textAlign = "left";
}

/** lines draws one link per client and one per model, weighted by how much of
 *  the window's traffic ran along it. Per pair would be truer and unreadable:
 *  what a reader takes from a link is "this one is busy", and a fan of forty
 *  says nothing at all. */
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
  if (external) g.setLineDash([6, 5]);
  g.beginPath();
  g.moveTo(path.x1, path.y1);
  g.bezierCurveTo(path.cx1, path.cy1, path.cx2, path.cy2, path.x2, path.y2);
  g.stroke();
  g.restore();
}

/** related decides what a hovered box lights up: itself, the gateway, and - for
 *  a client - the models it actually reached in this window. */
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

/** box draws one node: its name, the one number it is read for, and the line
 *  under it that says what that number is made of. */
function box(g, n, c, hover, face) {
  // A model whose backend cannot be classified is marked like one that is
  // outside, not like one that is inside. It is drawn above the line because
  // there is no evidence it crossed it - but a screen that promises to say
  // what leaves must never quietly promise that something stays.
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

  // A stripe on the leading edge, in the colour of what the box is. It is the
  // only thing on a box that can be seen from across a room.
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

  // The unit shares the number's baseline and takes whatever width the number
  // left it, so a four-figure count pushes it off rather than through it.
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

// The type on a box: a name, the number it is read for with its unit beside it,
// and the line that says what the number is made of.
const TEXT = { title: 13, value: 17, bigValue: 22, note: 11 };

/** rows is where those three lines sit inside a box.
 *
 *  It is one function rather than three numbers written where they are used,
 *  because they are not three numbers: they are one arrangement, and the last
 *  time they were written separately the count and the line under it were far
 *  enough apart on the tall box to look right and eight pixels apart on the
 *  short one, where they overlapped. Everything is measured up from the bottom
 *  edge, so a box that changes height keeps its footing. */
function rows(n) {
  const big = n.kind === "gateway";
  return {
    title: n.y + (big ? 22 : 21),
    value: n.y + n.h - (big ? 36 : 28),
    size: big ? TEXT.bigValue : TEXT.value,
    note: n.y + n.h - (big ? 14 : 10),
  };
}

/** The one number each kind of box is read for.
 *
 *  Three different numbers, deliberately. What goes wrong with a client is
 *  that it is calling more than expected; with a model, that it has gone slow;
 *  with the gateway, that it is refusing things. A single measure across all
 *  three would answer none of them.
 *
 *  The tone says which kind of thing a box is and comes from the panel's own
 *  palette: the two themes are not each other's inverse, and a hand-picked
 *  mid-tone that reads well on white is a smudge on the dark one. A model's
 *  tone is also the boundary's - green for what stays, amber for what leaves. */
function clientFace(n, currency, c) {
  return {
    title: n.label,
    value: compact(n.cell.requests),
    unit: "requests",
    note:
      money(n.cell.cost_micros, currency) +
      " · " +
      compact(n.cell.input_tokens + n.cell.output_tokens) +
      " tokens",
    tone: c.series,
  };
}

function gatewayFace(scene, c) {
  return {
    title: scene.gateway.label,
    value: String(scene.beats.length),
    unit: "req/min now",
    note:
      (scene.gateway.sub ? scene.gateway.sub + " · " : "") +
      compact(scene.total.requests) +
      " in window · " +
      compact(scene.total.refused) +
      " refused",
    tone: c.accent,
  };
}

function modelFace(n, c) {
  // Where it runs, in the place a client box puts its spend: on this screen
  // that is the more important half of what a model is.
  const where = n.endpoint
    ? n.endpoint
    : n.hosting === "internal"
      ? "your own network"
      : "not known";
  return {
    title: n.label,
    value: n.cell.ttft_median_ms ? String(n.cell.ttft_median_ms) : "-",
    unit: n.cell.ttft_median_ms
      ? "ms to first token"
      : "nothing in this window",
    note: where + " · " + compact(n.cell.requests) + " req",
    tone:
      n.hosting === "external"
        ? c.warn
        : n.hosting === "internal"
          ? c.good
          : c.faint,
  };
}

/** drawPacket is one request, mid-flight. Its colour is what happened to it,
 *  which is known before it is drawn because the row it came from is already
 *  the finished request - the map is a replay of the last few seconds, not a
 *  guess at the next few. */
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

  // A short tail, so that direction is visible in a still frame.
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

// roundRect is written out rather than taken from the context, which has had it
// only recently: the panel has no build step and no polyfills, and a rounded
// corner is not worth a blank screen on a browser two versions behind.
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

/** fit shortens a string until it is inside the width it was given. */
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

/* ---------------------------------------------------------------- tooltip */

function showTip(tip, canvas, n, currency) {
  const rows = [];
  if (n.kind === "model") {
    rows.push([
      "Runs at",
      n.endpoint ||
        (n.retired ? "no longer in the catalogue" : "not shown to your role"),
    ]);
    rows.push([
      "Prompts",
      n.hosting === "external"
        ? "leave your infrastructure"
        : n.hosting === "internal"
          ? "stay in your infrastructure"
          : "unknown",
    ]);
    if (n.kindName) rows.push(["Surface", n.kindName]);
    if (!n.folded && !n.retired && !n.enabled)
      rows.push(["Status", "disabled"]);
  }
  if (n.folded)
    rows.push(["Folded together", n.rest.map((r) => r.label).join(", ")]);
  rows.push(["Requests", num(n.cell.requests)]);
  rows.push(["Tokens", num(n.cell.input_tokens + n.cell.output_tokens)]);
  rows.push(["Spend", money(n.cell.cost_micros, currency)]);
  if (n.cell.ttft_median_ms)
    rows.push(["Median first token", ms(n.cell.ttft_median_ms)]);
  if (n.cell.refused)
    rows.push(["Refused by a guardrail", num(n.cell.refused)]);
  if (n.cell.failed) rows.push(["Failed upstream", num(n.cell.failed)]);
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

  // Kept inside the card: a tooltip hanging off the right edge is a tooltip
  // half of which cannot be read.
  const holder = canvas.parentElement;
  const right = n.x + n.w + 12;
  const flip = right + 236 > holder.clientWidth;
  tip.style.left = (flip ? Math.max(8, n.x - 248) : right) + "px";
  tip.style.top = Math.max(8, Math.min(n.y, canvas.clientHeight - 200)) + "px";
}

/* ------------------------------------------------------------------- live */

/** watch is the map's other half: the request log as it is written, turned
 *  into packets.
 *
 *  It is the same stream the Requests screen reads, asked the same way - so a
 *  packet crossing this screen and a row appearing on that one are the same
 *  request, and neither of them is a poll. */
function watch(ctx, { since, scene, painter, live }) {
  const listen = { org_id: ctx.orgID, since, after: 0 };
  let source = null;
  const close = () => {
    if (source) source.close();
    source = null;
  };
  ctx.onTeardown(close);

  const open = () => {
    close();
    live.state("connecting");
    source = api.requestStream(listen);
    source.addEventListener("open", () => live.state("live"));
    source.addEventListener("requests", (e) => {
      live.state("live");
      const payload = JSON.parse(e.data);
      if (payload.next_after) listen.after = payload.next_after;
      arrived(payload.data || []);
    });
    source.addEventListener("error", () => {
      live.state(
        source && source.readyState === EventSource.CLOSED
          ? "stopped"
          : "connecting",
      );
    });
  };

  function arrived(rows) {
    // Oldest first, so a batch leaves in the order it happened, and staggered,
    // so that a tick carrying forty rows looks like forty requests rather than
    // one wall of them.
    const drawn = rows.slice(0, MAX_SPAWN).reverse();
    drawn.forEach((row, i) => {
      const client = clientNode(scene, row.client || "");
      const model = modelNode(scene, row.alias);
      count(scene, client, model, row);
      painter.send(client, model, outcomeOf(row), i * 22);
    });
    // Anything past the cap still counts: the numbers on the boxes are the
    // window's traffic, not the traffic that happened to be drawn.
    for (const row of rows.slice(MAX_SPAWN)) {
      count(
        scene,
        clientNode(scene, row.client || ""),
        modelNode(scene, row.alias),
        row,
      );
    }
    painter.touch();
  }

  live.onChange((on) => (on ? open() : (close(), live.state("paused"))));
  if (live.on) open();
  else live.state("paused");
}

// The four statuses a guardrail refuses with: over budget, not allowed, no such
// model for this key, too many requests. They are the same four the dashboard
// counts as refusals, so the map and the dashboard cannot disagree about what
// one is.
const REFUSALS = [402, 403, 404, 429];

function outcomeOf(row) {
  if (REFUSALS.includes(row.status)) return "refused";
  if (row.status >= 500 || (row.status >= 400 && row.error)) return "failed";
  return "ok";
}

/** count folds one live row into the numbers already on the boxes, so the
 *  totals move with the packets rather than jumping every half minute when the
 *  report behind them is asked again. The medians are left alone: a median is
 *  not something a row can be added to, and the refresh below brings the real
 *  one. */
function count(scene, client, model, row) {
  for (const cell of [client.cell, model.cell, scene.total]) {
    cell.requests += 1;
    cell.input_tokens += row.input_tokens || 0;
    cell.output_tokens += row.output_tokens || 0;
    cell.cost_micros += row.cost_micros || 0;
    if (REFUSALS.includes(row.status)) cell.refused += 1;
    if (row.status >= 500) cell.failed += 1;
  }
}

/** clientNode and modelNode find the box a live row belongs to, and make one
 *  where there is none: the first request from an editor nobody had pointed
 *  here before is exactly what somebody watching this screen is watching for,
 *  and it must not have to wait for a reload to appear. The next refresh
 *  replaces the improvised box with the reported one, name and all. */
function clientNode(scene, key) {
  const found = scene.byClient.get(key);
  if (found) return found;
  const node = {
    kind: "client",
    key,
    label: key || "Unidentified",
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

function modelNode(scene, alias) {
  const found = scene.byModel.get(alias || "");
  if (found) return found;
  const node = {
    kind: "model",
    key: alias || "",
    label: alias || "unknown",
    cell: cellOf({}),
    hosting: "unknown",
    endpoint: "",
    retired: true,
    pulse: 0,
    x: 0,
    y: 0,
    w: 0,
    h: 0,
  };
  scene.inside.push(node);
  scene.byModel.set(node.key, node);
  return node;
}

/** refresh asks the report again on a slow timer.
 *
 *  The stream keeps the counts moving, but three of the numbers on this screen
 *  cannot be kept that way: a median is not summed, a model added to the
 *  catalogue is not a request, and a window that slides forward drops traffic
 *  off its back edge. Half a minute is slow enough to cost a deployment nothing
 *  and quick enough that a screen left open on a wall is not lying. */
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
      // A refresh that failed is a stale number, not a broken screen. The
      // stream is what says whether the control plane is still there.
    }
  }, 30000);
  ctx.onTeardown(() => clearInterval(timer));
}

/* --------------------------------------------------------------- controls */

/** liveControl is the switch, and the one thing on the screen that says whether
 *  what is in front of the reader is current. It is the request log's switch,
 *  down to the dot and the four things the dot can mean: a map that has quietly
 *  stopped following the log looks exactly like a deployment that has gone
 *  quiet, and those are not the same afternoon. */
function liveControl() {
  const KEY = "keera.map.live";
  const on = sessionStorage.getItem(KEY) !== "off";
  const blip = h("span", { class: "blip" });
  const text = h("span", {}, "Live");
  const button = h(
    "button",
    {
      class: "btn btn-sm",
      type: "button",
      "aria-pressed": String(on),
      "aria-label": "Follow the request log",
    },
    blip,
    text,
  );
  let listener = null;

  const control = {
    on,
    el: button,
    state(what) {
      blip.dataset.state = what;
      button.title = {
        live: "Live: requests cross the map as they happen. Click to pause.",
        connecting: "Reconnecting. Click to stop.",
        paused: "Paused. The numbers cover the selected window.",
        stopped:
          "The live stream stopped. Click to try again, or reload the page.",
      }[what];
      text.textContent =
        what === "paused"
          ? "Paused"
          : what === "stopped"
            ? "Reconnect"
            : "Live";
    },
    onChange(fn) {
      listener = fn;
    },
  };
  button.addEventListener("click", () => {
    control.on = !control.on;
    sessionStorage.setItem(KEY, control.on ? "on" : "off");
    button.setAttribute("aria-pressed", String(control.on));
    if (listener) listener(control.on);
  });
  return control;
}

function legend() {
  const mark = (cls, label) =>
    h("span", { class: "map-key" }, h("i", { class: cls }), label);
  return h(
    "div",
    { class: "map-legend" },
    mark("map-key-ok", "Served"),
    mark("map-key-refused", "Refused at the gateway"),
    mark("map-key-failed", "Failed upstream"),
    mark("map-key-inside", "Served from your network"),
    mark("map-key-outside", "Served by an external provider"),
  );
}

/* ---------------------------------------------------------------- reports */

/** modelReport is the picture as a table: every model, not only the ones that
 *  fitted on the canvas, and readable by everything a canvas is not. */
function modelReport(ctx, scene, currency) {
  const rows = scene.models;
  if (!rows.length) {
    return h(
      "div",
      { class: "stack" },
      h("h2", {}, "Every model, and where it runs"),
      h(
        "div",
        { class: "card" },
        empty(
          "No models in the catalogue",
          "Add one under Models to see it here.",
        ),
      ),
    );
  }
  return h(
    "div",
    { class: "stack" },
    h("h2", {}, "Every model, and where it runs"),
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
                onClick: (e) => {
                  e.preventDefault();
                  ctx.navigate("/models/" + encodeURIComponent(m.key));
                },
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
                ? h("span", { class: "pill pill-good" }, "Stay inside")
                : h("span", { class: "pill" }, "Unknown"),
        },
        {
          label: "Served by",
          cell: (m) =>
            m.endpoint
              ? h("span", { class: "mono muted" }, m.endpoint)
              : h("span", { class: "faint" }, "-"),
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
            m.cell.ttft_median_ms
              ? ms(m.cell.ttft_median_ms)
              : h("span", { class: "faint" }, "-"),
        },
        {
          label: "Spend",
          num: true,
          shrink: true,
          sortDir: "desc",
          sortKey: (m) => m.cell.cost_micros,
          cell: (m) => money(m.cell.cost_micros, currency),
        },
      ],
      rows,
      { sortBy: "Requests", sortDir: "desc" },
    ),
  );
}

/** clientReport is the other column of the picture as a table. It is separate
 *  from the one above because they are not the same list: one is what this
 *  deployment offers, the other is who took it up. */
function clientReport(scene, currency) {
  const rows = [];
  for (const c of scene.clients) {
    if (c.rest) rows.push(...c.rest);
    else rows.push(c);
  }
  if (!rows.length) {
    return h(
      "div",
      { class: "stack" },
      h("h2", {}, "Every client that called"),
      h(
        "div",
        { class: "card" },
        empty(
          "Nothing has called in this window",
          "Connect an editor from Connect a client. It shows here after its " +
            "first request.",
        ),
      ),
    );
  }
  return h(
    "div",
    { class: "stack" },
    h("h2", {}, "Every client that called"),
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
          cell: (c) => num(c.cell.input_tokens + c.cell.output_tokens),
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
          label: "Spend",
          num: true,
          shrink: true,
          sortDir: "desc",
          sortKey: (c) => c.cell.cost_micros,
          cell: (c) => money(c.cell.cost_micros, currency),
        },
      ],
      rows,
      { sortBy: "Requests", sortDir: "desc" },
    ),
  );
}
