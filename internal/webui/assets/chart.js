// The dashboard's chart, drawn as inline SVG.
//
// Hand-drawn rather than pulled from a library: it is one shape, and a charting
// library would be the largest dependency in a binary whose whole pitch is that
// it has almost none.

import { h, svg, compact, money, num } from "./ui.js";

const PAD = { top: 12, right: 8, bottom: 22, left: 42 };

// The width a chart is drawn at before it is on the page and can be measured.
// Close enough to a real card that the first paint is not visibly re-laid out.
const GUESS_W = 900;
const MIN_W = 280;

/**
 * areaChart draws requests over time with failures as a baseline bar.
 * points: [{ at, requests, tokens, cost_micros, errors }]
 */
export function areaChart(
  points,
  { height = 190, currency = "", bucket = "day" } = {},
) {
  const holder = h("div", { class: "chart-holder" });
  if (!points || points.length === 0) {
    holder.append(h("div", { class: "empty" }, "No requests in this period."));
    return holder;
  }

  // A single point has no line to draw, so it is widened into a flat pair.
  const data = points.length === 1 ? [points[0], { ...points[0] }] : points;
  const tip = h("div", { class: "chart-tip", style: { display: "none" } });

  // The chart is drawn in the pixels it will occupy rather than in a fixed unit
  // grid the browser then stretches to fit: a stretched grid stretches the axis
  // labels and the line along with it. A detached element has no width to draw
  // at, so it is drawn at a plausible one and redrawn once its own is known.
  let drawnAt = 0;
  function draw(width) {
    const w = Math.max(MIN_W, Math.round(width));
    if (w === drawnAt) return;
    drawnAt = w;
    const plot = paint(data, w, height, { currency, bucket, tip });
    const old = holder.querySelector("svg.chart");
    if (old) {
      // Whatever the pointer was over is gone with the shape it was over.
      tip.style.display = "none";
      old.replaceWith(plot);
    } else {
      holder.prepend(plot);
    }
  }

  draw(GUESS_W);
  holder.append(tip);
  // The chart's height does not follow its width, so redrawing cannot resize
  // what is being observed, and this needs no guard against feeding itself.
  new ResizeObserver((entries) => {
    draw(entries[entries.length - 1].contentRect.width);
  }).observe(holder);
  return holder;
}

/** paint draws the chart itself at a given pixel size. */
function paint(data, W, H, { currency, bucket, tip }) {
  const innerW = W - PAD.left - PAD.right;
  const innerH = H - PAD.top - PAD.bottom;

  const maxY = Math.max(1, ...data.map((p) => p.requests));
  const niceMax = niceCeil(maxY);
  const x = (i) =>
    PAD.left +
    (data.length === 1 ? innerW / 2 : (i / (data.length - 1)) * innerW);
  const y = (v) => PAD.top + innerH - (v / niceMax) * innerH;

  const line = data
    .map(
      (p, i) =>
        `${i === 0 ? "M" : "L"}${x(i).toFixed(1)},${y(p.requests).toFixed(1)}`,
    )
    .join(" ");
  const area =
    `${line} L${x(data.length - 1).toFixed(1)},${(PAD.top + innerH).toFixed(1)} ` +
    `L${x(0).toFixed(1)},${(PAD.top + innerH).toFixed(1)} Z`;

  const ticks = [0, 0.5, 1].map((f) => Math.round(niceMax * f));
  const gridlines = ticks.map((t) => [
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

  // Failures are drawn as short bars along the baseline: they are usually rare
  // enough to vanish on the same scale as requests, and they matter more.
  const maxErr = Math.max(1, ...data.map((p) => p.errors));
  const barW = Math.max(1.5, Math.min(9, innerW / data.length - 1.5));
  const errBars = data.map((p, i) =>
    p.errors > 0
      ? svg("rect", {
          class: "err-bar",
          x: (x(i) - barW / 2).toFixed(1),
          y: (PAD.top + innerH - (p.errors / maxErr) * (innerH * 0.28)).toFixed(
            1,
          ),
          width: barW.toFixed(1),
          height: ((p.errors / maxErr) * (innerH * 0.28)).toFixed(1),
          rx: 1,
        })
      : null,
  );

  const cursor = svg("line", {
    class: "cursor-line",
    x1: 0,
    x2: 0,
    y1: PAD.top,
    y2: PAD.top + innerH,
    opacity: 0,
  });

  const hits = data.map((p, i) => {
    const w = innerW / data.length;
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
          h("div", { class: "t" }, bucketLabel(p.at, bucket, true)),
          h(
            "div",
            { class: "muted" },
            `${num(p.requests)} requests · ${compact(p.tokens)} tokens`,
          ),
          h(
            "div",
            { class: "muted" },
            money(p.cost_micros, currency) +
              (p.errors ? ` · ${num(p.errors)} failed` : ""),
          ),
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

  // Label as many positions as fit: a date needs about seventy pixels of its
  // own, and a crowded axis is worth less than a sparse one. The last bucket is
  // always named, and whatever would have sat on top of it is dropped instead.
  const last = data.length - 1;
  const step = Math.max(
    1,
    Math.ceil(data.length / Math.max(2, Math.floor(innerW / 70))),
  );
  const labelled = new Set([last]);
  for (let i = 0; i < last; i += step) {
    if (x(last) - x(i) >= 60) labelled.add(i);
  }
  const xLabels = data.map((p, i) =>
    labelled.has(i)
      ? svg(
          "text",
          {
            class: "axis-text",
            x: x(i).toFixed(1),
            y: H - 6,
            "text-anchor": i === 0 ? "start" : i === last ? "end" : "middle",
          },
          bucketLabel(p.at, bucket, false),
        )
      : null,
  );

  return svg(
    "svg",
    {
      class: "chart",
      viewBox: `0 0 ${W} ${H}`,
      width: W,
      height: H,
      role: "img",
      "aria-label": "Requests over time",
    },
    gridlines,
    svg("path", { class: "area", d: area }),
    svg("path", { class: "line", d: line }),
    errBars,
    cursor,
    xLabels,
    hits,
  );
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

/** niceCeil rounds an axis maximum up to something a human would have chosen. */
function niceCeil(v) {
  const mag = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 1.5, 2, 3, 5, 10]) {
    if (v <= m * mag) return m * mag;
  }
  return 10 * mag;
}

/**
 * barList is the "top teams" and "top models" panel: a ranked list where the
 * bar is the row, so the label stays readable at any value.
 */
export function barList(
  rows,
  { label, value, currency, colorIndex = 0, color: colorFor } = {},
) {
  if (!rows || rows.length === 0) {
    return h("div", { class: "empty" }, "Nothing recorded yet.");
  }
  const max = Math.max(...rows.map((r) => Math.abs(value(r))), 1);
  return h(
    "div",
    { class: "bars" },
    rows.map((r, i) => {
      const v = value(r);
      // Ranked lists are categorical, so position picks the colour. A list
      // whose rows mean something - an outcome split, say - passes `color`
      // instead, so a row keeps its colour when the rows above it drop out.
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
        h(
          "div",
          { class: "nowrap muted" },
          currency ? money(r.cost_micros, currency) : compact(v),
        ),
      );
    }),
  );
}
