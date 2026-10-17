// Small DOM helpers: enough to keep the views declarative. A control panel
// this size needs no rendering framework.

/** h builds an element. Children may be nodes, strings, arrays, or null. */
export function h(tag, props, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === null || v === undefined || v === false) continue;
    if (k === "class") el.className = v;
    else if (k === "style" && typeof v === "object") Object.assign(el.style, v);
    else if (k.startsWith("on"))
      el.addEventListener(k.slice(2).toLowerCase(), v);
    else if (k === "dataset") Object.assign(el.dataset, v);
    else if (v === true) el.setAttribute(k, "");
    else el.setAttribute(k, v);
  }
  append(el, children);
  return el;
}

function append(el, children) {
  for (const c of children.flat(4)) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
}

/** svg builds an SVG element, which needs its own namespace. */
export function svg(tag, props, ...children) {
  const el = document.createElementNS("http://www.w3.org/2000/svg", tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === null || v === undefined || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2).toLowerCase(), v);
    else el.setAttribute(k, v);
  }
  append(el, children);
  return el;
}

export function replace(el, ...children) {
  el.replaceChildren();
  append(el, children);
  return el;
}

/* ------------------------------------------------------------ formatting */

const numberFmt = new Intl.NumberFormat();

export function num(n) {
  return numberFmt.format(Math.round(n || 0));
}

/** compact renders large counts short: 1.2M, not 1,203,344. */
export function compact(n) {
  n = n || 0;
  const abs = Math.abs(n);
  if (abs >= 1e9) return (n / 1e9).toFixed(abs >= 1e10 ? 0 : 1) + "B";
  if (abs >= 1e6) return (n / 1e6).toFixed(abs >= 1e7 ? 0 : 1) + "M";
  if (abs >= 1e4) return (n / 1e3).toFixed(0) + "k";
  if (abs >= 1e3) return (n / 1e3).toFixed(1) + "k";
  return numberFmt.format(n);
}

/** money renders integer micro-units of CHF, with the currency first: CHF 3.00. */
export function money(micros) {
  return `CHF ${amount(micros)}`;
}

/** amount renders integer micro-units without the currency, for where it is
 *  said once nearby. Small amounts keep their digits: a per-request cost
 *  rounded to two decimals reads as a bug. The CLI rounds the same way, in
 *  policy.FormatAmount. */
export function amount(micros) {
  const v = (micros || 0) / 1e6;
  let s;
  if (v === 0) s = "0.00";
  else if (Math.abs(v) >= 1) s = v.toFixed(2);
  else s = String(Number(v.toPrecision(2)));
  if (!s.includes(".")) s += ".00";
  else if (s.split(".")[1].length < 2) s += "0";
  return s;
}

/** listPrice renders a price per million tokens exactly, as `keera model list`
 *  does. It is what gets billed, so 0.125 must not read as 0.13. */
export function listPrice(micros) {
  let s = ((micros || 0) / 1e6).toFixed(6).replace(/0+$/, "");
  const decimals = s.length - s.indexOf(".") - 1;
  if (decimals < 2) s += "0".repeat(2 - decimals);
  return s;
}

export function ms(v) {
  if (!v) return "-";
  return v >= 1000
    ? (v / 1000).toFixed(v >= 10000 ? 0 : 1) + " s"
    : Math.round(v) + " ms";
}

/** bytes is a size for a person: 812 B, 4.2 kB, 3.1 MB. */
export function bytes(n) {
  if (n < 1000) return `${num(n)} B`;
  if (n < 1e6) return `${(n / 1e3).toFixed(1)} kB`;
  return `${(n / 1e6).toFixed(1)} MB`;
}

/** plural picks word for one and many for the rest. many defaults to
 *  word + "s", for words like "sandbox" that differ. */
export function plural(n, word, many = word + "s") {
  return n === 1 ? word : many;
}

export function capitalise(word) {
  return word ? word[0].toUpperCase() + word.slice(1) : "";
}

/** duration is how long a task ran, in minutes and hours. ms() is for one
 *  request; nobody reads "690 s" as eleven and a half minutes. */
export function duration(msValue) {
  const secs = Math.round((msValue || 0) / 1000);
  if (secs < 60) return secs + " s";
  const mins = Math.floor(secs / 60);
  if (mins < 60)
    return mins + " min" + (secs % 60 ? " " + (secs % 60) + " s" : "");
  const hours = Math.floor(mins / 60);
  return hours + " h" + (mins % 60 ? " " + (mins % 60) + " min" : "");
}

/** tokens writes a request's tokens in and out, the same on every screen. */
export function tokens(inTokens, outTokens) {
  if (!inTokens && !outTokens) return "-";
  return `${compact(inTokens)} in · ${compact(outTokens)} out`;
}

export function dateTime(iso) {
  if (!iso) return "-";
  const d = new Date(iso);
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function date(iso) {
  if (!iso) return "-";
  return new Date(iso).toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
}

/** releaseDay is a day written as YYYY-MM-DD, such as a model's release date.
 *  It is read as UTC, so it shows the same day in every time zone. */
export function releaseDay(day) {
  if (!day) return "-";
  return new Date(day + "T00:00:00Z").toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    timeZone: "UTC",
  });
}

/** locationName is where a model runs, in words. An unknown code is shown in
 *  capitals. */
export function locationName(loc) {
  const names = { ch: "Switzerland", usa: "USA", onprem: "On-prem" };
  if (!loc) return "-";
  return names[loc] || loc.toUpperCase();
}

/** ago is a relative time for tables, such as "5m ago". */
export function ago(iso) {
  if (!iso) return "-";
  const secs = (Date.now() - new Date(iso).getTime()) / 1000;
  const steps = [
    [60, "s"],
    [3600, "m", 60],
    [86400, "h", 3600],
    [2592000, "d", 86400],
    [Infinity, "mo", 2592000],
  ];
  if (secs < 45) return "just now";
  for (const [limit, unit, div] of steps) {
    if (secs < limit) return Math.round(secs / (div || 1)) + unit + " ago";
  }
  return date(iso);
}

/* ------------------------------------------------------------ components */

/** brandMark is the product mark. It uses the favicon file, so the two cannot
 *  drift apart. */
export function brandMark(size) {
  return h("img", {
    class: "brand-mark",
    src: "/assets/favicon.svg",
    alt: "",
    width: size,
    height: size,
  });
}

export function icon(path) {
  return svg(
    "svg",
    {
      viewBox: "0 0 24 24",
      fill: "none",
      stroke: "currentColor",
      "stroke-width": "1.8",
      "stroke-linecap": "round",
      "stroke-linejoin": "round",
      "aria-hidden": "true",
    },
    svg("path", { d: path }),
  );
}

export const icons = {
  dashboard: "M3 13h5v8H3zM10 3h5v18h-5zM17 9h4v12h-4z",
  // A rocket, while the first-run checklist stands in for the dashboard.
  start:
    "M4.5 16.5c-1.5 1.26-2 5-2 5s3.74-.5 5-2c.71-.84.7-2.13-.09-2.91a2.18 2.18 0 0 0-2.91-.09z" +
    "M12 15l-3-3a22 22 0 0 1 2-3.95A12.88 12.88 0 0 1 22 2c0 2.72-.78 7.5-6 11a22.35 22.35 0 0 1-4 2z" +
    "M9 12H4s.55-3.03 2-4c1.62-1.08 5 0 5 0M12 15v5s3.03-.55 4-2c1.08-1.62 0-5 0-5",
  // A folder, not people: a project holds keys and their guardrails.
  projects:
    "M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z",
  keys: "M21 2l-2 2m-7.6 7.6a5 5 0 1 1-7.1 7.1 5 5 0 0 1 7.1-7.1zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3",
  models: "M12 2l9 5v10l-9 5-9-5V7zM3.3 7L12 12l8.7-5M12 12v10",
  // The two places a model can run, for the tiles that choose between them: a
  // rack for a self-hosted URL, and a cloud for a hosted provider with no logo
  // here.
  selfHosted:
    "M3 4.5h18v6H3zM3 13.5h18v6H3zM6.5 7.5h.01M6.5 16.5h.01M10 7.5h7M10 16.5h7",
  cloud: "M18 10h-1.3A6 6 0 1 0 9 19h9a4.5 4.5 0 0 0 0-9z",
  usage: "M3 3v18h18M7 15l4-5 3 3 5-7",
  billing: "M6 2h12v20l-3-2-3 2-3-2-3 2zM9 7h6M9 11h6M9 15h4",
  audit:
    "M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8zM14 2v6h6M9 13h6M9 17h4",
  orgs: "M3 21h18M5 21V7l7-4 7 4v14M9 9h.01M9 13h.01M9 17h.01M15 9h.01M15 13h.01M15 17h.01",
  signout: "M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4M16 17l5-5-5-5M21 12H9",
  plus: "M12 5v14M5 12h14",
  passkey:
    "M2.6 17.4A2 2 0 0 0 2 18.8V21a1 1 0 0 0 1 1h3a1 1 0 0 0 1-1v-1a1 1 0 0 1 1-1h1a1 1 0 0 0 1-1v-1a1 1 0 0 1 1-1h.2a2 2 0 0 0 1.4-.6l.8-.8a6.5 6.5 0 1 0-4-4zM16.5 7.5h.01",
  copy: "M20 9h-9a2 2 0 0 0-2 2v9a2 2 0 0 0 2 2h9a2 2 0 0 0 2-2v-9a2 2 0 0 0-2-2zM5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1",
  close: "M18 6L6 18M6 6l12 12",
  menu: "M3 6h18M3 12h18M3 18h18",
  theme: "M12 3a9 9 0 1 0 9 9 7 7 0 0 1-9-9z",
  check: "M20 6L9 17l-5-5",
  playground:
    "M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8z",
  send: "M22 2L11 13M22 2l-7 20-4-9-9-4z",
  stop: "M7 7h10v10H7z",
  trash:
    "M3 6h18M8 6V4a1 1 0 0 1 1-1h6a1 1 0 0 1 1 1v2m3 0v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6",
  // For every edit button. The 'rewrite' mark below is the filter mode.
  pencil: "M12 20h9M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z",
  sliders:
    "M4 21v-7M4 10V3M12 21v-9M12 8V3M20 21v-5M20 12V3M1 14h6M9 8h6M17 16h6",
  connect:
    "M4 17l5-5-5-5M12 19h8M3 3h18a1 1 0 0 1 1 1v16a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1z",
  terminal: "M4 17l6-6-6-6M12 19h8",
  download: "M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M7 10l5 5 5-5M12 15V3",
  // A funnel with a shield: the guardrail that rewrites rather than refuses.
  filters:
    "M21 4H3l7 8.5V19l4 2v-8.5zM17 3l3.5 1.4v3c0 2.2-1.4 4.2-3.5 5-2.1-.8-3.5-2.8-3.5-5v-3z",
  // The three filter modes, drawn only on the tiles that pick one, so each
  // shows what the mode does.
  //
  // Rewriting: a pencil over lines of text.
  rewrite:
    "M4 7h8M4 12h5M4 17h10" +
    "M19.6 4.4a1.5 1.5 0 0 1 0 2.1l-5.8 5.8-2.8.7.7-2.8 5.8-5.8a1.5 1.5 0 0 1 2.1 0z",
  // Scales: a gate weighs the request and changes nothing.
  gate:
    "M12 4v17M7.5 21h9M12 7L5 9m7-2l7 2" +
    "M5 9l-2.5 5.5a2.7 2.7 0 0 0 5 0zM19 9l-2.5 5.5a2.7 2.7 0 0 0 5 0z",
  // Brackets, for a rule. Unlike the two marks above, no model is involved.
  pattern: "M8 5H5v14h3M16 5h3v14h-3M10 9.5h4M10 14.5h4",
  // One request in, one of several models out.
  routers: "M4 6h5l4 6h7M20 12l-3-3m3 3l-3 3M9 6l4 12h7",
  // A plug: the socket an agent's tools come through.
  mcp: "M9 3v5M15 3v5M6 8h12v3a6 6 0 0 1-12 0zM12 17v4",
  // The five router modes, drawn only on the tiles that pick one, beside
  // their names. Each shows what the mode reads.
  //
  // A model reading lines of text, with the spark of a decision.
  instruction:
    "M4 6h11M4 11h7M4 16h5" +
    "M17.5 12l1.1 2.4 2.4 1.1-2.4 1.1-1.1 2.4-1.1-2.4-2.4-1.1 2.4-1.1z",
  // Two chevrons: the second is where the request goes when the first does
  // not answer.
  fallback: "M5 7l7 6 7-6M5 13l7 6 7-6",
  // A stopwatch: this mode ranks by measured time.
  latency:
    "M9 2h6M12 2v3.5M20 14a8 8 0 1 1-16 0 8 8 0 0 1 16 0M12 10.5V14l2.5 1.5",
  // Columns of different heights: the request goes to the shortest.
  leastBusy: "M3 20h18M7 20v-7M12 20v-12M17 20v-4",
  // A ruler: this mode measures the request and reads nothing in it.
  size: "M2 9h20v6H2zM7 9v2.5M12 9v3.5M17 9v2.5",
  // A box with a clock: a sandbox is a machine that goes away by itself.
  sandboxes:
    "M21 8v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8M3 8l2-4h14l2 4zM3 8h18" +
    "M12 11v3l2 1.5",
  collapse: "M11 17l-5-5 5-5M18 17l-5-5 5-5",
  back: "M19 12H5M12 19l-7-7 7-7",
  access:
    "M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2M12 3a4 4 0 1 1 0 8 4 4 0 0 1 0-8z",
  people:
    "M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2M13 7a4 4 0 1 1-8 0 4 4 0 0 1 8 0M16 11l2 2 4-4",
  // Rows, not a warning triangle: most requests on this screen worked.
  requests: "M3 6h.01M3 12h.01M3 18h.01M8 6h13M8 12h13M8 18h13",
  // The request log's rows with a brace: many calls that are one task.
  sessions:
    "M6 4c-1.6 0-2 1-2 2v4c0 1-.4 2-2 2 1.6 0 2 1 2 2v4c0 1 .4 2 2 2" +
    "M10 8h11M10 12h11M10 16h11",
  // Two on the left, one in the middle, two on the right: traffic passes
  // through one point.
  map:
    "M4 5.5h3M4 18.5h3M17 5.5h3M17 18.5h3M10.5 12h3" +
    "M7 5.5c3 0 3 6.5 3.5 6.5M7 18.5c3 0 3-6.5 3.5-6.5" +
    "M17 5.5c-3 0-3 6.5-3.5 6.5M17 18.5c-3 0-3-6.5-3.5-6.5",
};

/** providerMarks are the hosted providers' logos, as path data rather than
 *  files: drawn in currentColor, a mark follows its pill through both themes.
 *  Each keeps the artwork's viewBox. A mark about twice as wide as tall sets
 *  `wide`, so a pill draws it smaller. A mark that needs more than one shade
 *  has `layers` instead of `path`: paths that do not overlap, each at its own
 *  opacity.
 *
 *  Keyed by the provider names in internal/catalog/providers.go. A provider
 *  without a mark draws nothing. The source artwork is in internal/webui/marks/,
 *  outside assets/ so the binary does not serve a second copy. Name a new mark
 *  in marks/NOTICE too. */
const providerMarks = {
  anthropic: {
    viewBox: "0 0 35 24",
    path: "M 24.547 0 L 19.338 0 L 28.837997 24 L 34.047 24 Z M 9.499 0 L 0 24 L 5.311 24 L 7.254 18.96 L 17.191 18.96 L 19.134 24 L 24.445 24 L 14.946001 0 Z M 8.972 14.503 L 12.222001 6.069 L 15.473999 14.503 Z",
  },
  cscs: {
    viewBox: "0 0 76.502 52.377",
    // The artwork has five overlapping colours. Each is one path here with the
    // overlaps cut out, so the opacities do not show through.
    layers: [
      {
        opacity: 0.3,
        path: "M34.364 0 26.182 0 15.953 10.228 24.136 10.228ZM18.411 15.959 10.228 15.959 0.004 26.184 8.182 26.188ZM29.864 36.415 26.182 36.415 23.932 34.165 15.953 42.143 24.136 42.143Z",
      },
      {
        opacity: 0.55,
        path: "M15.953 10.228 23.932 18.209 26.182 15.959 29.865 15.959 24.136 10.228ZM10.228 36.415 18.411 36.415 8.182 26.188 0 26.184ZM26.182 52.375 34.364 52.375 24.136 42.143 15.953 42.143Z",
      },
      {
        opacity: 0.6,
        path: "M50.321 0 42.135 0 31.911 10.228 40.093 10.228ZM26.182 15.959 15.957 26.184 24.136 26.188 34.364 15.959ZM58.095 15.959 47.87 26.184 56.048 26.188 66.276 15.959ZM42.14 31.915 31.911 42.143 40.093 42.143 50.321 31.915Z",
      },
      {
        opacity: 0.8,
        path: "M42.14 20.46 50.321 20.46 40.093 10.228 31.911 10.228ZM26.182 36.415 34.364 36.415 24.136 26.188 15.953 26.184ZM58.095 36.415 66.276 36.415 56.048 26.188 47.866 26.184ZM31.911 42.143 42.14 52.375 50.321 52.375 40.093 42.143Z",
      },
      {
        opacity: 1,
        path: "M50.319 0.004 40.095 10.23 50.322 20.461 60.549 10.231ZM24.136 26.188 34.362 36.413 44.588 26.184 34.363 15.96ZM56.048 26.188 66.275 36.415 76.502 26.188 66.275 15.961ZM40.095 42.145 50.321 52.375 50.318 52.375 50.32 52.377 60.547 42.147 50.317 31.92Z",
      },
    ],
  },
  infomaniak: {
    viewBox: "0 0 24 24",
    // One subpath: the tile is drawn and the letterform cut out of it, so this
    // mark is a filled square with a hole where the others are a glyph on
    // nothing. In currentColor that is what it is meant to look like.
    path: "M2.4 0A2.395 2.395 0 0 0 0 2.4v19.2C0 22.9296 1.0704 24 2.4 24h19.2c1.3296 0 2.4 -1.0704 2.4 -2.4V2.4C24 1.0704 22.9296 0 21.6 0H10.112v11.7119l3.648 -4.128h6l-4.58 4.3506 4.868 8.1296h-5.52l-2.5938 -5.0211L10.112 16.8v3.264H5.12V0Z",
  },
  openai: {
    viewBox: "0 0 512 512",
    // The mark's counters are cut by winding rather than by the default rule.
    fillRule: "nonzero",
    path: "M474.123 209.81c11.525-34.577 7.569-72.423-10.838-103.904-27.696-48.168-83.433-72.94-137.794-61.414a127.14 127.14 0 00-95.475-42.49c-55.564 0-104.936 35.781-122.139 88.593-35.781 7.397-66.574 29.76-84.637 61.414-27.868 48.167-21.503 108.72 15.826 150.007-11.525 34.578-7.569 72.424 10.838 103.733 27.696 48.34 83.433 73.111 137.966 61.585 24.084 27.18 58.833 42.835 95.303 42.663 55.564 0 104.936-35.782 122.139-88.594 35.782-7.397 66.574-29.76 84.465-61.413 28.04-48.168 21.676-108.722-15.654-150.008v-.172zm-39.567-87.218c11.01 19.267 15.139 41.803 11.354 63.65-.688-.516-2.064-1.204-2.924-1.72l-101.152-58.49a16.965 16.965 0 00-16.687 0L206.621 194.5v-50.232l97.883-56.597c45.587-26.32 103.732-10.666 130.052 34.921zm-227.935 104.42l49.888-28.9 49.887 28.9v57.63l-49.887 28.9-49.888-28.9v-57.63zm23.223-191.81c22.364 0 43.867 7.742 61.07 22.02-.688.344-2.064 1.204-3.097 1.72L186.666 117.26c-5.161 2.925-8.258 8.43-8.258 14.45v136.934l-43.523-25.116V130.333c0-52.64 42.491-95.13 95.131-95.302l-.172.172zM52.14 168.697c11.182-19.268 28.557-34.062 49.544-41.803V247.14c0 6.02 3.097 11.354 8.258 14.45l118.354 68.295-43.695 25.288-97.711-56.425c-45.415-26.32-61.07-84.465-34.75-130.052zm26.665 220.71c-11.182-19.095-15.139-41.802-11.354-63.65.688.516 2.064 1.204 2.924 1.72l101.152 58.49a16.965 16.965 0 0016.687 0l118.354-68.467v50.232l-97.883 56.425c-45.587 26.148-103.732 10.665-130.052-34.75h.172zm204.54 87.39c-22.192 0-43.867-7.741-60.898-22.02a62.439 62.439 0 003.097-1.72l101.152-58.317c5.16-2.924 8.429-8.43 8.257-14.45V243.527l43.523 25.116v113.022c0 52.64-42.663 95.303-95.131 95.303v-.172zM461.22 343.303c-11.182 19.267-28.729 34.061-49.544 41.63V264.687c0-6.021-3.097-11.526-8.257-14.45L284.893 181.77l43.523-25.116 97.883 56.424c45.587 26.32 61.07 84.466 34.75 130.053l.172.172z",
  },
  phoeniqs: {
    viewBox: "0 0 222.37407 106.13",
    // The source artwork cuts its holes by the even-odd rule.
    fillRule: "evenodd",
    wide: true,
    path: "m 111.005,0.125 c 0.35,0 0.69,0 1.04,0 6.89,7.14 13.77,14.29 20.66,21.44 -2.71,2.55 -5.43,5.11 -8.14,7.66 -4.33,-4.32 -8.67,-8.64 -13,-12.97 -3.72,2.4 -10.64,12.24 -14,12.95 -2.49,-2.55 -4.98,-5.09 -7.46,-7.64 6.96,-7.15 13.93,-14.3 20.9,-21.44 z m 5.69,105.88 c -3.53,0 -7.06,0 -10.58,0 -0.49,-1.11 -3.22,-1.46 -4.43,-2.09 -2.41,-1.26 -4.79,-3.34 -6.5,-5.45 -5.56,-6.83 -5.86,-17.17 -0.92,-24.37 2.44,-3.54 6.46,-5.94 8.74,-9.53 -12.48,-12.5 -24.96,-24.99 -37.44,-37.49 -16.16,16.27 -32.32,32.55 -48.49,48.82 2.04,4.3 13.52,1.64 16.84,0.21 4.14,-1.79 7.26,-5.84 10.37,-9 5.72,-5.82 11.52,-11.57 17.27,-17.37 1.87,-1.89 3.69,-4.45 6.01,-5.77 2.66,2.53 5.32,5.07 7.97,7.6 -2.88,5.18 -9.15,9.61 -13.3,13.83 -13.05,13.28 -23.53,27.89 -44.65,23.57 -4.62,-0.94 -9.2,-3.79 -12.68,-6.91 -1.38,-1.25 -3.15,-4.65 -4.78,-5.03 0,-0.53 0,-1.06 0,-1.59 21.81,-21.53 43.63,-43.07 65.44,-64.6 15.33,15.08 30.67,30.16 46,45.24 15.33,-15.08 30.67,-30.16 46,-45.24 21.55,21.58 43.1,43.15 64.65,64.73 0.41,2.47 -3.03,5.23 -4.73,6.75 -5.69,5.11 -14.44,8.62 -22.25,7.41 -16.29,-2.52 -24.58,-13.99 -35.5,-25.15 -4.1,-4.19 -9.31,-8.17 -12.62,-13.01 2.81,-2.52 5.63,-5.04 8.45,-7.56 7.33,6.85 14.23,14.27 21.31,21.42 3.61,3.66 7.24,8.54 11.99,10.7 3.39,1.55 15.29,4.04 17.1,-0.56 -16.13,-16.16 -32.27,-32.32 -48.4,-48.48 -12.56,12.43 -25.11,24.87 -37.67,37.31 1.34,3.46 5.79,6 8.05,8.94 5.02,6.54 5.7,16.44 0.93,23.39 -1.96,2.85 -4.69,5.57 -7.8,7.16 -1.2,0.62 -3.91,1.04 -4.38,2.12 z m -5.13,-32.98 c -1.44,0.62 -2.45,2.04 -3.56,3.13 -2.35,2.29 -4.81,4.47 -5.66,7.74 -1.36,5.21 3.81,10.75 8.89,10.89 6.42,0.19 10.92,-6.89 8.63,-12.69 -1.31,-3.34 -5.57,-6.84 -8.3,-9.07 z",
  },
  "stepping-stone": {
    viewBox: "0 0 49.43 56.77",
    // The source artwork sat far outside its viewBox; this is moved to the
    // origin and framed tightly.
    path: "m 27.99,10.726574 c 0,0 0.77,0.55 2.08,0.11 1.32,-0.45 3.19,-6.6400002 3.07,-7.6400002 -0.11,-1 -0.33,-2.31999999 -1.97,-2.98999999 -1.65,-0.66 -2.75,0.67 -3.07,1.10999999 -0.33,0.44 -1.21,5.09 -1.65,6.64 -0.44,1.55 1.54,2.7700002 1.54,2.7700002 z m 2.03,-7.7500002 c 0.16,-0.28 0.49,-0.5 0.49,-0.5 0,0 0.5,1.38 0.28,2.38 -0.22,1 -1.65,3.43 -1.65,3.43 0,0 0.71,-5.04 0.88,-5.31 z m 18.71,53.7700002 c -0.44,0.17 -2.42,-0.72 -4.17,-1 -1.75,-0.27 -3.95,0 -3.95,0 l -0.99,-0.77 c 0,0 -0.11,-11.62 -3.35,-20.36 -3.24,-8.74 -7.52,-11.84 -7.52,-11.84 l -0.99,-0.11 c 0,0 -1.1,4.26 -2.52,5.09 -1.43,0.83 -2.75,2.32 -8.45,2.54 -5.71,0.22 -10.1,-1.22 -10.7,-0.94 -0.6,0.28 -2.64,9.57 -3.24,9.79 -0.6,0.22 -1.81,0 -2.08,-0.22 -0.27,-0.22 -0.77,-1.11 -0.77,-1.66 0,-0.55 2.52,-8.07 2.42,-8.85 -0.11,-0.77 -0.22,-1.05 0,-1.33 0.22,-0.28 1.43,-0.33 1.43,-0.33 0,0 7.03,2.32 12.84,1.66 1.92,-0.17 5.49,-0.33 7.13,-2.43 1.65,-2.1 1.87,-4.31 1.87,-4.31 0,0 -0.55,-0.33 -0.77,-0.89 -0.22,-0.55 -0.27,-3.32 -0.33,-4.54 -0.06,-1.21 0.88,-3.87 0.88,-3.87 0,0 -1.32,-1.16 -2.85,-1.11 -1.54,0.06 -4.56,0.33 -7.69,1.99 -3.13,1.66 -3.18,2.55 -3.62,2.77 -0.44,0.22 -2.14,-0.17 -2.31,-0.55 -0.17,-0.39 2.03,-2.66 4.17,-3.43 2.14,-0.78 4.72,-1.66 7.96,-2.0500002 3.24,-0.38 6.15,1.5000002 6.75,1.7100002 0.61,0.22 0.93,0 1.87,0.22 0.93,0.22 1.87,2.6 1.76,3.49 -0.11,0.89 -1.43,5.7 -1.43,5.7 0,0 6.53,7.52 8.56,14.22 2.03,6.69 3.3,17.65 3.3,17.65 0,0 6.97,-0.27 7.35,-0.11 0.38,0.17 -0.11,3.71 -0.55,3.87 z",
  },
};

/** providerMark is a provider's logo at the size of the text beside it, or
 *  null for a provider this build has no mark for. */
export function providerMark(name) {
  return drawMark(providerMarks[String(name || "").toLowerCase()]);
}

function drawMark(mark) {
  if (!mark) return null;
  return svg(
    "svg",
    {
      class: "provider-mark" + (mark.wide ? " provider-mark-wide" : ""),
      viewBox: mark.viewBox,
      fill: "currentColor",
      "aria-hidden": "true",
    },
    mark.layers
      ? mark.layers.map((l) =>
          svg("path", { d: l.path, "fill-opacity": l.opacity }),
        )
      : svg("path", { d: mark.path, "fill-rule": mark.fillRule || null }),
  );
}

/** idpMarks are the identity providers' logos, like providerMarks. They are
 *  keyed by the names an operator picks in KEERA_OIDC_PROVIDERS, so the
 *  obvious spellings of each vendor (idpAliases) share the artwork. A name
 *  with no mark gets a generic icon. */
const idpMarks = {
  google: {
    viewBox: "0 0 24 24",
    path: "M12.24 10.285V14.4h6.806c-.275 1.765-2.056 5.174-6.806 5.174-4.095 0-7.439-3.389-7.439-7.574s3.344-7.574 7.439-7.574c2.33 0 3.891.989 4.785 1.849l3.254-3.138C18.189 1.186 15.479 0 12.24 0c-6.635 0-12 5.365-12 12s5.365 12 12 12c6.926 0 11.52-4.869 11.52-11.726 0-.788-.085-1.39-.189-1.989H12.24z",
  },
  microsoft: {
    viewBox: "0 0 23 23",
    path: "M0 0h11v11H0zM12 0h11v11H12zM0 12h11v11H0zM12 12h11v11H12z",
  },
};

const idpAliases = {
  workspace: "google",
  gsuite: "google",
  entra: "microsoft",
  entraid: "microsoft",
  azure: "microsoft",
  azuread: "microsoft",
  m365: "microsoft",
};

/** idpMark is an identity provider's logo at the size of the text beside it,
 *  or null for one this build has no mark for. */
export function idpMark(name) {
  const key = String(name || "").toLowerCase();
  const mark = idpMarks[idpAliases[key] || key];
  if (!mark) return null;
  return svg(
    "svg",
    {
      class: "provider-mark",
      viewBox: mark.viewBox,
      fill: "currentColor",
      "aria-hidden": "true",
    },
    svg("path", { d: mark.path }),
  );
}

/** clientMarks are the logos of clients that are not a provider's own, like
 *  providerMarks. Keyed by the client keys in internal/connect. */
const clientMarks = {
  opencode: {
    // The artwork's dark tile is left out. The frame is solid and the block
    // inside it is the artwork's grey, as an opacity.
    viewBox: "80 80 352 352",
    layers: [
      { opacity: 1, path: "M384 416H128V96H384ZM320 160H192V352H320Z" },
      { opacity: 0.3, path: "M320 224V352H192V224Z" },
    ],
  },
  pi: {
    // The artwork's three blocks in one shade, so the two that touch become
    // one shape with a square hole. The margin stops a solid mark looking
    // bigger than the outlined ones beside it.
    viewBox: "100 100 600 600",
    path:
      "M165.29 165.29H517.36V400H400V517.36H282.65V634.72H165.29Z" +
      "M282.65 282.65V400H400V282.65Z" +
      "M517.36 400H634.72V634.72H517.36Z",
  },
  python: {
    // The artwork's two snakes, blue and yellow, as two shades.
    viewBox: "0.21 -0.077 110 110",
    layers: [
      {
        opacity: 1,
        path: "M55.023-0.077c-25.971,0-26.25,10.081-26.25,12.156c0,3.148,0,12.594,0,12.594h26.75v3.781 c0,0-27.852,0-37.375,0c-7.949,0-17.938,4.833-17.938,26.25c0,19.673,7.792,27.281,15.656,27.281c2.335,0,9.344,0,9.344,0 s0-9.765,0-13.125c0-5.491,2.721-15.656,15.406-15.656c15.91,0,19.971,0,26.531,0c3.902,0,14.906-1.696,14.906-14.406 c0-13.452,0-17.89,0-24.219C82.054,11.426,81.515-0.077,55.023-0.077z M40.273,8.392c2.662,0,4.813,2.15,4.813,4.813 c0,2.661-2.151,4.813-4.813,4.813s-4.813-2.151-4.813-4.813C35.46,10.542,37.611,8.392,40.273,8.392z",
      },
      {
        opacity: 0.55,
        path: "M55.397,109.923c25.959,0,26.282-10.271,26.282-12.156c0-3.148,0-12.594,0-12.594H54.897v-3.781 c0,0,28.032,0,37.375,0c8.009,0,17.938-4.954,17.938-26.25c0-23.322-10.538-27.281-15.656-27.281c-2.336,0-9.344,0-9.344,0 s0,10.216,0,13.125c0,5.491-2.631,15.656-15.406,15.656c-15.91,0-19.476,0-26.532,0c-3.892,0-14.906,1.896-14.906,14.406 c0,14.475,0,18.265,0,24.219C28.366,100.497,31.562,109.923,55.397,109.923z M70.148,101.454c-2.662,0-4.813-2.151-4.813-4.813 s2.15-4.813,4.813-4.813c2.661,0,4.813,2.151,4.813,4.813S72.809,101.454,70.148,101.454z",
      },
    ],
  },
};

/** clientMark is a client's logo, or null for one this build has no mark for. */
export function clientMark(key) {
  return drawMark(clientMarks[key]);
}

/* The reporting window every screen with a picker shows.
 *
 *  One remembered value shared by all screens, because readers compare them:
 *  a filter's refusals this afternoon next to the project's traffic this
 *  afternoon. A picker that reset on each screen would break that. */
export const RANGES = [
  { label: "24h", since: "24h", long: "Last 24 hours" },
  { label: "7d", since: "168h", long: "Last 7 days" },
  { label: "30d", since: "720h", long: "Last 30 days" },
];

/** rangeOf is the entry in RANGES for since, or fallback when none matches. */
export function rangeOf(since, fallback) {
  return RANGES.find((r) => r.since === since) || fallback;
}

/* --------------------------------------------------------------- storage */

/** remembered is a setting kept for this tab, or with lasting for every visit,
 *  or null. A browser that refuses storage reads as nothing kept. */
export function remembered(key, { lasting } = {}) {
  try {
    return (lasting ? localStorage : sessionStorage).getItem(key);
  } catch {
    return null;
  }
}

/** remember keeps a setting, or forgets it when value is null. A browser that
 *  refuses storage still gets the change; it just does not last. */
export function remember(key, value, { lasting } = {}) {
  try {
    const store = lasting ? localStorage : sessionStorage;
    if (value === null) store.removeItem(key);
    else store.setItem(key, value);
  } catch {
    // Nothing to do: the setting holds until the page reloads.
  }
}

/** currentRange is the window in force. Screens read it instead of a default
 *  of their own. */
const DEFAULT_RANGE = "168h";

export function currentRange() {
  return remembered("keera.range") || DEFAULT_RANGE;
}

/** rangePicker is the segmented control that sets it, and reloads the screen.
 *
 *  A screen with a window of its own, like Usage or the audit log, passes its
 *  own ranges and the session key it stores the choice under. */
export function rangePicker(
  ctx,
  since,
  { ranges = RANGES, key = "keera.range" } = {},
) {
  return h(
    "div",
    { class: "seg" },
    ranges.map((r) =>
      h(
        "button",
        {
          "aria-pressed": String(r.since === since),
          title: r.long,
          onClick: () => {
            remember(key, r.since);
            ctx.reload();
          },
        },
        r.label,
      ),
    ),
  );
}

/** liveControl is the Live switch on a screen that follows the request log.
 *
 *  Its dot shows four states: watching, reconnecting, paused, or stopped. A
 *  screen that quietly stopped following looks just like a quiet afternoon.
 *
 *  key remembers the choice for the session, label names the button for a
 *  screen reader, titles explain each state, and words, if given, change the
 *  button's text with the state. */
export function liveControl({ key, label, titles, words = {} }) {
  const on = remembered(key) !== "off";
  const blip = h("span", { class: "blip" });
  const text = h("span", {}, "Live");
  const button = h(
    "button",
    {
      class: "btn btn-live",
      type: "button",
      "aria-pressed": String(on),
      "aria-label": label,
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
      button.title = titles[what];
      text.textContent = words[what] || "Live";
    },
    onChange(fn) {
      listener = fn;
    },
  };
  button.addEventListener("click", () => {
    control.on = !control.on;
    remember(key, control.on ? "on" : "off");
    button.setAttribute("aria-pressed", String(control.on));
    if (listener) listener(control.on);
  });
  // A hidden tab drops its stream, because each open stream costs the control
  // plane a query per request in its organisation. Screens pick up where they
  // stopped when the tab is shown again.
  const onVisibility = () => {
    if (!button.isConnected) {
      document.removeEventListener("visibilitychange", onVisibility);
      return;
    }
    if (control.on && listener) listener(!document.hidden);
  };
  document.addEventListener("visibilitychange", onVisibility);
  return control;
}

/** followRequests keeps the request stream open while a liveControl is on,
 *  and shows on the control whether it is connecting, live or stopped.
 *
 *  stream opens the EventSource, and is called again on every reconnect, so
 *  it can carry the newest position. onRequests gets each batch it sends.
 *  onResume, if given, runs when requests may have been missed: when the
 *  control is turned back on, and when a dropped stream comes back. */
export function followRequests(ctx, control, stream, { onRequests, onResume }) {
  let source = null;
  const close = () => {
    if (source) source.close();
    source = null;
  };
  ctx.onTeardown(close);

  const open = () => {
    close();
    control.state("connecting");
    let first = true;
    source = stream();
    source.addEventListener("open", () => {
      control.state("live");
      if (!first && onResume) onResume();
      first = false;
    });
    source.addEventListener("requests", (e) => {
      control.state("live");
      onRequests(JSON.parse(e.data));
    });
    // An EventSource retries a dropped connection by itself and gives up only
    // when refused, as for an expired session. Those are different states.
    source.addEventListener("error", () => {
      control.state(
        source && source.readyState === EventSource.CLOSED
          ? "stopped"
          : "connecting",
      );
    });
  };

  control.onChange((on) => {
    if (!on) {
      close();
      control.state("paused");
      return;
    }
    open();
    if (onResume) onResume();
  });
  if (control.on) open();
  else control.state("paused");
}

/** throttled runs fn at most once every ms. A call while it waits is not
 *  lost: fn runs once more after the wait. A failure is swallowed, so the
 *  screen keeps what it shows and the next call tries again. stop ends it. */
export function throttled(fn, ms) {
  let busy = false;
  let again = false;
  let stopped = false;
  async function run() {
    if (stopped) return;
    if (busy) {
      again = true;
      return;
    }
    busy = true;
    try {
      await fn();
    } catch {
      // Left as it is. The next call tries again.
    }
    setTimeout(() => {
      busy = false;
      if (again) {
        again = false;
        run();
      }
    }, ms);
  }
  run.stop = () => (stopped = true);
  return run;
}

export function pill(text, tone) {
  return h("span", { class: "pill" + (tone ? " pill-" + tone : "") }, text);
}

/** rowLink is a row's name as a link to the row's own screen. Tables have no
 *  whole-row click: rows that open on some screens and not others are worse
 *  than none. */
export function rowLink(ctx, path, text, title) {
  return h(
    "a",
    {
      class: "row-link",
      href: path,
      title: title || null,
      onClick: go(ctx, path),
    },
    text,
  );
}

/** go is a click handler that routes inside the panel instead of reloading
 *  it. The href is still the real path, so the link opens in a new tab. */
export function go(ctx, path) {
  return (e) => {
    e.preventDefault();
    ctx.navigate(path);
  };
}

/** isAdmin reports whether the signed-in person may change the organisation
 *  they are looking at: an operator, or one of its administrators. */
export function isAdmin(ctx) {
  return !!ctx.state.me.can_admin_org;
}

/** checkButton is a Check button with its verdict beside it once it has run.
 *  verdict turns what run returns into { label, tone, title, open }: a pill
 *  that opens the full report. The result is not kept: a check is about right
 *  now. */
export function checkButton(title, run, verdict) {
  const slot = h("span");
  const button = h(
    "button",
    {
      class: "btn btn-sm btn-quiet",
      title,
      onClick: async () => {
        button.disabled = true;
        slot.replaceChildren(
          h(
            "span",
            { class: "faint nowrap" },
            h("span", { class: "blip" }),
            " checking…",
          ),
        );
        try {
          slot.replaceChildren(verdictPill(verdict(await run())));
        } catch (ex) {
          slot.replaceChildren(
            h("span", { class: "pill pill-bad" }, "Check failed"),
          );
          toast(ex.message, "bad");
        } finally {
          button.disabled = false;
        }
      },
    },
    "Check",
  );
  return h("div", { class: "row-tight" }, button, slot);
}

function verdictPill({ label, tone, title, open }) {
  return h(
    "button",
    { class: "pill pill-" + tone + " pill-button", title, onClick: open },
    h("span", { class: "dot" }),
    label,
  );
}

export function crumb(ctx, path, label) {
  return h(
    "a",
    { class: "crumb", href: path, onClick: go(ctx, path) },
    icon(icons.back),
    label,
  );
}

/** gone is a detail screen for something that is not there. */
export function gone(ctx, path, label, title, body) {
  return h(
    "div",
    {},
    h("div", { class: "detail-head" }, crumb(ctx, path, label)),
    h("div", { class: "card" }, empty(title, body)),
  );
}

/** detailHead is the strip atop a detail screen: the link back to its list,
 *  chips, then controls on the right. */
export function detailHead(ctx, [path, label], chips, controls) {
  return h(
    "div",
    { class: "detail-head" },
    crumb(ctx, path, label),
    h(
      "div",
      { class: "row", style: { flexWrap: "wrap" } },
      h("div", { class: "wrap-chips" }, chips),
      h("div", { class: "spacer" }),
      controls,
    ),
  );
}

/** busyButton is disabled while run works. A failure is toasted. */
export function busyButton(props, label, run) {
  const button = h(
    "button",
    {
      class: "btn",
      ...props,
      onClick: async () => {
        button.disabled = true;
        try {
          await run();
        } catch (ex) {
          toast(ex.message, "bad");
        } finally {
          button.disabled = false;
        }
      },
    },
    label,
  );
  return button;
}

/** iconButton is a small button that shows only an icon. title is its
 *  tooltip and, unless label is given, what a screen reader reads. Other
 *  options are set on the button as they are. */
export function iconButton(
  glyph,
  title,
  onClick,
  { quiet, danger, cls, label = title, ...attrs } = {},
) {
  const classes = [
    "btn",
    quiet && "btn-quiet",
    "btn-sm",
    danger && "btn-danger",
  ];
  return h(
    "button",
    {
      class: [...classes, cls].filter(Boolean).join(" "),
      title,
      "aria-label": label,
      onClick,
      ...attrs,
    },
    icon(glyph),
  );
}

/** csvButton saves what a screen lists as a CSV file. title says what the
 *  file holds. */
export function csvButton(title, download) {
  return h(
    "button",
    { class: "btn", title, onClick: download },
    icon(icons.download),
    "CSV",
  );
}

/** listHead is the strip above a list: an intro, its controls and an add
 *  button. add is { label, onClick }, or null when the reader cannot add. */
export function listHead(intro, controls, add) {
  return h(
    "div",
    { class: "detail-head" },
    intro ? h("div", { class: "muted" }, intro) : null,
    h(
      "div",
      { class: "row", style: { flexWrap: "wrap" } },
      h("div", { class: "spacer" }),
      controls,
      add
        ? h(
            "button",
            { class: "btn btn-primary", onClick: add.onClick },
            icon(icons.plus),
            add.label,
          )
        : null,
    ),
  );
}

/** sectionHead is a heading between a page's tables. A text note is set faint
 *  on the right; anything else, such as buttons, goes there as is. */
export function sectionHead(title, note, style) {
  return h(
    "div",
    { class: "section-head", style },
    h("h2", {}, title),
    h("div", { class: "spacer" }),
    typeof note === "string" ? h("span", { class: "faint" }, note) : note,
  );
}

/** pct is part of whole as a percentage. Tiny shares keep three decimals,
 *  so they do not round to zero. */
export function pct(part, whole) {
  if (!whole) return "-";
  const frac = (part || 0) / whole;
  return `${(frac * 100).toFixed(frac > 0 && frac < 0.001 ? 3 : 1)}%`;
}

/** stat is a figure with its label. value may be an array, to add a unit. */
export function stat(label, value, note) {
  return statCard(label, [value], note);
}

/** moneyStat is a stat for an amount, with the currency first. */
export function moneyStat(label, micros, note) {
  return statCard(
    label,
    [h("span", { class: "unit unit-first" }, "CHF"), amount(micros)],
    note,
  );
}

function statCard(label, value, note) {
  return h(
    "div",
    { class: "card stat" },
    h("div", { class: "stat-label" }, label),
    h("div", { class: "stat-value" }, ...value),
    note ? h("div", { class: "stat-note" }, note) : null,
  );
}

/** loadMore is the button under a log that adds the next page to the same
 *  table, so the reader keeps what they have read. next adds a page and
 *  returns how many rows came; fewer than size is the end, and shows end. */
export function loadMore(size, next, end) {
  const label = `Load ${size} more`;
  const wrap = h("div", { style: { marginTop: "12px", textAlign: "center" } });
  const more = h(
    "button",
    {
      class: "btn",
      onClick: async () => {
        more.disabled = true;
        more.textContent = "Loading…";
        try {
          if ((await next()) < size) {
            wrap.replaceChildren(h("div", { class: "hint" }, end));
            return;
          }
        } catch (ex) {
          wrap.append(banner("bad", ex.message));
        } finally {
          more.disabled = false;
          more.textContent = label;
        }
      },
    },
    label,
  );
  wrap.append(more);
  return wrap;
}

/** trafficSummary is a row's traffic in two lines: how many, with a pill when
 *  something needs a look, then a faint note. A count of zero is no traffic. */
export function trafficSummary(count, noun, flag, note) {
  if (!count) return h("span", { class: "faint nowrap" }, "no traffic");
  return h(
    "div",
    { class: "stack" },
    h(
      "div",
      { class: "row-tight" },
      h("span", { class: "nowrap" }, compact(count), " ", noun),
      flag,
    ),
    h("div", { class: "faint nowrap small" }, note),
  );
}

export function dash() {
  return h("span", { class: "faint" }, "-");
}

export function empty(title, body) {
  return h("div", { class: "empty" }, h("strong", {}, title), body || null);
}

function toggleBox(t, checked, onChange) {
  return h(
    "label",
    { class: "row-tight" },
    h("input", {
      type: "checkbox",
      checked,
      onChange: (e) => onChange(e.target.checked),
    }),
    t.label,
  );
}

/** table renders rows in a card, with search, filters and sorting.
 *
 *  opts:
 *    search       (row) => string - the text the search box matches against.
 *                 Passing it adds the box.
 *    searchLabel  what is being searched, for the placeholder
 *    toggles      [{ label, test, hidden, on, onChange }] - a checkbox that,
 *                 while checked, keeps only the rows test() accepts. With
 *                 hidden() instead, it drops those rows while unchecked, so a
 *                 list can hide its dead rows behind a box that brings them
 *                 back. `on` is whether it starts checked. onChange(checked)
 *                 is for a toggle whose rows are not here: the caller fetches
 *                 a different list.
 *    choices      [{ label, options: [{ value, label }], test }] - a dropdown
 *                 that keeps only the rows test(row, value) accepts. Its first
 *                 option is "All", which keeps every row.
 *    sortBy       the label of the column to sort by at first
 *    sortDir      "asc" (the default) or "desc"
 *    rank         { by, set } - for a list the control plane ranks, because
 *                 only part of it is here. A column with a `rank` gets a
 *                 header that calls set(rank), and the one matching `by` is
 *                 marked. Rows are drawn in the order given.
 *    rowClass     (row) => string - a class for that row's <tr>
 *    emptyTitle / emptyBody
 *
 *  A column with a sortKey gets a header that sorts by it. Searching and
 *  sorting happen here, as most screens hold their whole list. One that pages
 *  passes `rank` instead, and its sortKeys are ignored: sorting one page would
 *  rank the wrong rows.
 *
 *  A column with a `width` keeps that width instead of one measured from its
 *  contents, so it stays still when the table is redrawn with other rows.
 *
 *  There is no whole-row click; see rowLink.
 */
export function table(columns, rows, opts = {}) {
  const toggles = opts.toggles || [];
  const choices = opts.choices || [];

  if (!rows.length) {
    const card = h(
      "div",
      { class: "card" },
      empty(opts.emptyTitle || "Nothing here yet", opts.emptyBody),
    );
    // Over an empty table, only a toggle that fetches its own list is kept:
    // it can bring rows back. The others have nothing to work on.
    const fetches = toggles.filter((t) => t.onChange);
    if (!fetches.length) return card;
    return h(
      "div",
      {},
      h(
        "div",
        { class: "toolbar" },
        fetches.map((t) => toggleBox(t, !!t.on, (v) => t.onChange(v))),
        h("div", { class: "spacer" }),
      ),
      card,
    );
  }

  const view = {
    q: "",
    on: toggles.map((t) => !!t.on),
    picked: choices.map(() => ""),
    sort: opts.rank
      ? null
      : columns.find((c) => c.sortKey && c.label === opts.sortBy) || null,
    dir: opts.sortDir === "desc" ? -1 : 1,
  };

  const body = h("tbody");
  const count = h("span", {
    class: "faint nowrap small",
  });
  const sortable = new Map();

  const heads = columns.map((c) => {
    const attrs = {
      class: cellClass(c),
      style: c.width ? { width: c.width } : null,
    };
    if (opts.rank) return rankHead(c, attrs, opts.rank);
    if (!c.sortKey) return h("th", attrs, c.label);
    const arrow = h("span", { class: "sort-arrow", "aria-hidden": "true" });
    const th = h(
      "th",
      attrs,
      h(
        "button",
        {
          class: "th-sort",
          title: `Sort by ${c.label.toLowerCase()}`,
          onClick: () => {
            if (view.sort === c) view.dir = -view.dir;
            else {
              view.sort = c;
              view.dir = c.sortDir === "desc" ? -1 : 1;
            }
            draw();
          },
        },
        c.label,
        arrow,
      ),
    );
    sortable.set(c, { th, arrow });
    return th;
  });

  function visible() {
    let out = rows;
    const q = view.q.trim().toLowerCase();
    if (q && opts.search) {
      out = out.filter((r) =>
        String(opts.search(r) ?? "")
          .toLowerCase()
          .includes(q),
      );
    }
    toggles.forEach((t, i) => {
      if (view.on[i]) {
        if (t.test) out = out.filter(t.test);
      } else if (t.hidden) {
        out = out.filter((r) => !t.hidden(r));
      }
    });
    choices.forEach((c, i) => {
      const v = view.picked[i];
      if (v) out = out.filter((r) => c.test(r, v));
    });
    if (view.sort) out = out.slice().sort(order(view.sort.sortKey, view.dir));
    return out;
  }

  function draw() {
    const shown = visible();
    replace(
      body,
      shown.length
        ? shown.map((row) =>
            h(
              "tr",
              { class: opts.rowClass ? opts.rowClass(row) : null },
              columns.map((c) => h("td", { class: cellClass(c) }, c.cell(row))),
            ),
          )
        : h(
            "tr",
            {},
            h(
              "td",
              { colspan: String(columns.length), class: "table-none" },
              "Nothing matches. Clear the search, or turn a filter off.",
            ),
          ),
    );
    // How many rows are shown, but only while that is not all of them.
    count.textContent =
      shown.length === rows.length ? "" : `${shown.length} of ${rows.length}`;
    for (const [c, { th, arrow }] of sortable) {
      const active = view.sort === c;
      arrow.textContent = active ? (view.dir === 1 ? "↑" : "↓") : "";
      if (active)
        th.setAttribute(
          "aria-sort",
          view.dir === 1 ? "ascending" : "descending",
        );
      else th.removeAttribute("aria-sort");
    }
  }

  let toolbar = null;
  if (opts.search || toggles.length || choices.length) {
    const label = opts.searchLabel ? `Search ${opts.searchLabel}` : "Search";
    toolbar = h(
      "div",
      { class: "toolbar" },
      opts.search
        ? h("input", {
            class: "input toolbar-search",
            type: "search",
            placeholder: label,
            "aria-label": label,
            autocomplete: "off",
            spellcheck: "false",
            onInput: (e) => {
              view.q = e.target.value;
              draw();
            },
          })
        : null,
      choices.map((c, i) =>
        h(
          "select",
          {
            class: "select toolbar-select",
            "aria-label": c.label,
            onChange: (e) => {
              view.picked[i] = e.target.value;
              draw();
            },
          },
          h("option", { value: "" }, `All ${c.label.toLowerCase()}`),
          c.options.map((o) => h("option", { value: o.value }, o.label)),
        ),
      ),
      toggles.map((t, i) =>
        toggleBox(t, view.on[i], (v) => {
          view.on[i] = v;
          draw();
          if (t.onChange) t.onChange(v);
        }),
      ),
      h("div", { class: "spacer" }),
      count,
    );
  }

  draw();
  const card = h(
    "div",
    { class: "card" },
    h(
      "div",
      { class: "table-wrap" },
      h("table", {}, h("thead", {}, h("tr", {}, heads)), body),
    ),
  );
  const el = toolbar ? h("div", {}, toolbar, card) : card;
  // The table carries its own redraw for the request log, which pushes new
  // rows onto its array and calls this. Calling table() again would build a
  // new table and lose the reader's search and sort.
  el.redraw = draw;
  return el;
}

/** rankHead is a header for a list the control plane ranks. Every ranking is
 *  largest first, so a second click does not reverse it. */
function rankHead(c, attrs, rank) {
  if (c.rank === undefined) return h("th", attrs, c.label);
  const active = c.rank === rank.by;
  return h(
    "th",
    { ...attrs, "aria-sort": active ? "descending" : null },
    h(
      "button",
      {
        class: "th-sort",
        title:
          c.rank === ""
            ? "Newest first"
            : `Sort by ${c.label.toLowerCase()}, highest first`,
        onClick: () => {
          if (!active) rank.set(c.rank);
        },
      },
      c.label,
      h(
        "span",
        { class: "sort-arrow", "aria-hidden": "true" },
        active ? "↓" : "",
      ),
    ),
  );
}

function cellClass(c) {
  return (
    [c.num && "num", c.shrink && "shrink"].filter(Boolean).join(" ") || null
  );
}

/** order sorts by one column's key, with rows that have no value at the bottom
 *  in either direction. A key never used is not the oldest; the question does
 *  not apply to it. */
function order(key, dir) {
  const missing = (v) => v === null || v === undefined || v === "";
  return (a, b) => {
    const x = key(a);
    const y = key(b);
    if (missing(x) || missing(y))
      return missing(x) && missing(y) ? 0 : missing(x) ? 1 : -1;
    if (typeof x === "number" && typeof y === "number") return (x - y) * dir;
    return (
      String(x).localeCompare(String(y), undefined, {
        numeric: true,
        sensitivity: "base",
      }) * dir
    );
  };
}

/** modal opens a dialog and returns a close function.
 *
 *  The actions live in the footer, outside the body's form, so the form's
 *  submit is wired to the primary action here. Otherwise pressing Enter would
 *  do nothing. */
export function modal({ title, subtitle, body, actions, wide }) {
  const overlay = h("div", { class: "overlay" });
  // Focus goes back where it came from, so a keyboard user keeps their place.
  const opener = document.activeElement;
  let open = true;
  const close = () => {
    if (!open) return;
    open = false;
    overlay.remove();
    document.removeEventListener("keydown", onKey, true);
    if (opener && opener.isConnected) opener.focus();
  };
  const onKey = (e) => {
    if (e.key === "Escape") return close();
    if (e.key === "Tab") trapTab(card, e);
  };
  document.addEventListener("keydown", onKey, true);
  overlay.addEventListener("mousedown", (e) => {
    if (e.target === overlay) close();
  });

  const titleID = "modal-title-" + Math.random().toString(36).slice(2, 8);
  const card = h(
    "div",
    {
      class: "modal" + (wide ? " modal-wide" : ""),
      role: "dialog",
      "aria-modal": "true",
      "aria-labelledby": titleID,
    },
    h(
      "div",
      { class: "modal-head" },
      h(
        "div",
        { class: "stack" },
        h("h2", { id: titleID }, title),
        subtitle ? h("div", { class: "page-sub" }, subtitle) : null,
      ),
      h("div", { class: "spacer" }),
      h(
        "button",
        {
          class: "btn btn-quiet btn-sm",
          "aria-label": "Close",
          onClick: close,
        },
        icon(icons.close),
      ),
    ),
    h("div", { class: "modal-body" }, body),
    actions ? h("div", { class: "modal-foot" }, actions(close)) : null,
  );
  overlay.append(card);
  document.body.append(overlay);

  // Enter anywhere in the body's form runs the primary action.
  const form = card.querySelector("form");
  const primary = card.querySelector(
    ".modal-foot .btn-primary, .modal-foot .btn-danger",
  );
  if (form && primary) {
    // A browser submits on Enter only if the form has a submit button or a
    // single text field. The real buttons are in the footer, so this hidden
    // one stands in, first so it is the form's default.
    if (!form.querySelector("[type=submit]")) {
      form.prepend(
        h("button", {
          type: "submit",
          class: "submit-proxy",
          tabindex: "-1",
          "aria-hidden": "true",
        }),
      );
    }
    form.addEventListener("submit", (e) => {
      e.preventDefault();
      if (!primary.disabled) primary.click();
    });
  }

  // The first field that can be typed in, skipping hidden or disabled ones.
  const first = [
    ...card.querySelectorAll(
      "input:not([type=hidden]), select, textarea, button.btn-primary",
    ),
  ].find((el) => !el.disabled && el.offsetParent !== null);
  if (first) first.focus();
  return close;
}

/** trapTab keeps Tab inside the dialog, for keyboard users. */
function trapTab(card, e) {
  const items = [
    ...card.querySelectorAll(
      "a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), " +
        "textarea:not([disabled]), [tabindex]:not([tabindex='-1'])",
    ),
  ].filter(
    (el) =>
      el.tabIndex >= 0 &&
      (el.offsetParent !== null || el === document.activeElement),
  );
  if (!items.length) return;
  const firstEl = items[0];
  const lastEl = items[items.length - 1];
  if (e.shiftKey && document.activeElement === firstEl) {
    e.preventDefault();
    lastEl.focus();
  } else if (!e.shiftKey && document.activeElement === lastEl) {
    e.preventDefault();
    firstEl.focus();
  }
}

/** formModal is a dialog that does one thing with its primary button.
 *  submit(close) does it, and returns false to keep the dialog open, as after
 *  focusing an empty field. A failure, or a thrown { message } of text or
 *  nodes, shows in the dialog. On success the dialog closes, toasts done (text,
 *  or a function of what submit returned) and reloads ctx, each if given.
 *  Returns { close, button }. */
export function formModal({
  title,
  subtitle,
  wide,
  body,
  submitLabel = "Save",
  danger,
  submit,
  done,
  ctx,
}) {
  const err = h("div");
  body.prepend(err);
  let button;
  const close = modal({
    title,
    subtitle,
    wide,
    body,
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Cancel"),
      (button = h(
        "button",
        {
          class: "btn " + (danger ? "btn-danger" : "btn-primary"),
          onClick: async () => {
            button.disabled = true;
            showError(err, "");
            try {
              const result = await submit(close);
              if (result === false) {
                button.disabled = false;
                return;
              }
              close();
              const said = typeof done === "function" ? done(result) : done;
              if (said) toast(said, "good");
              if (ctx) ctx.reload();
            } catch (ex) {
              showError(err, (ex && ex.message) || "That did not work.");
              button.disabled = false;
            }
          },
        },
        submitLabel,
      )),
    ],
  });
  return { close, button };
}

/** confirm asks before something irreversible. A failed action shows in the
 *  dialog, so a failure is as visible as success. */
export function confirm({
  title,
  body,
  confirmLabel = "Confirm",
  danger,
  onConfirm,
}) {
  formModal({
    title,
    body: h("div", {}, h("div", { class: "muted" }, body)),
    submitLabel: confirmLabel,
    danger,
    submit: onConfirm,
  });
}

/** rowActions is a row's edit and delete buttons. Delete asks first, then
 *  runs remove, toasts removed and reloads the page.
 *
 *  body may be a function, for a warning looked up only when the button is
 *  pressed. */
export function rowActions(
  ctx,
  name,
  { editTitle, onEdit, deleteTitle, body, confirmLabel, remove },
) {
  return h(
    "div",
    { class: "row-tight" },
    iconButton(icons.pencil, editTitle, () => onEdit(), {
      label: `Edit ${name}`,
    }),
    iconButton(
      icons.trash,
      deleteTitle,
      async () =>
        confirm({
          title: `Delete ${name}?`,
          body: typeof body === "function" ? await body() : body,
          confirmLabel,
          danger: true,
          onConfirm: async () => {
            await remove();
            toast(`${name} deleted`, "good");
            ctx.reload();
          },
        }),
      { danger: true, label: `Delete ${name}` },
    ),
  );
}

let toastHost;

export function toast(message, tone = "") {
  if (!toastHost) {
    // A live region, so screen readers announce toasts.
    toastHost = h("div", {
      class: "toasts",
      role: "status",
      "aria-live": "polite",
    });
    document.body.append(toastHost);
  }
  const el = h("div", { class: "toast " + tone }, message);
  toastHost.append(el);
  setTimeout(() => el.remove(), tone === "bad" ? 7000 : 3500);
}

export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast("Copied to the clipboard", "good");
  } catch {
    // Clipboard access needs a secure context, which a plain-http demo is not.
    toast("Could not copy. Select the text and copy it by hand.", "bad");
  }
}

// code renders a block to be copied, with its Copy button. Every such block is
// meant to end up in a file.
export function code(text) {
  return h(
    "div",
    { class: "code" },
    iconButton(icons.copy, "Copy", () => copyText(text), { cls: "code-copy" }),
    h("pre", {}, h("code", {}, text)),
  );
}

/** secretField shows a value that is handed over once, with a Copy button. */
export function secretField(value, ariaLabel) {
  return h(
    "div",
    { class: "row-tight" },
    h("input", {
      class: "input key-value mono",
      readonly: true,
      value,
      "aria-label": ariaLabel,
      // Selecting on focus makes copying by hand easy, for when there is no
      // clipboard: plain http on anything but localhost has none.
      onFocus: (e) => e.target.select(),
      onClick: (e) => e.target.select(),
    }),
    h(
      "button",
      { class: "btn", onClick: () => copyText(value) },
      icon(icons.copy),
      "Copy",
    ),
  );
}

/** banner is a coloured note. tone is bad, warn, info or good. Attributes,
 *  such as a style, may come before the children as a plain object. */
export function banner(tone, ...children) {
  const first = children[0];
  const props =
    first && Object.getPrototypeOf(first) === Object.prototype
      ? children.shift()
      : {};
  return h("div", { ...props, class: `banner banner-${tone}` }, ...children);
}

/** showError shows a failure in a dialog's error host and scrolls it into
 *  view. The host is at the top of .modal-body, which scrolls, while the
 *  buttons stay in the footer, so in a tall dialog the message would otherwise
 *  be out of sight. Passing no message clears the host, as before a retry. */
export function showError(host, message) {
  if (!message) {
    replace(host);
    return;
  }
  replace(host, banner("bad", message));
  // "nearest" so a dialog already showing the banner does not jump. Guarded
  // because jsdom and very old browsers lack it.
  if (host.scrollIntoView) {
    host.scrollIntoView({ block: "nearest" });
  }
}

/* aliasProblem checks the alias rule where the name is typed.
 *
 *  An alias is a path segment, so an empty one sends the write to the
 *  collection route, which answers 405 with no message. The control plane
 *  cannot explain that one, so it is caught here.
 *
 *  The shape mirrors policy.validName: lowercase letters, digits, and hyphens
 *  that neither open nor close the name. Returns "" when there is no problem. */
export function aliasProblem(alias) {
  if (!alias) return "An alias is required. It is the name clients use.";
  return nameProblem(alias, 64, "An alias");
}

/** modelAliasProblem is aliasProblem for a model, which may also carry its
 *  provider's name, as in "anthropic/claude-sonnet-4-5". It mirrors
 *  policy.ValidModelAlias: each part between slashes has the alias shape. */
export function modelAliasProblem(alias) {
  if (!alias) return "An alias is required. It is the name clients use.";
  if (alias.length > 64) return "An alias is at most 64 characters.";
  if (alias.split("/").some((part) => nameProblem(part, 64, "An alias"))) {
    return (
      "Use only lowercase letters, digits, hyphens and slashes, as in " +
      "anthropic/claude-sonnet-4-5. A part cannot start or end with a hyphen."
    );
  }
  return "";
}

/** takenProblem refuses an alias the organisation already has. Saving is a
 *  PUT, so "add" would otherwise overwrite the entry of that name. The CLI
 *  refuses the same way. */
export function takenProblem(entries, alias, noun) {
  return (entries || []).some((e) => e.alias === alias)
    ? `The ${noun} ${alias} already exists. Change it from its row instead.`
    : "";
}

/** sandboxNameProblem mirrors policy.ValidSandboxName: the alias rule, but at
 *  most 40 characters, because the name also becomes a hostname. */
export function sandboxNameProblem(name) {
  if (!name) return "Give the sandbox a name.";
  return nameProblem(name, 40, "A sandbox name");
}

function nameProblem(name, max, what) {
  if (name.length > max) return `${what} is at most ${max} characters.`;
  if (!/^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(name)) {
    return (
      "Use only lowercase letters, digits and hyphens. It cannot start or " +
      "end with a hyphen."
    );
  }
  return "";
}

/** modeTiles asks which mode something works in, as tiles with a sentence
 *  each: a long option list goes unread. Once a mode is chosen the tiles fold
 *  into one row with a Change button.
 *
 *  modes is a list of { value, icon, name, what }, with an optional mark()
 *  that draws a logo in place of the icon. chosen is the starting value, or
 *  null for none. onChoose runs after each choice; the caller decides where
 *  focus goes. choose(value) picks a mode from code, as an edit dialog does on
 *  opening. */
export function modeTiles(modes, chosen, onChoose) {
  const tiles = h(
    "div",
    { class: "tiles" },
    modes.map((m) =>
      h(
        "button",
        {
          class: "tile",
          type: "button",
          "aria-pressed": String(m.value === chosen),
          onClick: () => choose(m.value),
        },
        h("span", { class: "tile-mark" }, markOf(m)),
        h("span", { class: "tile-name" }, m.name),
        h("span", { class: "tile-what" }, m.what),
      ),
    ),
  );
  const chosenRow = h("div", { class: "tile-chosen", hidden: true });
  const change = h(
    "button",
    {
      class: "btn btn-sm",
      type: "button",
      onClick: () => {
        tiles.hidden = false;
        chosenRow.hidden = true;
        tiles.firstChild.focus();
      },
    },
    "Change",
  );
  const choose = (value) => {
    const m = modes.find((x) => x.value === value) || modes[0];
    tiles.childNodes.forEach((tile, i) => {
      tile.setAttribute("aria-pressed", String(modes[i].value === value));
    });
    chosenRow.replaceChildren(
      markOf(m),
      h(
        "div",
        { class: "stack", style: { gap: "1px" } },
        h("span", { class: "tile-name" }, m.name),
        h("span", { class: "tile-what" }, m.what),
      ),
      h("div", { class: "spacer" }),
      change,
    );
    tiles.hidden = true;
    chosenRow.hidden = false;
    onChoose(value);
  };
  return { tiles, chosenRow, change, choose };
}

function markOf(m) {
  return (m.mark && m.mark()) || icon(m.icon);
}

/** field renders a labelled control. hint may be a .hint element the caller
 *  keeps to change later, and extra goes below it. */
export function field(label, input, hint, ...extra) {
  return h(
    "div",
    { class: "field" },
    h("label", { for: input.id || null }, label),
    input,
    hint instanceof Element && hint.classList.contains("hint")
      ? hint
      : hint
        ? h("div", { class: "hint" }, hint)
        : null,
    extra,
  );
}

// readRow is a field that shows a value rather than asking for one.
export function readRow(label, value, hint) {
  return h(
    "div",
    { class: "field" },
    h("label", {}, label),
    h("div", {}, value),
    hint ? h("div", { class: "hint" }, hint) : null,
  );
}

export function meter(fraction, tone) {
  const pct = Math.max(0, Math.min(1, fraction || 0)) * 100;
  return h(
    "div",
    { class: "meter" },
    h("div", {
      class: "meter-fill " + (tone || ""),
      style: { width: pct + "%" },
    }),
  );
}
