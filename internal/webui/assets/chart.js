// The panel's charts, drawn as inline SVG by hand: a charting library would
// be the largest dependency in a binary that has almost none.

import { h, svg, compact, money, num, empty } from "./ui.js";

const PAD = { top: 12, right: 8, bottom: 22, left: 42 };

// The width to draw at before the chart can be measured. Close to a real
// card, so the first paint does not visibly shift.
const GUESS_W = 900;
const MIN_W = 280;

/**
 * areaChart draws requests over time with failures as a baseline bar.
 * points: [{ at, requests, tokens, cost_micros, failed }]
 */
export function areaChart(points, { height = 190, bucket = "day" } = {}) {
  if (!points || points.length === 0) {
    return noRequests();
  }
  // A single point has no line to draw, so it is widened into a flat pair.
  const data = points.length === 1 ? [points[0], { ...points[0] }] : points;
  return responsive((w, tip) => paintArea(data, w, height, { bucket, tip }));
}

/**
 * lineChart draws one line per series over the same times, with a legend.
 * lines: [{ label, color, counts, dashed }], counts holding one number per
 * time in at.
 */
export function lineChart(at, lines, { height = 190, bucket = "day" } = {}) {
  if (!at || at.length === 0 || !lines || lines.length === 0) {
    return noRequests();
  }
  const times = at.length === 1 ? [at[0], at[0]] : at;
  const series =
    at.length === 1
      ? lines.map((l) => ({ ...l, counts: [l.counts[0], l.counts[0]] }))
      : lines;
  const holder = responsive((w, tip) =>
    paintLines(times, series, w, height, { bucket, tip }),
  );
  holder.append(
    h(
      "div",
      { class: "chart-legend" },
      lines.map((l) => h("span", { class: "chart-key" }, swatch(l), l.label)),
    ),
  );
  return holder;
}

function noRequests() {
  return h(
    "div",
    { class: "chart-holder" },
    empty("No requests in this period"),
  );
}

function swatch(l) {
  return h("span", {
    class: "chart-swatch" + (l.dashed ? " dashed" : ""),
    style: { background: l.color },
  });
}

// responsive draws a chart in real pixels: a unit grid the browser stretches
// would stretch the labels and the line too. A detached element has no width,
// so it is drawn at a guess and redrawn once its width is known.
function responsive(paint) {
  const holder = h("div", { class: "chart-holder" });
  const tip = h("div", { class: "chart-tip", style: { display: "none" } });
  let drawnAt = 0;
  function draw(width) {
    const w = Math.max(MIN_W, Math.round(width));
    if (w === drawnAt) return;
    drawnAt = w;
    const plot = paint(w, tip);
    const old = holder.querySelector("svg.chart");
    if (old) {
      // The shape the pointer was over is gone.
      tip.style.display = "none";
      old.replaceWith(plot);
    } else {
      holder.prepend(plot);
    }
  }

  draw(GUESS_W);
  holder.append(tip);
  // The height does not follow the width, so a redraw cannot trigger another
  // resize.
  new ResizeObserver((entries) => {
    draw(entries[entries.length - 1].contentRect.width);
  }).observe(holder);
  return holder;
}

// frame is what every chart shares: the scales, the grid, the time labels,
// and a hover target per time that fills the tip through describe.
function frame(times, maxY, W, H, { bucket, tip, describe }) {
  const n = times.length;
  const innerW = W - PAD.left - PAD.right;
  const innerH = H - PAD.top - PAD.bottom;
  const niceMax = niceCeil(Math.max(1, maxY));
  const x = (i) => PAD.left + (n === 1 ? innerW / 2 : (i / (n - 1)) * innerW);
  const y = (v) => PAD.top + innerH - (v / niceMax) * innerH;
  const path = (values) =>
    values
      .map(
        (v, i) => `${i === 0 ? "M" : "L"}${x(i).toFixed(1)},${y(v).toFixed(1)}`,
      )
      .join(" ");

  const ticks = [0, 0.5, 1].map((f) => Math.round(niceMax * f));
  const grid = ticks.map((t) => [
    svg("line", {
      class: "grid-line",
      x1: PAD.left,
      x2: W - PAD.right,
      y1: y(t).toFixed(1),
      y2: y(t).toFixed(1),
    }),
    svg(
      "text",
      {
        class: "axis-text",
        x: PAD.left - 7,
        y: (y(t) + 3.5).toFixed(1),
        "text-anchor": "end",
      },
      compact(t),
    ),
  ]);

  const cursor = svg("line", {
    class: "cursor-line",
    x1: 0,
    x2: 0,
    y1: PAD.top,
    y2: PAD.top + innerH,
    opacity: 0,
  });

  const hits = times.map((at, i) => {
    const w = innerW / n;
    return svg("rect", {
      class: "hit",
      x: (PAD.left + i * w).toFixed(1),
      y: PAD.top,
      width: w.toFixed(1),
      height: innerH,
      onMouseenter: () => {
        cursor.setAttribute("x1", x(i).toFixed(1));
        cursor.setAttribute("x2", x(i).toFixed(1));
        cursor.setAttribute("opacity", 1);
        tip.style.display = "";
        tip.replaceChildren(
          h("div", { class: "t" }, bucketLabel(at, bucket, true)),
          ...describe(i),
        );
        const frac = x(i) / W;
        tip.style.left = `calc(${(frac * 100).toFixed(2)}% - ${frac > 0.6 ? 150 : -12}px)`;
        tip.style.top = "6px";
      },
      onMouseleave: () => {
        cursor.setAttribute("opacity", 0);
        tip.style.display = "none";
      },
    });
  });

  // Label as many positions as fit: a date needs about seventy pixels. The last
  // bucket is always named, and a label that would overlap it is dropped.
  const last = n - 1;
  const step = Math.max(1, Math.ceil(n / Math.max(2, Math.floor(innerW / 70))));
  const labelled = new Set([last]);
  for (let i = 0; i < last; i += step) {
    if (x(last) - x(i) >= 60) labelled.add(i);
  }
  const labels = times.map((at, i) =>
    labelled.has(i)
      ? svg(
          "text",
          {
            class: "axis-text",
            x: x(i).toFixed(1),
            y: H - 6,
            "text-anchor": i === 0 ? "start" : i === last ? "end" : "middle",
          },
          bucketLabel(at, bucket, false),
        )
      : null,
  );

  const chart = (label, ...marks) =>
    svg(
      "svg",
      {
        class: "chart",
        viewBox: `0 0 ${W} ${H}`,
        width: W,
        height: H,
        role: "img",
        "aria-label": label,
      },
      grid,
      ...marks,
      cursor,
      labels,
      hits,
    );
  return { x, y, path, innerW, innerH, chart };
}

function paintArea(data, W, H, { bucket, tip }) {
  const f = frame(
    data.map((p) => p.at),
    Math.max(...data.map((p) => p.requests)),
    W,
    H,
    {
      bucket,
      tip,
      describe: (i) => {
        const p = data[i];
        return [
          h(
            "div",
            { class: "muted" },
            `${num(p.requests)} requests · ${compact(p.tokens)} tokens`,
          ),
          h(
            "div",
            { class: "muted" },
            money(p.cost_micros) +
              (p.failed ? ` · ${num(p.failed)} failed` : ""),
          ),
        ];
      },
    },
  );
  const { x, innerW, innerH } = f;
  const line = f.path(data.map((p) => p.requests));
  const base = (PAD.top + innerH).toFixed(1);
  const area =
    `${line} L${x(data.length - 1).toFixed(1)},${base} ` +
    `L${x(0).toFixed(1)},${base} Z`;

  // Failures are drawn as short bars along the baseline: they are usually rare
  // enough to vanish on the same scale as requests, and they matter more.
  const maxErr = Math.max(1, ...data.map((p) => p.failed));
  const barW = Math.max(1.5, Math.min(9, innerW / data.length - 1.5));
  const errBars = data.map((p, i) => {
    if (!(p.failed > 0)) return null;
    const bh = (p.failed / maxErr) * (innerH * 0.28);
    return svg("rect", {
      class: "err-bar",
      x: (x(i) - barW / 2).toFixed(1),
      y: (PAD.top + innerH - bh).toFixed(1),
      width: barW.toFixed(1),
      height: bh.toFixed(1),
      rx: 1,
    });
  });

  return f.chart(
    "Requests over time",
    svg("path", { class: "area", d: area }),
    svg("path", { class: "line", d: line }),
    errBars,
  );
}

function paintLines(times, lines, W, H, { bucket, tip }) {
  const f = frame(times, Math.max(...lines.flatMap((l) => l.counts)), W, H, {
    bucket,
    tip,
    describe: (i) =>
      lines.map((l) =>
        h(
          "div",
          { class: "chart-tip-row" },
          swatch(l),
          h("span", { class: "muted" }, l.label),
          h("span", { class: "v" }, num(l.counts[i])),
        ),
      ),
  });
  // The rest is drawn first, so the named lines sit on top of it.
  const paths = lines
    .map((l) =>
      svg("path", {
        class: "series" + (l.dashed ? " dashed" : ""),
        d: f.path(l.counts),
        style: `stroke: ${l.color}`,
      }),
    )
    .reverse();
  return f.chart("Requests over time", paths);
}

function bucketLabel(iso, bucket, long) {
  const d = new Date(iso);
  if (bucket === "hour") {
    return long
      ? d.toLocaleString(undefined, {
          month: "short",
          day: "numeric",
          hour: "2-digit",
          minute: "2-digit",
        })
      : d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  }
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

/** niceCeil rounds an axis maximum up to a round number. */
function niceCeil(v) {
  const mag = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 1.5, 2, 3, 5, 10]) {
    if (v <= m * mag) return m * mag;
  }
  return 10 * mag;
}

/**
 * barList is a ranked list, such as "top models", where the bar is the row,
 * so the label stays readable at any value.
 */
export function barList(
  rows,
  { label, value, cost = false, colorIndex = 0, color: colorFor } = {},
) {
  if (!rows || rows.length === 0) {
    return empty("Nothing recorded yet");
  }
  const max = Math.max(...rows.map((r) => Math.abs(value(r) || 0)), 1);
  return h(
    "div",
    { class: "bars" },
    rows.map((r, i) => {
      const v = value(r) || 0;
      // Position picks the colour. A list whose rows mean something, such as
      // an outcome split, passes `color`, so a row keeps its colour when rows
      // above it drop out.
      const color = colorFor
        ? colorFor(r)
        : `var(--s${((colorIndex + i) % 6) + 1})`;
      return h(
        "div",
        { class: "bar-row" },
        h(
          "div",
          { class: "bar-track" },
          h("div", {
            class: "bar-fill",
            style: {
              width: Math.max(2, (v / max) * 100) + "%",
              background: color,
            },
          }),
          h("div", { class: "bar-label" }, label(r)),
        ),
        h("div", { class: "nowrap muted" }, cost ? money(v) : compact(v)),
      );
    }),
  );
}
