import { api } from "../api.js";
import {
  h,
  stat,
  compact,
  num,
  money,
  moneyStat,
  ms,
  tokens,
  go,
  currentRange,
  rangePicker,
  isAdmin,
  banner,
  rangeOf,
} from "../ui.js";
import { areaChart, barList } from "../chart.js";
import {
  firstRun,
  firstRunDue,
  memberRun,
  memberRunDue,
} from "./firstrun.js";
import { openFailed } from "./logfilters.js";
import { orgNameOf } from "./orgs.js";

export async function dashboardView(ctx) {
  const since = currentRange();

  // A dashboard of zeroes helps nobody. The checklist replaces it until there
  // is traffic and, with card payments, until the organisation has had credit.
  if (isAdmin(ctx)) {
    const setup = await api.setup(ctx.orgID).catch(() => null);
    // The sidebar names this screen from the same counts, read when the panel
    // loaded. A step done since then renames it.
    if (setup) ctx.setSetup(setup);
    if (firstRunDue(ctx, setup)) {
      ctx.setTitle("Getting started");
      const askCredit =
        ctx.state.me.payments && ctx.orgID && !setup.credits && !setup.invoiced;
      const credit = askCredit
        ? await api.creditAccount(ctx.orgID).catch(() => null)
        : null;
      return firstRun(ctx, setup, credit);
    }
  } else {
    // A member's guide follows their own keys, not the organisation's.
    const access = await api.access().catch(() => null);
    if (access) ctx.setAccess(access);
    if (memberRunDue(ctx, access)) {
      ctx.setTitle("Getting started");
      return memberRun(ctx, access);
    }
  }
  ctx.setTitle("Dashboard");

  const res = await api.overview(ctx.orgID, since);
  const o = res.overview;
  const names = res.project_names || {};

  ctx.setSubtitle(rangeOf(since)?.long || "");

  const failRate = o.requests ? (o.failed / o.requests) * 100 : 0;

  const header = h(
    "div",
    { class: "row", style: { marginBottom: "16px" } },
    h("div", { class: "spacer" }),
    rangePicker(ctx, since),
  );

  const tiles = h(
    "div",
    { class: "grid grid-4" },
    stat(
      "Requests",
      compact(o.requests),
      o.refused ? `${num(o.refused)} refused` : "none refused",
    ),
    stat(
      "Tokens",
      compact(o.input_tokens + o.output_tokens),
      tokens(o.input_tokens, o.output_tokens),
    ),
    // What Claude plans paid for is not part of the cost, so it is said
    // beside it rather than added to it.
    moneyStat(
      "Cost",
      o.cost_micros,
      o.subscription_micros
        ? `+ ${money(o.subscription_micros)} covered by Claude plans`
        : null,
    ),
    stat("First token", ms(o.ttft_median_ms), `p95 ${ms(o.ttft_p95_ms)}`),
  );

  const chartCard = h(
    "div",
    { class: "card" },
    h(
      "div",
      { class: "card-head" },
      h("h2", {}, "Traffic"),
      h("div", { class: "spacer" }),
      o.failed > 0
        ? failureLink(
            ctx,
            h("span", { class: "dot" }),
            `${num(o.failed)} failed (${failRate.toFixed(1)}%)`,
          )
        : h(
            "span",
            { class: "pill pill-good" },
            h("span", { class: "dot" }),
            "No failures",
          ),
    ),
    h("div", { class: "card-body" }, areaChart(o.series, { bucket: o.bucket })),
  );

  const panels = h(
    "div",
    { class: "grid grid-2", style: { marginTop: "16px" } },
    h(
      "div",
      { class: "card" },
      h(
        "div",
        { class: "card-head" },
        h("h2", {}, "Projects by cost"),
        h("div", { class: "spacer" }),
        h(
          "a",
          { href: "/projects", onClick: go(ctx, "/projects") },
          "All projects",
        ),
      ),
      h(
        "div",
        { class: "card-body" },
        barList(o.top_projects, {
          label: (r) => names[r.group] || (r.group ? r.group : "No project"),
          value: (r) => r.cost_micros,
          cost: true,
        }),
      ),
    ),
    h(
      "div",
      { class: "card" },
      h(
        "div",
        { class: "card-head" },
        h("h2", {}, "Models by tokens"),
        h("div", { class: "spacer" }),
        h("a", { href: "/usage", onClick: go(ctx, "/usage") }, "Usage report"),
      ),
      h(
        "div",
        { class: "card-body" },
        barList(o.top_models, {
          // Across every organisation, two of them can each have a model of
          // the same alias, so the row names whose it is.
          label: (r) =>
            r.org_id ? `${r.group} · ${orgNameOf(ctx, r.org_id)}` : r.group,
          value: (r) => r.input_tokens + r.output_tokens,
          colorIndex: 1,
        }),
      ),
    ),
  );

  const wrap = h(
    "div",
    {},
    header,
    tiles,
    h("div", { style: { marginTop: "16px" } }, chartCard),
    panels,
  );

  // The log itself is a screen of its own: it is read by narrowing it many
  // times, which is tiresome under four charts.
  if (o.requests === 0) {
    wrap.prepend(
      banner(
        "info",
        isAdmin(ctx)
          ? [
              "No traffic in this period yet. Issue a key under ",
              h("a", { href: "/keys", onClick: go(ctx, "/keys") }, "API keys"),
              " and point a client at the gateway to see it here.",
            ]
          : "No traffic in this period yet. Point a client at the gateway " +
              "with your key to see it here.",
      ),
    );
  }
  return wrap;
}

// failureLink links the failed count to the failed rows of the request log,
// over the same window. A member gets a plain pill: their log holds only their
// own keys' requests, so it would not add up to the count.
//
// A real link rather than a button, so it can open in a new tab.
function failureLink(ctx, ...content) {
  if (!isAdmin(ctx)) {
    return h("span", { class: "pill pill-bad" }, ...content);
  }
  return h(
    "a",
    {
      class: "pill pill-bad pill-button",
      href: "/requests",
      title: "Show the failed requests",
      onClick: (e) => {
        e.preventDefault();
        openFailed();
        ctx.navigate("/requests");
      },
    },
    ...content,
  );
}
