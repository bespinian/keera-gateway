// Where one request's time went, drawn as a waterfall.
//
// Everywhere else a request is a total. This shows which step took the time:
// a filter running a second model, a router deciding, a fallback waiting out a
// timeout, or the model itself.
//
// The bars are drawn against the request's own latency, so the gaps between
// them are real. Closing the gaps would hide what the chart is for.

import { h, ms } from "./ui.js";

// Each step in plain words, and which of the four groups its time counts to.
// The groups make the summary above the chart, which answers the reader's
// first question: is it the gateway, the guardrails, or the model?
const STEPS = {
  auth: {
    label: "Authenticating",
    group: "gateway",
    why: "Resolving the API key against the control plane.",
  },
  receive: {
    label: "Receiving",
    group: "client",
    why:
      "The request arriving from the client. This is the network between the " +
      "developer and the gateway, not work the gateway did.",
  },
  admit: {
    label: "Admitting",
    group: "gateway",
    why:
      "Reading the request, finding the model it named, and checking the rate " +
      "limits and budgets on the key.",
  },
  route: {
    label: "Routing",
    group: "guardrails",
    why:
      "A router reading the request and choosing which model should answer it. " +
      "That is a generation on another model, and the request waited for it.",
  },
  filter: {
    label: "Filtering",
    group: "guardrails",
    why:
      "A filter reading the request before it was forwarded. That is a " +
      "generation on another model, and the request waited for it.",
  },
  prepare: {
    label: "Preparing",
    group: "gateway",
    why:
      "Adding the guardrail's standing system prompt and the output ceiling, " +
      "and encoding the request for the model.",
  },
  upstream: {
    label: "Asking",
    group: "model",
    why:
      "One destination being asked, up to its response line. More than one of " +
      "these means a router tried a destination that did not answer.",
  },
  wait: {
    label: "Waiting",
    group: "model",
    why:
      "The model deciding what to say, between the response headers and the " +
      "first token. A long one is usually a queue or a cold start.",
  },
  stream: {
    label: "Streaming",
    group: "model",
    why: "The tokens arriving, for as long as the model kept generating them.",
  },
  respond: {
    label: "Answering",
    group: "model",
    why:
      "The answer arriving and being translated into the shape this client " +
      "speaks. It is one step because nothing is observable until the whole " +
      "body is here.",
  },
};

// The groups, in summary order. A group's steps are drawn in its colour, so a
// bar matches its figure in the summary.
const GROUPS = [
  { key: "client", label: "Client" },
  { key: "gateway", label: "Gateway" },
  { key: "guardrails", label: "Guardrails" },
  { key: "model", label: "Model" },
];

/** micros writes a duration recorded in microseconds.
 *
 *  Steps range from forty microseconds to forty seconds, so the unit follows
 *  the number. In milliseconds alone half the bars would round to zero. */
function micros(v) {
  const n = Math.max(0, Math.round(v || 0));
  if (n < 1000) return n + " µs";
  if (n < 10000) return (n / 1000).toFixed(1) + " ms";
  if (n < 1000000) return Math.round(n / 1000) + " ms";
  // Seconds as ms() writes them, so a step reads like the total above it.
  return ms(n / 1000);
}

// pct writes one bar's geometry. Three decimals is a tenth of a pixel, and
// keeps the inline style readable in the inspector.
function pct(fraction) {
  return (Math.max(0, Math.min(1, fraction)) * 100).toFixed(3) + "%";
}

// began is a bar's tooltip: how long the step took and when it started.
function began(what, at, took) {
  return at > 0
    ? `${what}: ${micros(took)}, ${micros(at)} into the request`
    : `${what}: ${micros(took)}, at the very start`;
}

// The notes that mean a step failed: a destination that could not be reached
// or failed, a filter whose model is gone, a router that could not decide.
// They are drawn as failures, because on a slow request they are usually the
// cause.
//
// A filter that refused is not among them: that is the guardrail working.
const WENT_WRONG = ["unreachable", "could not run", "could not choose"];

function tone(span) {
  const step = STEPS[span.name];
  if (wrong(span.note)) return "bad";
  return step ? step.group : "gateway";
}

function wrong(note) {
  if (!note) return false;
  if (WENT_WRONG.some((w) => note.startsWith(w))) return true;
  const status = Number(note.replace("answered ", ""));
  return Number.isFinite(status) && status >= 400;
}

// name is a bar's label: the step, plus what it was about, such as the model
// asked or the filter that ran.
function name(span) {
  const step = STEPS[span.name];
  const label = step ? step.label : span.name;
  return span.of ? label + " " + span.of : label;
}

/** flameChart draws the steps of one request, or nothing when there are none,
 *  as for a request recorded before steps were kept.
 *
 *  latencyMS is the request's total. The bars are scaled to it, not to the
 *  last step's end, so the chart can show steps that do not cover the whole
 *  wait. */
export function flameChart(spans, latencyMS) {
  if (!spans || spans.length === 0) return null;

  const steps = spans.slice().sort((a, b) => (a.at_us || 0) - (b.at_us || 0));
  const last = steps.reduce(
    (end, s) => Math.max(end, (s.at_us || 0) + (s.for_us || 0)),
    0,
  );
  // The longer of the two, so rounding between the latency in milliseconds
  // and the steps in microseconds cannot push the last bar off its track.
  const total = Math.max(last, Math.round((latencyMS || 0) * 1000), 1);

  // Time per group, for the summary line.
  const spent = {};
  for (const s of steps) {
    const step = STEPS[s.name];
    const g = step ? step.group : "gateway";
    spent[g] = (spent[g] || 0) + (s.for_us || 0);
  }

  return h(
    "div",
    { class: "stack", style: { gap: "8px" } },
    h(
      "div",
      { class: "flame-legend" },
      GROUPS.filter((g) => spent[g.key]).map((g) =>
        h(
          "span",
          { class: "flame-key" },
          h("span", { class: "flame-swatch " + g.key }),
          h("span", { class: "muted" }, g.label),
          h("span", { class: "mono" }, micros(spent[g.key])),
        ),
      ),
    ),
    h(
      "div",
      { class: "flame" },
      steps.map((s) => {
        const at = s.at_us || 0;
        const took = s.for_us || 0;
        const step = STEPS[s.name];
        const why = step ? step.why : "";
        return h(
          "div",
          { class: "flame-row" },
          h(
            "div",
            { class: "flame-name", title: why },
            h("span", {}, name(s)),
            s.note ? h("span", { class: "faint" }, s.note) : null,
          ),
          h(
            "div",
            { class: "flame-track" },
            h("div", {
              class: "flame-bar " + tone(s),
              // A step that took no measurable time still happened, and on a
              // refusal it is often the one being looked for, so it keeps a
              // sliver.
              style: {
                left: pct(at / total),
                width: pct(Math.max(took / total, 0.005)),
              },
              title: began(name(s), at, took),
            }),
          ),
          h("div", { class: "flame-took mono" }, micros(took)),
        );
      }),
    ),
  );
}
