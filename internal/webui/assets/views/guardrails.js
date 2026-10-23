// The guardrails dialog, for every level that has them. A guardrail is a set
// of unordered blocks, each holding on its own. Limits nest (key inside
// project inside organisation), so each field shows the ceiling above it
// rather than silently not taking effect.

import { api } from "../api.js";
import {
  h,
  icons,
  modal,
  num,
  compact,
  pill,
  readRow,
  plural,
  field,
  formModal,
  modeTiles,
  showError,
  banner,
} from "../ui.js";
import { orgNameOf } from "./orgs.js";
import { reaches } from "./router-tree.js";
import { modePills } from "./filters.js";
import {
  nodeCard,
  headButton,
  removeButton,
  emptyCard,
} from "./node-card.js";
import {
  rateShort,
  budgetShort,
  budgetText,
  rateFields,
  budgetFields,
} from "./limits.js";
import { oneLine } from "../status.js";

// One title at every level; the subtitle says which scope is open.
const SCOPES = {
  org: {
    title: "Guardrails",
    lead:
      "The ceiling for the whole organisation. No project or key can go past " +
      "it.",
  },
  project: {
    title: "Guardrails",
    lead:
      "Applies to every key in the project. A project can narrow what the " +
      "organisation allows, not widen it.",
  },
  key: {
    title: "Guardrails",
    lead:
      "Applies to this key only. Useful for a key given to a contractor, " +
      "or one that should not use the project's whole budget.",
    // For somebody who cannot edit: what holds their key, not how to set it.
    readLead:
      "Applies to this key, on top of its project's and organisation's " +
      "guardrails.",
  },
};

/** ceilings is the chain above, outermost first: [{label, limits}]. It keeps
 *  "unlimited" honest: a key without its own limit gets its project's.
 *  canEdit false shows the limits read-only, since members are held by them
 *  too. */
export async function openGuardrails(
  ctx,
  { scope, id, name, models, orgID, ceilings = [], canEdit = true },
) {
  // Filters and routers always belong to the organisation.
  const [limits, filters, routers] = await Promise.all([
    api.guardrails(scope, id).catch(() => null),
    orgID
      ? api
          .filters(orgID)
          .then((r) => r.data || [])
          .catch(() => null)
      : [],
    // Routers go in the allow-list: a client names one where it names a model.
    orgID
      ? api
          .routers(orgID)
          .then((r) => r.data || [])
          .catch(() => [])
      : [],
  ]);
  // A save replaces the whole guardrail, so editing after a failed read would
  // wipe the rest. A scope with no guardrail returns {}, not an error.
  if (limits === null) {
    modal({
      title: `Guardrails for ${name}`,
      body: banner(
        "bad",
        "Could not load these guardrails. Saving now would replace them " +
          "with only what this dialog shows. Try again shortly.",
      ),
    });
    return;
  }

  const args = {
    scope,
    id,
    name,
    // Routers are shaped like models so the code below treats them alike.
    models: models.concat(
      routers.map((p) => ({
        alias: p.alias,
        kind: "chat",
        router: true,
        destinations: reaches(p),
      })),
    ),
    filters,
    ceilings,
    limits,
    orgID,
    orgName: orgNameOf(ctx, orgID, { start: true }),
  };
  if (canEdit) render(ctx, args);
  else renderReadOnly(ctx, args);
}

/** Separate from openGuardrails so a caller that has the chain already does
 *  not fetch it twice. */
export async function ceilingsFor(
  scope,
  { orgID, orgName, projectID, projectName },
) {
  const out = [];
  if (scope === "project" || scope === "key") {
    if (orgID) {
      const lim = await api.guardrails("org", orgID).catch(() => null);
      if (lim) out.push({ label: orgName || "the organisation", limits: lim });
    }
  }
  if (scope === "key" && projectID) {
    const lim = await api.guardrails("project", projectID).catch(() => null);
    if (lim) out.push({ label: projectName || "the project", limits: lim });
  }
  return out;
}

// The fields the blocks set. Others, such as sandbox limits, must survive a
// save.
const OWNED_HERE = new Set([
  "allowed_models",
  "rate_limits",
  "budgets",
  "system_prompt",
  "filters",
  "allowed_tools",
]);

// Shared so the blocks and the read-only view use the same words.
const LIMITS = {
  rate: {
    label: "Rate limit",
    hint:
      "Tokens are counted when each request finishes. A minute refills as " +
      "it goes, per gateway replica unless the replicas share a Redis. A day " +
      "or a month starts again at midnight UTC or on the 1st.",
  },
  system_prompt: {
    label: "System prompt",
    hint: "Added before the messages on every chat request.",
  },
};

// One limit per period: two over the same period would count the same window.
const RATE_PERIODS = ["minute", "day", "month"];
const BUDGET_PERIODS = ["day", "month"];

// In display order.
const KINDS = [
  {
    value: "rate_limit",
    icon: icons.latency,
    name: "Rate limit",
    what: "Requests or tokens per minute, day or month.",
  },
  {
    value: "budget",
    icon: icons.billing,
    name: "Budget",
    what: "What this scope may spend per day or month.",
  },
  {
    value: "system_prompt",
    icon: icons.pencil,
    name: "System prompt",
    what: "Instructions added before every chat request.",
  },
  {
    value: "filter",
    icon: icons.filters,
    name: "Filter",
    what: "Redact or refuse, with one of the organisation's filters.",
  },
  {
    value: "models",
    icon: icons.models,
    name: "Models",
    what: "Allow only some of the models and routers.",
  },
  {
    value: "tools",
    icon: icons.mcp,
    name: "MCP tools",
    what: "Allow only some MCP servers or tools.",
  },
];

function kindOf(value) {
  return KINDS.find((k) => k.value === value);
}

/** A copy, so nothing changes until Save. */
function draftOf(lim) {
  return {
    rate_limits: (lim.rate_limits || []).map((r) => ({ ...r })),
    budgets: (lim.budgets || []).map((b) => ({ ...b })),
    system_prompt: lim.system_prompt || null,
    filters: [...(lim.filters || [])],
    allowed_models: lim.allowed_models ? [...lim.allowed_models] : null,
    allowed_tools: lim.allowed_tools ? [...lim.allowed_tools] : null,
  };
}

/** Limits by period, then the rest. */
function blocksOf(d) {
  // fallback is the period of a limit that has none.
  const byPeriod = (list, periods, fallback) =>
    [...list].sort(
      (a, b) =>
        periods.indexOf(a.period || fallback) -
        periods.indexOf(b.period || fallback),
    );
  return [
    ...byPeriod(d.rate_limits, RATE_PERIODS, "minute").map((item) => ({
      kind: "rate_limit",
      item,
    })),
    ...byPeriod(d.budgets, BUDGET_PERIODS, "month").map((item) => ({
      kind: "budget",
      item,
    })),
    d.system_prompt !== null ? { kind: "system_prompt" } : null,
    ...d.filters.map((alias, index) => ({ kind: "filter", alias, index })),
    d.allowed_models ? { kind: "models" } : null,
    d.allowed_tools ? { kind: "tools" } : null,
  ].filter(Boolean);
}

function freePeriods(list, periods, fallback, except) {
  const taken = list
    .filter((x) => x !== except)
    .map((x) => x.period || fallback);
  return periods.filter((p) => !taken.includes(p));
}

/** One of most kinds, one limit per period, and each filter once. */
function addable(env) {
  const d = env.draft;
  return KINDS.filter((k) => {
    switch (k.value) {
      case "rate_limit":
        return freePeriods(d.rate_limits, RATE_PERIODS, "minute").length > 0;
      case "budget":
        return freePeriods(d.budgets, BUDGET_PERIODS, "month").length > 0;
      case "system_prompt":
        return d.system_prompt === null;
      case "filter":
        return env.filters.some((f) => !d.filters.includes(f.alias));
      case "models":
        return !d.allowed_models;
      case "tools":
        return !d.allowed_tools;
    }
    return false;
  });
}

function render(
  ctx,
  { scope, id, name, models, filters, ceilings, limits, orgID, orgName },
) {
  const meta = SCOPES[scope] || SCOPES.project;
  const lim = limits || {};
  const env = {
    ctx,
    models,
    filters: filters || [],
    // Told apart from an organisation without filters, so a failed read does
    // not mark every filter as missing.
    filtersLost: filters === null,
    ceilings,
    orgID,
    orgName,
    draft: draftOf(lim),
  };

  // The dialog shows either the blocks or one block's form, to avoid stacking
  // a second dialog.
  const grid = h("div", { class: "gr-blocks" });
  const blocksPane = h(
    "div",
    {},
    h("p", { class: "muted", style: { marginTop: 0 } }, meta.lead),
    env.filtersLost
      ? banner(
          "warn",
          "Could not load the organisation's filters, so none can be added " +
            "now. Those already here stay as they are.",
        )
      : null,
    ceilingSummary(ceilings),
    grid,
  );
  const editPane = h("div", { hidden: true });
  let apply = null;

  const showBlocks = () => {
    apply = null;
    editPane.hidden = true;
    editPane.replaceChildren();
    blocksPane.hidden = false;
    foot().hidden = false;
    save.disabled = false;
    renderBlocks();
  };

  const renderBlocks = () => {
    const list = blocksOf(env.draft);
    const add = addable(env);
    grid.replaceChildren(
      ...list.map((b) => blockCard(env, b, { edit, remove, move })),
      add.length
        ? emptyCard({
            label: "Add",
            name: list.length ? "Add a guardrail" : "Add the first guardrail",
            note: list.length
              ? null
              : "Nothing is set here. A level above still applies.",
            onClick: () => edit(null),
          })
        : null,
    );
  };

  // b is null for a new block.
  const edit = (b) => {
    const err = h("div");
    const fieldsHost = h("div", {});
    let kind = b ? b.kind : "";
    let read = () => null;
    const choose = (value) => {
      kind = value;
      const built = blockFields(env, kind, b);
      fieldsHost.replaceChildren(built.el);
      fieldsHost.style.marginTop = b ? "" : "14px";
      read = built.read;
      const first = fieldsHost.querySelector("input, select, textarea");
      if (first) first.focus();
    };
    const picker = b ? null : modeTiles(addable(env), null, choose);
    apply = () => {
      showError(err, "");
      try {
        if (!kind) throw new Error("Choose what kind of guardrail it is.");
        put(env.draft, kind, b, read());
        showBlocks();
      } catch (ex) {
        showError(err, (ex && ex.message) || "That did not work.");
      }
    };

    editPane.replaceChildren(
      h(
        "h3",
        { style: { margin: "0 0 12px" } },
        b ? "Change " + kindOf(b.kind).name.toLowerCase() : "Add a guardrail",
      ),
      err,
      picker ? picker.tiles : null,
      picker ? picker.chosenRow : null,
      fieldsHost,
      h(
        "div",
        {
          class: "row",
          style: { justifyContent: "flex-end", marginTop: "14px" },
        },
        h(
          "button",
          { class: "btn", type: "button", onClick: showBlocks },
          "Cancel",
        ),
        h(
          "button",
          { class: "btn btn-primary", type: "button", onClick: () => apply() },
          b ? "Apply" : "Add",
        ),
      ),
    );
    blocksPane.hidden = true;
    editPane.hidden = false;
    // Saving now would store this block half done, so wait until it is
    // applied or dropped.
    foot().hidden = true;
    save.disabled = true;
    if (b) choose(b.kind);
    else picker.tiles.firstChild.focus();
  };

  const remove = (b) => {
    take(env.draft, b);
    renderBlocks();
  };

  const move = (b, by) => {
    const f = env.draft.filters;
    const to = b.index + by;
    [f[b.index], f[to]] = [f[to], f[b.index]];
    renderBlocks();
  };

  const form = h("form", {}, blocksPane, editPane);
  // Enter applies the open block, as the dialog's Save would otherwise.
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    if (apply) apply();
  });

  const { button: save } = formModal({
    wide: true,
    title: `${meta.title} - ${name}`,
    subtitle:
      "Each block is one guardrail. A level above still applies. Nothing is " +
      "saved until you press Save.",
    body: form,
    done: "Guardrails saved",
    ctx,
    submit: () => api.putGuardrails(scope, id, saved(lim, env)),
  });
  const foot = () => save.parentElement;
  renderBlocks();
}

/** A PUT replaces the whole guardrail, so fields the blocks do not set are
 *  carried through. */
function saved(lim, env) {
  const d = env.draft;
  const out = {};
  for (const [k, v] of Object.entries(lim)) {
    if (!OWNED_HERE.has(k)) out[k] = v;
  }
  if (d.rate_limits.length) out.rate_limits = d.rate_limits;
  if (d.budgets.length) out.budgets = d.budgets;
  if (d.system_prompt) out.system_prompt = d.system_prompt;
  if (d.filters.length) out.filters = d.filters;
  if (d.allowed_models) out.allowed_models = d.allowed_models;
  if (d.allowed_tools) out.allowed_tools = d.allowed_tools;
  return out;
}

/** Writes over b, or adds a new block when b is null. */
function put(d, kind, b, v) {
  switch (kind) {
    case "rate_limit":
    case "budget": {
      const list = kind === "budget" ? d.budgets : d.rate_limits;
      const at = b ? list.indexOf(b.item) : -1;
      if (at >= 0) list[at] = v;
      else list.push(v);
      return;
    }
    case "system_prompt":
      d.system_prompt = v;
      return;
    case "filter":
      if (b) d.filters[b.index] = v;
      else d.filters.push(v);
      return;
    case "models":
      d.allowed_models = v;
      return;
    case "tools":
      d.allowed_tools = v;
  }
}

function take(d, b) {
  switch (b.kind) {
    case "rate_limit":
      d.rate_limits = d.rate_limits.filter((x) => x !== b.item);
      return;
    case "budget":
      d.budgets = d.budgets.filter((x) => x !== b.item);
      return;
    case "system_prompt":
      d.system_prompt = null;
      return;
    case "filter":
      d.filters.splice(b.index, 1);
      return;
    case "models":
      d.allowed_models = null;
      return;
    case "tools":
      d.allowed_tools = null;
  }
}

function blockCard(env, b, { edit, remove, move }) {
  const kind = kindOf(b.kind);
  // Filters run in order, each on the previous one's output, so only they
  // can be moved.
  const n = env.draft.filters.length;
  const movable = b.kind === "filter" && n > 1;
  return nodeCard({
    kind: b.kind,
    glyph: kind.icon,
    label: kind.name,
    name: blockName(env, b),
    meta: blockMeta(env, b),
    title: "Change this guardrail",
    onOpen: () => edit(b),
    buttons: [
      movable && b.index > 0
        ? headButton("←", "Run this one earlier", () => move(b, -1))
        : null,
      movable && b.index < n - 1
        ? headButton("→", "Run this one later", () => move(b, 1))
        : null,
      removeButton("Remove this guardrail", () => remove(b)),
    ],
  });
}

function blockName(env, b) {
  const d = env.draft;
  switch (b.kind) {
    case "rate_limit":
      return rateShort(b.item).join(" · ");
    case "budget":
      return budgetShort(b.item);
    case "system_prompt": {
      // Just the start; the form has the whole text.
      return h(
        "span",
        { class: "pl-prompt", title: d.system_prompt },
        oneLine(d.system_prompt, 60),
      );
    }
    case "filter":
      return h("span", { class: "mono" }, b.alias);
    case "models":
      return listName(d.allowed_models, "model");
    case "tools":
      return listName(d.allowed_tools, "tool");
  }
  return "";
}

function listName(list, noun) {
  if (!list.length) return "None";
  if (list.length > 3) return `${list.length} ${plural(list.length, noun)}`;
  return h("span", { class: "mono" }, list.join(", "));
}

function blockMeta(env, b) {
  let bits = [];
  if (b.kind === "filter") {
    const f = env.filters.find((x) => x.alias === b.alias);
    if (f) bits = modePills(f);
    else if (!env.filtersLost) bits = [pill("Not found", "bad")];
    const n = env.draft.filters.length;
    if (n > 1)
      bits.push(h("span", { class: "faint small" }, `${b.index + 1} of ${n}`));
  } else if (b.kind === "models") {
    const list = env.draft.allowed_models;
    if (!list.length) bits = [pill("Nothing is served", "bad")];
    else if (list.some((a) => !allowedAbove(env.ceilings, a))) {
      bits = [pill("Some blocked above", "warn")];
    }
  } else if (b.kind === "tools" && !env.draft.allowed_tools.length) {
    bits = [pill("No tools", "warn")];
  }
  return bits.length ? h("div", { class: "wrap-chips" }, bits) : null;
}

/** read returns what the form sets or throws what is missing. b is null for
 *  a new block. */
function blockFields(env, kind, b) {
  const d = env.draft;
  if (kind === "rate_limit") {
    const periods = freePeriods(
      d.rate_limits,
      RATE_PERIODS,
      "minute",
      b && b.item,
    );
    return rateFields((b && b.item) || {}, periods, [
      LIMITS.rate.hint +
        " A scope has one rate limit per minute, per day and per month.",
      ceilingsOf(env.ceilings, rateTexts),
    ]);
  }
  if (kind === "budget") {
    // Month first: most scopes have a monthly budget.
    const periods = freePeriods(d.budgets, BUDGET_PERIODS, "month", b && b.item)
      .slice()
      .reverse();
    return budgetFields((b && b.item) || {}, periods, [
      "Past this, requests get a 402 until the period resets, at " +
        "midnight UTC or on the 1st. A busy scope can overshoot slightly. " +
        "A scope has one budget per day and one per month.",
      ceilingsOf(env.ceilings, budgetTexts),
    ]);
  }
  if (kind === "system_prompt") {
    const prompt = h("textarea", {
      class: "input",
      rows: "6",
      placeholder:
        "Answer in British English. Never suggest a dependency that is not already in the lockfile.",
    });
    prompt.value = d.system_prompt || "";
    return {
      el: field("Prompt", prompt, [
        LIMITS.system_prompt.hint +
          " The organisation's prompt is sent first, then the project's.",
        inheritedPrompts(env.ceilings),
      ]),
      read: () => {
        const text = prompt.value.trim();
        if (!text) throw new Error("Write the prompt.");
        return text;
      },
    };
  }
  if (kind === "filter") {
    // Picked from a list, not typed: an unknown alias would refuse every
    // request, and the control plane refuses to store one.
    const usable = env.filters.filter(
      (f) => !d.filters.includes(f.alias) || (b && f.alias === b.alias),
    );
    const select = h(
      "select",
      { class: "select" },
      h("option", { value: "" }, "Choose a filter"),
      usable.map((f) =>
        h(
          "option",
          { value: f.alias, selected: b && f.alias === b.alias },
          f.alias +
            " (" +
            (f.mode || "rewrite") +
            (f.shadow ? ", in shadow" : "") +
            ")",
        ),
      ),
    );
    return {
      el: field(
        "Filter",
        select,
        h(
          "div",
          { class: "hint" },
          "Filters run in order before the request is forwarded, each over " +
            "what the one before left. A gate placed first saves the cost of " +
            "the filters after it when it refuses. The organisation's filters " +
            "run first and cannot be removed here.",
          inheritedFilters(env.ceilings),
        ),
      ),
      read: () => {
        if (!select.value) throw new Error("Choose a filter.");
        return select.value;
      },
    };
  }
  if (kind === "models") {
    // Ticked from the catalogue. An alias the dialog has no box for, such as
    // a disabled model, is kept: nobody unticked it.
    const had = d.allowed_models;
    const boxes = env.models.map((m) =>
      h(
        "label",
        { class: "row-tight", style: { padding: "3px 0" } },
        h("input", {
          type: "checkbox",
          value: m.alias,
          checked: had ? had.includes(m.alias) : true,
        }),
        h("span", { class: "mono" }, m.alias),
        m.router
          ? // A router grants every model its steps may send to, whatever
            // this list says.
            h(
              "span",
              {
                class: "pill pill-accent",
                title:
                  "Router. Allowing it allows the models it picks from: " +
                  (m.destinations.join(", ") || "none"),
              },
              "router",
            )
          : h("span", { class: "faint small" }, m.kind),
        allowedAbove(env.ceilings, m.alias)
          ? null
          : h("span", { class: "pill pill-warn" }, "Blocked above"),
      ),
    );
    return {
      el: field(
        "Models and routers this scope may reach",
        boxes.length
          ? h("div", {}, boxes)
          : h("span", { class: "faint" }, `${env.orgName} has no models.`),
        "Without this block, every model of the organisation is allowed. " +
          "Removing one takes effect on the next request.",
      ),
      read: () => {
        const shown = new Set(env.models.map((m) => m.alias));
        return [
          ...boxes
            .map((l) => l.querySelector("input"))
            .filter((i) => i.checked)
            .map((i) => i.value),
          ...(had || []).filter((a) => !shown.has(a)),
        ];
      },
    };
  }
  // MCP tools are typed rather than ticked: the gateway does not list every
  // server's tools, and the control plane refuses a server it does not know.
  const tools = h("input", {
    class: "input mono",
    placeholder: "github/search_code, jira",
    value: (d.allowed_tools || []).join(", "),
  });
  return {
    el: field(
      "MCP tools this scope may call",
      tools,
      "A server alias allows all its tools; alias/tool allows one. Tools " +
        "left out are hidden from the agent and refused if called. Without " +
        "this block, every tool of every MCP server is allowed.",
    ),
    read: () =>
      tools.value
        .split(",")
        .map((t) => t.trim())
        .filter(Boolean),
  };
}

/** For somebody who may not change the guardrails. Unlike a disabled form,
 *  which would show only this scope's values, each line combines every level
 *  and names the one that decided. */
function renderReadOnly(
  ctx,
  { scope, name, models, filters, ceilings, limits, orgName },
) {
  const meta = SCOPES[scope] || SCOPES.project;
  // Outermost first: the order limits combine in and prompts are sent in.
  const levels = [...ceilings, { label: name, limits: limits || {} }];

  modal({
    wide: true,
    title: `${meta.title} - ${name}`,
    subtitle:
      "What applies here. To change it, ask your organisation's " +
      "administrator.",
    body: h(
      "div",
      {},
      h(
        "p",
        { class: "muted", style: { marginTop: 0 } },
        meta.readLead || meta.lead,
      ),
      readRow(
        LIMITS.rate.label,
        levelsValue(levels, rateTexts, "unlimited"),
        "Each level's rate limit applies on its own. " + LIMITS.rate.hint,
      ),
      readRow(
        "Budget",
        levelsValue(levels, budgetTexts, "no budget"),
        "Each budget applies on its own. Past any of them, requests are " +
          "refused until the period resets.",
      ),
      readRow(
        LIMITS.system_prompt.label,
        promptsValue(levels),
        LIMITS.system_prompt.hint,
      ),
      readRow(
        "Filters",
        filtersValue(levels, filters),
        "Run in this order before your requests are forwarded. If one " +
          "cannot run, the request is refused, unless it is in shadow.",
      ),
      readRow(
        "Models",
        modelsValue(levels, models, orgName),
        narrowedBy(levels),
      ),
      readRow(
        "MCP tools",
        toolsValue(levels),
        "Each level can only narrow the one above. Tools left out are " +
          "hidden from your agent and refused if called.",
      ),
    ),
  });
}

/** Each level's list is shown, since they narrow each other. */
function toolsValue(levels) {
  const said = levels.filter((lv) => lv.limits && lv.limits.allowed_tools);
  if (!said.length) {
    return h("span", { class: "muted" }, "Every tool of every MCP server");
  }
  return h(
    "div",
    { class: "stack" },
    said.map((lv) =>
      h(
        "span",
        {},
        h("span", { class: "muted" }, `${lv.label}: `),
        h(
          "span",
          { class: "mono" },
          lv.limits.allowed_tools.length
            ? lv.limits.allowed_tools.join(", ")
            : "none",
        ),
      ),
    ),
  );
}

/** A level that names no allow-list restricts nothing. */
function effectiveModels(levels, models) {
  let allowed = null;
  for (const lv of levels) {
    const list = lv.limits && lv.limits.allowed_models;
    if (!list) continue;
    allowed =
      allowed === null ? list.slice() : allowed.filter((a) => list.includes(a));
  }
  const catalogue = models.map((m) => m.alias);
  return allowed === null
    ? catalogue
    : catalogue.filter((a) => allowed.includes(a));
}

function modelsValue(levels, models, orgName) {
  const allowed = effectiveModels(levels, models);
  if (allowed.length) {
    return h(
      "div",
      { class: "wrap-chips" },
      allowed.map((a) => pill(a, "accent")),
    );
  }
  return h(
    "span",
    { class: "muted" },
    models.length
      ? "None. No model will be served."
      : `${orgName} has no models.`,
  );
}

/** Names the levels that removed models, so a short list reads as
 *  somebody's decision. */
function narrowedBy(levels) {
  const said = levels.filter((lv) => lv.limits && lv.limits.allowed_models);
  if (!said.length) return "Every model the organisation has.";
  return `Narrowed by ${said.map((lv) => lv.label).join(", then ")}.`;
}

/** Every level's rate limits rather than one number: each may have its own
 *  period, and each has to hold. */
/** levelsValue lists what texts says of each level's limits, with the level
 *  that set it, or none when no level sets any. Limits of every level hold,
 *  so none is left out. */
function levelsValue(levels, texts, none) {
  const said = levels.flatMap((lv) =>
    texts(lv.limits).map((text) => ({ text, from: lv.label })),
  );
  if (!said.length) return h("span", { class: "muted" }, none);
  return h(
    "div",
    { class: "stack" },
    said.map((r) =>
      h(
        "span",
        {},
        r.text,
        h("span", { class: "hint-origin" }, ` - set by ${r.from}`),
      ),
    ),
  );
}

/** Such as "120 requests and 50k tokens per minute". */
function rateTexts(lim) {
  return ((lim && lim.rate_limits) || [])
    .map((rl) => {
      const parts = [];
      if (rl.requests) parts.push(`${num(rl.requests)} requests`);
      if (rl.tokens) parts.push(`${compact(rl.tokens)} tokens`);
      return parts.length
        ? `${parts.join(" and ")} per ${rl.period || "minute"}`
        : "";
    })
    .filter(Boolean);
}

function budgetTexts(lim) {
  return ((lim && lim.budgets) || []).map(budgetText);
}

/** In full, in the order sent: only the full text tells the reader what is
 *  added to their requests. */
function promptsValue(levels) {
  const said = levels.filter((lv) => lv.limits && lv.limits.system_prompt);
  if (!said.length) {
    return h("span", { class: "muted" }, "None.");
  }
  return h(
    "div",
    { class: "stack", style: { gap: "8px" } },
    said.map((lv) =>
      h(
        "div",
        { class: "stack", style: { gap: "4px" } },
        h("span", { class: "hint-origin" }, `${lv.label} sends:`),
        h("div", { class: "prompt-text" }, lv.limits.system_prompt),
      ),
    ),
  );
}

/** known is the organisation's filters, to tell enforcing from shadow. */
function filtersValue(levels, known) {
  const defs = new Map((known || []).map((f) => [f.alias, f]));
  const seen = [];
  for (const lv of levels) {
    for (const alias of (lv.limits && lv.limits.filters) || []) {
      if (!seen.some((f) => f.alias === alias))
        seen.push({ alias, from: lv.label });
    }
  }
  if (!seen.length) {
    return h(
      "span",
      { class: "muted" },
      "None. Requests are forwarded as sent.",
    );
  }
  return h(
    "div",
    { class: "stack" },
    seen.map((f) =>
      h(
        "span",
        {},
        h("span", { class: "mono" }, f.alias),
        defs.get(f.alias) && defs.get(f.alias).shadow
          ? [" ", pill("Shadow", "warn")]
          : null,
        h("span", { class: "hint-origin" }, ` - applied by ${f.from}`),
      ),
    ),
  );
}

/** So an administrator does not add a filter twice. */
function inheritedFilters(ceilings) {
  const said = ceilings.filter(
    (c) => c.limits && (c.limits.filters || []).length,
  );
  if (!said.length) return null;
  return said.map((c) =>
    h(
      "span",
      { class: "ceiling" },
      ` ${c.label} already runs ${c.limits.filters.join(", ")}.`,
    ),
  );
}

function ceilingSummary(ceilings) {
  const said = ceilings.filter((c) => hasAny(c.limits));
  if (!said.length) return null;
  return banner(
    "info",
    "Limited by ",
    said.map((c, i) => [
      i > 0 ? ", then " : null,
      h("strong", {}, c.label),
      ": ",
      describe(c.limits),
    ]),
    ". Values here can only narrow this.",
  );
}

function describe(lim) {
  const bits = [];
  if (lim.allowed_models) {
    const n = lim.allowed_models.length;
    bits.push(n ? `${n} ${plural(n, "model")}` : "no model");
  }
  for (const rl of lim.rate_limits || []) bits.push(...rateShort(rl));
  for (const b of lim.budgets || []) bits.push(budgetText(b));
  if (lim.system_prompt) bits.push("a system prompt");
  if ((lim.filters || []).length) {
    bits.push(`${lim.filters.length} ${plural(lim.filters.length, "filter")}`);
  }
  return bits.join(" · ") || "nothing set";
}

/** A prompt is appended to these, so seeing them avoids repeating the
 *  wording. */
function inheritedPrompts(ceilings) {
  const said = ceilings.filter((c) => c.limits && c.limits.system_prompt);
  if (!said.length) return null;
  return said.map((c) =>
    h(
      "div",
      { class: "ceiling", style: { marginTop: "6px" } },
      `${c.label} already sends, before this: `,
      h("em", { style: { whiteSpace: "pre-wrap" } }, c.limits.system_prompt),
    ),
  );
}

/** ceilingsOf says what each level above allows, as texts reads it. */
function ceilingsOf(ceilings, texts) {
  return ceilings.flatMap((c) =>
    texts(c.limits).map((text) =>
      h("span", { class: "ceiling" }, ` ${c.label} allows ${text}.`),
    ),
  );
}

function allowedAbove(ceilings, alias) {
  return ceilings.every(
    (c) =>
      !c.limits ||
      !c.limits.allowed_models ||
      c.limits.allowed_models.includes(alias),
  );
}

function hasAny(lim) {
  return !!(
    lim &&
    (lim.allowed_models ||
      (lim.rate_limits || []).length ||
      (lim.budgets || []).length ||
      lim.system_prompt ||
      (lim.filters || []).length)
  );
}

export function summarise(lim) {
  const bits = (lim.rate_limits || []).flatMap(rateShort);
  if (lim.system_prompt) bits.push("system prompt");
  for (const name of lim.filters || []) bits.push(`filter: ${name}`);
  return bits;
}

export function modelsCell(lim) {
  if (!lim || !lim.allowed_models) return h("span", { class: "muted" }, "All");
  return h(
    "div",
    { class: "wrap-chips" },
    lim.allowed_models.length
      ? lim.allowed_models.map((m) => pill(m, "accent"))
      : pill("None", "bad"),
  );
}
