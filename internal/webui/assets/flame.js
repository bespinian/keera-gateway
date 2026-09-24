// Where one request's time went, drawn as a waterfall.
//
// Every other reading of a request is a total. The row says it took nine
// seconds and the dashboard says the model's median is two; neither says which
// of the things this gateway does was the nine seconds. A filter chain running
// a second model, a router spending four hundred milliseconds deciding, a
// fallback waiting out a dial timeout - or the model, in which case there is
// nothing here to fix.
//
// The bars are drawn against the request's own latency, so the gaps between
// them are real: a chart that closed the gaps would hide the one thing it was
// opened for.

import { h } from "./ui.js";

// What each step is, in the words somebody reading their own request would use,
// and which of the four things a request's time can go on it belongs to.
//
// The group is what the summary above the chart is built from, and it is the
// division a reader actually arrives with: is this us, is this the guardrails
// we turned on, or is this the model. Nothing else about the chart matters if
// that question needs three screens to answer.
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

// The four groups, in the order they are summarised. Each has the colour its
// steps are drawn in, so a bar and the figure above it are the same thing.
const GROUPS = [
  { key: "client", label: "Client" },
  { key: "gateway", label: "Gateway" },
  { key: "guardrails", label: "Guardrails" },
  { key: "model", label: "Model" },
];

/** micros writes a duration recorded in microseconds.
 *
 *  The steps here span five orders of magnitude - an admission is forty
 *  microseconds and a completion is forty seconds - so the unit changes with
 *  the number rather than the whole chart being written in one of them. A chart
 *  in milliseconds rounds half its bars to zero; one in microseconds asks its
 *  reader to count digits to find out whether a model took two seconds. */
export function micros(v) {
  const n = Math.max(0, Math.round(v || 0));
  if (n < 1000) return n + " µs";
  if (n < 10000) return (n / 1000).toFixed(1) + " ms";
  if (n < 1000000) return Math.round(n / 1000) + " ms";
  return (n / 1000000).toFixed(n < 10000000 ? 2 : 1) + " s";
}

// pct writes one bar's geometry. Three decimals is a tenth of a pixel on any
// screen this is read on, and keeps the inline style a person can read in the
// inspector.
function pct(fraction) {
  return (Math.max(0, Math.min(1, fraction)) * 100).toFixed(3) + "%";
}

// began is a bar's own sentence, for the pointer: how long the step took and
// how far into the request it started.
function began(what, at, took) {
  return at > 0
    ? `${what}: ${micros(took)}, ${micros(at)} into the request`
    : `${what}: ${micros(took)}, at the very start`;
}

// The notes that mean a step did not do what it was there to do: a destination
// that could not be reached or failed, a filter whose own model has gone, a
// router that could not decide. They are drawn as failures wherever they sit,
// because on a slow request those bars are usually the whole story.
//
// A filter that refused is not among them: that is the guardrail working, and
// colouring it as a fault would be the panel disagreeing with it.
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

// name is what one bar is called: the step, and what it was about where that is
// not the request itself - the model that was asked, the filter that ran.
function name(span) {
  const step = STEPS[span.name];
  const label = step ? step.label : span.name;
  return span.of ? label + " " + span.of : label;
}

/** flameChart draws the steps of one request, or nothing when there are none.
 *
 *  A request recorded before this gateway kept them has no steps, and so does
 *  one served by a replica that has not been restarted. Neither is worth a
 *  paragraph in a dialog somebody opened to read a different number, so the
 *  section is simply absent.
 *
 *  latencyMS is the request's own total, and is what the bars are scaled
 *  against rather than the last step's end: a chart scaled to its own contents
 *  can never show that they do not account for the whole wait. */
export function flameChart(spans, latencyMS) {
  if (!spans || spans.length === 0) return null;

  const steps = spans.slice().sort((a, b) => (a.at_us || 0) - (b.at_us || 0));
  const last = steps.reduce(
    (end, s) => Math.max(end, (s.at_us || 0) + (s.for_us || 0)),
    0,
  );
  // The scale is the longer of the two, so a rounding difference between the
  // latency the row reports in milliseconds and the steps recorded in
  // microseconds cannot push the final bar off the end of its track.
  const total = Math.max(last, Math.round((latencyMS || 0) * 1000), 1);

  // What each group cost between them, which is the line most readers stop at.
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
              // refusal it is often the one being looked for - so it keeps a
              // sliver rather than collapsing to nothing.
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
