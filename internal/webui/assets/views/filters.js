// Filters rewrite, gate or pattern-match a request before it is forwarded.
// A filter that cannot run refuses what it covers, so a broken one must be
// visible at a glance. Its instruction is prose, so Check runs it to show what
// it does.

import { api } from "../api.js";
import {
  h,
  table,
  modal,
  pill,
  stat,
  meter,
  empty,
  rowLink,
  compact,
  num,
  money,
  amount,
  moneyStat,
  ms,
  dateTime,
  icon,
  icons,
  aliasProblem,
  takenProblem,
  currentRange,
  rangePicker,
  isAdmin,
  checkButton,
  plural,
  pct,
  field,
  modeTiles,
  rowActions,
  listHead,
  sectionHead,
  dash,
  trafficSummary,
  formModal,
  detailHead,
  busyButton,
  banner,
} from "../ui.js";
import { areaChart, barList } from "../chart.js";
import { orgNameOf } from "./orgs.js";
// A filter's screen shares the period with the project, key and model
// screens, so moving between them keeps it.
import { facts } from "./detail.js";

export async function filtersView(ctx) {
  const since = currentRange();
  const [res, models] = await Promise.all([
    api.filters(ctx.orgID, { stats: true, since }),
    api.models(ctx.orgID).then((r) => r.data || []),
  ]);
  const filters = res.data || [];
  const stats = res.stats || {};
  // A subscription model only answers its caller's own sign-in, so no
  // filter can run on it.
  const readers = models.filter(canRun);
  const canEdit = isAdmin(ctx);
  ctx.setSubtitle(`${filters.length} ${plural(filters.length, "filter")}`);

  const head = listHead(
    null,
    [rangePicker(ctx, since)],
    canEdit && {
      label: "Create filter",
      onClick: () => editFilter(ctx, null, readers),
    },
  );

  const known = new Set(models.map((m) => m.alias));
  const rows = table(
    [
      {
        label: "Alias",
        sortKey: (f) => f.alias,
        cell: (f) =>
          rowLink(
            ctx,
            "/filters/" + encodeURIComponent(f.alias),
            h("strong", { class: "mono" }, f.alias),
            "What this filter did to real requests",
          ),
      },
      {
        label: "Mode",
        shrink: true,
        sortKey: (f) => modeName(f),
        cell: (f) => h("div", { class: "row-tight" }, modePills(f)),
      },
      {
        label: "Runs on",
        shrink: true,
        sortKey: (f) => (usesModel(f) ? f.model : null),
        cell: (f) => {
          // A pattern filter runs no model; say what it holds rather than
          // leave the cell looking unfinished.
          if (!usesModel(f)) {
            const rules = (f.rules || []).length;
            return h(
              "span",
              { class: "faint nowrap" },
              `${rules} ${plural(rules, "rule")}, no model`,
            );
          }
          return known.has(f.model)
            ? h("span", { class: "mono muted" }, f.model)
            : h(
                "div",
                { class: "row-tight" },
                h("span", { class: "mono muted" }, f.model),
                pill("Missing", "bad"),
              );
        },
      },
      {
        label: "In this period",
        shrink: true,
        sortKey: (f) => stats[f.alias]?.runs || null,
        sortDir: "desc",
        cell: (f) => trafficCell(stats[f.alias]),
      },
      {
        label: "Description",
        sortKey: (f) => f.description || null,
        cell: (f) =>
          f.description
            ? h("span", { class: "muted clamp-2" }, f.description)
            : dash(),
      },
      canEdit && {
        label: "Check",
        shrink: true,
        cell: (f) => checkCell(ctx, f),
      },
      canEdit && {
        label: "",
        shrink: true,
        cell: (f) =>
          rowActions(ctx, f.alias, {
            editTitle: "Edit this filter",
            onEdit: () => editFilter(ctx, f, readers),
            deleteTitle: "Delete this filter",
            body:
              "A filter still used by a guardrail cannot be deleted. " +
              "Take it off those guardrails first; the error lists them.",
            confirmLabel: "Delete filter",
            remove: () => api.deleteFilter(ctx.orgID, f.alias),
          }),
      },
    ].filter(Boolean),
    filters,
    {
      emptyTitle: "No filters yet",
      emptyBody:
        "Filters can block or rewrite prompts for improved privacy and " +
        "security.",
    },
  );

  return h("div", {}, head, rows);
}

// A filter from before modes existed has none, and it rewrote.
function mode(f) {
  return f.mode === "gate" || f.mode === "pattern" ? f.mode : "rewrite";
}

function usesModel(f) {
  return mode(f) !== "pattern";
}

function modeName(f) {
  return MODES.find((m) => m.value === mode(f)).name;
}

// modePills adds Shadow when it applies: a gate in shadow enforces nothing.
function modePills(f) {
  const tone = { gate: "accent", pattern: "good" }[mode(f)] || "";
  return [
    pill(modeName(f), tone),
    f.shadow ? pill("Shadow", "warn") : null,
  ].filter(Boolean);
}

function trafficCell(st) {
  if (!st || !st.runs) return trafficSummary(0);
  const refusalRate = st.refused / st.runs;
  const tone = st.errored > 0 ? "bad" : refusalRate >= 0.05 ? "warn" : "";
  return trafficSummary(
    st.runs,
    "runs",
    st.errored > 0
      ? pill(`${num(st.errored)} could not run`, "bad")
      : st.refused > 0
        ? pill(`${pct(st.refused, st.runs)} refused`, tone)
        : null,
    [ms(st.latency_median_ms), " added · ", money(st.cost_micros)],
  );
}

// The result is not cached: a small model may answer differently next time.
function checkCell(ctx, f) {
  return checkButton(
    "Run this filter on a sample",
    () => api.checkFilter(ctx.orgID, f.alias),
    (probe) => verdict(probe),
  );
}

function verdict(probe) {
  const warned = (probe.warnings || []).length > 0;
  // The sample holds things to refuse, so a refusal may mean it works; it
  // still gets its own label.
  const tone = probe.ok ? (probe.refused || warned ? "warn" : "good") : "bad";
  const label = !probe.ok
    ? "Not usable"
    : probe.refused
      ? "Refused the sample"
      : warned
        ? "Working, with notes"
        : "Working";
  return {
    label,
    tone,
    title: "What the filter did to the sample",
    open: () => report(probe),
  };
}

// The sample is shown before and after: only the reader can judge whether
// the right things were removed.
function report(probe) {
  modal({
    wide: true,
    title: `Check - ${probe.alias}`,
    subtitle: !probe.ok
      ? probe.shadow
        ? "This filter cannot run. It is in shadow, so requests go on as sent."
        : "This filter would refuse every request its guardrails cover."
      : probe.mode === "gate"
        ? "The gate judged two requests: one it should stop, one it should not."
        : probe.refused
          ? "The filter refused the sample. Nothing would be forwarded."
          : probe.mode === "pattern"
            ? "The rules ran on the sample. Real requests with the same text " +
              "get the same result."
            : "The filter ran and returned every segment. Check what it changed.",
    body: h(
      "div",
      {},
      // A check runs even in shadow, where nothing below would really happen.
      // Mistaking such a refusal for a real one is costly.
      probe.shadow
        ? banner(
            "warn",
            "This filter is in shadow. Nothing below would be acted on: " +
              "requests go on as sent.",
          )
        : null,
      probe.error
        ? banner("bad", probe.error)
        : probe.refused
          ? banner(
              "warn",
              h(
                "div",
                {},
                "It refused instead of rewriting. A request like this would " +
                  "be dropped with a 403 and this reason:",
              ),
              h(
                "div",
                { class: "prompt-text", style: { marginTop: "8px" } },
                probe.refusal || "REFUSED, no reason given",
              ),
              h(
                "div",
                { class: "hint" },
                "The samples contain a credential and a client name, so a " +
                  "refusal may be right. But this run cannot show a rewrite.",
              ),
            )
          : banner(
              "good",
              "It answered in the expected format and returned the whole sample.",
            ),
      (probe.warnings || []).map((w) => banner("warn", w)),
      (probe.verdicts || []).map((v) =>
        h(
          "div",
          { class: "field" },
          h(
            "label",
            {},
            v.expect_refusal
              ? "A credential and a named client"
              : "Ordinary source code",
            " ",
            pill(
              v.refused ? "Refused" : "Allowed",
              v.refused === v.expect_refusal ? "good" : "warn",
            ),
            // A gate right on both halves at 55% cannot tell them apart; the
            // pill alone hides that. Absent without backend probabilities.
            v.confidence
              ? h(
                  "span",
                  { class: "muted", style: { marginLeft: "8px" } },
                  `${Math.round(v.confidence * 100)}% sure`,
                )
              : null,
          ),
          v.sent.map((sent) => h("div", { class: "prompt-text" }, sent)),
          v.reason
            ? h(
                "div",
                {},
                h(
                  "div",
                  { class: "hint" },
                  "the gate's reason, shown to the sender",
                ),
                h("div", { class: "prompt-text" }, v.reason),
              )
            : h(
                "div",
                { class: "hint" },
                v.refused
                  ? "Refused with no reason given."
                  : "Allowed through untouched.",
              ),
        ),
      ),
      // Shows which pattern rule did what.
      (probe.hits || []).length
        ? h(
            "div",
            { class: "field" },
            h("label", {}, "Rules that matched"),
            h(
              "div",
              { class: "stack", style: { gap: "4px" } },
              probe.hits.map((hit) =>
                h(
                  "div",
                  { class: "row-tight" },
                  h("code", { class: "mono" }, hit.rule),
                  pill(
                    `${hit.matches} ${plural(hit.matches, "match", "matches")}`,
                    hit.refused ? "bad" : "accent",
                  ),
                  hit.refused ? pill("Refused the request", "bad") : null,
                ),
              ),
            ),
            h(
              "div",
              { class: "hint" },
              "No other rule matched the sample. They may still match real " +
                "traffic.",
            ),
          )
        : null,
      (probe.segments || []).map((seg, i) =>
        h(
          "div",
          { class: "field" },
          h(
            "label",
            {},
            `Sample ${i + 1}`,
            " ",
            seg.changed ? pill("Rewritten", "accent") : pill("Unchanged"),
          ),
          h("div", { class: "prompt-text" }, seg.before),
          h("div", { class: "hint" }, "came back as"),
          h("div", { class: "prompt-text" }, seg.after),
        ),
      ),
      probe.mode === "gate"
        ? h(
            "div",
            { class: "hint", style: { marginTop: "14px" } },
            "A gate answers once per request, so it is checked twice. The " +
              "second check matters most: a gate that refuses ordinary code " +
              "blocks everyone the guardrail covers.",
          )
        : probe.mode === "pattern" && !probe.refused
          ? h(
              "div",
              { class: "hint", style: { marginTop: "14px" } },
              "The third sample is ordinary code no filter should touch. A " +
                "rule that matches it will also change real source code, and " +
                "nothing will report it.",
            )
          : probe.refused
            ? null
            : h(
                "div",
                { class: "hint", style: { marginTop: "14px" } },
                "The third sample is ordinary code no filter should touch. If " +
                  "it changed, the instruction will also rewrite real source " +
                  "code, and nothing will report it.",
              ),
      probe.total_ms
        ? h(
            "div",
            { class: "hint" },
            "One run took ",
            ms(probe.total_ms),
            probe.cost_micros
              ? [
                  " and cost ",
                  money(probe.cost_micros),
                  ". Every request it covers pays this on top.",
                ]
              : ". Every request it covers waits this long on top.",
          )
        : null,
    ),
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Close"),
    ],
  });
}

/* ------------------------------------------------------- one filter's screen */

// filterDetailView shows what a filter has been doing over its own runs: is
// it firing, what does it do, what does it cost in time and money, and which
// project gets the refusals.
export async function filterDetailView(ctx) {
  const since = currentRange();
  const res = await api.filterReport(ctx.orgID, ctx.param, since);
  const rep = res.report || {};
  const f = res.filter || null;
  const projectNames = res.project_names || {};
  const canEdit = isAdmin(ctx);

  ctx.setTitle(ctx.param, f ? identity(f) : `no longer in ${orgNameOf(ctx)}`);

  const wrap = h(
    "div",
    {},
    detailHead(
      ctx,
      ["/filters", "Filters"],
      [
        f ? modePills(f) : pill(`Deleted from ${orgNameOf(ctx)}`, "warn"),
        f && f.model ? pill("Runs on " + f.model) : null,
      ],
      [
        rangePicker(ctx, since),
        f && canEdit
          ? busyButton(
              { title: "Run this filter on a sample" },
              "Check",
              async () => report(await api.checkFilter(ctx.orgID, f.alias)),
            )
          : null,
        f && canEdit
          ? busyButton(
              { title: "Edit this filter" },
              [icon(icons.pencil), "Edit"],
              async () => {
                const r = await api.models(ctx.orgID);
                editFilter(ctx, f, (r.data || []).filter(canRun));
              },
            )
          : null,
      ],
    ),
  );

  for (const banner of detailBanners(rep, f)) wrap.append(banner);

  // "Fired" leads because the rest means nothing without it: 100% refused
  // over four runs is a filter nobody has used yet.
  wrap.append(
    h(
      "div",
      { class: "grid grid-4" },
      stat(
        "Fired",
        compact(rep.runs || 0),
        rep.requests
          ? `${pct(rep.runs, rep.requests)} of ${compact(rep.requests)} requests`
          : "no requests in this period",
      ),
      stat(
        f && f.shadow ? "Would refuse" : "Refused",
        pct(rep.refused, rep.runs),
        rep.refused
          ? `${num(rep.refused)} ${plural(rep.refused, "request")} ` +
              (f && f.shadow ? "would have been dropped" : "dropped")
          : "nothing refused",
      ),
      stat(
        "Added wait",
        ms(rep.latency_median_ms || 0),
        `p95 ${ms(rep.latency_p95_ms || 0)}, per request`,
      ),
      moneyStat(
        "Cost",
        rep.cost_micros || 0,
        rep.org_cost_micros
          ? `${pct(rep.cost_micros, rep.org_cost_micros)} of the organisation's bill`
          : "nothing charged yet",
      ),
    ),
  );

  wrap.append(
    h(
      "div",
      { class: "grid grid-2", style: { marginTop: "16px" } },
      h(
        "div",
        { class: "card" },
        h(
          "div",
          { class: "card-head" },
          h("h2", {}, "When it fired"),
          h("div", { class: "spacer" }),
          rep.runs
            ? pill(`${compact(rep.runs)} ${plural(rep.runs, "run")}`)
            : null,
        ),
        h(
          "div",
          { class: "card-body" },
          rep.runs
            ? // Refusals take the failure bars: they are rare and would
              // vanish on the same scale.
              areaChart(
                (rep.series || []).map((p) => ({
                  at: p.at,
                  requests: p.runs,
                  failed: p.refused + p.errored,
                  tokens: 0,
                  cost_micros: p.cost_micros,
                })),
                { bucket: rep.bucket },
              )
            : empty(
                "Nothing ran in this period",
                f
                  ? "No guardrail uses this filter, or nothing it covers was " +
                      "called. Widen the period or check the project's guardrails."
                  : "This filter no longer exists.",
              ),
        ),
      ),
      h(
        "div",
        { class: "card" },
        h("div", { class: "card-head" }, h("h2", {}, "What it did")),
        h(
          "div",
          { class: "card-body" },
          rep.runs
            ? h(
                "div",
                {},
                barList(outcomeRows(rep), {
                  label: (r) => r.label,
                  value: (r) => r.value,
                  color: (r) => r.color,
                }),
                // Shows whether an instruction is too eager: changing one
                // segment of forty is fine, thirty-eight is not.
                rep.segments
                  ? h(
                      "div",
                      { class: "hint", style: { marginTop: "12px" } },
                      `It was shown ${num(rep.segments)} ` +
                        `${plural(rep.segments, "segment")} of text and changed ` +
                        `${num(rep.changed)} of them (${pct(rep.changed, rep.segments)}). `,
                    )
                  : null,
              )
            : empty(
                "Nothing to show",
                "No run of this filter is in this period.",
              ),
        ),
      ),
    ),
  );

  // Refusals by project: 4% across an organisation can be a whole project's
  // working day.
  if ((rep.projects || []).length > 0) {
    wrap.append(
      h(
        "div",
        { style: { marginTop: "20px" } },
        sectionHead(
          "Who it happened to",
          h("span", { class: "faint small" }, "refusals by project"),
        ),
        projectTable(ctx, rep.projects, projectNames),
      ),
    );
  }

  if (f) {
    wrap.append(
      h(
        "div",
        { class: "grid grid-2", style: { marginTop: "16px" } },
        h(
          "div",
          { class: "card" },
          h("div", { class: "card-head" }, h("h2", {}, "What this filter is")),
          h(
            "div",
            { class: "card-body" },
            facts([
              ["Mode", h("div", { class: "row-tight" }, modePills(f))],
              [
                "Runs on",
                usesModel(f)
                  ? h("span", { class: "mono muted" }, f.model)
                  : h(
                      "span",
                      { class: "muted" },
                      "nothing - the rules run in the gateway and cost nothing",
                    ),
              ],
              [
                "Enforcing",
                f.shadow
                  ? h(
                      "span",
                      { class: "muted" },
                      "no - it runs, but the request goes on as sent",
                    )
                  : h(
                      "span",
                      { class: "muted" },
                      {
                        gate: "yes - a refusal drops the request",
                        pattern:
                          "yes - the model gets the replaced text, and a REFUSE rule drops the request",
                      }[mode(f)] || "yes - the model gets the rewrite",
                    ),
              ],
              [
                "Description",
                f.description
                  ? h("span", { class: "muted" }, f.description)
                  : dash(),
              ],
              [
                "Last changed",
                h("span", { class: "muted" }, dateTime(f.updated_at)),
              ],
            ]),
          ),
        ),
        h(
          "div",
          { class: "card" },
          h(
            "div",
            { class: "card-head" },
            h("h2", {}, usesModel(f) ? "Instruction" : "Rules"),
          ),
          h(
            "div",
            { class: "card-body" },
            h(
              "div",
              { class: "prompt-text" },
              usesModel(f) ? f.prompt : formatRules(f.rules),
            ),
          ),
        ),
      ),
    );
  }

  wrap.append(
    h(
      "div",
      { class: "hint", style: { marginTop: "16px" } },
      "No request text is stored. These are counts only: segments seen, " +
        "segments changed, and the outcome.",
    ),
  );
  return wrap;
}

// These come before the numbers, since each changes what they mean.
function detailBanners(rep, f) {
  const out = [];
  const style = { marginBottom: "16px" };
  if (!f) {
    out.push(
      banner(
        "warn",
        { style },
        "This filter no longer exists. Below is the traffic it saw before " +
          "it was deleted.",
      ),
    );
  } else if (f.shadow) {
    out.push(
      banner(
        "info",
        { style },
        h(
          "div",
          {},
          h("strong", {}, "This filter is in shadow."),
          " It runs but enforces nothing: the refusals below did not " +
            "happen, the rewrites were discarded, and every request reached " +
            "the model as sent.",
        ),
        h(
          "div",
          { class: "hint" },
          "The cost is real: the generation runs and is charged. When the " +
            "numbers look right, edit the filter and turn shadow off.",
        ),
      ),
    );
  }
  if (rep.errored > 0) {
    out.push(
      banner(
        "bad",
        { style },
        `${num(rep.errored)} ${plural(rep.errored, "run")} failed. ` +
          (f && f.shadow
            ? "In shadow those requests were still forwarded, but they are " +
              "missing from these numbers."
            : "Those requests were refused, not sent unfiltered. Check the " +
              "model this filter runs on."),
      ),
    );
  }
  if (rep.shadowed > 0 && rep.shadowed < rep.runs) {
    out.push(
      banner(
        "warn",
        { style },
        `${num(rep.shadowed)} of these runs were in shadow: the filter was ` +
          "switched during this period, so the refusal rate mixes both modes.",
      ),
    );
  }
  return out;
}

// Each outcome has a fixed colour, so a pass is always neutral and an error
// always red.
function outcomeRows(rep) {
  return [
    {
      label: "Passed through untouched",
      value: rep.passed || 0,
      color: "var(--muted)",
    },
    { label: "Rewritten", value: rep.rewrote || 0, color: "var(--s1)" },
    { label: "Refused", value: rep.refused || 0, color: "var(--warn)" },
    { label: "Could not run", value: rep.errored || 0, color: "var(--bad)" },
  ].filter((r) => r.value > 0);
}

function projectTable(ctx, projects, names) {
  return table(
    [
      {
        label: "Project",
        cell: (t) =>
          t.project_id
            ? rowLink(
                ctx,
                "/projects/" + encodeURIComponent(t.project_id),
                names[t.project_id] || t.project_id,
                "This project's traffic",
              )
            : h("span", { class: "faint" }, "No project"),
      },
      { label: "Runs", shrink: true, num: true, cell: (t) => compact(t.runs) },
      {
        label: "Refused",
        shrink: true,
        cell: (t) => {
          const rate = t.runs ? t.refused / t.runs : 0;
          const tone = rate >= 0.2 ? "bad" : rate >= 0.05 ? "warn" : "";
          return h(
            "div",
            { class: "row-tight" },
            meter(rate, tone),
            h(
              "span",
              { class: "nowrap" },
              `${num(t.refused)} · ${pct(t.refused, t.runs)}`,
            ),
          );
        },
      },
      {
        label: "Rewritten",
        shrink: true,
        num: true,
        cell: (t) => compact(t.rewrote),
      },
      {
        label: "Could not run",
        shrink: true,
        num: true,
        cell: (t) =>
          t.errored
            ? h("span", { class: "pill pill-bad" }, num(t.errored))
            : dash(),
      },
      {
        label: "Cost (CHF)",
        shrink: true,
        num: true,
        cell: (t) =>
          h("span", { class: "nowrap muted" }, amount(t.cost_micros)),
      },
    ],
    projects,
    { emptyTitle: "Nobody yet" },
  );
}

function identity(f) {
  const what = usesModel(f) ? `${mode(f)} on ${f.model}` : mode(f);
  return f.shadow ? `${what}, in shadow` : what;
}

const MODES = [
  {
    value: "rewrite",
    icon: icons.rewrite,
    name: "Rewrite",
    what: "A model edits the request. Its edit is what gets sent.",
  },
  {
    value: "gate",
    icon: icons.gate,
    name: "Gate",
    what: "A model passes or refuses the request, without editing it.",
  },
  {
    value: "pattern",
    icon: icons.pattern,
    name: "Pattern",
    what: "Regular expressions over the text. No model, no cost.",
  },
];

// canRun: a chat model, or for a gate a decision model, which answers the
// verdict and writes no text.
function canRun(m) {
  return (m.kind === "chat" || m.kind === "decision") && !m.subscription;
}

function editFilter(ctx, existing, readers) {
  const creating = !existing;
  // No default mode: choosing one is the first question the dialog asks.
  const f = existing || {
    alias: "",
    model: "",
    shadow: true,
    prompt: "",
    description: "",
  };

  const alias = h("input", {
    class: "input mono",
    placeholder: "redact-secrets",
    value: f.alias,
    disabled: !creating,
  });
  const description = h("input", {
    class: "input",
    placeholder:
      "Takes credentials and client names out of anything leaving the cluster",
    value: f.description || "",
  });
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
        { value: m.alias, selected: m.alias === f.model },
        m.alias +
          (m.kind === "decision" ? " - decision model, gates only" : "") +
          (m.max_context_tokens
            ? ` - ${compact(m.max_context_tokens)} context`
            : ""),
      ),
    ),
  );
  const prompt = h("textarea", { class: "input", rows: "8" });

  // A new filter starts in shadow: nobody's first instruction is right, and
  // finding out on live traffic is costly.
  const enforceSelect = h(
    "select",
    { class: "select" },
    h(
      "option",
      { value: "shadow", selected: !!f.shadow },
      "Shadow - run, but enforce nothing",
    ),
    h(
      "option",
      { value: "enforce", selected: !f.shadow },
      "Enforcing - act on its answer",
    ),
  );
  const enforceHint = h("div", { class: "hint" });

  // These hints and the example follow the mode. The example is only swapped
  // while unedited.
  const modelHint = h("div", { class: "hint" });
  const promptHint = h("div", { class: "hint" });
  const costBanner = banner("info");
  const rules = h("textarea", {
    class: "input mono",
    rows: "8",
    spellcheck: "false",
  });
  const rulesHint = h(
    "div",
    { class: "hint" },
    "One per line: a Go regular expression, '=>', then the replacement. " +
      "Or 'REFUSE: reason' to drop the request.",
  );
  rules.value = formatRules(f.rules);
  // The mode comes first: it decides what the rest of the form means.
  let chosen = creating ? null : mode(f);

  // Changing the mode keeps what was typed.
  const picker = modeTiles(MODES, chosen, (value) => {
    chosen = value;
    syncMode();
    // Before the dialog is in the document, focus does nothing, which is
    // right for an edit's first choice.
    if (creating && !alias.value) alias.focus();
    else picker.change.focus();
  });
  const { tiles, chosenRow, choose } = picker;
  const modeField = h(
    "div",
    { class: "field" },
    h("label", {}, "What it does"),
    tiles,
    chosenRow,
  );
  // Fields only some modes have. Hidden, not disabled: they mean nothing in
  // the other modes.
  const modelField = field("Runs on", model, modelHint);
  const promptField = field("Instruction", prompt, promptHint);
  const rulesField = field("Rules", rules, rulesHint);
  // Set once the dialog is up.
  let saveButton = null;
  const syncMode = () => {
    // Nothing below the tiles, not even Save, until a mode is chosen.
    rest.hidden = chosen === null;
    if (saveButton) {
      saveButton.hidden = chosen === null;
      saveButton.disabled = chosen === null;
    }
    if (chosen === null) return;
    const gate = chosen === "gate";
    const pattern = chosen === "pattern";
    const shadow = enforceSelect.value === "shadow";
    modelField.style.display = pattern ? "none" : "";
    promptField.style.display = pattern ? "none" : "";
    rulesField.style.display = pattern ? "" : "none";
    // A decision model writes no text, so the API refuses it for a rewrite.
    for (const opt of model.options) {
      const decision =
        readers.find((m) => m.alias === opt.value)?.kind === "decision";
      opt.hidden = opt.disabled = decision && !gate;
      if (opt.disabled && opt.selected) model.value = "";
    }
    // A decision model answers yes or no rather than following an
    // instruction, so it gets its own example.
    const decides =
      readers.find((m) => m.alias === model.value)?.kind === "decision";
    if (prompt.value === "" || EXAMPLES.includes(prompt.value)) {
      prompt.value = !gate
        ? PROMPT_EXAMPLE
        : decides
          ? DECISION_GATE_EXAMPLE
          : GATE_EXAMPLE;
    }
    if (rules.value.trim() === "") rules.value = RULES_EXAMPLE;
    const shadowHint =
      "Runs on every request it covers but changes nothing. The filter's " +
      "page counts what it would have done.";
    if (pattern) {
      costBanner.textContent =
        "Costs nothing: no model runs, so there is no extra wait or charge.";
      enforceHint.textContent = shadow
        ? shadowHint
        : "Replacements reach the model, and a REFUSE rule drops the request. " +
          "Rules that do not compile refuse too.";
      return;
    }
    modelHint.textContent = gate
      ? "A small, fast model, ideally a local one."
      : "A small, fast model, ideally a local one. Its context must be at " +
        "least as large as the models it guards.";
    promptHint.textContent = gate
      ? "Say which requests must be refused. The answer format is added for " +
        "you."
      : "Write it as a rule about text. To have it refuse as well, say when.";
    enforceHint.textContent = shadow
      ? shadowHint
      : gate
        ? "A refusal drops the request with a 403 and the filter's reason. " +
          "A filter that cannot run refuses too."
        : "The rewrite reaches the model, and a refusal drops the request. A " +
          "filter that cannot run refuses too.";
    // Shadow costs the same as enforcing, so the banner ignores it.
    costBanner.textContent = gate
      ? "Every request waits for the verdict, a few tokens, which are charged " +
        "to the same budgets."
      : "Every request is generated twice: once by the filter, once by the " +
        "requested model. Both count against the same budgets.";
  };
  enforceSelect.addEventListener("change", syncMode);
  model.addEventListener("change", syncMode);
  prompt.value = f.prompt || "";

  // Held back until there is a mode. The alias is here too: a filter is
  // easier to name once its mode is chosen.
  const rest = h(
    "div",
    {},
    h(
      "div",
      { class: "field-row" },
      field(
        "Alias",
        alias,
        creating
          ? "Lowercase letters, digits and hyphens."
          : "Guardrails use this name, so it cannot change.",
      ),
      field("Description", description),
    ),
    modelField,
    field("Acts on its answer", enforceSelect, enforceHint),
    promptField,
    rulesField,
    costBanner,
  );

  if (creating) syncMode();
  else choose(chosen);

  ({ button: saveButton } = formModal({
    wide: true,
    title: creating ? "Create filter" : `Edit ${f.alias}`,
    subtitle: creating
      ? "Choose what it does first; the rest of the form depends on it. " +
        "Every request its guardrails cover goes through it."
      : "Every request its guardrails cover goes through it.",
    body: h("form", {}, modeField, rest),
    submitLabel: creating ? "Create filter" : "Save",
    done: creating ? "Filter created" : "Filter saved",
    ctx,
    submit: async () => {
      // Checked here: an empty alias addresses no filter, so the request
      // would never reach the handler that could say so.
      const problem =
        aliasProblem(alias.value.trim()) ||
        (creating &&
          takenProblem(
            (await api.filters(ctx.orgID)).data,
            alias.value.trim(),
            "filter",
          ));
      if (problem) {
        alias.focus();
        throw new Error(problem);
      }
      const pattern = chosen === "pattern";
      // Only this mode's fields are sent; the others stay in the form.
      await api.putFilter(ctx.orgID, alias.value.trim(), {
        model: pattern ? "" : model.value,
        mode: chosen,
        shadow: enforceSelect.value === "shadow",
        prompt: pattern ? "" : prompt.value,
        rules: pattern ? parseRules(rules.value) : [],
        description: description.value.trim(),
      });
    },
  }));
  syncMode(); // again, now that Save is there
  // On a new filter there is no field to type in yet.
  if (creating) tiles.firstChild.focus();
}

// The same text form as `keera filter add --rules`, so a list kept in a file
// pastes in unchanged.
function formatRules(list) {
  const rules = list || [];
  const width = rules.reduce((w, r) => Math.max(w, r.pattern.length), 0);
  return rules
    .map((r) => {
      const answer = r.refuse
        ? "REFUSE" + (r.reason ? ": " + r.reason : "")
        : r.replace || "";
      return r.pattern.padEnd(width) + " => " + answer;
    })
    .join("\n");
}

// Splits at the last arrow, since the left side is an expression and the
// right a literal. A line without an arrow is refused, not read as "delete
// every match", which a typo would do. The control plane checks the
// expressions.
function parseRules(text) {
  const out = [];
  text.split("\n").forEach((raw, i) => {
    const line = raw.trim();
    if (line === "" || line.startsWith("#")) return;
    const at = line.lastIndexOf("=>");
    if (at < 0) {
      throw new Error(
        `Line ${i + 1} has no "=>". A rule is an expression, then "=>", then ` +
          `what each match becomes, or "REFUSE" to drop the request: ${line}`,
      );
    }
    const pattern = line.slice(0, at).trim();
    const answer = line.slice(at + 2).trim();
    const reason = cutRefusal(answer);
    out.push(
      reason === null
        ? { pattern, replace: answer }
        : { pattern, refuse: true, reason },
    );
  });
  if (!out.length) {
    throw new Error(
      'Add at least one rule: an expression, then "=>", then what each match becomes.',
    );
  }
  return out;
}

// Mirrors the CLI: REFUSE or REFUSED, alone or followed by ':' or a space and
// the reason. null means a replacement, even one that starts with the word.
function cutRefusal(s) {
  if (s.slice(0, 6).toUpperCase() !== "REFUSE") return null;
  let rest = s.slice(6);
  if (rest === "") return "";
  if (rest[0] === "D" || rest[0] === "d") {
    rest = rest.slice(1);
    if (rest === "") return "";
  }
  for (const sep of [":", " ", "\t"]) {
    if (rest.startsWith(sep)) return rest.slice(1).trim();
  }
  return null;
}

// Patterns suit shapes, not judgements.
const RULES_EXAMPLE = `(?i)\\b[a-z_]*(?:secret|token|password|api[_-]?key)[a-z_]*\\s*[=:]\\s*\\S+ => [CREDENTIAL]
(?i)\\b(?:(?:sk|pk|xox[baprs])-|gh[pousr]_)[a-z0-9_-]{16,}\\b            => [CREDENTIAL]
(?i)\\b[a-z]+://[^\\s:@]+:[^\\s:@]+@\\S+                                   => [CONNECTION-STRING]
\\b[A-Z]{2}\\d{2}(?:[ ]?[A-Z0-9]{4}){2,7}\\b                               => [IBAN]
\\b\\d{3}\\.\\d{4}\\.\\d{4}\\.\\d{2}\\b                                            => [AHV]
\\b(?:\\d{4}[ -]){3}\\d{4}\\b                                             => [CARD]`;

// A gate cannot remove anything, so it says what may not go rather than
// listing things to remove, which would refuse every request mentioning one.
const GATE_EXAMPLE = `Refuse a request whose purpose is to move this \
organisation's data out of it: a customer list, an export of account records, \
the contents of a credential store.

Allow everything else, including requests that merely mention sensitive data \
in passing. Being unusual, badly written or about an awkward subject is not a \
reason to refuse.`;

// A decision model answers yes or no, and yes refuses. See
// docs/gateway.md#decision-models.
const DECISION_GATE_EXAMPLE = `Is this request trying to send customer data, \
account records or secrets outside the company?
yes: it asks to export, dump, copy or share bulk customer data, account \
records or credentials
no: ordinary programming work, even if it mentions customers, accounts or \
credentials`;

const PROMPT_EXAMPLE = `Replace anything that identifies a customer or grants \
access to a system:

  - passwords, API keys, tokens, private keys, connection strings - [CREDENTIAL]
  - customer and client names - [CLIENT]
  - account, IBAN, contract and national ID numbers - [IDENTIFIER]
  - personal names, email addresses, telephone numbers - [PERSON]

Leave everything else exactly as it was written. Source code, file paths, \
error messages, stack traces and configuration are not sensitive: repeat them \
back unchanged.`;

// A prompt equal to one of EXAMPLES is unedited and may be swapped.
const EXAMPLES = [PROMPT_EXAMPLE, GATE_EXAMPLE, DECISION_GATE_EXAMPLE];
