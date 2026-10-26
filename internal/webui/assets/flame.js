// Where one request's time went, drawn as a waterfall: a filter running a
// second model, a router deciding, a fallback waiting out a timeout, or the
// model itself.

import { h, ms } from "./ui.js";

// Each step in plain words, and the group its time counts to. The groups make
// the summary: is it the gateway, the guardrails, or the model?
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
      "Adding the guardrail's standing system prompt, " +
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

// In summary order. A bar takes its group's colour.
const GROUPS = [
  { key: "client", label: "Client" },
  { key: "gateway", label: "Gateway" },
  { key: "guardrails", label: "Guardrails" },
  { key: "model", label: "Model" },
];

/** micros writes a duration given in microseconds. Steps range from 40µs to
 *  40s, so the unit follows the number. */
function micros(v) {
  const n = Math.max(0, Math.round(v || 0));
  if (n < 1000) return n + " µs";
  if (n < 10000) return (n / 1000).toFixed(1) + " ms";
  if (n < 1000000) return Math.round(n / 1000) + " ms";
  // As ms() writes seconds, to match the total above.
  return ms(n / 1000);
}

// Three decimals is a tenth of a pixel.
function pct(fraction) {
  return (Math.max(0, Math.min(1, fraction)) * 100).toFixed(3) + "%";
}

function began(what, at, took) {
  return at > 0
    ? `${what}: ${micros(took)}, ${micros(at)} into the request`
    : `${what}: ${micros(took)}, at the very start`;
}

// The notes that mean a step failed: on a slow request they are usually the
// cause. A filter that refused is not among them: that is the guardrail
// working.
const WENT_WRONG = ["unreachable", "could not run", "could not choose"];

function groupOf(span) {
  const step = STEPS[span.name];
  return step ? step.group : "gateway";
}

function tone(span) {
  return wrong(span.note) ? "bad" : groupOf(span);
}

function wrong(note) {
  if (!note) return false;
  if (WENT_WRONG.some((w) => note.startsWith(w))) return true;
  const status = Number(note.replace("answered ", ""));
  return Number.isFinite(status) && status >= 400;
}

function name(span) {
  const step = STEPS[span.name];
  const label = step ? step.label : span.name;
  return span.of ? label + " " + span.of : label;
}

/** flameChart draws the steps of one request, or nothing when there are
 *  none. The bars are scaled to latencyMS, the request's total, so the gaps
 *  between steps are real. */
export function flameChart(spans, latencyMS) {
  if (!spans || spans.length === 0) return null;

  const steps = spans.slice().sort((a, b) => (a.at_us || 0) - (b.at_us || 0));
  const last = steps.reduce(
    (end, s) => Math.max(end, (s.at_us || 0) + (s.for_us || 0)),
    0,
  );
  // The longer of the two, so rounding cannot push the last bar off its
  // track.
  const total = Math.max(last, Math.round((latencyMS || 0) * 1000), 1);

  const spent = {};
  for (const s of steps) {
    const g = groupOf(s);
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
              // A step that took no time still happened, and on a refusal is
              // often the one looked for, so it keeps a sliver.
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
