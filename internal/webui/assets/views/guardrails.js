// The guardrails dialog, for every level that has them.
//
// Limits nest: a project's must fit inside its organisation's, and a key's
// inside its project's. So the ceiling is shown beside every field, rather
// than found out as a number that silently did not take effect.

import { api } from "../api.js";
import {
  h,
  modal,
  money,
  num,
  compact,
  pill,
  readRow,
  plural,
  field,
  formModal,
  banner,
} from "../ui.js";
import { orgNameOf } from "./orgs.js";

// One title at every level, so a ceiling and what it holds read as the same
// kind of thing. The subtitle says which scope is open.
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
    // Somebody who cannot edit wants to know what holds their key, not how to
    // set it.
    readLead:
      "Applies to this key, on top of its project's and organisation's " +
      "guardrails.",
  },
};

/** open shows the dialog for one scope.
 *
 *  ceilings is the chain above this scope, outermost first: [{label, limits}].
 *  It keeps "unlimited" honest: a key with no rpm of its own gets whatever its
 *  project allows.
 *
 *  canEdit false shows the limits read-only, as for every member: they are
 *  held by them, so they still get to see them. */
export async function openGuardrails(
  ctx,
  { scope, id, name, models, orgID, ceilings = [], canEdit = true },
) {
  // Filters and routers always belong to the organisation, whatever the
  // scope. Routers go in the allow-list because a client names a router where
  // it names a model.
  const [limits, filters, routers] = await Promise.all([
    api.guardrails(scope, id).catch(() => null),
    orgID
      ? api
          .filters(orgID)
          .then((r) => r.data || [])
          .catch(() => [])
      : [],
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
    // Routers are shaped like models so the code below need not tell them
    // apart.
    models: models.concat(
      routers.map((rt) => ({
        alias: rt.alias,
        kind: "chat",
        router: true,
        destinations: rt.destinations || [],
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

/** section is a band across the dialog. The bands let somebody find the
 *  budget without reading the models list first. Sandbox limits are set from
 *  the command line, so they have no band. */
function section(title, lead) {
  return h(
    "div",
    { class: "field-section" },
    h("h3", {}, title),
    lead ? h("p", {}, lead) : null,
  );
}

// OWNED_HERE is every field the edit dialog renders. Other fields must
// survive a save.
const OWNED_HERE = new Set([
  "allowed_models",
  "rpm",
  "tpm",
  "max_output_tokens",
  "system_prompt",
  "filters",
  "budget_micros",
  "budget_period",
  "allowed_tools",
]);

// LIMITS is shared so the form and the read-only view use the same words.
const LIMITS = {
  rpm: {
    label: "Requests per minute",
    hint: "Per gateway replica, unless the replicas share a Redis.",
  },
  tpm: {
    label: "Tokens per minute",
    hint: "Counted when each request finishes.",
  },
  max_output_tokens: {
    label: "Maximum output tokens per request",
    hint: "Requests asking for more are capped to this, not refused.",
  },
  system_prompt: {
    label: "System prompt",
    hint: "Added before the messages on every chat request.",
  },
};

function render(
  ctx,
  { scope, id, name, models, filters, ceilings, limits, orgID, orgName },
) {
  const meta = SCOPES[scope] || SCOPES.project;
  const lim = limits || {};

  const allowAll = h("input", {
    type: "checkbox",
    checked: !lim.allowed_models,
    onChange: () => {
      modelList.style.display = allowAll.checked ? "none" : "";
    },
  });
  const boxes = models.map((m) =>
    h(
      "label",
      { class: "row-tight", style: { padding: "3px 0" } },
      h("input", {
        type: "checkbox",
        value: m.alias,
        checked: !lim.allowed_models || lim.allowed_models.includes(m.alias),
      }),
      h("span", { class: "mono" }, m.alias),
      m.router
        ? // Ticking a router grants every model it may send to, by its own
          // list, not this one.
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
      allowedAbove(ceilings, m.alias)
        ? null
        : h("span", { class: "pill pill-warn" }, "Blocked above"),
    ),
  );
  const modelList = h(
    "div",
    {
      style: { display: lim.allowed_models ? "" : "none", marginTop: "6px" },
    },
    boxes.length
      ? boxes
      : h("span", { class: "faint" }, `${orgName} has no models.`),
  );

  // MCP tools are typed rather than ticked: the gateway does not list every
  // server's tools, and the control plane refuses a server it does not know.
  const allTools = h("input", {
    type: "checkbox",
    checked: !lim.allowed_tools,
    onChange: (e) => {
      toolList.style.display = e.target.checked ? "none" : "";
    },
  });
  const toolList = h("input", {
    class: "input mono",
    placeholder: "github/search_code, jira",
    value: (lim.allowed_tools || []).join(", "),
    style: { display: lim.allowed_tools ? "" : "none", marginTop: "6px" },
  });

  const rpm = numberInput(lim.rpm);
  const tpm = numberInput(lim.tpm);
  const maxOut = numberInput(lim.max_output_tokens);
  const budget = h("input", {
    class: "input",
    type: "number",
    min: "0",
    step: "0.01",
    placeholder: "not set here",
    value: lim.budget_micros ? lim.budget_micros / 1e6 : "",
  });
  // Filters are picked from a list, not typed: an unknown alias would refuse
  // every request, and the control plane refuses to store one.
  //
  // The picks are kept in an array because their order matters: each filter
  // reads what the one before left, so a gate before a rewrite judges the
  // original text and after it the redacted text. Aliases the organisation has
  // since dropped are left out.
  const picked = (lim.filters || []).filter((alias) =>
    (filters || []).some((f) => f.alias === alias),
  );

  // Shown only once two filters are picked.
  const order = h("div", { class: "stack", style: { gap: "4px" } });
  const orderField = field(
    "In this order",
    order,
    "Each filter gets the output of the one above. A gate placed first " +
      "saves the cost of the filters below when it refuses.",
  );
  const renderOrder = () => {
    orderField.hidden = picked.length < 2;
    if (picked.length < 2) return;
    const move = (i, by) => {
      const to = i + by;
      [picked[i], picked[to]] = [picked[to], picked[i]];
      renderOrder();
    };
    order.replaceChildren(
      ...picked.map((alias, i) => {
        const f = (filters || []).find((x) => x.alias === alias) || {};
        return h(
          "div",
          { class: "row-tight" },
          h("span", { class: "faint" }, i + 1 + "."),
          h("span", { class: "mono" }, alias),
          // The mode again, because it decides the order: a gate first saves
          // paying for rewrites it would refuse anyway.
          h(
            "span",
            { class: f.mode === "gate" ? "pill pill-accent" : "pill" },
            f.mode === "gate" || f.mode === "pattern" ? f.mode : "rewrite",
          ),
          f.shadow ? pill("Shadow", "warn") : null,
          h("div", { class: "spacer" }),
          h(
            "button",
            {
              class: "btn btn-sm",
              type: "button",
              title: "Run this one earlier",
              disabled: i === 0,
              onClick: () => move(i, -1),
            },
            "\u2191",
          ),
          h(
            "button",
            {
              class: "btn btn-sm",
              type: "button",
              title: "Run this one later",
              disabled: i === picked.length - 1,
              onClick: () => move(i, 1),
            },
            "\u2193",
          ),
        );
      }),
    );
  };

  const filterBoxes = (filters || []).map((f) => {
    const box = h("input", {
      type: "checkbox",
      value: f.alias,
      checked: picked.includes(f.alias),
      onChange: () => {
        // Append, so the ones already ordered do not move.
        if (box.checked) {
          if (!picked.includes(f.alias)) picked.push(f.alias);
        } else {
          const at = picked.indexOf(f.alias);
          if (at >= 0) picked.splice(at, 1);
        }
        renderOrder();
      },
    });
    return h(
      "label",
      { class: "row-tight", style: { padding: "3px 0" } },
      box,
      h("span", { class: "mono" }, f.alias),
      // Attaching a gate stops some requests; a rewrite filter does not.
      h(
        "span",
        { class: f.mode === "gate" ? "pill pill-accent" : "pill" },
        f.mode === "gate" || f.mode === "pattern" ? f.mode : "rewrite",
      ),
      // A filter in shadow costs a second generation per request and
      // protects from nothing.
      f.shadow ? pill("Shadow", "warn") : null,
      f.description ? h("span", { class: "faint small" }, f.description) : null,
    );
  });
  renderOrder();

  const systemPrompt = h("textarea", {
    class: "input",
    rows: "4",
    placeholder:
      "Answer in British English. Never suggest a dependency that is not already in the lockfile.",
  });
  systemPrompt.value = lim.system_prompt || "";

  const period = h(
    "select",
    { class: "select" },
    h(
      "option",
      { value: "month", selected: (lim.budget_period || "month") === "month" },
      "per month",
    ),
    h(
      "option",
      { value: "day", selected: lim.budget_period === "day" },
      "per day",
    ),
  );

  formModal({
    wide: true,
    title: `${meta.title} - ${name}`,
    subtitle:
      "Leave a field empty to set no limit here; a level above still applies.",
    body: h(
      "form",
      {},
      h("p", { class: "muted", style: { marginTop: 0 } }, meta.lead),
      ceilingSummary(ctx, ceilings),
      section("Rate limits"),
      h(
        "div",
        { class: "field-row" },
        field(LIMITS.rpm.label, rpm, [
          LIMITS.rpm.hint,
          ceilingNote(ceilings, "rpm", num),
        ]),
        field(LIMITS.tpm.label, tpm, [
          LIMITS.tpm.hint,
          ceilingNote(ceilings, "tpm", compact),
        ]),
      ),
      field(LIMITS.max_output_tokens.label, maxOut, [
        LIMITS.max_output_tokens.hint,
        ceilingNote(ceilings, "max_output_tokens", compact),
      ]),
      section("Cost"),
      field("Budget (CHF)", h("div", { class: "field-row" }, budget, period), [
        "Past this, requests get a 402 until the period resets. A busy " +
          "scope can overshoot slightly.",
        ceilingNote(ceilings, "budget_micros", money),
      ]),
      section("Every request", "Applied to each request."),
      field(LIMITS.system_prompt.label, systemPrompt, [
        LIMITS.system_prompt.hint +
          " The organisation's prompt is sent first, then the project's.",
        inheritedPrompts(ceilings),
      ]),
      h(
        "div",
        { class: "field" },
        h("label", {}, "Filters"),
        filterBoxes.length
          ? h("div", {}, filterBoxes)
          : h(
              "div",
              { class: "muted" },
              `${orgName} has no filters. Create one under Filters.`,
            ),
        h(
          "div",
          { class: "hint" },
          "Run in order before the request is forwarded. The " +
            "organisation's filters run first and cannot be removed here.",
          inheritedFilters(ceilings),
        ),
      ),
      orderField,
      section("Access", "Which models and routers this scope may reach."),
      h(
        "div",
        { class: "field" },
        h("label", {}, "Models"),
        h(
          "label",
          { class: "row-tight" },
          allowAll,
          `Every model of ${orgNameOf(ctx, orgID)}`,
        ),
        modelList,
        h(
          "div",
          { class: "hint" },
          "Removing one takes effect on the next request.",
        ),
      ),
      section("Tools", "What agents may use besides models."),
      h(
        "div",
        { class: "field" },
        h("label", {}, "MCP tools"),
        h(
          "label",
          { class: "row-tight" },
          allTools,
          "Every tool of every MCP server",
        ),
        toolList,
        h(
          "div",
          { class: "hint" },
          "A server alias allows all its tools; alias/tool allows one. " +
            "Tools left out are hidden from the agent and refused if called.",
        ),
      ),
    ),
    done: "Guardrails saved",
    ctx,
    submit: () => {
      // A PUT replaces the whole guardrail, so fields this dialog does not
      // show, such as sandbox limits, are carried through unchanged.
      const out = {};
      for (const [k, v] of Object.entries(lim)) {
        if (!OWNED_HERE.has(k)) out[k] = v;
      }
      if (!allowAll.checked) {
        // An alias the dialog has no box for, such as a disabled model, is
        // kept. Nobody unticked it.
        const shown = new Set(models.map((m) => m.alias));
        out.allowed_models = [
          ...boxes
            .filter((b) => b.querySelector("input").checked)
            .map((b) => b.querySelector("input").value),
          ...(lim.allowed_models || []).filter((a) => !shown.has(a)),
        ];
      }
      const put = (key, el) => {
        const v = parseInt(el.value, 10);
        if (Number.isFinite(v) && v > 0) out[key] = v;
      };
      put("rpm", rpm);
      put("tpm", tpm);
      put("max_output_tokens", maxOut);
      const prompt = systemPrompt.value.trim();
      if (prompt) out.system_prompt = prompt;
      if (picked.length) out.filters = [...picked];
      if (!allTools.checked) {
        out.allowed_tools = toolList.value
          .split(",")
          .map((t) => t.trim())
          .filter(Boolean);
      }
      const b = parseFloat(budget.value);
      if (Number.isFinite(b) && b > 0) {
        out.budget_micros = Math.round(b * 1e6);
        out.budget_period = period.value;
      }
      return api.putGuardrails(scope, id, out);
    },
  });
}

/* ------------------------------------------------------------- read-only */

/** renderReadOnly states what applies to a scope, for somebody who may not
 *  change it.
 *
 *  It is not the form with inputs disabled: that would show only this scope's
 *  own values. Each line here is the result of every level combined, with the
 *  level that decided it named. */
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
      h(
        "div",
        { class: "field-row" },
        limitRow(levels, "rpm", num),
        limitRow(levels, "tpm", compact),
      ),
      limitRow(levels, "max_output_tokens", compact),
      readRow(
        "Budget",
        budgetsValue(levels),
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
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Close"),
    ],
  });
}

function limitRow(levels, key, format) {
  return readRow(
    LIMITS[key].label,
    tightest(levels, key, format),
    LIMITS[key].hint,
  );
}

/** toolsValue shows each level's MCP tool list, outermost first, since they
 *  narrow each other. */
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

/** narrowedBy names the levels that removed models, so a short list reads
 *  as somebody's decision. */
function narrowedBy(levels) {
  const said = levels.filter((lv) => lv.limits && lv.limits.allowed_models);
  if (!said.length) return "Every model the organisation has.";
  return `Narrowed by ${said.map((lv) => lv.label).join(", then ")}.`;
}

/** tightest is the smallest value any level sets for one field: the one a
 *  request actually meets. */
function tightest(levels, field, fmt) {
  let value = null;
  let from = "";
  for (const lv of levels) {
    const v = lv.limits && lv.limits[field];
    if (v && (value === null || v < value)) {
      value = v;
      from = lv.label;
    }
  }
  if (value === null) return h("span", { class: "muted" }, "unlimited");
  return h(
    "span",
    {},
    fmt(value),
    h("span", { class: "hint-origin" }, ` - set by ${from}`),
  );
}

/** budgetsValue lists a budget per level rather than one number: each
 *  level's cap has to hold on its own. */
function budgetsValue(levels) {
  const said = levels.filter((lv) => lv.limits && lv.limits.budget_micros);
  if (!said.length) return h("span", { class: "muted" }, "no budget");
  return h(
    "div",
    { class: "stack" },
    said.map((lv) =>
      h(
        "span",
        {},
        `${money(lv.limits.budget_micros)} per ${lv.limits.budget_period || "month"}`,
        h("span", { class: "hint-origin" }, ` - set by ${lv.label}`),
      ),
    ),
  );
}

/** promptsValue shows each level's wording in full, in the order it is sent.
 *  A prompt is text added to the reader's own requests, so only the full text
 *  tells them anything. */
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

/* ------------------------------------------------------------ shared bits */

/** ceilingSummary states what already applies above this scope. */
function ceilingSummary(ctx, ceilings) {
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
  if (lim.rpm) bits.push(`${num(lim.rpm)}/min`);
  if (lim.tpm) bits.push(`${compact(lim.tpm)} tok/min`);
  if (lim.max_output_tokens) bits.push(`${compact(lim.max_output_tokens)} out`);
  if (lim.budget_micros) {
    bits.push(
      `${money(lim.budget_micros)} per ${lim.budget_period || "month"}`,
    );
  }
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

/** ceilingNote is the tightest value any level above sets for one field. */
function ceilingNote(ceilings, field, fmt) {
  let tightest = null;
  let from = "";
  for (const c of ceilings) {
    const v = c.limits && c.limits[field];
    if (v && (tightest === null || v < tightest)) {
      tightest = v;
      from = c.label;
    }
  }
  if (tightest === null) return null;
  return h("span", { class: "ceiling" }, ` ${from} allows ${fmt(tightest)}.`);
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
      lim.rpm ||
      lim.tpm ||
      lim.max_output_tokens ||
      lim.budget_micros ||
      lim.system_prompt ||
      (lim.filters || []).length)
  );
}

export function summarise(lim) {
  const bits = [];
  if (lim.rpm) bits.push(`${num(lim.rpm)}/min`);
  if (lim.tpm) bits.push(`${compact(lim.tpm)} tok/min`);
  if (lim.max_output_tokens) bits.push(`${compact(lim.max_output_tokens)} out`);
  if (lim.system_prompt) bits.push("system prompt");
  for (const name of lim.filters || []) bits.push(`filter: ${name}`);
  return bits;
}

/** modelsCell is shared by the projects and keys tables. */
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

function numberInput(value) {
  return h("input", {
    class: "input",
    type: "number",
    min: "0",
    step: "1",
    placeholder: "not set here",
    value: value || "",
  });
}
