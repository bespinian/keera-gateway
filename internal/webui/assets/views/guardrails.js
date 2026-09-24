// The guardrails dialog, for every level that has them.
//
// Limits nest: an organisation's are the ceiling a team's must fit inside, and
// a team's are the ceiling a key's must fit inside. That is the one thing about
// this screen a person has to hold in their head, so the ceiling is shown
// against every field rather than described in a sentence and discovered as a
// number that silently did not take effect.

import { api } from "../api.js";
import {
  h,
  modal,
  toast,
  money,
  num,
  compact,
  pill,
  showError,
} from "../ui.js";

// One word for the thing at every level. The dialog used to call itself
// "Organisation guardrails", "Guardrails" and "Limits" depending on which
// scope opened it, which made three things out of one: the nesting is the
// whole point, and a reader cannot see a ceiling and the thing it holds as the
// same kind of object while they are named differently. The scope is in the
// dialog's own subtitle, which already says which one is open.
const SCOPES = {
  org: {
    title: "Guardrails",
    lead:
      "The ceiling for the whole organisation. No team or key can go past " +
      "it.",
  },
  team: {
    title: "Guardrails",
    lead:
      "Applies to every key in the team. A team can narrow what the " +
      "organisation allows, not widen it.",
  },
  key: {
    title: "Guardrails",
    lead:
      "Applies to this key only. Useful for a key given to a contractor, " +
      "or one that should not use the team's whole budget.",
    // What to do with the field is not what somebody who cannot set it came
    // here to read; what holds their key is.
    readLead:
      "Applies to this key, on top of its team's and organisation's " +
      "guardrails.",
  },
};

/** open shows the dialog for one scope.
 *
 *  ceilings is the chain above this scope, outermost first: [{label, limits}].
 *  It is what makes "unlimited" honest - a key with no rpm of its own is
 *  whatever its team allows.
 *
 *  canEdit false opens the same guardrails to somebody who may not change
 *  them, which is every member. They are held by these limits, so not being
 *  allowed to set them is a reason to drop the Save button and not a reason to
 *  withhold the numbers. */
export async function openGuardrails(
  ctx,
  { scope, id, name, models, orgID, ceilings = [], canEdit = true },
) {
  // The filters are the organisation's, whatever scope this dialog is for: a
  // team applies one of its organisation's filters or none at all. The routers
  // are the organisation's for the same reason, and they are here because they
  // belong in the allow-list rather than in a section of their own - a client
  // names a router where it names a model, so "which models may this scope
  // use" is the question that also answers "may it route", and narrowing the
  // list to a router is how a scope is made to.
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
  // A save replaces the whole guardrail, so an editor opened over limits that
  // could not be read would write this dialog's fields over the rest of them.
  // Saying so is the only safe answer; a scope with no guardrail of its own
  // answers with an empty object rather than an error, so this really is a
  // failed read and not an empty one.
  if (limits === null) {
    modal({
      title: `Guardrails for ${name}`,
      body: h(
        "div",
        { class: "banner banner-bad" },
        "Could not load these guardrails. Saving now would replace them " +
          "with only what this dialog shows. Try again shortly.",
      ),
      actions: (close) => [
        h("button", { class: "btn", onClick: close }, "Close"),
      ],
    });
    return;
  }

  const args = {
    scope,
    id,
    name,
    // Routers are folded into the same list the catalogue's models are, in the
    // shape this dialog already reads, so that everything below counts and
    // renders them without knowing which is which.
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
  };
  if (canEdit) render(ctx, args);
  else renderReadOnly(ctx, args);
}

/** ceilingsFor loads the chain above a scope. It is separate from open so a
 *  caller that already has the org's limits does not fetch them twice. */
export async function ceilingsFor(scope, { orgID, orgName, teamID, teamName }) {
  const out = [];
  if (scope === "team" || scope === "key") {
    if (orgID) {
      const lim = await api.guardrails("org", orgID).catch(() => null);
      if (lim) out.push({ label: orgName || "the organisation", limits: lim });
    }
  }
  if (scope === "key" && teamID) {
    const lim = await api.guardrails("team", teamID).catch(() => null);
    if (lim) out.push({ label: teamName || "the team", limits: lim });
  }
  return out;
}

/** section is a band across the dialog.
 *
 *  A guardrail is one object with four unrelated halves in it - what may be
 *  reached, how fast, how much money, what every request passes through - and
 *  as eight fields in a column they read as eight unrelated settings. The
 *  bands are not a change to the object; they are the reason somebody who came
 *  to set a budget can find it without reading the models list first.
 *
 *  The sandbox half of a guardrail has no fields here at all - it is set from
 *  the command line - so there is no band for it rather than an empty one. */
function section(title, lead) {
  return h(
    "div",
    { class: "field-section" },
    h("h3", {}, title),
    lead ? h("p", {}, lead) : null,
  );
}

// OWNED_HERE is every field the edit dialog below renders. A guardrail carries
// more than these, and the ones it does not render have to survive a save.
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
  "block_hosted_tools",
]);

function render(ctx, { scope, id, name, models, filters, ceilings, limits }) {
  const meta = SCOPES[scope] || SCOPES.team;
  const lim = limits || {};
  const err = h("div");

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
        ? // A router is worth marking, because ticking it is not like ticking a
          // model: it grants everywhere the router may send, which is the
          // router's own list and not this one.
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
        : h("span", { class: "faint", style: { fontSize: "11.5px" } }, m.kind),
      allowedAbove(ceilings, m.alias)
        ? null
        : h("span", { class: "pill pill-warn" }, "blocked above"),
    ),
  );
  const modelList = h(
    "div",
    {
      style: { display: lim.allowed_models ? "" : "none", marginTop: "6px" },
    },
    boxes.length
      ? boxes
      : h("span", { class: "faint" }, "The catalogue is empty."),
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
  const blockHosted = h("input", {
    type: "checkbox",
    checked: !!lim.block_hosted_tools,
  });

  const rpm = numberInput(lim.rpm);
  const tpm = numberInput(lim.tpm);
  const maxOut = numberInput(lim.max_output_tokens);
  const budget = h("input", {
    class: "input",
    type: "number",
    min: "0",
    step: "0.01",
    placeholder: "unlimited",
    value: lim.budget_micros ? lim.budget_micros / 1e6 : "",
  });
  // A filter is chosen from the organisation's list rather than typed, because
  // an alias nothing answers to is a guardrail that refuses every request it
  // covers - and the control plane refuses to store one for that reason.
  //
  // What is chosen is held in an array rather than read off the boxes when
  // Save is pressed, because the order they run in is part of the answer and
  // not something to leave as a side effect of which box was ticked first:
  // each filter reads what the one before it left behind, so a gate in front
  // of a rewrite judges the text somebody wrote and the same gate behind it
  // judges the redacted version. An alias the organisation has since dropped
  // is left out here, the way the boxes leave it out.
  const picked = (lim.filters || []).filter((alias) =>
    (filters || []).some((f) => f.alias === alias),
  );

  // The order, shown once there is an order to get wrong. One filter has
  // nothing to be ahead of, and what runs before this scope's own filters is
  // already said under the boxes.
  const order = h("div", { class: "stack", style: { gap: "4px" } });
  const orderField = h(
    "div",
    { class: "field" },
    h("label", {}, "In this order"),
    order,
    h(
      "div",
      { class: "hint" },
      "Each filter gets the output of the one above. A gate placed first " +
        "saves the cost of the filters below when it refuses.",
    ),
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
          // The mode again, because it is what this list is ordered by: a gate
          // is worth putting ahead of the rewrites its refusals would
          // otherwise have made the deployment pay for.
          h(
            "span",
            { class: f.mode === "gate" ? "pill pill-accent" : "pill" },
            f.mode === "gate" || f.mode === "pattern" ? f.mode : "rewrite",
          ),
          f.shadow ? pill("shadow", "warn") : null,
          h("div", { style: { flex: 1 } }),
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
        // A filter ticked now goes to the end, which is the only place for it
        // that does not move the ones already ordered.
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
      // Which mode it is belongs here rather than only on the Filters screen:
      // attaching a gate to a scope is a decision to stop some of that scope's
      // requests, and attaching a rewrite filter is not.
      h(
        "span",
        { class: f.mode === "gate" ? "pill pill-accent" : "pill" },
        f.mode === "gate" || f.mode === "pattern" ? f.mode : "rewrite",
      ),
      // And whether it enforces at all, which matters more than the mode:
      // attaching a filter in shadow costs this scope a second generation per
      // request and protects it from nothing.
      f.shadow ? pill("shadow", "warn") : null,
      f.description
        ? h(
            "span",
            { class: "faint", style: { fontSize: "11.5px" } },
            f.description,
          )
        : null,
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

  modal({
    wide: true,
    title: `${meta.title} - ${name}`,
    subtitle: "Leave a field empty for unlimited.",
    body: h(
      "form",
      {},
      err,
      h("p", { class: "muted", style: { marginTop: 0 } }, meta.lead),
      ceilingSummary(ctx, ceilings),
      section("Access", "Which models and routers this scope may reach."),
      h(
        "div",
        { class: "field" },
        h("label", {}, "Models"),
        h(
          "label",
          { class: "row-tight" },
          allowAll,
          "Every model in the catalogue",
        ),
        modelList,
        h(
          "div",
          { class: "hint" },
          "Removing one takes effect on the next request.",
        ),
      ),
      section("Rate limits"),
      h(
        "div",
        { class: "field-row" },
        h(
          "div",
          { class: "field" },
          h("label", {}, "Requests per minute"),
          rpm,
          h(
            "div",
            { class: "hint" },
            "Per gateway replica.",
            ceilingNote(ceilings, "rpm", num),
          ),
        ),
        h(
          "div",
          { class: "field" },
          h("label", {}, "Tokens per minute"),
          tpm,
          h(
            "div",
            { class: "hint" },
            "Counted when each request finishes.",
            ceilingNote(ceilings, "tpm", compact),
          ),
        ),
      ),
      h(
        "div",
        { class: "field" },
        h("label", {}, "Maximum output tokens per request"),
        maxOut,
        h(
          "div",
          { class: "hint" },
          "Requests asking for more are capped to this, not refused.",
          ceilingNote(ceilings, "max_output_tokens", compact),
        ),
      ),
      section("Spend"),
      h(
        "div",
        { class: "field" },
        h("label", {}, `Budget (${ctx.currency})`),
        h("div", { class: "field-row" }, budget, period),
        h(
          "div",
          { class: "hint" },
          "Past this, requests get a 402 until the period resets. A busy " +
            "scope can overshoot slightly.",
          ceilingNote(ceilings, "budget_micros", (v) => money(v, ctx.currency)),
        ),
      ),
      section("Every request", "Applied to each request."),
      h(
        "div",
        { class: "field" },
        h("label", {}, "System prompt"),
        systemPrompt,
        h(
          "div",
          { class: "hint" },
          "Added before the messages on every chat request. The " +
            "organisation's prompt is sent first, then the team's.",
          inheritedPrompts(ceilings),
        ),
      ),
      h(
        "div",
        { class: "field" },
        h("label", {}, "Filters"),
        filterBoxes.length
          ? h("div", {}, filterBoxes)
          : h(
              "div",
              { class: "muted" },
              "This organisation has no filters. Add one under Filters.",
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
      section("Tools", "What agents may use besides models."),
      h(
        "div",
        { class: "field" },
        h("label", {}, "MCP tools"),
        h("label", { class: "row-tight" }, allTools, "Every tool of every MCP server"),
        toolList,
        h(
          "div",
          { class: "hint" },
          "A server alias allows all its tools; alias/tool allows one. " +
            "Tools left out are hidden from the agent and refused if called.",
        ),
      ),
      h(
        "div",
        { class: "field" },
        h(
          "label",
          { class: "row-tight" },
          blockHosted,
          "Take hosted tools out of every request",
        ),
        h(
          "div",
          { class: "hint" },
          "Web search, URL fetch or remote MCP servers that Anthropic or " +
            "OpenAI run on their side, out of reach of any guardrail. If an " +
            "outer level removes them, this cannot add them back.",
        ),
      ),
    ),
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Cancel"),
      h(
        "button",
        {
          class: "btn btn-primary",
          onClick: async (e) => {
            const button = e.currentTarget;
            button.disabled = true;
            // A PUT replaces the whole guardrail, and this dialog shows eight
            // of its fields. Everything else it holds - the sandbox half, set
            // from the command line - is carried through unchanged, so saving
            // a budget here does not quietly drop a sandbox quota set there.
            const out = {};
            for (const [k, v] of Object.entries(lim)) {
              if (!OWNED_HERE.has(k)) out[k] = v;
            }
            if (!allowAll.checked) {
              out.allowed_models = boxes
                .filter((b) => b.querySelector("input").checked)
                .map((b) => b.querySelector("input").value);
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
            if (blockHosted.checked) out.block_hosted_tools = true;
            const b = parseFloat(budget.value);
            if (Number.isFinite(b) && b > 0) {
              out.budget_micros = Math.round(b * 1e6);
              out.budget_period = period.value;
            }
            try {
              await api.putGuardrails(scope, id, out);
              close();
              toast("Guardrails saved", "good");
              ctx.reload();
            } catch (ex) {
              showError(err, ex.message);
              button.disabled = false;
            }
          },
        },
        "Save",
      ),
    ],
  });
}

/* ------------------------------------------------------------- read-only */

/** renderReadOnly states what applies to a scope, for somebody who may not
 *  change it.
 *
 *  It is not the form with its inputs disabled. That would hand a member
 *  fields to fill in that do nothing, and it would show them this scope's own
 *  row rather than what a request actually meets - which is every level at
 *  once. So each line here is the answer after the chain has been combined,
 *  with the level that decided it named beside it. */
function renderReadOnly(
  ctx,
  { scope, name, models, filters, ceilings, limits },
) {
  const meta = SCOPES[scope] || SCOPES.team;
  // Outermost first with this scope last, which is the order limits combine in
  // and the order the prompts are sent in.
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
      readRow("Models", modelsValue(levels, models), narrowedBy(levels)),
      h(
        "div",
        { class: "field-row" },
        readRow(
          "Requests per minute",
          tightest(levels, "rpm", num),
          "Per gateway replica.",
        ),
        readRow(
          "Tokens per minute",
          tightest(levels, "tpm", compact),
          "Counted when each request finishes.",
        ),
      ),
      readRow(
        "Maximum output tokens per request",
        tightest(levels, "max_output_tokens", compact),
        "Requests asking for more are capped to this, not refused.",
      ),
      readRow(
        "Budget",
        budgetsValue(levels, ctx.currency),
        "Each budget applies on its own. Past any of them, requests are " +
          "refused until the period resets.",
      ),
      readRow(
        "System prompt",
        promptsValue(levels),
        "Added before the messages on every chat request.",
      ),
      readRow(
        "Filters",
        filtersValue(levels, filters),
        "Run in this order before your requests are forwarded. If one " +
          "cannot run, the request is refused.",
      ),
      readRow(
        "MCP tools",
        toolsValue(levels),
        "Each level can only narrow the one above. Tools left out are " +
          "hidden from your agent and refused if called.",
      ),
      readRow(
        "Hosted tools",
        hostedValue(levels),
        "Web search and similar tools the provider runs on its side.",
      ),
    ),
    actions: (close) => [
      h("button", { class: "btn btn-primary", onClick: close }, "Close"),
    ],
  });
}

/** toolsValue is what each level allows of the MCP tools. They narrow each
 *  other, so every level that says anything is shown, outermost first. */
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

/** hostedValue says whether hosted tools are taken out, and by which level. */
function hostedValue(levels) {
  const by = levels.find((lv) => lv.limits && lv.limits.block_hosted_tools);
  if (!by) return h("span", { class: "muted" }, "Allowed");
  return h("span", {}, `Taken out, by ${by.label}`);
}

function readRow(label, value, hint) {
  return h(
    "div",
    { class: "field" },
    h("label", {}, label),
    h("div", {}, value),
    hint ? h("div", { class: "hint" }, hint) : null,
  );
}

/** effectiveModels is the allow-list every level has agreed on. Allow-lists
 *  intersect, so a level can drop a model and never add one; a level that
 *  names none has not restricted anything. */
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

function modelsValue(levels, models) {
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
      : "The catalogue is empty.",
  );
}

/** narrowedBy names the levels that took something out of the catalogue, so a
 *  short list of models reads as a decision somebody made rather than as the
 *  whole of what this deployment serves. */
function narrowedBy(levels) {
  const said = levels.filter((lv) => lv.limits && lv.limits.allowed_models);
  if (!said.length) return "Every model in the catalogue.";
  return `Narrowed by ${said.map((lv) => lv.label).join(", then ")}.`;
}

/** tightest is the smallest value any level sets for one field. Every level's
 *  value has to hold, so the smallest is the one a request actually meets. */
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

/** budgetsValue lists a budget per level rather than one number. Budgets are
 *  the one limit that is not collapsed: an organisation's 10,000 and a team's
 *  1,000 are two caps that both have to hold, not one. */
function budgetsValue(levels, currency) {
  const said = levels.filter((lv) => lv.limits && lv.limits.budget_micros);
  if (!said.length) return h("span", { class: "muted" }, "no budget");
  return h(
    "div",
    { class: "stack" },
    said.map((lv) =>
      h(
        "span",
        {},
        `${money(lv.limits.budget_micros, currency)} per ${lv.limits.budget_period || "month"}`,
        h("span", { class: "hint-origin" }, ` - set by ${lv.label}`),
      ),
    ),
  );
}

/** promptsValue shows each level's wording in full, in the order it is sent.
 *
 *  Every other line here is a number, and a summary of a number is the number.
 *  A system prompt is prose somebody else put into the reader's own requests,
 *  so "a system prompt" would say nothing they could act on. */
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

/** filtersValue lists the filters a request actually passes through.
 *
 *  Filters accumulate rather than collapse, so this is every level's in the
 *  order they run, each named with the level that put it there: which filters
 *  touched a request, and who decided they should.
 *
 *  known is the organisation's filters, for the one thing a guardrail row
 *  cannot say - whether the filter it names is enforcing. A member told their
 *  requests run through a filter that is in shadow has been told the opposite
 *  of what happens. */
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
          ? [" ", pill("shadow", "warn")]
          : null,
        h("span", { class: "hint-origin" }, ` - applied by ${f.from}`),
      ),
    ),
  );
}

/** inheritedFilters names the filters already running above this scope. They
 *  cannot be taken off here, so a form that did not show them would invite an
 *  administrator to add one that is already there. */
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

/** ceilingSummary states what is already true above this scope. Without it,
 *  every field on this form reads as though it is the only thing in the way. */
function ceilingSummary(ctx, ceilings) {
  const said = ceilings.filter((c) => hasAny(c.limits));
  if (!said.length) return null;
  return h(
    "div",
    { class: "banner banner-info" },
    "Limited by ",
    said.map((c, i) => [
      i > 0 ? ", then " : null,
      h("strong", {}, c.label),
      ": ",
      describe(c.limits, ctx.currency),
    ]),
    ". Values here can only narrow this.",
  );
}

function describe(lim, currency) {
  const bits = [];
  if (lim.allowed_models) {
    bits.push(
      lim.allowed_models.length
        ? `${lim.allowed_models.length} model${lim.allowed_models.length === 1 ? "" : "s"}`
        : "no model",
    );
  }
  if (lim.rpm) bits.push(`${num(lim.rpm)}/min`);
  if (lim.tpm) bits.push(`${compact(lim.tpm)} tok/min`);
  if (lim.max_output_tokens) bits.push(`${compact(lim.max_output_tokens)} out`);
  if (lim.budget_micros) {
    bits.push(
      `${money(lim.budget_micros, currency)} per ${lim.budget_period || "month"}`,
    );
  }
  if (lim.system_prompt) bits.push("a system prompt");
  if ((lim.filters || []).length) {
    bits.push(
      `${lim.filters.length} filter${lim.filters.length === 1 ? "" : "s"}`,
    );
  }
  return bits.join(" · ") || "no limits";
}

/** inheritedPrompts shows the wording the levels above already send.
 *
 *  Every other field on this form is narrowed by what is above it, and a number
 *  in a hint says enough. A prompt is appended to what is above it, so a team
 *  that cannot read the organisation's wording would write it out again. */
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

/** summarise renders a scope's limits for a table cell. */
export function summarise(lim) {
  const bits = [];
  if (lim.rpm) bits.push(`${num(lim.rpm)}/min`);
  if (lim.tpm) bits.push(`${compact(lim.tpm)} tok/min`);
  if (lim.max_output_tokens) bits.push(`${compact(lim.max_output_tokens)} out`);
  if (lim.system_prompt) bits.push("system prompt");
  for (const name of lim.filters || []) bits.push(`filter: ${name}`);
  return bits;
}

/** modelsCell renders an allow-list the way both the teams and keys tables do. */
export function modelsCell(lim) {
  if (!lim || !lim.allowed_models) return h("span", { class: "muted" }, "all");
  return h(
    "div",
    { class: "wrap-chips" },
    lim.allowed_models.length
      ? lim.allowed_models.map((m) => pill(m, "accent"))
      : pill("none", "bad"),
  );
}

function numberInput(value) {
  return h("input", {
    class: "input",
    type: "number",
    min: "0",
    step: "1",
    placeholder: "unlimited",
    value: value || "",
  });
}
