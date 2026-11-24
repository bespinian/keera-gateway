// Try sends your own prompts through a filter or a router's instruction
// steps. For each it shows the answer, the model's reason, and the whole
// prompt the model was sent. A check says whether a filter or step works at
// all; a try is for a prompt that went wrong.

import { api } from "../api.js";
import { oneLine } from "../status.js";
import {
  h,
  modal,
  pill,
  banner,
  field,
  icons,
  iconButton,
  meter,
  pct,
} from "../ui.js";

// The control plane takes at most this many at once.
const MAX_PROMPTS = 10;

/** tryFilter opens the try dialog for one filter. instruction is its
 *  instruction, or a pattern filter's rules, as the filter's screen shows
 *  them: a try is read against it. */
export function tryFilter(ctx, f, instruction) {
  tryDialog({
    title: `Try - ${f.alias}`,
    // A pattern filter runs no model.
    model: f.mode === "pattern" ? "" : f.model,
    note: [
      instruction && instruction.text
        ? block(instruction.label, instruction.text, "try-instruction")
        : null,
      f.shadow
        ? banner(
            "warn",
            "This filter is in shadow, so nothing below would be acted on.",
          )
        : null,
    ],
    run: (prompts) => api.tryFilter(ctx.orgID, f.alias, prompts),
    render: (res) => [
      res.error ? banner("bad", res.error) : null,
      res.trials.map((t, i) => {
        const o = trialOutcome(res.mode, t.outcome);
        return trial(i, t, pill(o.label, o.tone), filterAnswer(t), t.error);
      }),
    ],
  });
}

/** tryRouter opens it for a router's instruction steps, the only ones that
 *  read a prompt, in the order the editor draws them. */
export function tryRouter(ctx, alias, routeSteps) {
  // A step's id means nothing to a reader, so several are numbered instead.
  const names = new Map(
    routeSteps.map((s, i) => [
      s.id,
      routeSteps.length > 1 ? `Instruction ${i + 1}` : "Instruction",
    ]),
  );
  const select =
    routeSteps.length > 1
      ? h(
          "select",
          { class: "select" },
          h("option", { value: "" }, "All instructions"),
          routeSteps.map((s) => h("option", { value: s.id }, names.get(s.id))),
        )
      : null;
  // Steps on different models are not named one by one.
  const models = [...new Set(routeSteps.map((s) => s.model).filter(Boolean))];
  // The one chosen below is the only one shown.
  const instructions = routeSteps.map((s) => ({
    id: s.id,
    el: block(names.get(s.id), s.prompt || "", "try-instruction"),
  }));
  if (select) {
    select.addEventListener("change", () => {
      for (const { id, el } of instructions) {
        el.style.display = !select.value || select.value === id ? "" : "none";
      }
    });
  }
  tryDialog({
    title: `Try - ${alias}`,
    model: models.length === 1 ? models[0] : "",
    note: instructions.map((i) => i.el),
    extra: select ? field("Instruction", select) : null,
    run: (prompts) =>
      api.tryRouter(ctx.orgID, alias, prompts, select ? select.value : ""),
    render: (res) =>
      inOrder(res.routes, routeSteps).map((rt) => [
        res.routes.length > 1
          ? h(
              "h3",
              { style: { margin: "0" } },
              names.get(rt.step) || "Instruction",
            )
          : null,
        rt.error ? banner("bad", rt.error) : null,
        rt.trials.map((t, i) =>
          trial(
            i,
            t,
            t.error
              ? pill("Could not decide", "bad")
              : h("span", { class: "pill pill-good mono" }, t.chosen),
            t.error ? modelAnswer(t) : t.chosen,
            t.error ? `${t.error}. So it is ${t.becomes}.` : "",
            shares(rt.destinations, t),
          ),
        ),
      ]),
  });
}

/** inOrder sorts a try's results by their steps' order in routeSteps. */
export function inOrder(routes, routeSteps) {
  const at = (rt) => routeSteps.findIndex((s) => s.id === rt.step);
  return [...routes].sort((a, b) => at(a) - at(b));
}

function tryDialog({ title, model, note, extra, run, render }) {
  const list = h("div", { class: "stack", style: { gap: "8px" } });
  const add = h(
    "button",
    { class: "btn btn-sm", type: "button", onClick: () => addBox("") },
    "Add a prompt",
  );
  const boxes = () => [...list.querySelectorAll("textarea")];
  const sync = () => {
    const n = boxes().length;
    add.disabled = n >= MAX_PROMPTS;
    // The last box stays, so there is always somewhere to type.
    for (const b of list.querySelectorAll(".try-remove")) {
      b.style.display = n > 1 ? "" : "none";
    }
  };
  const addBox = (text) => {
    const box = h("textarea", {
      class: "input",
      rows: "3",
      "aria-label": "Prompt",
      placeholder: "A prompt as a developer would send it",
    });
    box.value = text;
    // The remove button sits in the box's corner, so the box spans the
    // whole dialog.
    const row = h(
      "div",
      { class: "try-box" },
      box,
      iconButton(
        icons.close,
        "Remove this prompt",
        () => {
          row.remove();
          sync();
        },
        { quiet: true, cls: "try-remove", type: "button" },
      ),
    );
    list.append(row);
    sync();
    box.focus();
  };
  addBox("");

  const results = h("div", {
    class: "stack",
    style: { gap: "16px", marginTop: "18px" },
  });

  const runButton = h(
    "button",
    {
      class: "btn btn-primary",
      onClick: async () => {
        const prompts = boxes()
          .map((b) => b.value)
          .filter((p) => p.trim());
        if (!prompts.length) {
          const first = boxes()[0];
          if (first) first.focus();
          return;
        }
        runButton.disabled = true;
        runButton.textContent = "Running...";
        results.replaceChildren(
          h("div", { class: "muted" }, "Waiting for the model..."),
        );
        try {
          const res = await run(prompts);
          results.replaceChildren(...[render(res)].flat(4).filter(Boolean));
        } catch (ex) {
          results.replaceChildren(banner("bad", ex.message));
        } finally {
          runButton.disabled = false;
          runButton.textContent = "Run";
          results.scrollIntoView({ block: "start", behavior: "smooth" });
        }
      },
    },
    "Run",
  );

  modal({
    wide: true,
    title,
    subtitle: model
      ? `Each prompt is sent as a request of its own to ${model}. Nothing is stored.`
      : "Each prompt is sent as a request of its own. Nothing is stored.",
    body: h(
      "form",
      {},
      note,
      extra,
      field(
        "Prompts",
        list,
        null,
        h("div", { style: { marginTop: "8px" } }, add),
      ),
      results,
    ),
    actions: (close) => [
      h("button", { class: "btn", type: "button", onClick: close }, "Close"),
      runButton,
    ],
  });
}

/** trialOutcome names what a filter would do with one prompt. */
export function trialOutcome(mode, outcome) {
  switch (outcome) {
    case "refuse":
      return { label: "Refused", tone: "warn" };
    case "rewrite":
      return { label: "Rewritten", tone: "accent" };
    case "error":
      return { label: "Could not run", tone: "bad" };
  }
  return { label: mode === "gate" ? "Allowed" : "Unchanged", tone: "good" };
}

/** filterAnswer is what the filter's model answered, or for a pattern
 *  filter, which runs none, what the rules did. */
function filterAnswer(t) {
  const asked = decision(t.calls);
  if (asked && asked.answer) return asked.answer;
  if (t.error) return modelAnswer(t);
  if (t.outcome === "refuse") return t.reason || "Refused, with no reason.";
  if (t.after) return t.after;
  return (t.hits || []).length
    ? t.hits.map((hit) => `${hit.rule} matched`).join("\n")
    : "No rule matched.";
}

/** modelAnswer is what the deciding model wrote, or when it wrote nothing,
 *  what came back instead, such as a backend's error page. */
export function modelAnswer(t) {
  const asked = decision(t.calls);
  if (!asked) return "No model was asked.";
  if (!asked.sent) return "Not sent.";
  if (asked.answer) return asked.answer;
  if (asked.response) {
    const status = asked.status ? `HTTP ${asked.status}\n` : "";
    return status + pretty(asked.response);
  }
  return asked.error || "No answer.";
}

/** decision is the last call that decided, as opposed to the one asking why. */
export function decision(calls) {
  return (calls || []).filter((c) => c.purpose !== "explain").pop() || null;
}

/** promptSent is the whole prompt a call carried, as one text: each
 *  message under its role, or a decision model's request, which has none. */
export function promptSent(call) {
  const msgs = call.messages || [];
  if (!msgs.length) return pretty(call.request);
  return msgs.map((m) => `[${m.role}]\n${m.content}`).join("\n\n");
}

/** trial is one prompt's card. failed is why it could not run or decide,
 *  shown above the answer: an answer that could not be read says nothing on
 *  its own. */
function trial(i, t, verdict, answer, failed, extra) {
  const asked = decision(t.calls);
  return h(
    "div",
    { class: "card" },
    h(
      "div",
      { class: "card-head" },
      h("h2", {}, `Prompt ${i + 1}`),
      h("span", { class: "muted try-prompt" }, firstLine(t.prompt)),
      h("div", { class: "spacer" }),
      verdict,
    ),
    h(
      "div",
      { class: "card-body" },
      failed ? banner("bad", failed) : null,
      block("Answer", answer),
      extra,
      why(asked, t),
      asked
        ? folded(
            asked.sent ? "Prompt sent" : "Prompt (not sent)",
            promptSent(asked),
          )
        : null,
    ),
  );
}

/** shares is each destination's share of the decision, when it was read
 *  from the probabilities: a close call shows as one. */
function shares(destinations, t) {
  const list = destinations || [];
  if (t.error || !t.shares || t.shares.length !== list.length) return null;
  return h(
    "div",
    { class: "field" },
    h("label", {}, "Certainty"),
    h(
      "div",
      { class: "try-shares" },
      list.map((alias, i) => [
        h("span", { class: alias === t.chosen ? "mono" : "mono muted" }, alias),
        meter(t.shares[i], alias === t.chosen ? "" : "muted"),
        h("span", { class: "muted num" }, pct(t.shares[i], 1)),
      ]),
    ),
  );
}

/** why is what the model thought before answering, or when it showed
 *  nothing, what it said when asked afterwards. */
function why(asked, t) {
  if (asked && asked.reasoning) return block("Reasoning", asked.reasoning);
  if (t.explanation) return block("Reasoning", t.explanation);
  if (t.explain_error) {
    return block("Reasoning", h("span", { class: "muted" }, t.explain_error));
  }
  return null;
}

/** block is a labelled text. The instruction a try is read against takes
 *  the class "try-instruction", so a long one scrolls. */
function block(label, text, cls) {
  return h(
    "div",
    { class: "field" },
    h("label", {}, label),
    h("div", { class: cls ? `prompt-text ${cls}` : "prompt-text" }, text),
  );
}

/** folded is a block that starts closed: the prompt sent is long, and the
 *  answer and the reason are what a try is usually for. */
function folded(label, text) {
  return h(
    "details",
    { class: "field try-sent" },
    h("summary", {}, label),
    h("div", { class: "prompt-text" }, text),
  );
}

/** firstLine is how a prompt is named in its card's head. */
export function firstLine(text) {
  return oneLine((text || "").trim().split("\n")[0], 80);
}

/** pretty indents JSON, given as text or a value, and leaves anything else
 *  as it is. */
export function pretty(v) {
  if (v === undefined || v === null) return "";
  if (typeof v !== "string") return JSON.stringify(v, null, 2);
  try {
    return JSON.stringify(JSON.parse(v), null, 2);
  } catch {
    return v;
  }
}
