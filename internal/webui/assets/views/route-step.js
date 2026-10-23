// A router's route step: its modes, its form, and the check of what it would
// do.
//
// A broken route step still returns good answers, just from the wrong model.
// Only the split between its destinations shows it.

import {
  h,
  table,
  modal,
  pill,
  money,
  ms,
  icons,
  dash,
  field,
  modeTiles,
  banner,
} from "../ui.js";
import { facts } from "./detail.js";

// MODES feeds both the mode tiles and the rows that name a mode.
const MODES = [
  {
    value: "instruction",
    icon: icons.instruction,
    name: "Instruction",
    what: "A model reads each request and chooses where it goes.",
  },
  {
    value: "fallback",
    icon: icons.fallback,
    name: "Fallback",
    what: "Try the destinations in order until one answers.",
  },
  {
    value: "latency",
    icon: icons.latency,
    name: "Latency",
    what: "Try the one with the fastest recent first token.",
  },
  {
    value: "least-busy",
    icon: icons.leastBusy,
    name: "Least busy",
    what: "Try the one with the fewest requests in flight.",
  },
  {
    value: "size",
    icon: icons.size,
    name: "Size",
    what: "Choose by how many tokens the request has.",
  },
];

/** modeOf is a step's mode, instruction when left out. */
export function modeOf(step) {
  return step.mode || "instruction";
}

export function modeName(mode) {
  return (MODES.find((m) => m.value === mode) || { name: mode }).name;
}

/** decides reports whether a step chooses by reading the request. With one
 *  destination there is nothing to choose, so the gateway does not ask. */
export function decides(step) {
  return modeOf(step) === "instruction" && (step.destinations || []).length > 1;
}

// measures reports whether the gateway works out this step's order. That
// order is a reading of recent minutes on one replica, so screens must not
// show it numbered.
function measures(mode) {
  return mode === "latency" || mode === "least-busy";
}

/** ordersBy says, as a clause, how a step that reads nothing orders its
 *  destinations. */
function ordersBy(mode) {
  switch (mode) {
    case "latency":
      return "recent time to first token, fastest first";
    case "least-busy":
      return "requests in flight, least busy first";
    case "size":
      return "request size, against each one's ceiling";
    default:
      return "the order they are written in";
  }
}

/** tokens renders a ceiling as it is typed, such as '32k'. */
export function tokens(n) {
  return n >= 1000 && n % 1000 === 0 ? n / 1000 + "k" : String(n);
}

// A decision model only answers choices, so it can decide but never be a
// destination. A subscription model only answers its caller's own sign-in.
function canDecide(m) {
  return (m.kind === "chat" || m.kind === "decision") && !m.subscription;
}

/** routeFields builds the form for a route step. read returns what it sets
 *  or throws what is missing. The mode comes first: it decides whether the
 *  list is a menu, an order, or a set of size bands. */
export function routeFields(models, step) {
  const readers = models.filter(canDecide);
  const chatModels = readers.filter((m) => m.kind === "chat");
  const creating = !step.mode && !(step.destinations || []).length;

  const model = h(
    "select",
    { class: "select" },
    h(
      "option",
      { value: "" },
      readers.length ? "Choose a model" : "No chat model exists",
    ),
    readers.map((m) =>
      h(
        "option",
        { value: m.alias, selected: m.alias === step.model },
        m.alias + (m.kind === "decision" ? " - decision model" : ""),
      ),
    ),
  );
  const prompt = h("textarea", { class: "input", rows: "8" });
  // A decision model answers a short question rather than following an
  // instruction, so it gets its own example.
  const exampleFor = (alias) =>
    readers.find((m) => m.alias === alias)?.kind === "decision"
      ? DECISION_EXAMPLE
      : PROMPT_EXAMPLE;
  prompt.value = step.prompt || exampleFor(model.value);
  model.addEventListener("change", () => {
    if (prompt.value === PROMPT_EXAMPLE || prompt.value === DECISION_EXAMPLE) {
      prompt.value = exampleFor(model.value);
    }
  });

  let chosen = creating ? null : modeOf(step);
  const reads = () => chosen === "instruction";
  const bySize = () => chosen === "size";
  // On measured modes the order only breaks ties.
  const ranks = () => measures(chosen);

  // Changing the mode hides unused fields rather than clearing them, so trying
  // another tile loses nothing.
  const picker = modeTiles(MODES, chosen, (value) => {
    chosen = value;
    syncMode();
  });
  const modeField = h(
    "div",
    { class: "field" },
    h("label", {}, "How it chooses"),
    picker.tiles,
    picker.chosenRow,
  );

  // Checkboxes over the catalogue, so a step cannot name an unknown model. An
  // array, not a set: on a fallback step the order is the configuration.
  const picked = [...(step.destinations || [])];
  const fallback = h("select", { class: "select" });
  const syncFallback = () => {
    const previous = fallback.value || step.fallback || "";
    fallback.replaceChildren(
      h("option", { value: "" }, "Refuse the request"),
      ...picked.map((alias) =>
        h(
          "option",
          { value: alias, selected: alias === previous },
          "Send it to " + alias + " without a decision",
        ),
      ),
    );
    fallback.value = picked.includes(previous) ? previous : "";
  };

  // Ticked in the wrong order, a fallback step serves everything from its
  // second choice.
  const order = h("div", { class: "stack", style: { gap: "4px" } });
  const renderOrder = () => {
    if (picked.length === 0) {
      order.replaceChildren(
        h("div", { class: "empty" }, "Nothing chosen yet."),
      );
      return;
    }
    const move = (i, by) => {
      const to = i + by;
      [picked[i], picked[to]] = [picked[to], picked[i]];
      renderOrder();
      syncFallback();
    };
    order.replaceChildren(
      ...picked.map((alias, i) =>
        h(
          "div",
          { class: "row-tight" },
          h("span", { class: "faint" }, i + 1 + "."),
          h("span", { class: "mono" }, alias),
          i === 0
            ? pill("Tried first", "good")
            : h("span", { class: "faint" }, "if the ones above fail"),
          h("div", { class: "spacer" }),
          h(
            "button",
            {
              class: "btn btn-sm",
              type: "button",
              title: "Try this one earlier",
              disabled: i === 0,
              onClick: () => move(i, -1),
            },
            "↑",
          ),
          h(
            "button",
            {
              class: "btn btn-sm",
              type: "button",
              title: "Try this one later",
              disabled: i === picked.length - 1,
              onClick: () => move(i, 1),
            },
            "↓",
          ),
        ),
      ),
    );
  };

  // Kept here, not read off the inputs, so re-ticking a destination keeps its
  // ceiling.
  const ceilings = { ...(step.ceilings || {}) };
  // Boxes holding something that is not a ceiling. Applying refuses them, as
  // `keera router` does, rather than reading them as "no ceiling".
  const badCeilings = new Set();
  const ceilingList = h("div", { class: "stack", style: { gap: "4px" } });
  const ceilingHint = h("div", { class: "hint" });
  // Notes update on their own: rebuilding the rows would take the cursor out
  // of the box being typed into.
  const notes = new Map();
  const syncCeilingNotes = () => {
    for (const [alias, note] of notes) {
      note.textContent = ceilings[alias]
        ? "tokens at most"
        : "takes what is larger than every ceiling";
    }
    // The control plane needs exactly one destination without a ceiling;
    // say so before it refuses.
    const open = picked.filter((alias) => !ceilings[alias]);
    ceilingHint.textContent =
      open.length === 1
        ? "Each request goes to the smallest ceiling it fits under; anything " +
          `larger goes to ${open[0]}.`
        : open.length === 0
          ? "Leave one box empty, usually for the largest model, to take " +
            "what fits under no ceiling."
          : `${open.join(" and ")} have no ceiling. Exactly one ` +
            "destination may be left without one.";
  };
  const renderCeilings = () => {
    notes.clear();
    if (picked.length === 0) {
      ceilingList.replaceChildren(
        h("div", { class: "empty" }, "Nothing chosen yet."),
      );
      ceilingHint.textContent = "";
      return;
    }
    ceilingList.replaceChildren(
      ...picked.map((alias) => {
        const box = h("input", {
          class: "input mono",
          style: { maxWidth: "120px" },
          placeholder: "no ceiling",
          value: ceilings[alias] ? tokens(ceilings[alias]) : "",
          onInput: () => {
            // The CLI's form: whole tokens, where 'k' means a thousand.
            const raw = box.value.trim().toLowerCase();
            const m = /^(\d+)(k?)$/.exec(raw);
            const n = m ? Number(m[1]) * (m[2] ? 1000 : 1) : 0;
            badCeilings.delete(alias);
            if (n > 0) {
              ceilings[alias] = n;
            } else {
              delete ceilings[alias];
              if (raw !== "") badCeilings.add(alias);
            }
            syncCeilingNotes();
          },
        });
        const note = h("span", { class: "faint" });
        notes.set(alias, note);
        return h(
          "div",
          { class: "row-tight" },
          h("span", { class: "mono", style: { minWidth: "160px" } }, alias),
          box,
          note,
        );
      }),
    );
    syncCeilingNotes();
  };
  const ceilingField = field(
    "Up to this many input tokens",
    ceilingList,
    ceilingHint,
  );

  const destinationBoxes = h(
    "div",
    { class: "stack", style: { gap: "6px" } },
    chatModels.map((m) => {
      const box = h("input", {
        type: "checkbox",
        checked: picked.includes(m.alias),
        onChange: () => {
          if (box.checked) {
            if (!picked.includes(m.alias)) picked.push(m.alias);
          } else {
            const at = picked.indexOf(m.alias);
            if (at >= 0) picked.splice(at, 1);
          }
          renderOrder();
          syncFallback();
          renderCeilings();
        },
      });
      return h(
        "label",
        { class: "row-tight", style: { alignItems: "flex-start" } },
        box,
        h(
          "div",
          {},
          h("span", { class: "mono" }, m.alias),
          // The deciding model is told this description.
          m.description
            ? h("div", { class: "hint" }, m.description)
            : h(
                "div",
                { class: "hint" },
                "No description, so the step sees only the alias. Add one " +
                  "on the model.",
              ),
        ),
      );
    }),
  );
  syncFallback();
  renderOrder();
  renderCeilings();

  // Fields only an instruction step has. Hidden, not disabled, so nobody
  // writes an instruction the step then ignores.
  const modelField = field(
    "Runs on",
    model,
    // Where this model runs can be wrong without anything here going red.
    "A small, fast model. It reads every request that reaches this step, so " +
      "keep it inside the cluster.",
  );
  const fallbackField = field(
    "When it cannot decide",
    fallback,
    // The right answer depends on the deployment, so the hint gives both.
    "Fall back if the step saves money. Refuse if it keeps prompts inside " +
      "the cluster.",
  );
  const promptField = field(
    "Instruction",
    prompt,
    "Name kinds of request, not models. The destinations and their " +
      "descriptions are added for you.",
  );
  const orderHint = h("div", { class: "hint" });
  const orderField = field("In this order", order, orderHint);
  const destinationsLabel = h("label", {}, "Chooses between");
  const destinationsHint = h("div", { class: "hint" });
  const costBanner = banner("info");

  const syncMode = () => {
    rest.hidden = chosen === null;
    if (chosen === null) return;
    for (const el of [modelField, fallbackField, promptField]) {
      el.hidden = !reads();
    }
    ceilingField.hidden = !bySize();
    // A size step's order is its ceilings.
    orderField.hidden = reads() || bySize();
    orderHint.textContent = ranks()
      ? "Only a tie-break. The gateway reorders these on every request " +
        "from what it measures."
      : "The first takes every request. The rest are used when the ones " +
        "above cannot answer.";
    destinationsLabel.textContent = reads()
      ? "Chooses between"
      : bySize()
        ? "Places between"
        : "Tries these";
    destinationsHint.textContent = reads()
      ? "With only one, every request goes there and nothing is asked."
      : bySize()
        ? "Each with the largest request it should be given."
        : "Tried until one answers. One is enough to give a model a lasting name.";
    if (bySize()) {
      costBanner.textContent =
        "Costs nothing: it counts the request's tokens without reading " +
        "them. It cannot tell a hard request from an easy one of the same " +
        "length.";
      renderCeilings();
      return;
    }
    costBanner.textContent = reads()
      ? "Every request that reaches it waits for the decision, which is " +
        "charged to the same budgets."
      : ranks()
        ? "Costs nothing itself; it ranks by what each gateway process " +
          "measures. A request pays for every destination that fails before " +
          "one answers."
        : "Costs nothing itself. A request pays for every destination that " +
          "fails before one answers.";
  };

  const rest = h(
    "div",
    {},
    modelField,
    h(
      "div",
      { class: "field" },
      destinationsLabel,
      destinationBoxes,
      destinationsHint,
    ),
    ceilingField,
    orderField,
    fallbackField,
    promptField,
    costBanner,
  );

  if (creating) syncMode();
  else picker.choose(chosen);

  return {
    el: h("div", {}, modeField, rest),
    read: () => {
      if (!chosen) throw new Error("Choose how it chooses.");
      if (!picked.length) throw new Error("Choose at least one destination.");
      const bad = bySize() ? picked.find((a) => badCeilings.has(a)) : null;
      if (bad) {
        throw new Error(
          `The ceiling on ${bad} is not a number of tokens. Write 4000 ` +
            "or 4k, or leave it empty for no ceiling.",
        );
      }
      if (reads() && !model.value) {
        throw new Error("Choose the model it runs on.");
      }
      if (reads() && !prompt.value.trim()) {
        throw new Error("Write the instruction.");
      }
      // Leftovers of another mode are dropped.
      return {
        mode: chosen,
        destinations: [...picked],
        model: reads() ? model.value : "",
        prompt: reads() ? prompt.value.trim() : "",
        fallback: reads() ? fallback.value : "",
        ceilings: bySize()
          ? Object.fromEntries(
              picked.filter((a) => ceilings[a]).map((a) => [a, ceilings[a]]),
            )
          : {},
      };
    },
  };
}

/** checkReport shows what checking a router's route steps found: each
 *  sample and the model chosen, or each destination in the order tried.
 *  There is no pass: only the reader can judge the choices. Every sample
 *  going the same way is always wrong. */
export function checkReport(check) {
  modal({
    wide: true,
    title: `Check - ${check.router}`,
    subtitle:
      check.routes.length === 1
        ? null
        : "Each route step, in the order a request may meet them.",
    body: h(
      "div",
      { class: "stack", style: { gap: "20px" } },
      check.routes.map((probe) => routeProbe(probe, check.routes.length > 1)),
    ),
  });
}

function routeProbe(probe, named) {
  const chained = probe.mode !== "instruction";
  const ranked = measures(probe.mode);
  const banded = probe.mode === "size";
  const intro = !probe.ok
    ? chained
      ? "This step has nowhere to send a request."
      : "This step could not place the samples."
    : banded
      ? "Each destination, the request sizes it takes first, and whether it " +
        "is answering. This depends only on size, so it is the full picture."
      : ranked
        ? "Each destination in the order this step would try them now, with " +
          "what it measured. The order can change from minute to minute."
        : chained
          ? "Each destination in the order this step would try them. The " +
            "first that answers serves every request."
          : "Three very different prompts. They should not all go to the " +
            "same model.";
  return h(
    "div",
    {},
    named
      ? h(
          "h3",
          { style: { margin: "0 0 8px" } },
          h("span", { class: "mono" }, probe.step),
          " - ",
          modeName(probe.mode),
        )
      : null,
    h("p", { class: "muted", style: { marginTop: 0 } }, intro),
    probe.error ? banner("bad", probe.error) : null,
    (probe.warnings || []).map((warning) => banner("warn", warning)),
    facts([
      probe.model
        ? ["Runs on", h("span", { class: "mono" }, probe.model)]
        : null,
      ranked || banded ? ["In the order of", ordersBy(probe.mode)] : null,
      [
        banded ? "Places between" : chained ? "Would try, in order" : "Offered",
        h(
          "div",
          { class: "wrap-chips" },
          (probe.destinations || []).map((alias) =>
            h("span", { class: "pill mono" }, alias),
          ),
        ),
      ],
      (probe.dropped || []).length
        ? [
            "Not offered",
            h(
              "div",
              { class: "stack" },
              probe.dropped.map((d) => h("div", { class: "muted" }, d)),
            ),
          ]
        : null,
      probe.total_ms ? ["Took", ms(probe.total_ms)] : null,
      probe.cost_micros
        ? [
            "Cost of these runs",
            money(probe.cost_micros) +
              " - each request through this step pays a share of this on top",
          ]
        : null,
    ]),
    (probe.chain || []).length
      ? h(
          "div",
          { style: { marginTop: "12px" } },
          table(
            [
              {
                label: banded ? "Placed on" : "Tries",
                shrink: true,
                cell: (hop) =>
                  h(
                    "div",
                    { class: "row-tight" },
                    h(
                      "span",
                      { class: "faint" },
                      probe.chain.indexOf(hop) + 1 + ".",
                    ),
                    h("span", { class: "mono" }, hop.alias),
                  ),
              },
              {
                label: "Answers",
                shrink: true,
                cell: (hop) =>
                  hop.answers
                    ? pill("Yes", "good")
                    : pill(hop.status ? String(hop.status) : "No", "bad"),
              },
              {
                label: "Took",
                shrink: true,
                cell: (hop) => (hop.ms ? ms(hop.ms) : dash()),
              },
              // Why a destination is where it is, when the order is measured
              // rather than written.
              ranked || banded
                ? {
                    label: banded ? "Takes" : "Reading",
                    cell: (hop) =>
                      hop.reading
                        ? h("span", { class: "muted" }, hop.reading)
                        : dash(),
                  }
                : null,
              {
                label: "Note",
                cell: (hop) =>
                  hop.note ? h("span", { class: "muted" }, hop.note) : dash(),
              },
            ].filter(Boolean),
            probe.chain,
            {},
          ),
        )
      : null,
    h(
      "div",
      { class: "stack", style: { marginTop: "12px" } },
      (probe.decisions || []).map((d) =>
        h(
          "div",
          { class: "card" },
          h(
            "div",
            { class: "card-head" },
            h("h2", {}, d.asks),
            h("div", { class: "spacer" }),
            d.error
              ? pill("Could not decide", "bad")
              : h("span", { class: "pill pill-good mono" }, d.chosen),
            // A sample decided at 35% went there by a hair. Absent without
            // backend probabilities.
            !d.error && d.confidence
              ? h(
                  "span",
                  { class: "muted", style: { marginLeft: "8px" } },
                  `${Math.round(d.confidence * 100)}% sure`,
                )
              : null,
          ),
          h(
            "div",
            { class: "card-body" },
            h(
              "div",
              { class: "mono muted", style: { whiteSpace: "pre-wrap" } },
              d.prompt,
            ),
            d.error ? banner("bad", d.error) : null,
          ),
        ),
      ),
    ),
  );
}

// PROMPT_EXAMPLE names kinds of request, not models, since the catalogue adds
// what each model is for. It says which way to err, since a small model
// deciding in one word is sometimes wrong.
const PROMPT_EXAMPLE = `Choose the model that suits this request.

Send small, self-contained requests to the cheapest capable model: a rename, a \
syntax question, a short edit to one file, a commit message.

Send requests needing design, several steps at once, or judgement about a \
system to a model that can reason.

Send anything containing a client's name, an account number, personal details \
or a credential to a model inside the cluster, whatever else it is asking for. \
That takes priority.

If you are unsure, choose the more capable model rather than the cheaper one.`;

// A decision model is offered each destination by its description alone. See
// docs/gateway.md#decision-models.
const DECISION_EXAMPLE = "Which model should answer this request?";
