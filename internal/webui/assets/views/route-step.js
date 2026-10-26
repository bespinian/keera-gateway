// A router's route step: its modes, its form, and its check.
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
  icon,
  icons,
  iconButton,
  dash,
  field,
  modeTiles,
  banner,
} from "../ui.js";
import { facts } from "./detail.js";

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
  {
    value: "header",
    icon: icons.header,
    name: "Header",
    what: "The client's app says where, with a request header.",
  },
];

/** modeOf defaults to instruction. */
export function modeOf(step) {
  return step.mode || "instruction";
}

export function modeName(mode) {
  return (MODES.find((m) => m.value === mode) || { name: mode }).name;
}

/** decides reports whether a step chooses by reading the request. With one
 *  destination there is nothing to choose. */
export function decides(step) {
  return modeOf(step) === "instruction" && (step.destinations || []).length > 1;
}

/** chooses reports whether a step picks one destination itself, and so has
 *  a fallback. */
export function chooses(step) {
  return decides(step) || modeOf(step) === "header";
}

/** valuesFor is the header values that send a request to dest. */
export function valuesFor(step, dest) {
  return Object.keys(step.values || {})
    .filter((v) => step.values[v] === dest)
    .sort();
}

// measures reports whether the gateway works out this step's order. That
// order reflects recent minutes on one replica, so screens must not number it.
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
    case "header":
      return "the value of a header the client sends";
    default:
      return "the order they are written in";
  }
}

/** tokens renders a ceiling as typed, such as '32k'. */
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
 *  list is a menu, an order, or size bands. */
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
  // A decision model answers a short question, so it gets its own example.
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
  const byHeader = () => chosen === "header";
  // On measured modes the order only breaks ties.
  const ranks = () => measures(chosen);

  // Changing the mode hides unused fields rather than clearing them.
  const picker = modeTiles(MODES, chosen, (value) => {
    chosen = value;
    syncMode();
  });
  // More room under it: the mode decides what every field below means.
  const modeField = h(
    "div",
    { class: "field", style: { marginBottom: "24px" } },
    h("label", {}, "Routing strategy"),
    picker.tiles,
    picker.chosenRow,
  );

  // Dropdowns, so a step cannot name an unknown model. An array, not a set:
  // on a fallback step the order matters.
  const picked = [...(step.destinations || [])];
  // One per dropdown. An empty one is not a destination yet.
  const slots = picked.length ? [...picked] : [""];
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

  // In the wrong order, a fallback step serves everything from its second
  // choice.
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
      // The dropdowns follow, so they never show another order.
      slots.splice(0, slots.length, ...picked, ...slots.filter((a) => !a));
      renderDestinations();
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

  // perDestination fills list with a row per destination.
  const perDestination = (list, row) => {
    list.replaceChildren(
      ...(picked.length
        ? picked.map((alias) =>
            h(
              "div",
              { class: "row-tight" },
              h("span", { class: "mono", style: { minWidth: "160px" } }, alias),
              row(alias),
            ),
          )
        : [h("div", { class: "empty" }, "Nothing chosen yet.")]),
    );
  };

  // Kept here, not read off the inputs, so choosing a destination again
  // keeps its ceiling.
  const ceilings = { ...(step.ceilings || {}) };
  // Boxes holding something that is not a ceiling. Applying refuses them
  // rather than reading them as "no ceiling".
  const badCeilings = new Set();
  const ceilingList = h("div", { class: "stack", style: { gap: "4px" } });
  const ceilingHint = h("div", { class: "hint" });
  // Notes update on their own: rebuilding the rows would move the cursor.
  const notes = new Map();
  const syncCeilingNotes = () => {
    for (const [alias, note] of notes) {
      note.textContent = ceilings[alias]
        ? "tokens at most"
        : "takes what is larger than every ceiling";
    }
    // Exactly one destination needs no ceiling; say so before the control
    // plane refuses.
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
    perDestination(ceilingList, (alias) => {
      const box = h("input", {
        class: "input mono",
        style: { maxWidth: "120px" },
        placeholder: "no ceiling",
        value: ceilings[alias] ? tokens(ceilings[alias]) : "",
        onInput: () => {
          // Whole tokens, where 'k' means a thousand, as in the CLI.
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
      return [box, note];
    });
    if (picked.length) syncCeilingNotes();
    else ceilingHint.textContent = "";
  };
  const ceilingField = field(
    "Up to this many input tokens",
    ceilingList,
    ceilingHint,
  );

  // A header step's values per destination, kept like the ceilings.
  const headerName = h("input", {
    class: "input mono",
    placeholder: "X-Keera-Route",
    value: step.header || "",
  });
  const valueText = {};
  for (const alias of picked) {
    valueText[alias] = valuesFor(step, alias).join(", ");
  }
  const valueList = h("div", { class: "stack", style: { gap: "4px" } });
  const renderValues = () =>
    perDestination(valueList, (alias) =>
      h("input", {
        class: "input mono",
        placeholder: "cheap, quick",
        value: valueText[alias] || "",
        onInput: (e) => {
          valueText[alias] = e.target.value;
        },
      }),
    );
  const headerField = field(
    "Header",
    headerName,
    "The request header the client's app sets, such as X-Keera-Route.",
  );
  const valuesField = field(
    "Header values per model",
    valueList,
    "Lowercase words, separated by commas. Case does not matter in the " +
      "request. A destination without values can only be reached as the " +
      "fallback.",
  );

  const destinationList = h("div", { class: "stack", style: { gap: "6px" } });
  const addDestination = h(
    "button",
    {
      class: "btn btn-sm",
      type: "button",
      style: { justifySelf: "start" },
      onClick: () => {
        slots.push("");
        renderDestinations();
      },
    },
    icon(icons.plus),
    "Add",
  );
  const syncPicked = () => {
    picked.splice(0, picked.length, ...slots.filter(Boolean));
    renderDestinations();
    renderOrder();
    syncFallback();
    renderCeilings();
    renderValues();
  };
  // Each dropdown offers only what the others have not taken.
  const renderDestinations = () => {
    destinationList.replaceChildren(
      ...slots.map((alias, i) => {
        const taken = new Set(slots.filter((a, j) => a && j !== i));
        const known = chatModels.find((m) => m.alias === alias);
        const select = h(
          "select",
          {
            class: "select",
            onChange: () => {
              slots[i] = select.value;
              syncPicked();
            },
          },
          h("option", { value: "" }, "Choose a model"),
          // A deleted model stays named, so applying does not drop it unseen.
          alias && !known
            ? h("option", { value: alias, selected: true }, alias)
            : null,
          chatModels
            .filter((m) => !taken.has(m.alias))
            .map((m) =>
              h(
                "option",
                { value: m.alias, selected: m.alias === alias },
                m.alias,
              ),
            ),
        );
        return h(
          "div",
          {},
          h(
            "div",
            { class: "row-tight" },
            select,
            slots.length > 1 || alias
              ? iconButton(
                  icons.trash,
                  "Remove this model",
                  () => {
                    slots.splice(i, 1);
                    if (!slots.length) slots.push("");
                    syncPicked();
                  },
                  { quiet: true },
                )
              : null,
          ),
          alias && !known
            ? h("div", { class: "hint" }, "This model no longer exists.")
            : null,
        );
      }),
    );
    addDestination.disabled = slots.length >= chatModels.length;
  };
  const destinationPicker = h(
    "div",
    { class: "stack", style: { gap: "8px" } },
    destinationList,
    addDestination,
  );
  renderDestinations();
  syncFallback();
  renderOrder();
  renderCeilings();
  renderValues();

  // Fields only an instruction step has. Hidden, so nobody writes an
  // instruction the step then ignores.
  const modelField = field(
    "Deciding model",
    model,
    // Where this model runs can be wrong without anything here going red.
    "A small, fast model. It reads every request that reaches this step, so " +
      "keep it inside the cluster.",
  );
  const fallbackHint = h("div", { class: "hint" });
  const fallbackField = field("When it cannot decide", fallback, fallbackHint);
  const fallbackLabel = fallbackField.querySelector("label");
  const promptField = field(
    "Instruction",
    prompt,
    "Name kinds of request, not models. The destinations and their " +
      "descriptions are added for you.",
  );
  const orderHint = h("div", { class: "hint" });
  const orderField = field("In this order", order, orderHint);
  const destinationsHint = h("div", { class: "hint" });
  const costBanner = banner(
    "info",
    "Requests are sent to the deciding model, then to the destination " +
      "model. Both count against the same budgets.",
  );

  const syncMode = () => {
    rest.hidden = chosen === null;
    if (chosen === null) return;
    for (const el of [modelField, promptField]) {
      el.hidden = !reads();
    }
    fallbackField.hidden = !reads() && !byHeader();
    fallbackLabel.textContent = byHeader()
      ? "Without a known value"
      : "When it cannot decide";
    // The answer depends on the deployment, so the hint gives both.
    fallbackHint.textContent = byHeader()
      ? "Where a request goes when its header is missing or names no " +
        "destination. Refused requests get a 400 that lists the values."
      : "Fall back if the step saves money. Refuse if it keeps prompts " +
        "inside the cluster.";
    ceilingField.hidden = !bySize();
    headerField.hidden = !byHeader();
    valuesField.hidden = !byHeader();
    // A size step's order is its ceilings, and a header step has none.
    orderField.hidden = reads() || bySize() || byHeader();
    orderHint.textContent = ranks()
      ? "Only in a tie-break. The gateway reorders these on every request " +
        "from what it measures."
      : "The first takes every request. The rest are used when the ones " +
        "above cannot answer.";
    destinationsHint.textContent = bySize()
      ? "Each with the largest request it should be given."
      : byHeader()
        ? "Each with the values of the header that send a request there."
        : "";
    destinationsHint.hidden = !destinationsHint.textContent;
    costBanner.hidden = !reads();
  };

  const rest = h(
    "div",
    {},
    modelField,
    field("Destination models", destinationPicker, destinationsHint),
    ceilingField,
    headerField,
    valuesField,
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
      if (!chosen) throw new Error("Choose a routing strategy.");
      if (!picked.length) throw new Error("Choose at least one destination.");
      const bad = bySize() ? picked.find((a) => badCeilings.has(a)) : null;
      if (bad) {
        throw new Error(
          `The ceiling on ${bad} is not a number of tokens. Write 4000 ` +
            "or 4k, or leave it empty for no ceiling.",
        );
      }
      if (reads() && !model.value) {
        throw new Error("Choose the deciding model.");
      }
      if (reads() && !prompt.value.trim()) {
        throw new Error("Write the instruction.");
      }
      const values = byHeader() ? readValues(picked, valueText) : {};
      if (byHeader() && !headerName.value.trim()) {
        throw new Error("Name the header it reads.");
      }
      // Leftovers of another mode are dropped.
      return {
        mode: chosen,
        destinations: [...picked],
        model: reads() ? model.value : "",
        prompt: reads() ? prompt.value.trim() : "",
        fallback: reads() || byHeader() ? fallback.value : "",
        header: byHeader() ? headerName.value.trim() : "",
        values,
        ceilings: bySize()
          ? Object.fromEntries(
              picked.filter((a) => ceilings[a]).map((a) => [a, ceilings[a]]),
            )
          : {},
      };
    },
  };
}

/** readValues turns the typed values into a header step's map, refusing a
 *  value given to two destinations. */
export function readValues(picked, valueText) {
  const values = {};
  for (const alias of picked) {
    for (const raw of (valueText[alias] || "").split(",")) {
      const v = raw.trim().toLowerCase();
      if (!v) continue;
      if (values[v] && values[v] !== alias) {
        throw new Error(
          `The value ${v} is given to ${values[v]} and ${alias}.`,
        );
      }
      values[v] = alias;
    }
  }
  if (!Object.keys(values).length) {
    throw new Error("Give at least one destination a value.");
  }
  return values;
}

/** checkReport shows each sample and the model chosen, or each destination
 *  in the order tried. There is no pass: only the reader can judge. Every
 *  sample going the same way is always wrong. */
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
  const labelled = probe.mode === "header";
  const intro = !probe.ok
    ? chained
      ? "This step has nowhere to send a request."
      : "This step could not place the samples."
    : banded
      ? "Each destination, the request sizes it takes first, and whether it " +
        "is answering. This depends only on size, so it is the full picture."
      : labelled
        ? "Each destination, the header values that send a request there, " +
          "and whether it is answering. This is the full picture."
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
        ? ["Deciding model", h("span", { class: "mono" }, probe.model)]
        : null,
      ranked || banded ? ["In the order of", ordersBy(probe.mode)] : null,
      [
        banded || labelled
          ? "Places between"
          : chained
            ? "Would try, in order"
            : "Offered",
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
                label: banded || labelled ? "Placed on" : "Tries",
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
              // Why a destination is where it is, when the order is
              // measured.
              ranked || banded || labelled
                ? {
                    label: banded ? "Takes" : labelled ? "For" : "Reading",
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
            // A sample decided at 35% went there by a hair. Needs backend
            // probabilities.
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

// PROMPT_EXAMPLE names kinds of request, not models, since the catalogue says
// what each model is for. It says which way to err, as a small model is
// sometimes wrong.
const PROMPT_EXAMPLE = `Send small, simple requests to the cheapest model, \
such as a rename or a commit message.

Send requests that need planning or reasoning to a more capable model.

If you are unsure, choose the more capable model.`;

// A decision model sees each destination's description only. See
// docs/gateway.md#decision-models.
const DECISION_EXAMPLE = "Which model should answer this request?";
