// Small DOM helpers. Enough structure to keep the views declarative, and no
// more - this is a control panel with ten screens, not an application that
// needs a rendering framework.

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
  for (const c of children.flat(4)) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

export function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
  return el;
}

export function replace(el, ...children) {
  clear(el);
  append(el, children);
  return el;
}

/* ------------------------------------------------------------ formatting */

const numberFmt = new Intl.NumberFormat();

export function num(n) {
  return numberFmt.format(Math.round(n || 0));
}

/** compact renders large counts the way a dashboard should: 1.2M, not 1,203,344. */
export function compact(n) {
  n = n || 0;
  const abs = Math.abs(n);
  if (abs >= 1e9) return (n / 1e9).toFixed(abs >= 1e10 ? 0 : 1) + "B";
  if (abs >= 1e6) return (n / 1e6).toFixed(abs >= 1e7 ? 0 : 1) + "M";
  if (abs >= 1e4) return (n / 1e3).toFixed(0) + "k";
  if (abs >= 1e3) return (n / 1e3).toFixed(1) + "k";
  return numberFmt.format(n);
}

/** money renders integer micro-units. Small amounts keep their digits: a
 *  per-request cost rounded to two decimals reads as a bug, not as a number. */
export function money(micros, currency) {
  const v = (micros || 0) / 1e6;
  let s;
  if (v === 0) s = "0.00";
  else if (Math.abs(v) >= 1) s = v.toFixed(2);
  else s = String(Number(v.toPrecision(2)));
  if (!s.includes(".")) s += ".00";
  else if (s.split(".")[1].length < 2) s += "0";
  return currency ? `${s} ${currency}` : s;
}

export function ms(v) {
  if (!v) return "-";
  return v >= 1000
    ? (v / 1000).toFixed(v >= 10000 ? 0 : 1) + " s"
    : Math.round(v) + " ms";
}

/** duration is how long something ran, in the units somebody would say it in.
 *
 *  ms() is for one request, where the interesting range is milliseconds to a
 *  few seconds. A task runs for minutes or for an afternoon, and "690 s" is a
 *  number nobody reads as eleven and a half minutes. */
export function duration(msValue) {
  const secs = Math.round((msValue || 0) / 1000);
  if (secs < 60) return secs + "s";
  const mins = Math.floor(secs / 60);
  if (mins < 60) return mins + "m" + (secs % 60 ? " " + (secs % 60) + "s" : "");
  const hours = Math.floor(mins / 60);
  return hours + "h" + (mins % 60 ? " " + (mins % 60) + "m" : "");
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

/** ago is what a table wants for "when", with the exact time on hover. */
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

/** brandMark is the product mark. It points at the same file the browser draws
 *  in the tab, so the logo and the favicon can never drift apart. */
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
  overview: "M3 13h5v8H3zM10 3h5v18h-5zM17 9h4v12h-4z",
  teams:
    "M17 20v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2M9.5 7a3.5 3.5 0 1 1-7 0 3.5 3.5 0 0 1 7 0M22 20v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75",
  keys: "M21 2l-2 2m-7.6 7.6a5 5 0 1 1-7.1 7.1 5 5 0 0 1 7.1-7.1zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3",
  models: "M12 2l9 5v10l-9 5-9-5V7zM3.3 7L12 12l8.7-5M12 12v10",
  // The two places a model can run, drawn for the tiles that choose between
  // them. A rack of machines addressed by a URL you wrote - and a cloud, for a
  // hosted provider whose own logo this build has none of.
  selfHosted:
    "M3 4.5h18v6H3zM3 13.5h18v6H3zM6.5 7.5h.01M6.5 16.5h.01M10 7.5h7M10 16.5h7",
  cloud: "M18 10h-1.3A6 6 0 1 0 9 19h9a4.5 4.5 0 0 0 0-9z",
  usage: "M3 3v18h18M7 15l4-5 3 3 5-7",
  audit:
    "M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8zM14 2v6h6M9 13h6M9 17h4",
  orgs: "M3 21h18M5 21V7l7-4 7 4v14M9 9h.01M9 13h.01M9 17h.01M15 9h.01M15 13h.01M15 17h.01",
  signout: "M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4M16 17l5-5-5-5M21 12H9",
  plus: "M12 5v14M5 12h14",
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
  // A pencil, for renaming a thing in place. The 'rewrite' mark below is a
  // pencil too, but over lines of text: that one is a filter changing what a
  // request says, and this one is a label being corrected.
  pencil: "M12 20h9M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z",
  sliders:
    "M4 21v-7M4 10V3M12 21v-9M12 8V3M20 21v-5M20 12V3M1 14h6M9 8h6M17 16h6",
  connect:
    "M4 17l5-5-5-5M12 19h8M3 3h18a1 1 0 0 1 1 1v16a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1z",
  download: "M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M7 10l5 5 5-5M12 15V3",
  filter: "M22 3H2l8 9.5V19l4 2v-8.5z",
  // The guardrail that rewrites rather than refuses: a funnel with the shield
  // the plain one lacks, so a table's filter control and this screen are not
  // the same mark in two places.
  filters:
    "M21 4H3l7 8.5V19l4 2v-8.5zM17 3l3.5 1.4v3c0 2.2-1.4 4.2-3.5 5-2.1-.8-3.5-2.8-3.5-5v-3z",
  // The three things a filter can do to a request, one mark each. Like the
  // router modes below, they are drawn only on the tiles that pick one, so
  // each says what the mode does rather than repeating the guardrail's shield.
  //
  // Rewriting: a pencil over the lines it is going through.
  rewrite:
    "M4 7h8M4 12h5M4 17h10" +
    "M19.6 4.4a1.5 1.5 0 0 1 0 2.1l-5.8 5.8-2.8.7.7-2.8 5.8-5.8a1.5 1.5 0 0 1 2.1 0z",
  // Scales. A gate weighs the request and nothing else happens to it - a
  // barrier or a door would have been the mark for where it ends up, which is
  // the same place it started unless the verdict goes against it.
  gate:
    "M12 4v17M7.5 21h9M12 7L5 9m7-2l7 2" +
    "M5 9l-2.5 5.5a2.7 2.7 0 0 0 5 0zM19 9l-2.5 5.5a2.7 2.7 0 0 0 5 0z",
  // A rule, in the brackets a developer reads as one. No model anywhere in it,
  // and nothing borrowed from the two marks above, which are both a model at
  // work on lines of text.
  pattern: "M8 5H5v14h3M16 5h3v14h-3M10 9.5h4M10 14.5h4",
  // The other hook: a path that arrives and leaves by two of three ways, which
  // is the whole of what a router does - one request in, one of several models
  // out.
  routers: "M4 6h5l4 6h7M20 12l-3-3m3 3l-3 3M9 6l4 12h7",
  // A plug: what an MCP server is to an agent, a socket its tools come
  // through.
  mcp: "M9 3v5M15 3v5M6 8h12v3a6 6 0 0 1-12 0zM12 17v4",
  // The five things a router can choose with, one mark each. They are only
  // ever drawn on the tiles that pick a mode, where the words beside them do
  // the naming - so each says what the mode reads, not what it is called.
  //
  // A model reading: lines of text, and the spark of a decision beside them.
  instruction:
    "M4 6h11M4 11h7M4 16h5" +
    "M17.5 12l1.1 2.4 2.4 1.1-2.4 1.1-1.1 2.4-1.1-2.4-2.4-1.1 2.4-1.1z",
  // Down the written order. Two chevrons rather than one: the second is where
  // the request goes when the destination above it does not answer.
  fallback: "M5 7l7 6 7-6M5 13l7 6 7-6",
  // A stopwatch, because this mode ranks by time measured rather than promised.
  latency:
    "M9 2h6M12 2v3.5M20 14a8 8 0 1 1-16 0 8 8 0 0 1 16 0M12 10.5V14l2.5 1.5",
  // What each destination is carrying: columns of different heights, and the
  // request goes to the short one.
  leastBusy: "M3 20h18M7 20v-7M12 20v-12M17 20v-4",
  // A ruler. This mode measures the request and reads nothing in it.
  size: "M2 9h20v6H2zM7 9v2.5M12 9v3.5M17 9v2.5",
  // Sandboxes: a box with a clock face in it. The box is the machine and the
  // clock is the whole point - what distinguishes this from a virtual machine
  // is that it goes away by itself - so a plain container mark would have been
  // the wrong half of the idea.
  sandboxes:
    "M21 8v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8M3 8l2-4h14l2 4zM3 8h18" +
    "M12 11v3l2 1.5",
  collapse: "M11 17l-5-5 5-5M18 17l-5-5 5-5",
  // The way back to the list a detail screen was opened from.
  back: "M19 12H5M12 19l-7-7 7-7",
  access:
    "M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2M12 3a4 4 0 1 1 0 8 4 4 0 0 1 0-8z",
  people:
    "M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2M13 7a4 4 0 1 1-8 0 4 4 0 0 1 8 0M16 11l2 2 4-4",
  // The event log: rows, each with its own mark. Not the warning triangle a log
  // of failures would have carried - most of what is on this screen worked, and
  // a sidebar that calls the whole traffic a warning is a sidebar that lies.
  requests: "M3 6h.01M3 12h.01M3 18h.01M8 6h13M8 12h13M8 18h13",
  // The request log's rows with a brace around them: this screen is those
  // rows, grouped into the tasks they were made for. Not a clock and not a
  // speech bubble - what a session is about is that many calls are one thing.
  sessions:
    "M6 4c-1.6 0-2 1-2 2v4c0 1-.4 2-2 2 1.6 0 2 1 2 2v4c0 1 .4 2 2 2" +
    "M10 8h11M10 12h11M10 16h11",
  // The map: two things on the left, one in the middle, two on the right, and
  // the lines between them. Not a globe and not a network - what this screen is
  // about is that the traffic goes through one point and out the other side.
  map:
    "M4 5.5h3M4 18.5h3M17 5.5h3M17 18.5h3M10.5 12h3" +
    "M7 5.5c3 0 3 6.5 3.5 6.5M7 18.5c3 0 3-6.5 3.5-6.5" +
    "M17 5.5c-3 0-3 6.5-3.5 6.5M17 18.5c-3 0-3-6.5-3.5-6.5",
};

/** providerMarks are the hosted providers' own logos, as path data rather than
 *  files, for the reason `icons` is: a mark drawn in currentColor follows the
 *  pill it sits in through both themes, which a brand SVG with its own
 *  near-black fill does not. Each keeps the artwork's own viewBox.
 *
 *  Keyed by the provider names in internal/catalog/providers.go. A provider
 *  with no mark here is not an error - providerMark draws nothing. The source
 *  artwork is in internal/webui/marks/, outside the embedded assets/ tree so
 *  the binary does not serve a second copy of every logo. A new mark is named
 *  in marks/NOTICE too, which says they are their owners' trademarks. */
export const providerMarks = {
  anthropic: {
    viewBox: "0 0 35 24",
    path: "M 24.547 0 L 19.338 0 L 28.837997 24 L 34.047 24 Z M 9.499 0 L 0 24 L 5.311 24 L 7.254 18.96 L 17.191 18.96 L 19.134 24 L 24.445 24 L 14.946001 0 Z M 8.972 14.503 L 12.222001 6.069 L 15.473999 14.503 Z",
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
  "stepping-stone": {
    viewBox: "0 0 49.43 56.77",
    // The source artwork sat far outside its own viewBox; this one is moved
    // to the origin and framed tightly.
    path: "m 27.99,10.726574 c 0,0 0.77,0.55 2.08,0.11 1.32,-0.45 3.19,-6.6400002 3.07,-7.6400002 -0.11,-1 -0.33,-2.31999999 -1.97,-2.98999999 -1.65,-0.66 -2.75,0.67 -3.07,1.10999999 -0.33,0.44 -1.21,5.09 -1.65,6.64 -0.44,1.55 1.54,2.7700002 1.54,2.7700002 z m 2.03,-7.7500002 c 0.16,-0.28 0.49,-0.5 0.49,-0.5 0,0 0.5,1.38 0.28,2.38 -0.22,1 -1.65,3.43 -1.65,3.43 0,0 0.71,-5.04 0.88,-5.31 z m 18.71,53.7700002 c -0.44,0.17 -2.42,-0.72 -4.17,-1 -1.75,-0.27 -3.95,0 -3.95,0 l -0.99,-0.77 c 0,0 -0.11,-11.62 -3.35,-20.36 -3.24,-8.74 -7.52,-11.84 -7.52,-11.84 l -0.99,-0.11 c 0,0 -1.1,4.26 -2.52,5.09 -1.43,0.83 -2.75,2.32 -8.45,2.54 -5.71,0.22 -10.1,-1.22 -10.7,-0.94 -0.6,0.28 -2.64,9.57 -3.24,9.79 -0.6,0.22 -1.81,0 -2.08,-0.22 -0.27,-0.22 -0.77,-1.11 -0.77,-1.66 0,-0.55 2.52,-8.07 2.42,-8.85 -0.11,-0.77 -0.22,-1.05 0,-1.33 0.22,-0.28 1.43,-0.33 1.43,-0.33 0,0 7.03,2.32 12.84,1.66 1.92,-0.17 5.49,-0.33 7.13,-2.43 1.65,-2.1 1.87,-4.31 1.87,-4.31 0,0 -0.55,-0.33 -0.77,-0.89 -0.22,-0.55 -0.27,-3.32 -0.33,-4.54 -0.06,-1.21 0.88,-3.87 0.88,-3.87 0,0 -1.32,-1.16 -2.85,-1.11 -1.54,0.06 -4.56,0.33 -7.69,1.99 -3.13,1.66 -3.18,2.55 -3.62,2.77 -0.44,0.22 -2.14,-0.17 -2.31,-0.55 -0.17,-0.39 2.03,-2.66 4.17,-3.43 2.14,-0.78 4.72,-1.66 7.96,-2.0500002 3.24,-0.38 6.15,1.5000002 6.75,1.7100002 0.61,0.22 0.93,0 1.87,0.22 0.93,0.22 1.87,2.6 1.76,3.49 -0.11,0.89 -1.43,5.7 -1.43,5.7 0,0 6.53,7.52 8.56,14.22 2.03,6.69 3.3,17.65 3.3,17.65 0,0 6.97,-0.27 7.35,-0.11 0.38,0.17 -0.11,3.71 -0.55,3.87 z",
  },
};

/** providerMark is a provider's logo at the size of the text beside it, or
 *  null for a provider this build has no mark for. */
export function providerMark(name) {
  const mark = providerMarks[String(name || "").toLowerCase()];
  if (!mark) return null;
  return svg(
    "svg",
    {
      class: "provider-mark",
      viewBox: mark.viewBox,
      fill: "currentColor",
      "aria-hidden": "true",
    },
    svg("path", { d: mark.path, "fill-rule": mark.fillRule || null }),
  );
}

/** idpMarks are the identity providers' own logos, on the same terms as
 *  providerMarks. Keyed by the provider names a deployment configures in
 *  KEERA_OIDC_PROVIDERS, which is why the obvious spellings of each vendor all
 *  land on the same artwork: the name is the operator's to choose, and a button
 *  with no mark is all a wrong guess costs.
 *
 *  idpMark returns null for anything unlisted and the button falls back to a
 *  generic icon, because an unrecognisable logo is worse than none. */
export const idpMarks = {
  google: {
    viewBox: "0 0 24 24",
    path: "M12.24 10.285V14.4h6.806c-.275 1.765-2.056 5.174-6.806 5.174-4.095 0-7.439-3.389-7.439-7.574s3.344-7.574 7.439-7.574c2.33 0 3.891.989 4.785 1.849l3.254-3.138C18.189 1.186 15.479 0 12.24 0c-6.635 0-12 5.365-12 12s5.365 12 12 12c6.926 0 11.52-4.869 11.52-11.726 0-.788-.085-1.39-.189-1.989H12.24z",
  },
  microsoft: {
    viewBox: "0 0 23 23",
    path: "M0 0h11v11H0zM12 0h11v11H12zM0 12h11v11H0zM12 12h11v11H12z",
  },
};

/** idpAliases map the names an operator might reasonably pick onto the marks
 *  above. */
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

/* The reporting window every screen with a picker is looking at.
 *
 *  One list and one remembered value, shared rather than per screen, because
 *  moving between them is how these screens are read - a filter's refusals this
 *  afternoon next to the team's traffic this afternoon. A picker that reset on
 *  each arrival would make that comparison something the reader sets up twice,
 *  and a screen with a default of its own would answer a different question
 *  than the one asked a click ago. */
export const RANGES = [
  { label: "24h", since: "24h", long: "Last 24 hours" },
  { label: "7d", since: "168h", long: "Last 7 days" },
  { label: "30d", since: "720h", long: "Last 30 days" },
];

/** currentRange is the window in force. Screens read it rather than a default
 *  of their own; DEFAULT_RANGE is the one they all start on. */
export const DEFAULT_RANGE = "168h";

export function currentRange() {
  try {
    return sessionStorage.getItem("keera.range") || DEFAULT_RANGE;
  } catch {
    return DEFAULT_RANGE;
  }
}

/** rangePicker is the segmented control that sets it. ctx.reload() re-renders
 *  the screen against the new window, which is every screen's whole response to
 *  the change. */
export function rangePicker(ctx, since) {
  return h(
    "div",
    { class: "seg" },
    RANGES.map((r) =>
      h(
        "button",
        {
          "aria-pressed": String(r.since === since),
          title: r.long,
          onClick: () => {
            try {
              sessionStorage.setItem("keera.range", r.since);
            } catch {
              // A browser that refuses storage still gets the new window; it
              // just does not carry to the next screen.
            }
            ctx.reload();
          },
        },
        r.label,
      ),
    ),
  );
}

export function pill(text, tone) {
  return h("span", { class: "pill" + (tone ? " pill-" + tone : "") }, text);
}

/** rowLink is a row's name, as the way in to that row's own screen.
 *
 *  A table here has no whole-row click - a row that opens on five screens and
 *  is inert on four is a worse affordance than none - so the name carries it
 *  instead. It reads as the name it already was and behaves as the link every
 *  reader tries first. */
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

/** go is the click handler that routes inside the panel rather than reloading
 *  it, for the links that carry their own class instead of rowLink's. The href
 *  is still the real path, so the link opens in a new tab like any other. */
export function go(ctx, path) {
  return (e) => {
    e.preventDefault();
    ctx.navigate(path);
  };
}

export function stat(label, value, unit, note) {
  return h(
    "div",
    { class: "card stat" },
    h("div", { class: "stat-label" }, label),
    h(
      "div",
      { class: "stat-value" },
      value,
      unit ? h("span", { class: "unit" }, unit) : null,
    ),
    note ? h("div", { class: "stat-note" }, note) : null,
  );
}

export function empty(title, body) {
  return h("div", { class: "empty" }, h("strong", {}, title), body || null);
}

/** toggleBox is one of the table toolbar's checkboxes, wherever it is drawn. */
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

/** table renders rows in a card, with the controls a list that grows needs.
 *
 *  opts:
 *    search       (row) => string - the text the search box matches against.
 *                 Passing it is what puts the box there.
 *    searchLabel  what is being searched, for the placeholder
 *    toggles      [{ label, test, hidden, on, onChange }] - a checkbox that,
 *                 while it is checked, keeps only the rows test() accepts. A
 *                 toggle with hidden() instead reads the other way round: it
 *                 drops those rows while it is unchecked, which is how a list
 *                 hides its dead rows behind a box labelled for what it brings
 *                 back. `on` is whether it starts checked, and onChange(checked)
 *                 is for the toggle whose rows are not here to filter: the
 *                 caller asks the control plane for a different list, and gets
 *                 the box in the toolbar with every other one.
 *    sortBy       the label of the column to sort by before anybody asks
 *    sortDir      "asc" (the default) or "desc"
 *    rowClass     (row) => string - a class for that row's <tr>, for a table
 *                 whose rows are not all the same age or the same kind
 *    emptyTitle / emptyBody
 *
 *  A column with a sortKey gets a header that sorts by it. Searching and
 *  sorting both happen here rather than in the control plane: every screen
 *  using this already holds its whole list.
 *
 *  A column with a `width` gets that width instead of one measured from its
 *  contents, which is what keeps a column still on a screen that redraws the
 *  same table over a different grouping.
 *
 *  There is no whole-row click. Every screen with a row worth opening has the
 *  button that opens it, and a row that is clickable on two screens and inert
 *  on four is a worse affordance than none.
 */
export function table(columns, rows, opts = {}) {
  const toggles = opts.toggles || [];

  if (!rows.length) {
    const card = h(
      "div",
      { class: "card" },
      empty(opts.emptyTitle || "Nothing here yet", opts.emptyBody),
    );
    // A toggle that fetches its own list is the one control still worth having
    // over an empty table, because it is what brings rows back to it. A toggle
    // that only filters these rows, and the search box, have nothing to work on.
    const fetches = toggles.filter((t) => t.onChange);
    if (!fetches.length) return card;
    return h(
      "div",
      {},
      h(
        "div",
        { class: "toolbar" },
        fetches.map((t) => toggleBox(t, !!t.on, (v) => t.onChange(v))),
        h("div", { style: { flex: 1 } }),
      ),
      card,
    );
  }

  const view = {
    q: "",
    on: toggles.map((t) => !!t.on),
    sort: columns.find((c) => c.sortKey && c.label === opts.sortBy) || null,
    dir: opts.sortDir === "desc" ? -1 : 1,
  };

  const body = h("tbody");
  const count = h("span", {
    class: "faint nowrap",
    style: { fontSize: "11.5px" },
  });
  const sortable = new Map();

  const heads = columns.map((c) => {
    const attrs = {
      class: cellClass(c),
      style: c.width ? { width: c.width } : null,
    };
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
    // How much of the list is being looked at, but only while that is not all
    // of it - a count against itself is noise on every screen that has one.
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
  if (opts.search || toggles.length) {
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
      toggles.map((t, i) =>
        toggleBox(t, view.on[i], (v) => {
          view.on[i] = v;
          draw();
          if (t.onChange) t.onChange(v);
        }),
      ),
      h("div", { style: { flex: 1 } }),
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
  // The table carries its own redraw, for the one caller whose rows keep
  // arriving after it is already on the screen: the request log pushes a row
  // onto the array it was given and calls this. Going back through table()
  // instead would build a second table and throw away what the reader had done
  // to the first - the search they typed, the column they sorted by.
  el.redraw = draw;
  return el;
}

function cellClass(c) {
  return (
    [c.num && "num", c.shrink && "shrink"].filter(Boolean).join(" ") || null
  );
}

/** order sorts by one column's key, putting the rows that have no value at the
 *  bottom whichever way the column is pointing. A key that has never been used
 *  is not the oldest one - it is the one the question does not apply to, and
 *  reversing the sort should not march it to the top. */
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

/** modal opens a dialog. Returns a close function; resolves nothing.
 *
 *  The actions live in the footer, outside the body's form - so the form's own
 *  submit is wired to the primary action here. Otherwise typing a name and
 *  pressing Enter, which is what everybody does, would do nothing at all. */
export function modal({ title, subtitle, body, actions, wide }) {
  const overlay = h("div", { class: "overlay" });
  // Focus goes back where it came from. A dialog that drops a keyboard user at
  // the top of the document on close has taken their place in the page away.
  const opener = document.activeElement;
  const close = () => {
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

  // Enter anywhere in the body's form runs the primary action, whatever that
  // dialog's primary action happens to be.
  const form = card.querySelector("form");
  const primary = card.querySelector(
    ".modal-foot .btn-primary, .modal-foot .btn-danger",
  );
  if (form && primary) {
    form.addEventListener("submit", (e) => {
      e.preventDefault();
      if (!primary.disabled) primary.click();
    });
  }

  // The first field somebody can actually type in: a dialog that hides a
  // section, or disables its fields, must not put the cursor in one of those.
  const first = [
    ...card.querySelectorAll(
      "input:not([type=hidden]), select, textarea, button.btn-primary",
    ),
  ].find((el) => !el.disabled && el.offsetParent !== null);
  if (first) first.focus();
  return close;
}

/** trapTab keeps Tab inside the dialog, which is what "modal" means to anyone
 *  who is not using a mouse. */
function trapTab(card, e) {
  const items = [
    ...card.querySelectorAll(
      "a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), " +
        "textarea:not([disabled]), [tabindex]:not([tabindex='-1'])",
    ),
  ].filter((el) => el.offsetParent !== null || el === document.activeElement);
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

/** confirm asks before something irreversible.
 *
 *  A refused or failed action is shown in the dialog rather than swallowed: the
 *  whole point of this dialog is that the thing behind it cannot be undone, so
 *  "it did not happen" has to be as visible as "it did". */
export function confirm({
  title,
  body,
  confirmLabel = "Confirm",
  danger,
  onConfirm,
}) {
  const err = h("div");
  modal({
    title,
    body: h("div", {}, err, h("div", { class: "muted" }, body)),
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Cancel"),
      h(
        "button",
        {
          class: "btn " + (danger ? "btn-danger" : "btn-primary"),
          onClick: async (e) => {
            const button = e.currentTarget;
            button.disabled = true;
            showError(err, "");
            try {
              await onConfirm();
              close();
            } catch (ex) {
              showError(err, (ex && ex.message) || "That did not work.");
            } finally {
              button.disabled = false;
            }
          },
        },
        confirmLabel,
      ),
    ],
  });
}

let toastHost;

export function toast(message, tone = "") {
  if (!toastHost) {
    // Announced, because "Key revoked" and every error toast are otherwise
    // invisible to anyone using a screen reader.
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

/** field renders a labelled control and returns { el, input }. */
/** showError puts a failure where the person who caused it is looking.
 *
 *  Every dialog renders its errors into a host element at the top of the body,
 *  which is right until the body scrolls - and .modal-body does, at 88vh,
 *  while the buttons live in a footer that does not. A tall dialog then fails
 *  silently: the message is written above the fold of a container the reader is
 *  at the bottom of, the button re-enables, and nothing appears to have
 *  happened.
 *
 *  So setting the message also brings it into view. Passing no message clears
 *  the host, which is what a retry should do before it starts. */
export function showError(host, message) {
  if (!message) {
    replace(host);
    return;
  }
  replace(host, h("div", { class: "banner banner-bad" }, message));
  // "nearest" so a dialog that already shows the banner does not jump, and
  // guarded because jsdom and very old browsers have neither.
  if (host.scrollIntoView) {
    host.scrollIntoView({ block: "nearest" });
  }
}

/* aliasProblem is the alias rule, checked here so that a name the control
 *  plane cannot accept is refused where it was typed.
 *
 *  It is not only politeness. An alias is a path segment: every write addresses
 *  its entity by name, so an empty one does not address anything at all, and
 *  the request lands on the collection route - which answers a write with 405
 *  and no message. That is the one bad name the control plane cannot explain,
 *  because it never reaches the handler that knows what was meant.
 *
 *  The shape mirrors policy.validName: lowercase letters, digits, and hyphens
 *  that neither open nor close the name. Returns "" when there is no problem. */
export function aliasProblem(alias) {
  if (!alias) return "An alias is required. It is the name clients use.";
  if (alias.length > 64) return "An alias is at most 64 characters.";
  if (!/^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(alias)) {
    return (
      "Use only lowercase letters, digits and hyphens. It cannot start or " +
      "end with a hyphen."
    );
  }
  return "";
}

export function field(label, input, hint) {
  return h(
    "div",
    { class: "field" },
    h("label", { for: input.id || null }, label),
    input,
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
