// The dashboard.

import { api } from "../api.js";
import {
  h,
  stat,
  compact,
  num,
  money,
  ms,
  go,
  RANGES,
  currentRange,
  rangePicker,
} from "../ui.js";
import { areaChart, barList } from "../chart.js";
import { firstRun } from "./firstrun.js";
import { openFailed } from "./requestlog.js";

export async function overviewView(ctx) {
  const since = currentRange();

  // Before anything has traffic, a dashboard of zeroes is not the screen
  // anybody needs. The checklist replaces it until the deployment is actually
  // carrying requests, and then never appears again.
  if (isAdmin(ctx)) {
    const setup = await api.setup(ctx.orgID).catch(() => null);
    if (setup && setup.requests === 0) {
      ctx.setSubtitle("First run");
      return firstRun(ctx, setup);
    }
  }

  const res = await api.overview(ctx.orgID, since);
  const o = res.overview;
  const currency = res.currency;
  const names = res.team_names || {};

  ctx.setSubtitle(rangeLabel(since));

  const failRate = o.requests ? (o.failed / o.requests) * 100 : 0;

  const header = h(
    "div",
    { class: "row", style: { marginBottom: "16px" } },
    rangePicker(ctx, since),
  );

  const tiles = h(
    "div",
    { class: "grid grid-4" },
    stat(
      "Requests",
      compact(o.requests),
      null,
      o.refused ? `${num(o.refused)} refused by a guardrail` : "none refused",
    ),
    stat(
      "Tokens",
      compact(o.input_tokens + o.output_tokens),
      null,
      `${compact(o.input_tokens)} in · ${compact(o.output_tokens)} out`,
    ),
    stat("Spend", money(o.cost_micros, ""), currency),
    stat("First token", ms(o.ttft_median_ms), null, `p95 ${ms(o.ttft_p95_ms)}`),
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
        ? // The count is where the question starts, so it is also the way to the
          // answer: the failed rows on the request log, with the model, the key
          // and what the backend said. The window it opens on is the one being
          // read here, so nobody has to rebuild the filter they arrived with.
          // Only for a reader who can open them - a member is sent nowhere rather
          // than somewhere that would refuse them.
          failureLink(
            ctx,
            h("span", { class: "dot" }),
            `${num(o.failed)} failed (${failRate.toFixed(1)}%)`,
          )
        : h(
            "span",
            { class: "pill pill-good" },
            h("span", { class: "dot" }),
            "no failures",
          ),
    ),
    h(
      "div",
      { class: "card-body" },
      areaChart(o.series, { currency, bucket: o.bucket }),
    ),
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
        h("h2", {}, "Teams by spend"),
        h("div", { class: "spacer" }),
        h("a", { href: "/teams", onClick: go(ctx, "/teams") }, "All teams"),
      ),
      h(
        "div",
        { class: "card-body" },
        barList(o.top_teams, {
          label: (r) => names[r.group] || (r.group ? r.group : "No team"),
          value: (r) => r.cost_micros || r.requests,
          currency,
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
          label: (r) => r.group,
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

  // The dashboard is the window counted up, and it stops there. The window
  // itself - one row per request - is a screen of its own, because a log is
  // read by narrowing it half a dozen times over and a reader doing that under
  // four charts is a reader scrolling past them on every pass.
  if (o.requests === 0) {
    wrap.prepend(
      h(
        "div",
        { class: "banner banner-info" },
        "No traffic in this period yet. Issue a key under ",
        h("a", { href: "/keys", onClick: go(ctx, "/keys") }, "API keys"),
        " and point a client at the gateway to see it here.",
      ),
    );
  }
  return wrap;
}

// failureLink is the failed count as the way in to the failures behind it: the
// request log, opened on the failed rows rather than on all of them. Whoever
// cannot read that log gets the same count as a plain pill - a link to a screen
// the control plane would refuse them is worse than no link.
//
// It is a real link rather than a button, so that it opens in a new tab like
// anything else somebody wants to keep beside the dashboard.
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

// isAdmin is whoever can act on the first-run checklist and read the request
// log. A member gets the ordinary empty dashboard instead of the checklist -
// telling them to create an organisation would be telling them to do something
// the control plane will refuse - and the numbers without a way through to the
// rows behind them, which name other people's keys.
function isAdmin(ctx) {
  return ctx.state.me.unrestricted || ctx.state.me.role === "admin";
}

function rangeLabel(since) {
  const r = RANGES.find((x) => x.since === since);
  return r ? r.long : "";
}
