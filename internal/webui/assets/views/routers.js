// Routers: filters, route steps, limits and models chained behind one alias.
// Each router's screen is its editor, drawn as the tree the gateway walks.
// The tree is sent whole on Save, so a half-drawn router never answers a
// request. See policy.Router.

import { api } from "../api.js";
import {
  h,
  icon,
  icons,
  providerMark,
  iconButton,
  pill,
  rowLink,
  compact,
  num,
  money,
  ms,
  stat,
  moneyStat,
  table,
  listHead,
  detailHead,
  rowActions,
  formModal,
  confirm,
  field,
  modeTiles,
  banner,
  toast,
  dash,
  isAdmin,
  plural,
  pct,
  aliasProblem,
  locationName,
  currentRange,
  rangePicker,
  trafficSummary,
  gone,
  busyButton,
} from "../ui.js";
import { barList } from "../chart.js";
import { oneLine } from "../status.js";
import { orgNameOf } from "./orgs.js";
import { modePills, newFilter } from "./filters.js";
import { nodeCard, removeButton, emptyCard, addButton } from "./node-card.js";
import {
  rateShort,
  budgetShort,
  rateFields,
  budgetFields,
  budgetUse,
  rateUse,
} from "./limits.js";
import {
  routeFields,
  modeOf,
  modeName,
  decides,
  chooses,
  valuesFor,
  checkReport,
  tokens,
} from "./route-step.js";
import {
  KINDS,
  limits,
  passes,
  steps,
  newID,
  summary,
  reaches,
  problems,
  warnings,
  clean,
} from "./router-tree.js";

const KIND_TILES = [
  {
    value: "filter",
    icon: icons.filters,
    what: "Redact or refuse, with one of the organisation's filters.",
  },
  {
    value: "rate_limit",
    icon: icons.latency,
    what: "Requests or tokens per minute, day or month through this step.",
  },
  {
    value: "budget",
    icon: icons.billing,
    what: "What requests through this step may cost per day or month.",
  },
  {
    value: "system_prompt",
    icon: icons.pencil,
    what: "Instructions for the model, on requests that pass this step.",
  },
  {
    value: "route",
    icon: icons.route,
    what: "Choose the model. Each destination can get its own steps.",
  },
  {
    value: "model",
    icon: icons.models,
    what: "Send the request to one model. This ends the branch.",
  },
].map((t) => ({ ...t, name: KINDS[t.value] }));

const KIND_ICONS = Object.fromEntries(KIND_TILES.map((t) => [t.value, t.icon]));

const DELETE_WARNING =
  "Deleting it takes it off the organisation's allow-list. It cannot be " +
  "deleted while a project's or key's allow-list names it. Clients still " +
  "using the alias will be told the model does not exist. The filters and " +
  "models it uses stay.";

export async function routersView(ctx) {
  const since = currentRange();
  const res = await api.routers(ctx.orgID, { stats: true, since });
  const routers = res.data || [];
  const stats = res.stats || {};
  const canEdit = isAdmin(ctx);
  ctx.setSubtitle(`${routers.length} ${plural(routers.length, "router")}`);

  const head = listHead(
    null,
    [rangePicker(ctx, since)],
    canEdit && {
      label: "Create router",
      onClick: () => createRouter(ctx, routers),
    },
  );

  const rows = table(
    [
      {
        label: "Alias",
        sortKey: (p) => p.alias,
        cell: (p) =>
          h(
            "div",
            { class: "stack" },
            rowLink(
              ctx,
              "/routers/" + encodeURIComponent(p.alias),
              h("strong", { class: "mono" }, p.alias),
              canEdit ? "Open the editor" : "See its steps",
            ),
            p.display_name
              ? h("span", { class: "faint small" }, p.display_name)
              : null,
          ),
      },
      {
        label: "Steps",
        sortKey: (p) => steps(p.start).length,
        cell: (p) => {
          const n = steps(p.start).length;
          return h(
            "div",
            { class: "stack" },
            h("span", { class: "nowrap" }, summary(p.start)),
            h("span", { class: "faint small" }, `${n} ${plural(n, "step")}`),
          );
        },
      },
      // Allowing the router allows every one of these.
      {
        label: "Reaches",
        cell: (p) => {
          const to = reaches(p);
          if (!to.length) return dash();
          return h(
            "div",
            { class: "wrap-chips" },
            to.slice(0, 3).map((a) => pill(a)),
            to.length > 3 ? pill(`+${to.length - 3}`) : null,
          );
        },
      },
      {
        label: "In this period",
        shrink: true,
        sortKey: (p) => stats[p.alias]?.requests || null,
        sortDir: "desc",
        cell: (p) => trafficCell(stats[p.alias]),
      },
      {
        label: "Description",
        sortKey: (p) => p.description || null,
        cell: (p) =>
          p.description
            ? h("span", { class: "muted clamp-2" }, p.description)
            : dash(),
      },
      canEdit && {
        label: "",
        shrink: true,
        cell: (p) =>
          rowActions(ctx, p.alias, {
            editTitle: "Open the editor",
            onEdit: () =>
              ctx.navigate("/routers/" + encodeURIComponent(p.alias)),
            deleteTitle: "Delete this router",
            body: DELETE_WARNING,
            confirmLabel: "Delete router",
            remove: () => api.deleteRouter(ctx.orgID, p.alias),
          }),
      },
    ].filter(Boolean),
    routers,
    {
      emptyTitle: "No routers yet",
      emptyBody:
        "A router chains filters, route steps, rate limits and budgets in " +
        "the order you draw them, and ends at a model. Clients name it like " +
        "a model.",
      ctx,
      open: (p) => "/routers/" + encodeURIComponent(p.alias),
    },
  );

  return h("div", {}, head, rows);
}

// trafficCell flags a row when much was refused, or when a route step could
// not decide, since those requests still got answers and hide the failure.
function trafficCell(st) {
  if (!st || !st.requests) return trafficSummary(0);
  const refusedRate = st.refused / st.requests;
  const placed = (st.chose || 0) + (st.fell_back || 0) + (st.errored || 0);
  const undecided = (st.fell_back || 0) + (st.errored || 0);
  return trafficSummary(
    st.requests,
    plural(st.requests, "request"),
    st.failed
      ? pill(`${num(st.failed)} failed`, "bad")
      : refusedRate >= 0.05
        ? pill(`${pct(st.refused, st.requests)} refused`, "warn")
        : undecided
          ? pill(
              `${pct(undecided, placed)} undecided`,
              undecided / placed >= 0.25 ? "bad" : "warn",
            )
          : null,
    `${money(st.cost_micros)} · ${compact(st.input_tokens + st.output_tokens)} tokens`,
  );
}

// createRouter asks only for the alias. Nothing is saved until the editor
// has steps.
async function createRouter(ctx, routers) {
  const alias = h("input", {
    class: "input mono",
    placeholder: "private-first",
  });
  formModal({
    title: "Create router",
    subtitle:
      "Next you draw its steps. Nothing is saved until every branch ends at " +
      "a model.",
    body: h(
      "form",
      {},
      field(
        "Alias",
        alias,
        "Lowercase letters, digits and hyphens. Clients use it as the model " +
          "name, so it cannot change later.",
      ),
    ),
    submitLabel: "Draw its steps",
    submit: async () => {
      const name = alias.value.trim();
      const problem = aliasProblem(name);
      if (problem) {
        alias.focus();
        throw new Error(problem);
      }
      // A client names both in the same field, so they may not share it.
      const models = await api.models(ctx.orgID).then((r) => r.data || []);
      const taken = [
        [routers, "router"],
        [models, "model"],
      ].find(([list]) => list.some((x) => x.alias === name));
      if (taken) {
        alias.focus();
        throw new Error(
          `${name} is already a ${taken[1]}. Clients name routers and ` +
            "models in the same field, so pick another alias.",
        );
      }
      ctx.navigate("/routers/" + encodeURIComponent(name));
    },
  });
  alias.focus();
}

export async function routerDetailView(ctx) {
  const since = currentRange();
  const alias = ctx.param;
  const [res, filters, models] = await Promise.all([
    api.routerReport(ctx.orgID, alias, since),
    api.filters(ctx.orgID).then((r) => r.data || []),
    api.models(ctx.orgID).then((r) => r.data || []),
  ]);
  const saved = res.router || null;
  const rep = res.report || {};
  const canEdit = isAdmin(ctx);
  const total = rep.total || {};

  if (!saved && !canEdit) {
    return gone(
      ctx,
      "/routers",
      "Routers",
      "No such router",
      `It was deleted, or ${orgNameOf(ctx)} never had one of this alias.`,
    );
  }
  // A deleted router with traffic in the period opens as new: saving it
  // brings the alias back.
  const fresh = !saved;
  ctx.setTitle(
    alias,
    fresh ? "New router, not saved yet" : saved.display_name || "",
  );

  const env = {
    ctx,
    canEdit,
    filters,
    models,
    draft: {
      display_name: saved ? saved.display_name || "" : "",
      description: saved ? saved.description || "" : "",
      // A copy: the editor changes it in place, and "Unsaved changes"
      // compares against the saved one.
      start: saved
        ? JSON.parse(JSON.stringify(saved.start))
        : { id: newID(null), kind: "model", ref: "" },
    },
  };
  env.original = JSON.stringify(clean(env.draft.start));

  const canvas = h("div", { class: "pl-canvas" });
  const notes = h("div", { class: "stack", style: { gap: "8px" } });
  const status = h("span", { class: "faint small" });
  const dirtyPill = h("span", {});
  const saveButton = busyButton({ class: "btn btn-primary" }, "Save", () =>
    save(env),
  );

  env.render = () => {
    const refs = { filters: env.filters, models };
    const found = problems(env.draft.start, refs);
    env.problems = found;
    canvas.replaceChildren(
      h(
        "div",
        { class: "pl-chain" },
        entry(env),
        renderSlot(env, rootSlot(env), { required: true }),
      ),
    );
    notes.replaceChildren(
      ...warnings(env.draft.start, env.filters).map((w) => banner("warn", w)),
    );
    const n = [...found.keys()].filter(Boolean).length;
    const ready = found.size === 0;
    const dirty =
      fresh ||
      JSON.stringify(clean(env.draft.start)) !== env.original ||
      env.draft.display_name !== (saved?.display_name || "") ||
      env.draft.description !== (saved?.description || "");
    saveButton.disabled = !ready || !dirty;
    status.textContent = !ready
      ? found.get("") ||
        `${n} ${plural(n, "step needs", "steps need")} attention before saving.`
      : dirty
        ? "Ready to save."
        : "Saved.";
    dirtyPill.replaceChildren(
      !fresh && dirty ? pill("Unsaved changes", "warn") : "",
    );
  };

  const savedSteps = saved ? steps(saved.start) : [];
  const wrap = h(
    "div",
    {},
    detailHead(
      ctx,
      ["/routers", "Routers"],
      [
        fresh ? pill("Not saved yet", "warn") : null,
        dirtyPill,
        saved
          ? pill(`${savedSteps.length} ${plural(savedSteps.length, "step")}`)
          : null,
      ],
      [
        fresh ? null : rangePicker(ctx, since),
        // Checks the saved route steps, not the draft.
        saved && canEdit && savedSteps.some((s) => s.kind === "route")
          ? busyButton(
              { title: "Test its route steps with sample prompts" },
              "Check",
              async () => checkReport(await api.checkRouter(ctx.orgID, alias)),
            )
          : null,
        saved && canEdit
          ? iconButton(icons.trash, "Delete this router", () =>
              confirm({
                title: `Delete ${alias}?`,
                body: DELETE_WARNING,
                confirmLabel: "Delete router",
                danger: true,
                onConfirm: async () => {
                  await api.deleteRouter(ctx.orgID, alias);
                  toast(`${alias} deleted`, "good");
                  ctx.navigate("/routers");
                },
              }),
            )
          : null,
      ],
    ),
  );

  const placed = (rep.chose || 0) + (rep.fell_back || 0) + (rep.errored || 0);
  if (!fresh) {
    for (const b of decisionBanners(rep, placed)) wrap.append(b);
    wrap.append(
      h(
        "div",
        { class: "grid grid-4", style: { marginBottom: "16px" } },
        stat(
          "Requests",
          compact(total.requests || 0),
          total.requests ? "named this router" : "none in this period",
        ),
        stat(
          "Refused",
          total.requests ? pct(total.refused, total.requests) : "-",
          total.refused
            ? `${num(total.refused)} by a step or the key's guardrails`
            : "none refused",
        ),
        moneyStat(
          "Cost",
          total.cost_micros || 0,
          "filters and decisions included",
        ),
        // A generation when a step reads the request, or the destinations
        // that failed when it tries them.
        placed
          ? stat(
              "Decided",
              pct(rep.chose, placed),
              rep.decision_median_ms
                ? `added ${ms(rep.decision_median_ms)}, ` +
                    `p95 ${ms(rep.decision_p95_ms)}`
                : "added no wait",
            )
          : stat("First token", ms(total.ttft_median_ms), "median"),
      ),
    );
  }

  wrap.append(
    settingsCard(env),
    h(
      "div",
      { class: "card", style: { marginTop: "16px" } },
      h(
        "div",
        { class: "card-head" },
        h("h2", {}, "Steps"),
        h("div", { class: "spacer" }),
        canEdit ? status : null,
        canEdit ? saveButton : null,
      ),
      h(
        "div",
        { class: "card-body stack", style: { gap: "12px" } },
        h(
          "p",
          { class: "muted", style: { margin: 0 } },
          canEdit
            ? "Requests flow from left to right. Click a step to change it, " +
                "or a + on a line to add one there. The key's own guardrails " +
                "still apply around the router."
            : "Requests flow from left to right. An administrator changes " +
                "the steps.",
        ),
        notes,
        canvas,
      ),
    ),
  );

  // They share a row like a key's models and facts. Either alone takes it all.
  const panels = fresh
    ? []
    : [
        (rep.destinations || []).length ? answeredCard(rep.destinations) : null,
        (res.limits || []).length ? limitsCard(res.limits, savedSteps) : null,
      ].filter(Boolean);
  if (panels.length) {
    wrap.append(
      h(
        "div",
        {
          class: panels.length === 2 ? "grid grid-2" : "grid",
          style: { marginTop: "16px" },
        },
        ...panels,
      ),
    );
  }

  env.render();
  return wrap;
}

// settingsCard: clients see the display name and description on /v1/models.
function settingsCard(env) {
  const displayName = h("input", {
    class: "input",
    placeholder: "Private first",
    value: env.draft.display_name,
    disabled: !env.canEdit,
    onInput: (e) => {
      env.draft.display_name = e.target.value.trim();
      env.render();
    },
  });
  const description = h("input", {
    class: "input",
    placeholder: "Keeps customer data inside, and the hosted spend capped",
    value: env.draft.description,
    disabled: !env.canEdit,
    onInput: (e) => {
      env.draft.description = e.target.value.trim();
      env.render();
    },
  });
  return h(
    "div",
    { class: "card" },
    h(
      "div",
      { class: "card-body field-row" },
      field(
        "Display name",
        displayName,
        "Its name for people. Clients still use the alias.",
      ),
      field("Description", description),
    ),
  );
}

async function save(env) {
  const { ctx } = env;
  const saved = await api.putRouter(ctx.orgID, ctx.param, {
    display_name: env.draft.display_name,
    description: env.draft.description,
    start: clean(env.draft.start),
  });
  if (saved && saved.warning) {
    toast(
      "Router saved, but " +
        saved.warning +
        ". Change the list in the organisation's Guardrails.",
      "warn",
    );
  } else {
    toast("Router saved", "good");
  }
  ctx.reload();
}

// A slot is a place in the tree for a step: the start, a step's next or
// exceeded, or a route step's branch. Editing goes through slots, so a step
// never needs to know its parent.

function rootSlot(env) {
  return {
    get: () => env.draft.start,
    set: (s) => (env.draft.start = s || null),
  };
}

function fieldSlot(step, key) {
  return { get: () => step[key], set: (s) => (step[key] = s || undefined) };
}

function branchSlot(step, dest) {
  return {
    get: () => (step.branches || {})[dest],
    set: (s) => {
      step.branches = step.branches || {};
      if (s) step.branches[dest] = s;
      else delete step.branches[dest];
    },
  };
}

/** entry is where requests come in. Its + is the only way to add a step
 *  before the first, and the only + while the router is one model. */
function entry(env) {
  const slot = rootSlot(env);
  return h(
    "div",
    { class: "pl-entry" },
    h("div", { class: "pl-end pl-start" }, icon(icons.send), "Request"),
    h(
      "div",
      { class: "pl-edge" },
      // An empty start is filled through its own placeholder.
      env.canEdit && slot.get()
        ? addButton("Insert a step before the first one", () =>
            addStep(env, slot, { required: true }),
          )
        : null,
    ),
  );
}

/** renderSlot draws a slot's step and all after it, or how an empty branch
 *  ends. end says what an empty slot means: required, refuse, or a
 *  destination. */
function renderSlot(env, slot, end) {
  const step = slot.get();
  if (step) return renderChain(env, step, slot);
  if (end.dest) return endChip(env, end.dest);
  if (end.refuse) {
    return h(
      "div",
      { class: "pl-end pl-end-refuse" },
      icon(icons.close),
      "Refused",
    );
  }
  // The router cannot be saved until this is filled.
  return emptyCard({
    label: "Next step",
    name: env.canEdit ? "Choose a model or another step" : "Nothing here yet",
    problem: "Every branch has to end at a model.",
    title: "Choose what comes next",
    onClick: env.canEdit ? () => addStep(env, slot, end) : null,
  });
}

// modelGlyph is the provider's mark, or the plain model icon.
function modelGlyph(m) {
  return (m && m.provider && providerMark(m.provider)) || icon(icons.models);
}

// endChip is a destination with no steps of its own.
function endChip(env, dest) {
  const m = env.models.find((x) => x.alias === dest);
  return h(
    "div",
    { class: "pl-end" },
    modelGlyph(m),
    h("span", { class: "mono" }, dest),
    m && m.location
      ? h("span", { class: "faint" }, locationName(m.location))
      : null,
  );
}

function renderChain(env, step, slot) {
  const out = outgoing(env, step);
  return h(
    "div",
    { class: "pl-chain" },
    renderNode(env, step, slot),
    out ? h("div", { class: "pl-stem" }) : null,
    out,
  );
}

/** outgoing is a step's branches, each labelled with when it is taken. */
function outgoing(env, step) {
  const branches = [];
  if (passes(step.kind) && !limits(step.kind)) {
    branches.push(branch(env, "", fieldSlot(step, "next"), { required: true }));
  } else if (limits(step.kind)) {
    branches.push(
      branch(env, "within", fieldSlot(step, "next"), { required: true }),
      branch(env, "over limit", fieldSlot(step, "exceeded"), {
        refuse: true,
      }),
    );
  } else if (step.kind === "route") {
    for (const dest of step.destinations || []) {
      // A size step's ceilings, or a header step's values, decide which
      // branch a request takes.
      const ceiling = modeOf(step) === "size" && (step.ceilings || {})[dest];
      const values = modeOf(step) === "header" ? valuesFor(step, dest) : [];
      const label = ceiling
        ? `${dest} ≤ ${tokens(ceiling)}`
        : values.length
          ? `${dest} = ${values.join(", ")}`
          : dest;
      branches.push(branch(env, label, branchSlot(step, dest), { dest }));
    }
  }
  if (!branches.length) return null;
  return h("div", { class: "pl-branches" }, branches);
}

function branch(env, label, slot, end) {
  const filled = Boolean(slot.get());
  return h(
    "div",
    { class: "pl-branch" },
    h(
      "div",
      { class: "pl-edge" },
      label
        ? h(
            "span",
            {
              class:
                "pl-edge-label" +
                (end.refuse ? " pl-edge-over" : "") +
                (end.dest ? " mono" : ""),
            },
            label,
          )
        : null,
      // Before a filled slot, only steps that pass the request on; an empty
      // one may take any.
      env.canEdit && (filled || end.dest || end.refuse)
        ? addButton(
            filled ? "Insert a step here" : "Add steps to this branch",
            () => addStep(env, slot, end),
          )
        : null,
    ),
    renderSlot(env, slot, end),
  );
}

function renderNode(env, step, slot) {
  // A missing next step is marked on the empty slot after it, not here too.
  const problem =
    passes(step.kind) && !step.next ? "" : env.problems.get(step.id);
  return nodeCard({
    kind: step.kind,
    glyph:
      step.kind === "model"
        ? modelGlyph(env.models.find((x) => x.alias === step.ref))
        : KIND_ICONS[step.kind] || icons.more,
    label: KINDS[step.kind] || step.kind,
    name: nodeName(step),
    meta: nodeMeta(env, step),
    problem,
    title: "Change this step",
    onOpen: env.canEdit ? () => editStep(env, step, slot) : null,
    buttons: env.canEdit
      ? removeButton("Remove this step", () => removeStep(env, step, slot))
      : null,
  });
}

function nodeName(step) {
  switch (step.kind) {
    case "rate_limit":
      return rateShort(step).join(" · ") || "Not set";
    case "budget":
      return step.budget_micros > 0 ? budgetShort(step) : "Not set";
    case "route": {
      const n = (step.destinations || []).length;
      if (modeOf(step) === "header") {
        return h("span", {}, "By ", h("span", { class: "mono" }, step.header));
      }
      return decides(step)
        ? h("span", {}, "Decided by ", h("span", { class: "mono" }, step.model))
        : `${n} ${plural(n, "destination")}`;
    }
    case "system_prompt": {
      const text = oneLine(step.prompt || "", 60);
      if (!text) return h("span", { class: "faint" }, "Not written");
      return h("span", { class: "pl-prompt", title: step.prompt }, text);
    }
  }
  return step.ref
    ? h("span", { class: "mono" }, step.ref)
    : h("span", { class: "faint" }, "Not chosen");
}

// nodeMeta is the setting that decides what a step does.
function nodeMeta(env, step) {
  let bits = [];
  if (step.kind === "filter") {
    const f = env.filters.find((x) => x.alias === step.ref);
    if (f) bits = modePills(f);
  } else if (step.kind === "route") {
    bits = [pill(modeName(modeOf(step)), decides(step) ? "accent" : "")];
    if (chooses(step) && !step.fallback) {
      bits.push(pill("Refuses if undecided", "warn"));
    }
  } else if (step.kind === "model") {
    const m = env.models.find((x) => x.alias === step.ref);
    if (m && m.location) {
      bits = [
        pill(locationName(m.location), m.location === "onprem" ? "good" : ""),
      ];
    }
  } else if (limits(step.kind)) {
    bits = [
      step.exceeded
        ? pill("Reroutes when over")
        : pill("Refuses when over", "warn"),
    ];
  }
  return bits.length ? h("div", { class: "wrap-chips" }, bits) : null;
}

/** addStep puts a new step in a slot. A step that passes the request on goes
 *  in front of what is there. A route step moves it onto the branch of the
 *  destination it ends at, or else its first. In an empty branch the
 *  destination becomes a model step, so the branch still ends where it did. */
function addStep(env, slot, end) {
  const existing = slot.get();
  // An unchosen model, as a new router starts with, holds nothing to keep.
  const keep = existing && !(existing.kind === "model" && !existing.ref);
  const kinds = keep
    ? KIND_TILES.filter((t) => passes(t.value) || t.value === "route")
    : KIND_TILES;
  stepDialog(env, {
    kinds,
    title: keep ? "Insert a step" : "Add a step",
    preset: end.dest ? { model: end.dest } : {},
    onDone: (step) => {
      step.id = newID(env.draft.start);
      if (passes(step.kind)) {
        step.next =
          existing ||
          (end.dest
            ? { id: newID(env.draft.start) + "m", kind: "model", ref: end.dest }
            : undefined);
      }
      if (step.kind === "route" && keep) {
        const ends = steps(existing)
          .filter((x) => x.kind === "model")
          .map((x) => x.ref);
        const dest =
          step.destinations.find((d) => ends.includes(d)) ||
          step.destinations[0];
        // A branch that only sends to its own destination equals no branch.
        if (!(existing.kind === "model" && existing.ref === dest)) {
          step.branches = { [dest]: existing };
        }
      }
      slot.set(step);
      env.render();
    },
  });
}

function editStep(env, step, slot) {
  stepDialog(env, {
    step,
    title: "Change " + (KINDS[step.kind] || "step").toLowerCase(),
    onDone: (changed) => {
      Object.assign(step, changed);
      if (step.kind === "route" && step.branches) {
        for (const d of Object.keys(step.branches)) {
          if (!step.destinations.includes(d)) delete step.branches[d];
        }
      }
      slot.set(step);
      env.render();
    },
  });
}

/** removeStep takes a step out. A filter or a limit hands its place to the
 *  next step; a route step or a model takes what follows with it. */
function removeStep(env, step, slot) {
  const gone =
    steps(step).length - 1 - (passes(step.kind) ? steps(step.next).length : 0);
  const apply = () => {
    slot.set(passes(step.kind) ? step.next : null);
    env.render();
  };
  if (gone === 0) return apply();
  confirm({
    title: "Remove this step?",
    body: `${gone} ${plural(gone, "step")} after it, on ${
      step.kind === "route" ? "its branches" : "its over-limit branch"
    }, ${gone === 1 ? "goes" : "go"} with it. Nothing is saved until you press Save.`,
    confirmLabel: "Remove",
    danger: true,
    onConfirm: apply,
  });
}

/** stepDialog edits a step. An existing step keeps its kind, since what
 *  follows depends on it. */
function stepDialog(env, { step, kinds, title, preset = {}, onDone }) {
  let kind = step ? step.kind : "";
  const fieldsHost = h("div", {});
  let read = () => null;

  const choose = (value) => {
    kind = value;
    const built = fieldsFor(env, kind, step || {}, preset);
    fieldsHost.replaceChildren(built.el);
    // Space between a new step's kind and its fields.
    fieldsHost.style.marginTop = step ? "" : "14px";
    read = built.read;
  };

  let picker = null;
  if (!step) {
    picker = modeTiles(kinds, null, choose);
  } else {
    choose(kind);
  }

  formModal({
    wide: true,
    title,
    subtitle: step
      ? null
      : "Steps run left to right. A filter or a limit passes the request " +
        "on; a route step branches; a model ends the branch.",
    body: h(
      "form",
      {},
      picker ? picker.tiles : null,
      picker ? picker.chosenRow : null,
      fieldsHost,
    ),
    submitLabel: step ? "Apply" : "Add step",
    submit: () => {
      if (!kind) throw new Error("Choose what kind of step it is.");
      const values = read();
      onDone({ ...values, kind });
    },
  });
  if (picker) picker.tiles.firstChild.focus();
}

/** fieldsFor builds a step's fields; read returns their values or throws
 *  what is missing. */
function fieldsFor(env, kind, step, preset) {
  if (kind === "route") return routeFields(env.models, step);
  if (kind === "rate_limit") {
    return rateFields(
      step,
      ["minute", "day", "month"],
      "Tokens are counted when each request ends. A minute refills as it " +
        "goes. A day or a month starts again at midnight UTC or on the 1st, " +
        "and keeps its count across restarts.",
    );
  }
  if (kind === "system_prompt") {
    const prompt = h("textarea", {
      class: "input",
      rows: 6,
      placeholder: "Answer in German. Never name a customer.",
    });
    prompt.value = step.prompt || "";
    return {
      el: field(
        "Prompt",
        prompt,
        "Added after the key's own system prompt, on the requests that pass " +
          "this step. Like that one, it is added after every filter, so no " +
          "filter or route step reads or changes it.",
      ),
      read: () => {
        const text = prompt.value.trim();
        if (!text) throw new Error("Write the prompt.");
        return { prompt: text };
      },
    };
  }
  if (kind === "budget") {
    return budgetFields(
      step,
      ["month", "day"],
      "What requests through this step may cost, counted in full: the " +
        "answer, and every filter and decision before and after it. It " +
        "starts again at midnight UTC or on the 1st.",
    );
  }
  if (kind === "model") {
    const usable = env.models.filter(
      (m) => m.enabled !== false && m.kind === "chat" && !m.subscription,
    );
    const select = h(
      "select",
      { class: "select" },
      h("option", { value: "" }, "Choose a model"),
      usable.map((m) =>
        h(
          "option",
          {
            value: m.alias,
            selected: m.alias === (step.ref || preset.model),
          },
          m.alias + (m.location ? ` (${locationName(m.location)})` : ""),
        ),
      ),
    );
    return {
      el: field(
        "Model",
        select,
        "The request is sent here, after the key's own filters and system " +
          "prompt.",
      ),
      read: () => {
        if (!select.value) throw new Error("Choose a model.");
        return { ref: select.value };
      },
    };
  }
  // One of the organisation's filters, or a new one made here.
  const select = h("select", { class: "select" });
  const fill = (chosen) => {
    select.replaceChildren(
      h("option", { value: "" }, "Choose a filter"),
      ...env.filters.map((x) =>
        h(
          "option",
          { value: x.alias, selected: x.alias === chosen },
          x.alias + " (" + (x.mode || "rewrite") + ")",
        ),
      ),
    );
  };
  fill(step.ref);
  const create = h(
    "button",
    {
      class: "btn",
      type: "button",
      onClick: () =>
        newFilter(env.ctx, env.models, async (alias) => {
          env.filters = (await api.filters(env.ctx.orgID)).data || [];
          fill(alias);
          env.render();
        }),
    },
    icon(icons.plus),
    "New filter",
  );
  return {
    el: field(
      "Filter",
      h("div", { class: "row", style: { flexWrap: "wrap" } }, select, create),
      "It reads what the steps before it left. A new filter is created at " +
        "once, and stays if the router is not saved.",
    ),
    read: () => {
      if (!select.value) throw new Error("Choose a filter.");
      return { ref: select.value };
    },
  };
}

function answeredCard(destinations) {
  return h(
    "div",
    { class: "card" },
    h("div", { class: "card-head" }, h("h2", {}, "Answered by")),
    h(
      "div",
      { class: "card-body" },
      barList(destinations, {
        label: (d) => d.alias || "refused before a model",
        value: (d) => d.requests,
      }),
    ),
  );
}

// limitsCard shows how much of each budget, and each rate limit per day or
// month, is used. Each counts in its own window, not the period picked above.
// A limit is named by its kind alone, since step ids are never shown and the
// line below it already says how much it allows.
function limitsCard(limits, saved) {
  const rows = limits.map((l) => {
    const step = saved.find((s) => s.id === l.step);
    const kind = l.budget ? "budget" : "rate_limit";
    const full = step?.exceeded
      ? "used up, so requests take its exceeded branch until it resets"
      : "used up, so requests are refused until it resets";
    return h(
      "div",
      { class: "stack", style: { gap: "6px" } },
      h("strong", {}, KINDS[kind]),
      l.budget ? budgetUse(l.budget, full) : rateUse(l.rate, full),
    );
  });
  return h(
    "div",
    { class: "card" },
    h("div", { class: "card-head" }, h("h2", {}, "Limits")),
    h("div", { class: "card-body stack", style: { gap: "16px" } }, ...rows),
  );
}

// decisionBanners come before the numbers, since each changes how they read.
// A route step's failures are otherwise silent: every request still gets an
// answer.
function decisionBanners(rep, placed) {
  const out = [];
  if (!placed) return out;
  if (!rep.chose) {
    out.push(
      banner(
        "bad",
        "No route step got its first choice in this period. Each request was " +
          "placed by a fallback, or waited for a destination to fail. Check " +
          "the route steps and the models they use.",
      ),
    );
  } else if (rep.fell_back / placed >= 0.1) {
    out.push(
      banner(
        "warn",
        `${pct(rep.fell_back, placed)} of the requests a route step placed ` +
          "went somewhere other than its first choice: without a decision, " +
          "or after a destination failed. Not an error, but reported nowhere " +
          "else.",
      ),
    );
  }
  if (rep.errored > 0) {
    out.push(
      banner(
        "bad",
        `${num(rep.errored)} ${plural(rep.errored, "request")} could not be ` +
          "placed: refused for want of a fallback, or every destination " +
          "failed.",
      ),
    );
  }
  // One destination taking everything, on enough traffic to mean it, is a
  // hard-coded alias in disguise.
  const destinations = rep.destinations || [];
  if (rep.chose > 0 && destinations.length === 1 && placed >= 20) {
    out.push(
      banner(
        "warn",
        `Every request went to ${destinations[0].alias}. If a route step ` +
          "decides with a model, it costs a generation per request for the " +
          "same result as naming that model directly.",
      ),
    );
  }
  return out;
}
