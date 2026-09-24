// Filters: the guardrail that rewrites a request instead of refusing it.
//
// Every other guardrail decides whether a request may go. A filter decides
// what goes - it runs the prompt through a small model, with an instruction,
// before the request is forwarded. It exists for a prompt allowed to reach a
// hosted model while carrying a credential or a client's name.
//
// Three things about it need a screen rather than a field. A filter that
// cannot run refuses the requests it covers, so a broken one has to be visible
// at a glance. A filter is prose given to a model, so the only honest way to
// know what it does is to run it and read what came back - which is Check. And
// its behaviour is a matter of degree: it fires on every request, spends money
// on each, and refuses some, so each one gets its own screen.

import { api } from "../api.js";
import {
  h,
  table,
  modal,
  confirm,
  toast,
  pill,
  stat,
  meter,
  empty,
  rowLink,
  go,
  compact,
  num,
  money,
  ms,
  dateTime,
  icon,
  icons,
  aliasProblem,
  showError,
  currentRange,
  rangePicker,
} from "../ui.js";
import { areaChart, barList } from "../chart.js";
import { chooseOrg } from "./orgs.js";
// A filter's own screen is an entity's own screen, so it takes the window
// picker the team, key and model screens use - and the window itself, which is
// shared between them: arriving here from a team showing this afternoon and
// being given the last thirty days would answer a question nobody asked.
import { facts } from "./detail.js";

export async function filtersView(ctx) {
  // A filter is one organisation's judgement about what its own requests may
  // carry, so it belongs to one rather than to the deployment.
  if (!ctx.orgID) return chooseOrg(ctx, "Filters");

  const since = currentRange();
  const [res, models] = await Promise.all([
    api.filters(ctx.orgID, { stats: true, since }),
    api.models().then((r) => r.data || []),
  ]);
  const filters = res.data || [];
  const stats = res.stats || {};
  const currency = res.currency || ctx.currency;
  const chatModels = models.filter((m) => m.kind === "chat");
  const canEdit = ctx.state.me.can_admin_org || ctx.state.me.unrestricted;
  ctx.setSubtitle(`${filters.length} filter${filters.length === 1 ? "" : "s"}`);

  const head = h(
    "div",
    { class: "detail-head" },
    h(
      "div",
      { class: "muted" },
      "A filter checks each request its guardrails cover before it is " +
        "forwarded. Rewrite and gate use a small model, so each request is " +
        "generated twice. Run that model in your own infrastructure: a hosted " +
        "one receives the prompt before anything is redacted. Pattern uses " +
        "regular expressions in the gateway and costs nothing.",
    ),
    h(
      "div",
      { class: "row", style: { flexWrap: "wrap" } },
      h("div", { style: { flex: 1 } }),
      rangePicker(ctx, since),
      canEdit
        ? h(
            "button",
            {
              class: "btn btn-primary",
              onClick: () => editFilter(ctx, null, chatModels),
            },
            icon(icons.plus),
            "New filter",
          )
        : null,
    ),
  );

  const known = new Set(models.map((m) => m.alias));
  const rows = table(
    [
      {
        label: "Alias",
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
        cell: (f) => h("div", { class: "row-tight" }, modePills(f)),
      },
      {
        label: "Runs on",
        shrink: true,
        cell: (f) => {
          // A pattern filter runs no model. An empty cell would read as one
          // somebody had not finished setting, so it says what it holds
          // instead.
          if (!usesModel(f)) {
            return h(
              "span",
              { class: "faint nowrap" },
              `${(f.rules || []).length} rule${(f.rules || []).length === 1 ? "" : "s"}, no model`,
            );
          }
          return known.has(f.model)
            ? h("span", { class: "mono muted" }, f.model)
            : h(
                "div",
                { class: "row-tight" },
                h("span", { class: "mono muted" }, f.model),
                pill("missing", "bad"),
              );
        },
      },
      // What it has actually been doing, which is the column this screen was
      // missing. A filter's alias, mode and instruction say what it is meant to
      // do; nothing here said whether it fires on everything or on nothing, and
      // an instruction cannot be tuned against an intention.
      {
        label: "In this window",
        shrink: true,
        cell: (f) => trafficCell(stats[f.alias], currency),
      },
      {
        label: "Description",
        cell: (f) =>
          f.description
            ? h("span", { class: "muted clamp-2" }, f.description)
            : h("span", { class: "faint" }, "-"),
      },
      {
        label: "Check",
        shrink: true,
        cell: (f) => (canEdit ? checkCell(ctx, f) : null),
      },
      {
        label: "",
        shrink: true,
        cell: (f) => {
          if (!canEdit) return null;
          return h(
            "div",
            { class: "row-tight" },
            h(
              "button",
              {
                class: "btn btn-sm",
                onClick: () => editFilter(ctx, f, chatModels),
              },
              "Edit",
            ),
            h(
              "button",
              {
                class: "btn btn-sm btn-danger",
                onClick: () =>
                  confirm({
                    title: `Remove ${f.alias}?`,
                    body:
                      "A filter still used by a guardrail cannot be removed. " +
                      "Take it off those guardrails first; the error lists " +
                      "them.",
                    confirmLabel: "Remove filter",
                    danger: true,
                    onConfirm: async () => {
                      await api.deleteFilter(ctx.orgID, f.alias);
                      toast("Filter removed", "good");
                      ctx.reload();
                    },
                  }),
              },
              "Remove",
            ),
          );
        },
      },
    ],
    filters,
    {
      emptyTitle: "No filters",
      emptyBody:
        "Requests are forwarded as sent. Add a filter to keep something " +
        "from reaching the model.",
    },
  );

  return h("div", {}, head, rows);
}

// mode is the mode a filter runs in. A filter written before there were two
// says nothing, and rewriting is what it did.
function mode(f) {
  return f.mode === "gate" || f.mode === "pattern" ? f.mode : "rewrite";
}

// usesModel says whether this filter is a model and an instruction. Most of
// this screen follows from it: whether there is a model to name, whether a run
// costs anything, and whether what is edited is prose or a list of rules.
function usesModel(f) {
  return mode(f) !== "pattern";
}

// modePills are the mode and, when it applies, the thing that matters more than
// the mode: that the filter is enforcing nothing. A row that says "gate" for a
// filter in shadow reads as a guardrail, and it is not one.
function modePills(f) {
  const tone = { gate: "accent", pattern: "good" }[mode(f)] || "";
  return [
    pill(mode(f), tone),
    f.shadow ? pill("shadow", "warn") : null,
  ].filter(Boolean);
}

// trafficCell is what this filter did to the window's requests, in the room a
// row has: how often it ran, how much of that was a refusal, and what each run
// cost the request waiting for it. The rest is on the filter's own screen,
// which the alias in the first column opens.
function trafficCell(st, currency) {
  if (!st || !st.runs) {
    return h("span", { class: "faint nowrap" }, "no traffic");
  }
  const refusalRate = st.refused / st.runs;
  const tone = st.errors > 0 ? "bad" : refusalRate >= 0.05 ? "warn" : "";
  return h(
    "div",
    { class: "stack", style: { gap: "3px" } },
    h(
      "div",
      { class: "row-tight" },
      h("span", { class: "nowrap" }, compact(st.runs), " runs"),
      st.errors > 0
        ? pill(`${num(st.errors)} could not run`, "bad")
        : st.refused > 0
          ? pill(`${(refusalRate * 100).toFixed(1)}% refused`, tone)
          : null,
    ),
    h(
      "div",
      { class: "faint nowrap", style: { fontSize: "11.5px" } },
      ms(st.latency_median_ms),
      " added · ",
      money(st.cost_micros, currency),
    ),
  );
}

// checkCell runs the filter over a sample and shows the verdict in place.
//
// Nothing about the result is cached. A filter is a small model following prose,
// and whether it did so ten minutes ago is not a claim worth showing next to
// what it will do to the next request.
function checkCell(ctx, f) {
  const slot = h("span");
  const run = h(
    "button",
    {
      class: "btn btn-sm btn-quiet",
      title: "Run this filter on a sample",
      onClick: async () => {
        run.disabled = true;
        slot.replaceChildren(
          h(
            "span",
            { class: "faint nowrap" },
            h("span", { class: "blip" }),
            " checking…",
          ),
        );
        try {
          const probe = await api.checkFilter(ctx.orgID, f.alias);
          slot.replaceChildren(verdict(ctx, probe));
        } catch (ex) {
          slot.replaceChildren(
            h("span", { class: "pill pill-bad" }, "check failed"),
          );
          toast(ex.message, "bad");
        } finally {
          run.disabled = false;
        }
      },
    },
    "Check",
  );
  return h("div", { class: "row-tight" }, run, slot);
}

function verdict(ctx, probe) {
  const warned = (probe.warnings || []).length > 0;
  // A filter that refused the sample gets its own label rather than "Working".
  // It may well be working - the sample carries the things one would refuse -
  // but it is a different answer from a rewrite and reads as one.
  const tone = probe.ok ? (probe.refused || warned ? "warn" : "good") : "bad";
  const label = !probe.ok
    ? "Not usable"
    : probe.refused
      ? "Refused the sample"
      : warned
        ? "Ran, with notes"
        : "Working";
  return h(
    "button",
    {
      class: "pill pill-" + tone + " pill-button",
      title: "What the filter did to the sample",
      onClick: () => report(ctx, probe),
    },
    h("span", { class: "dot" }),
    label,
  );
}

// report shows the sample as it went in and as it came back.
//
// A verdict on a filter is not something the gateway can give: whether the right
// things were removed and the wrong things left alone is a judgement about this
// organisation's data, and only the person reading it can make it. So the pill
// says whether the filter ran, and this says what it did.
function report(ctx, probe) {
  modal({
    wide: true,
    title: `Check - ${probe.alias}`,
    subtitle: !probe.ok
      ? "This filter would refuse every request its guardrails cover."
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
      // A check runs the filter whether or not it is enforcing, which is the
      // point of shadow - but everything below then describes something that
      // would not have happened, and a refusal read as real is the one
      // misreading of this dialog that would cost somebody an afternoon.
      probe.shadow
        ? h(
            "div",
            { class: "banner banner-warn" },
            "This filter is in shadow. Nothing below would be acted on: " +
              "requests go on as sent.",
          )
        : null,
      probe.error
        ? h("div", { class: "banner banner-bad" }, probe.error)
        : probe.refused
          ? h(
              "div",
              { class: "banner banner-warn" },
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
          : h(
              "div",
              { class: "banner banner-good" },
              "It answered in the expected format and returned the whole sample.",
            ),
      (probe.warnings || []).map((w) =>
        h("div", { class: "banner banner-warn" }, w),
      ),
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
            v.refused === v.expect_refusal
              ? pill(v.refused ? "refused" : "allowed", "good")
              : pill(v.refused ? "refused" : "allowed", "warn"),
            // How much of the verdict that verdict took. A gate answering both
            // halves right at 55% cannot tell them apart, which the pill alone
            // cannot show. Absent where the model's backend serves no
            // probabilities.
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
      // Which rules fired, and how often - the half of a check a model filter
      // cannot have. When a pattern filter does the wrong thing there is a
      // line responsible, and this is the line.
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
                    hit.matches === 1 ? "1 match" : `${hit.matches} matches`,
                    hit.refused ? "bad" : "accent",
                  ),
                  hit.refused ? pill("refused the request", "bad") : null,
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
            seg.changed ? pill("rewritten", "accent") : pill("unchanged"),
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
                  money(probe.cost_micros, ctx.currency),
                  ". Every request it covers pays this on top.",
                ]
              : ". Every request it covers waits this long on top.",
          )
        : null,
    ),
  });
}

/* ------------------------------------------------------- one filter's screen */

// filterDetailView is what a filter has actually been doing: the five
// questions an instruction is tuned on, over this filter's own runs.
//
// Is it firing, what does it do when it fires, what does it cost the request
// that waits for it, what share of the bill is it, and - the reason most
// people arrive - which team is living with the refusals. An instruction
// cannot be tuned against three sample segments.
export async function filterDetailView(ctx) {
  // A filter belongs to one organisation, as the list does: an operator looking
  // at every tenant at once has not said whose filter this is.
  if (!ctx.orgID) return chooseOrg(ctx, "Filters");

  const since = currentRange();
  const res = await api.filterReport(ctx.orgID, ctx.param, since);
  const rep = res.report || {};
  const f = res.filter || null;
  const currency = res.currency || ctx.currency;
  const teamNames = res.team_names || {};
  const canEdit = ctx.state.me.can_admin_org || ctx.state.me.unrestricted;

  ctx.setTitle(ctx.param, f ? identity(f) : "no longer in this organisation");

  const wrap = h(
    "div",
    {},
    h(
      "div",
      { class: "detail-head" },
      h(
        "a",
        { class: "crumb", href: "/filters", onClick: go(ctx, "/filters") },
        icon(icons.back),
        "Filters",
      ),
      h(
        "div",
        { class: "row", style: { flexWrap: "wrap" } },
        h(
          "div",
          { class: "wrap-chips" },
          f ? modePills(f) : [pill("Removed from this organisation", "warn")],
          f && f.model ? pill("runs on " + f.model) : null,
        ),
        h("div", { style: { flex: 1 } }),
        rangePicker(ctx, since),
        f && canEdit
          ? h(
              "button",
              {
                class: "btn",
                title: "Run this filter on a sample",
                onClick: async (e) => {
                  const button = e.currentTarget;
                  button.disabled = true;
                  try {
                    report(ctx, await api.checkFilter(ctx.orgID, f.alias));
                  } catch (ex) {
                    toast(ex.message, "bad");
                  } finally {
                    button.disabled = false;
                  }
                },
              },
              "Check",
            )
          : null,
        f && canEdit
          ? h(
              "button",
              {
                class: "btn",
                onClick: () =>
                  api.models().then((r) =>
                    editFilter(
                      ctx,
                      f,
                      (r.data || []).filter((m) => m.kind === "chat"),
                    ),
                  ),
              },
              icon(icons.sliders),
              "Edit",
            )
          : null,
      ),
    ),
  );

  for (const banner of detailBanners(rep, f)) wrap.append(banner);

  // The four numbers, in the order the questions are asked in. "Fired" leads
  // because every other number on the screen is meaningless without it: a
  // hundred percent refusal rate over four requests is not a guardrail
  // refusing everything, it is a filter nobody has used yet.
  wrap.append(
    h(
      "div",
      { class: "grid grid-4" },
      stat(
        "Fired",
        compact(rep.runs || 0),
        null,
        rep.requests
          ? `${pct(rep.runs, rep.requests)} of ${compact(rep.requests)} requests`
          : "no requests in this window",
      ),
      stat(
        f && f.shadow ? "Would refuse" : "Refused",
        pct(rep.refused, rep.runs),
        null,
        rep.refused
          ? `${num(rep.refused)} request${rep.refused === 1 ? "" : "s"} ` +
              (f && f.shadow ? "would have been dropped" : "dropped")
          : "nothing refused",
      ),
      stat(
        "Added wait",
        ms(rep.latency_median_ms || 0),
        null,
        `p95 ${ms(rep.latency_p95_ms || 0)}, per request`,
      ),
      stat(
        "Spend",
        money(rep.cost_micros || 0, ""),
        currency,
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
            ? pill(`${compact(rep.runs)} run${rep.runs === 1 ? "" : "s"}`)
            : null,
        ),
        h(
          "div",
          { class: "card-body" },
          rep.runs
            ? // The dashboard's chart, given this filter's runs as its requests and
              // its refusals as the baseline bars the failures usually occupy. On
              // this screen a refusal is the event worth seeing against the volume,
              // and it is rare enough to vanish on the same scale.
              areaChart(
                (rep.series || []).map((p) => ({
                  at: p.at,
                  requests: p.runs,
                  errors: p.refused + p.errors,
                  tokens: 0,
                  cost_micros: p.cost_micros,
                })),
                { currency, bucket: rep.bucket },
              )
            : empty(
                "Nothing ran in this window",
                f
                  ? "No guardrail uses this filter, or nothing it covers was " +
                      "called. Widen the range or check the team's guardrails."
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
                // The per-segment reading, which is the one that says whether an
                // instruction is too eager. A filter that rewrites every request
                // and touches one line of forty is doing its job; one that
                // rewrites thirty-eight of forty is rewriting somebody's work.
                rep.segments
                  ? h(
                      "div",
                      { class: "hint", style: { marginTop: "12px" } },
                      `It was shown ${num(rep.segments)} segment` +
                        `${rep.segments === 1 ? "" : "s"} of text and changed ` +
                        `${num(rep.changed)} of them (${pct(rep.changed, rep.segments)}). `,
                    )
                  : null,
              )
            : empty(
                "Nothing to show",
                "No run of this filter is in this window.",
              ),
        ),
      ),
    ),
  );

  // Refusals by team, which is the reading a filter is actually judged on. Four
  // percent across an organisation is a rounding error to whoever wrote the
  // guardrail and the whole working day of the one team it lands on.
  if ((rep.teams || []).length > 0) {
    wrap.append(
      h(
        "div",
        { style: { marginTop: "20px" } },
        h(
          "div",
          { class: "section-head" },
          h("h2", {}, "Who it happened to"),
          h("div", { style: { flex: 1 } }),
          h(
            "span",
            { class: "faint", style: { fontSize: "11.5px" } },
            "refusals by team",
          ),
        ),
        teamTable(ctx, rep.teams, teamNames, currency),
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
                        pattern: "yes - the model gets the replaced text",
                      }[mode(f)] || "yes - the model gets the rewrite",
                    ),
              ],
              [
                "Description",
                f.description
                  ? h("span", { class: "muted" }, f.description)
                  : h("span", { class: "faint" }, "-"),
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

// detailBanners are the things that have to be said before any of the numbers
// are read, because each of them changes what the numbers mean.
function detailBanners(rep, f) {
  const out = [];
  const style = { marginBottom: "16px" };
  if (!f) {
    out.push(
      h(
        "div",
        { class: "banner banner-warn", style },
        "This filter no longer exists. Below is the traffic it saw before " +
          "it was removed.",
      ),
    );
  } else if (f.shadow) {
    out.push(
      h(
        "div",
        { class: "banner banner-info", style },
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
  if (rep.errors > 0) {
    out.push(
      h(
        "div",
        { class: "banner banner-bad", style },
        `${num(rep.errors)} run${rep.errors === 1 ? "" : "s"} failed. ` +
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
      h(
        "div",
        { class: "banner banner-warn", style },
        `${num(rep.shadowed)} of these runs were in shadow: the filter was ` +
          "switched during this window, so the refusal rate mixes both modes.",
      ),
    );
  }
  return out;
}

// outcomeRows is the split, as counts. The rates are on the tiles above; what a
// ranked list adds is the shape - whether a filter mostly passes, mostly
// rewrites, or mostly says no.
//
// Each outcome carries its own colour rather than taking one from the
// categorical palette by position: a pass is the quiet baseline and reads
// neutral, and an error stays the same red it is everywhere else on the page
// even on a filter that never passed anything.
function outcomeRows(rep) {
  return [
    {
      label: "Passed through untouched",
      value: rep.pass || 0,
      color: "var(--muted)",
    },
    { label: "Rewritten", value: rep.rewrote || 0, color: "var(--s1)" },
    { label: "Refused", value: rep.refused || 0, color: "var(--warn)" },
    { label: "Could not run", value: rep.errors || 0, color: "var(--bad)" },
  ].filter((r) => r.value > 0);
}

function teamTable(ctx, teams, names, currency) {
  return table(
    [
      {
        label: "Team",
        cell: (t) =>
          t.team_id
            ? rowLink(
                ctx,
                "/teams/" + encodeURIComponent(t.team_id),
                names[t.team_id] || t.team_id,
                "This team's traffic",
              )
            : h("span", { class: "faint" }, "No team"),
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
          t.errors
            ? h("span", { class: "pill pill-bad" }, num(t.errors))
            : h("span", { class: "faint" }, "-"),
      },
      {
        label: "Spend",
        shrink: true,
        num: true,
        cell: (t) =>
          h("span", { class: "nowrap muted" }, money(t.cost_micros, currency)),
      },
    ],
    teams,
    { emptyTitle: "Nobody yet" },
  );
}

// pct is one count against another, which is how these numbers are compared -
// this team against that one, this week against last. "4.1%" compares at a
// glance where "173 of 4,219" does not.
function pct(part, whole) {
  if (!whole) return "-";
  const frac = (part || 0) / whole;
  return `${(frac * 100).toFixed(frac > 0 && frac < 0.001 ? 3 : 1)}%`;
}

// identity is the subtitle under a filter's alias: what it does and what it does
// it on, with the one thing that outranks both when it applies.
function identity(f) {
  const what = usesModel(f) ? `${mode(f)} on ${f.model}` : mode(f);
  return f.shadow ? `${what}, in shadow` : what;
}

/* ------------------------------------------------------------------- form */

// MODES are the three things a filter can do to a request, as the dialog
// offers them: a mark, a name, and the whole of what the mode does. One list,
// because the tiles that pick a mode and the row that reports the one picked
// have to say the same thing about it.
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

function modeNamed(value) {
  return MODES.find((m) => m.value === value) || MODES[0];
}

function editFilter(ctx, existing, chatModels) {
  const err = h("div");
  const creating = !existing;
  // No mode among the defaults: a new filter has none until one of the tiles
  // is clicked, and a default here would be the answer to the one question
  // this dialog asks before any other.
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
      chatModels.length ? "Choose a model" : "No chat model exists",
    ),
    chatModels.map((m) =>
      h(
        "option",
        { value: m.alias, selected: m.alias === f.model },
        m.alias +
          (m.max_context ? ` - ${m.max_context.toLocaleString()} context` : ""),
      ),
    ),
  );
  const prompt = h("textarea", { class: "input", rows: "8" });

  // Whether the filter enforces at all, which is a separate question from what
  // it produces. A new filter starts in shadow: an instruction is prose given
  // to a small model, nobody's first draft of one is right, and the cost of
  // finding that out on live traffic is a department's afternoon. Turning it on
  // is one edit away and the numbers to make that decision on are on the
  // filter's own screen.
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

  // The mode decides what the instruction is for, so the form's wording about
  // the instruction, the model's context and the cost all follow it - and the
  // example in the box is swapped with it, as long as the box still holds an
  // example rather than somebody's own writing.
  const modelHint = h("div", { class: "hint" });
  const promptHint = h("div", { class: "hint" });
  const costBanner = h("div", { class: "banner banner-info" });
  // A pattern filter's rules, in the same text form the command line reads and
  // prints, so a list written in one place pastes into the other. A row of
  // inputs per rule would be worse at what this is for: reading a list and
  // editing one line of it.
  const rules = h("textarea", {
    class: "input mono",
    rows: "8",
    spellcheck: "false",
  });
  const rulesHint = h("div", { class: "hint" });
  rules.value = formatRules(f.rules);
  // What it does. It is asked first and on its own, because it decides what
  // the rest of the form means - and on a pattern filter, most of that form
  // does not exist at all. As a select halfway down, the question was answered
  // after the fields that follow from it.
  let chosen = creating ? null : mode(f);

  // The modes as tiles rather than as options, because each of them needs a
  // sentence and an option list that reads like prose is one nobody reads to
  // the end.
  const tiles = h(
    "div",
    { class: "tiles" },
    MODES.map((m) =>
      h(
        "button",
        {
          class: "tile",
          type: "button",
          "aria-pressed": String(m.value === chosen),
          onClick: () => choose(m.value),
        },
        h("span", { class: "tile-mark" }, icon(m.icon)),
        h("span", { class: "tile-name" }, m.name),
        h("span", { class: "tile-what" }, m.what),
      ),
    ),
  );
  const chosenRow = h("div", { class: "tile-chosen", hidden: true });
  // The way back to the three. Changing the mode keeps the form filled in: an
  // instruction written for a rewrite is most of the one a gate wants, and the
  // rules a pattern filter was given are still there if the mode comes back -
  // so going to look at the other tiles costs nothing.
  const change = h(
    "button",
    {
      class: "btn btn-sm",
      type: "button",
      onClick: () => {
        tiles.hidden = false;
        chosenRow.hidden = true;
        tiles.firstChild.focus();
      },
    },
    "Change",
  );
  const choose = (value) => {
    chosen = value;
    const m = modeNamed(value);
    tiles.childNodes.forEach((tile, i) => {
      tile.setAttribute("aria-pressed", String(MODES[i].value === value));
    });
    chosenRow.replaceChildren(
      icon(m.icon),
      h(
        "div",
        { class: "stack", style: { gap: "1px" } },
        h("span", { class: "tile-name" }, m.name),
        h("span", { class: "tile-what" }, m.what),
      ),
      h("div", { class: "spacer" }),
      change,
    );
    tiles.hidden = true;
    chosenRow.hidden = false;
    syncMode();
    // Where the cursor lands is the point of asking this first: on a new
    // filter, the first thing left to type; on one being changed, the control
    // that opened the tiles. Before the dialog is in the document neither does
    // anything, which is what the first choice of an edit wants.
    if (creating && !alias.value) alias.focus();
    else change.focus();
  };
  const modeField = h(
    "div",
    { class: "field" },
    h("label", {}, "What it does"),
    tiles,
    chosenRow,
  );
  // The fields only some of the modes have. Hidden rather than disabled: a
  // model on a pattern filter is not a field that happens to be unavailable,
  // there is nothing for it to mean.
  const modelField = h("div", { class: "field" });
  const promptField = h("div", { class: "field" });
  const rulesField = h("div", { class: "field" });
  fill(modelField, h("label", {}, "Runs on"), model, modelHint);
  const syncMode = () => {
    // Nothing below the tiles until one of them is chosen, Save included:
    // there is nothing to save yet, and a Save button beside three unanswered
    // tiles is one inviting somebody to skip the question.
    rest.hidden = chosen === null;
    saveButton.hidden = chosen === null;
    saveButton.disabled = chosen === null;
    if (chosen === null) return;
    const gate = chosen === "gate";
    const pattern = chosen === "pattern";
    const shadow = enforceSelect.value === "shadow";
    modelField.style.display = pattern ? "none" : "";
    promptField.style.display = pattern ? "none" : "";
    rulesField.style.display = pattern ? "" : "none";
    if (
      prompt.value === "" ||
      prompt.value === PROMPT_EXAMPLE ||
      prompt.value === GATE_EXAMPLE
    ) {
      prompt.value = gate ? GATE_EXAMPLE : PROMPT_EXAMPLE;
    }
    if (rules.value.trim() === "") rules.value = RULES_EXAMPLE;
    rulesHint.textContent =
      "One per line: a Go regular expression, '=>', then the replacement. " +
      "Or 'REFUSE: reason' to drop the request.";
    if (pattern) {
      costBanner.textContent =
        "Costs nothing: no model runs, so there is no extra wait or charge.";
      enforceHint.textContent = shadow
        ? "Runs on every request it covers but changes nothing. The filter's " +
          "page counts what it would have done."
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
      ? "Runs on every request it covers but changes nothing. The filter's " +
        "page counts what it would have done."
      : gate
        ? "A refusal drops the request with a 403 and the filter's reason. " +
          "A filter that cannot run refuses too."
        : "The rewrite reaches the model, and a refusal drops the request. A " +
          "filter that cannot run refuses too.";
    // Shadow costs the same as enforcing - the generation happens either way -
    // so the banner does not change with it.
    costBanner.textContent = gate
      ? "Every request waits for the verdict, a few tokens, which are charged " +
        "to the same budgets."
      : "Every request is generated twice: once by the filter, once by the " +
        "requested model. Both count against the same budgets.";
  };
  enforceSelect.addEventListener("change", syncMode);
  prompt.value = f.prompt || "";

  // Everything the mode governs, in one wrapper so the dialog can hold it back
  // until there is a mode. The alias is in here too: a filter is named for
  // what it does, and that is easier to name once it has been chosen.
  const rest = h(
    "div",
    {},
    h(
      "div",
      { class: "field-row" },
      h(
        "div",
        { class: "field" },
        h("label", {}, "Alias"),
        alias,
        h(
          "div",
          { class: "hint" },
          creating
            ? "Lowercase letters, digits and hyphens."
            : "Guardrails use this name, so it cannot change.",
        ),
      ),
      h("div", { class: "field" }, h("label", {}, "Description"), description),
    ),
    modelField,
    h(
      "div",
      { class: "field" },
      h("label", {}, "Acts on its answer"),
      enforceSelect,
      enforceHint,
    ),
    fill(promptField, h("label", {}, "Instruction"), prompt, promptHint),
    fill(rulesField, h("label", {}, "Rules"), rules, rulesHint),
    costBanner,
  );

  // Save is built out here rather than in the footer's callback, because the
  // tiles turn it on: syncMode holds it back until a mode is chosen. It closes
  // the dialog through the handle modal hands back.
  let close;
  const saveButton = h(
    "button",
    {
      class: "btn btn-primary",
      onClick: async (e) => {
        const button = e.currentTarget;
        // Checked before the request rather than after it: an empty alias
        // addresses no filter, so the write would not reach the handler that
        // could say so.
        const problem = aliasProblem(alias.value.trim());
        if (problem) {
          showError(err, problem);
          alias.focus();
          return;
        }
        button.disabled = true;
        try {
          const pattern = chosen === "pattern";
          // Only the fields this mode carries are sent. The others stay in the
          // form, so switching back and forth loses nothing that was typed -
          // but sending them would be refused, and rightly.
          await api.putFilter(ctx.orgID, alias.value.trim(), {
            model: pattern ? "" : model.value,
            mode: chosen,
            shadow: enforceSelect.value === "shadow",
            prompt: pattern ? "" : prompt.value,
            rules: pattern ? parseRules(rules.value) : [],
            description: description.value.trim(),
          });
          close();
          toast("Filter saved", "good");
          ctx.reload();
        } catch (ex) {
          showError(err, ex.message);
          button.disabled = false;
        }
      },
    },
    "Save",
  );

  if (creating) syncMode();
  else choose(chosen);

  close = modal({
    wide: true,
    title: creating ? "New filter" : `Edit ${f.alias}`,
    subtitle: creating
      ? "Choose what it does first; the rest of the form depends on it. " +
        "Every request its guardrails cover goes through it."
      : "Every request its guardrails cover goes through it.",
    body: h("form", {}, err, modeField, rest),
    actions: (dismiss) => [
      h("button", { class: "btn", onClick: dismiss }, "Cancel"),
      saveButton,
    ],
  });

  // The keyboard starts on the tiles. The dialog's own rule - the first field
  // somebody can type in - finds nothing on a new filter, because until a mode
  // is chosen there is nothing to type in.
  if (creating) tiles.firstChild.focus();
}

// fill puts children into an existing element, so that a field the form shows
// and hides can be built where it is declared and rendered where it belongs.
function fill(el, ...children) {
  el.replaceChildren(...children);
  return el;
}

// formatRules and parseRules are the text form of a pattern filter's rules, and
// deliberately the same form `keera filter --rules` reads and prints: a list
// kept in a file should paste into this box unchanged.
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

// parseRules is lenient in the same places the command line is: blank lines and
// '#' comments are skipped, and the arrow is found at its last occurrence
// because the left side is an expression and the right side is a literal.
//
// Nothing is validated here. The control plane compiles every expression and is
// the only place that check can be trusted, so a bad rule comes back as the
// banner at the top of this form rather than as a second opinion in the
// browser.
function parseRules(text) {
  return text
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "" && !line.startsWith("#"))
    .map((line) => {
      const at = line.lastIndexOf("=>");
      if (at < 0) return { pattern: line, replace: "" };
      const pattern = line.slice(0, at).trim();
      const answer = line.slice(at + 2).trim();
      const refusal = /^REFUSED?\b[:\s]*/i.exec(answer);
      if (refusal) {
        return { pattern, refuse: true, reason: answer.slice(refusal[0].length) };
      }
      return { pattern, replace: answer };
    });
}

// RULES_EXAMPLE is a starting point, and the half of redaction a model was
// never the right instrument for: each of these is a shape rather than a
// judgement.
const RULES_EXAMPLE = `(?i)\\b[a-z_]*(?:secret|token|password|api[_-]?key)[a-z_]*\\s*[=:]\\s*\\S+ => [CREDENTIAL]
(?i)\\b(sk|pk|ghp|gho|xox[baprs])-[a-z0-9_-]{16,}\\b                     => [CREDENTIAL]
(?i)\\b[a-z]+://[^\\s:@]+:[^\\s:@]+@\\S+                                   => [CONNECTION-STRING]
\\b[A-Z]{2}\\d{2}(?:[ ]?[A-Z0-9]{4}){2,7}\\b                               => [IBAN]
\\b\\d{3}\\.\\d{4}\\.\\d{4}\\.\\d{2}\\b                                            => [AHV]
\\b(?:\\d{4}[ -]){3}\\d{4}\\b                                             => [CARD]`;

// PROMPT_EXAMPLE is a starting point rather than a default. It is the shape an
// instruction has to have to be safe: say what to remove, say what to replace it
// with, and say - in as many words - to leave everything else exactly as it is.
// GATE_EXAMPLE is the same for a gate: it names what makes a request one that
// may not go at all, and - as important - says that everything else may. A gate
// cannot take anything out of a request, so an instruction listing things to
// remove would refuse every request that mentioned one.
const GATE_EXAMPLE = `Refuse a request whose purpose is to move this \
organisation's data out of it: a customer list, an export of account records, \
the contents of a credential store.

Allow everything else, including requests that merely mention sensitive data \
in passing. Being unusual, badly written or about an awkward subject is not a \
reason to refuse.`;

const PROMPT_EXAMPLE = `Replace anything that identifies a customer or grants \
access to a system:

  - passwords, API keys, tokens, private keys, connection strings - [CREDENTIAL]
  - customer and client names - [CLIENT]
  - account, IBAN, contract and national ID numbers - [IDENTIFIER]
  - personal names, email addresses, telephone numbers - [PERSON]

Leave everything else exactly as it was written. Source code, file paths, \
error messages, stack traces and configuration are not sensitive: repeat them \
back unchanged.`;
