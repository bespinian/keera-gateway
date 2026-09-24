// Routers: the hook that chooses which model answers a request.
//
// A filter reads a request and changes it. A router reads a request and
// changes where it goes, so the requests that need the large model get it and
// a prompt carrying a client's data is not the one that leaves the cluster.
//
// It earns a screen because every request a router places is answered. A
// filter that goes wrong edits or refuses somebody's work and somebody
// complains; a router that goes wrong returns a perfectly good answer out of
// the wrong model and produces traffic indistinguishable from one that works.
// The one number that tells them apart is the split between its destinations.
//
// The screen also has to make visible that a router is reached by a client
// naming it: nothing an administrator does here points traffic at one.

import { api } from "../api.js";
import {
  h,
  table,
  modal,
  confirm,
  toast,
  pill,
  stat,
  rowLink,
  go,
  compact,
  num,
  money,
  ms,
  icon,
  icons,
  aliasProblem,
  showError,
  currentRange,
  rangePicker,
} from "../ui.js";
import { barList } from "../chart.js";
import { chooseOrg } from "./orgs.js";
import { facts } from "./detail.js";

export async function routersView(ctx) {
  // A router is one organisation's judgement about which of its requests
  // deserve which model, so it belongs to one rather than to the deployment.
  if (!ctx.orgID) return chooseOrg(ctx, "Routers");

  const since = currentRange();
  const [res, models] = await Promise.all([
    api.routers(ctx.orgID, { stats: true, since }),
    api.models().then((r) => r.data || []),
  ]);
  const routers = res.data || [];
  const stats = res.stats || {};
  const currency = res.currency || ctx.currency;
  const chatModels = models.filter((m) => m.kind === "chat");
  const canEdit = ctx.state.me.can_admin_org || ctx.state.me.unrestricted;
  ctx.setSubtitle(`${routers.length} router${routers.length === 1 ? "" : "s"}`);

  const head = h(
    "div",
    { class: "detail-head" },
    h(
      "div",
      { class: "muted" },
      "A router picks which model answers a request: a model reads the " +
        "request, or the destinations are tried until one answers - in the " +
        "listed order, fastest first or least busy first. Clients use a " +
        "router's alias as the model name. Give them the alias, or limit a " +
        "scope's allowed models to it. A router that reads requests should " +
        "use a fast local model: a hosted one receives the prompt before it " +
        "decides.",
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
              onClick: () => editRouter(ctx, null, chatModels),
            },
            icon(icons.plus),
            "New router",
          )
        : null,
    ),
  );

  const known = new Set(models.map((m) => m.alias));
  const rows = table(
    [
      {
        label: "Alias",
        cell: (rt) =>
          rowLink(
            ctx,
            "/routers/" + encodeURIComponent(rt.alias),
            h("strong", { class: "mono" }, rt.alias),
            "Where this router sent traffic",
          ),
      },
      // The mode. It is the first thing to know about a row, because every
      // other column means something different depending on it: a fallback
      // router's list is an order of preference, not a menu.
      {
        label: "Mode",
        shrink: true,
        cell: (rt) => modeCell(rt, known),
      },
      // How many places this router may send a prompt. The list itself is on
      // the router's own screen: a table of routers is read to find the one
      // row worth opening, and a column of chips that wraps to three lines
      // per row is what stops it being readable enough to do that.
      {
        label: "Destinations",
        shrink: true,
        cell: (rt) => destinationCount(rt, known),
      },
      // Where it has actually been sending traffic. Without this a row says
      // what a router is meant to do and nothing about whether it does it.
      {
        label: "In this window",
        shrink: true,
        cell: (rt) => trafficCell(stats[rt.alias], currency),
      },
      // The column the alias was widening to fill. Without it the first
      // column takes every pixel the rest do not, so a six-character alias
      // sits alone in half a table; with it the row says what the router is
      // for in the space that was empty.
      {
        label: "Description",
        cell: (rt) =>
          rt.description
            ? h("span", { class: "muted clamp-2" }, rt.description)
            : h("span", { class: "faint" }, "-"),
      },
      {
        label: "Check",
        shrink: true,
        cell: (rt) => (canEdit ? checkCell(ctx, rt) : null),
      },
      {
        label: "",
        shrink: true,
        cell: (rt) => {
          if (!canEdit) return null;
          return h(
            "div",
            { class: "row-tight" },
            h(
              "button",
              {
                class: "btn btn-sm",
                onClick: () => editRouter(ctx, rt, chatModels),
              },
              "Edit",
            ),
            h(
              "button",
              {
                class: "btn btn-sm btn-danger",
                onClick: () =>
                  confirm({
                    title: `Remove ${rt.alias}?`,
                    body:
                      "Clients still using this alias will be told the " +
                      "model does not exist. The panel cannot see editor " +
                      "settings, so check who has the alias first.",
                    confirmLabel: "Remove router",
                    danger: true,
                    onConfirm: async () => {
                      await api.deleteRouter(ctx.orgID, rt.alias);
                      toast("Router removed", "good");
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
    routers,
    {
      emptyTitle: "No routers",
      emptyBody:
        "Each request goes to the model the client named, and fails if that " +
        "model fails. Add a router to send most requests to a smaller model, " +
        "keep some inside the cluster, or fall back when a model is down.",
    },
  );

  return h("div", {}, head, rows);
}

// decides reports whether this router chooses by reading the request. A router
// saved before modes existed says nothing, and that means the only thing it
// could have been.
function decides(rt) {
  return modeOf(rt) === "instruction";
}

// modeOf is the mode, with the one a router saved before modes existed meant.
function modeOf(rt) {
  return rt.mode || "instruction";
}

// measures reports whether this router's order is something the gateway worked
// out rather than something somebody wrote. It is the question every screen
// here has to ask before it prints a list of destinations in order: on these
// two modes that order is a reading of the last few minutes on one gateway
// replica, so showing it numbered would be a claim this page cannot make.
function measures(rt) {
  const mode = modeOf(rt);
  return mode === "latency" || mode === "least-busy";
}

// sizes reports whether this router chooses by how much text is in the request.
// It is the odd mode out here: it reads the request like an instruction router,
// and costs nothing like the three that read nothing at all.
function sizes(rt) {
  return modeOf(rt) === "size";
}

// ceilingOf is the largest request, in estimated tokens, this router means to
// give a destination - or null for the one that takes what the rest will not.
function ceilingOf(rt, alias) {
  const n = (rt.ceilings || {})[alias];
  return n > 0 ? n : null;
}

// tokens renders a ceiling the way somebody would have typed it: these get set
// to round numbers, and '32k' is how they are said.
function tokens(n) {
  return n >= 1000 && n % 1000 === 0 ? n / 1000 + "k" : String(n);
}

// ordersBy is what puts a router's destinations in the order it tries them, in
// a clause. It is the sentence that makes the three modes that read nothing
// tell themselves apart, and it is the only thing that does.
function ordersBy(rt) {
  return ordersByMode(modeOf(rt));
}

// ordersByMode is the same sentence taken from a mode alone, for the check,
// which reports the mode it ran without the router around it.
function ordersByMode(mode) {
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

// modeCell is the mode, which is the thing to know before reading any other
// column on the row, and on an instruction router the model it reads with -
// which there is no other column for.
function modeCell(rt, known) {
  if (sizes(rt)) {
    return h(
      "span",
      { title: "places each request by " + ordersBy(rt) },
      pill("size", "good"),
    );
  }
  if (!decides(rt)) {
    return h(
      "span",
      { title: "tries its destinations in the order of " + ordersBy(rt) },
      pill(modeOf(rt)),
    );
  }
  return h(
    "div",
    { class: "row-tight" },
    pill("instruction", "accent"),
    known.has(rt.model)
      ? h("span", { class: "mono muted" }, rt.model)
      : h(
          "div",
          { class: "row-tight" },
          h("span", { class: "mono muted" }, rt.model),
          pill("missing", "bad"),
        ),
  );
}

// destinationCount is how many models this router may send a request to, with
// the ones the catalogue can no longer serve called out.
//
// The names are on the router's own screen. What the table owes a reader is
// the size of the choice: one destination is not a router, and eight is a
// deployment nobody is holding in their head.
//
// The missing count stays here even though the names do not. A destination
// that cannot be reached is quietly left out, so a router listed as choosing
// between four models and actually choosing between two looks exactly like one
// that works.
function destinationCount(rt, known) {
  const destinations = rt.destinations || [];
  if (destinations.length === 0) {
    return h("span", { class: "faint" }, "-");
  }
  const missing = destinations.filter((alias) => !known.has(alias)).length;
  return h(
    "div",
    { class: "row-tight" },
    h(
      "span",
      { class: "nowrap" },
      destinations.length,
      destinations.length === 1 ? " model" : " models",
    ),
    missing > 0 ? pill(`${missing} missing`, "bad") : null,
  );
}

// fallbackPill says what happens to a request the router cannot place.
//
// It is on the router's own screen rather than in the list. The two answers
// are opposites and neither is right in general - one that saves money has
// somewhere safe to fall back to, one that keeps prompts inside the cluster
// does not - but that is a decision read while looking at one router, not a
// column scanned down a table of them.
//
// A fallback router has no such setting. By the time it has failed it has
// tried everywhere, so the client gets the last destination's own answer.
function fallbackPill(rt) {
  if (sizes(rt)) {
    return h("span", { class: "muted nowrap" }, "the next destination up");
  }
  if (!decides(rt)) {
    return h("span", { class: "muted nowrap" }, "the last one's answer");
  }
  if (!rt.fallback) return pill("refuse the request", "warn");
  return h("span", { class: "pill mono" }, "→ " + rt.fallback);
}

// trafficCell is what this router did with the window's requests, in the room a
// row has: how many it placed, and how much of that was placed without a
// decision. The split between destinations is on the router's own screen.
function trafficCell(st, currency) {
  if (!st || !st.requests) {
    return h("span", { class: "faint nowrap" }, "no traffic");
  }
  const undecided = (st.fell_back || 0) + (st.errored || 0);
  const rate = undecided / st.requests;
  return h(
    "div",
    { class: "stack", style: { gap: "3px" } },
    h(
      "div",
      { class: "row-tight" },
      h("span", { class: "nowrap" }, compact(st.requests), " placed"),
      undecided > 0
        ? pill(
            `${(rate * 100).toFixed(1)}% undecided`,
            rate >= 0.25 ? "bad" : "warn",
          )
        : null,
    ),
    h(
      "div",
      { class: "faint nowrap", style: { fontSize: "11.5px" } },
      money(st.cost_micros, currency),
      " on the destinations",
    ),
  );
}

// checkCell puts three sample prompts through the router and shows where each
// one went.
//
// Nothing about the result is cached, for the reason a filter's check is not: a
// router is a small model following prose, and where it sent a sample ten
// minutes ago is not a claim worth showing beside what it will do next.
function checkCell(ctx, rt) {
  const slot = h("span");
  const run = h(
    "button",
    {
      class: "btn btn-sm btn-quiet",
      title: "Test this router with sample prompts",
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
          const probe = await api.checkRouter(ctx.orgID, rt.alias);
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
  const tone = probe.ok ? (warned ? "warn" : "good") : "bad";
  const label = !probe.ok
    ? "Not usable"
    : warned
      ? "Ran, with notes"
      : "Working";
  return h(
    "button",
    {
      class: "pill pill-" + tone + " pill-button",
      title: "Where the router sent each sample",
      onClick: () => report(ctx, probe),
    },
    h("span", { class: "dot" }),
    label,
  );
}

// report shows each sample prompt and the model the router chose for it.
//
// There is no pass here, for the reason a filter's check has none: whether
// these are the right destinations for this organisation's work is a judgement
// only the person reading it can make. What the check does say plainly is the
// one thing that is wrong in any organisation - a router that answered every
// sample the same way, which is not a router deciding anything.
function report(ctx, probe) {
  const chained = probe.mode !== "instruction";
  const ranked = probe.mode === "latency" || probe.mode === "least-busy";
  const banded = probe.mode === "size";
  modal({
    wide: true,
    title: `Check - ${probe.alias}`,
    subtitle: !probe.ok
      ? chained
        ? "This router has nowhere to send a request."
        : "This router could not place the samples."
      : banded
        ? "Each destination, the request sizes it takes first, and whether " +
          "it is answering. This depends only on size, so it is the full " +
          "picture."
        : ranked
          ? "Each destination in the order this router would try them now, " +
            "with what it measured. The order can change from minute to minute."
          : chained
            ? "Each destination in the order this router would try them. The " +
              "first that answers serves every request."
            : "Three very different prompts. They should not all go to the " +
              "same model.",
    body: h(
      "div",
      {},
      probe.error
        ? h("div", { class: "banner banner-bad" }, probe.error)
        : null,
      (probe.warnings || []).map((warning) =>
        h("div", { class: "banner banner-warn" }, warning),
      ),
      facts([
        probe.model
          ? ["Runs on", h("span", { class: "mono" }, probe.model)]
          : null,
        ranked || banded ? ["In the order of", ordersByMode(probe.mode)] : null,
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
              money(probe.cost_micros, ctx.currency) +
                " - each request to this router pays a share of this on top",
            ]
          : null,
      ]),
      (probe.chain || []).length
        ? h(
            "div",
            { class: "card", style: { marginTop: "12px" } },
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
                      ? pill("yes", "good")
                      : pill(hop.status ? String(hop.status) : "no", "bad"),
                },
                {
                  label: "Took",
                  shrink: true,
                  cell: (hop) =>
                    hop.ms ? ms(hop.ms) : h("span", { class: "faint" }, "-"),
                },
                // What the router measured of this destination, on the two
                // modes where the position above is a measurement rather than
                // a setting. It is the column that answers the question the
                // order provokes - why is this one first - and it is absent on
                // the modes where the answer is that somebody wrote it there.
                ranked || banded
                  ? {
                      label: banded ? "Takes" : "Reading",
                      cell: (hop) =>
                        hop.reading
                          ? h("span", { class: "muted" }, hop.reading)
                          : h("span", { class: "faint" }, "-"),
                    }
                  : null,
                {
                  label: "Note",
                  cell: (hop) =>
                    hop.note
                      ? h("span", { class: "muted" }, hop.note)
                      : h("span", { class: "faint" }, "-"),
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
                ? pill("could not decide", "bad")
                : h("span", { class: "pill pill-good mono" }, d.chosen),
              // How much of the choice that destination took. A sample decided
              // at 35% went where it went by a hair, which is the state a
              // destination alone cannot show. Absent where the deciding
              // model's backend serves no probabilities.
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
              d.error
                ? h("div", { class: "banner banner-bad" }, d.error)
                : null,
            ),
          ),
        ),
      ),
    ),
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Close"),
    ],
  });
}

/* ------------------------------------------------------------ one router */

export async function routerDetailView(ctx) {
  if (!ctx.orgID) return chooseOrg(ctx, "Routers");

  const since = currentRange();
  const res = await api.routerReport(ctx.orgID, ctx.param, since);
  const rep = res.report || {};
  const rt = res.router || null;
  const currency = res.currency || ctx.currency;
  const canEdit = ctx.state.me.can_admin_org || ctx.state.me.unrestricted;
  const placed = rep.total ? rep.total.requests || 0 : 0;

  ctx.setTitle(ctx.param, rt ? identity(rt) : "no longer in this organisation");

  const wrap = h(
    "div",
    {},
    h(
      "div",
      { class: "detail-head" },
      h(
        "a",
        { class: "crumb", href: "/routers", onClick: go(ctx, "/routers") },
        icon(icons.back),
        "Routers",
      ),
      h(
        "div",
        { class: "row", style: { flexWrap: "wrap" } },
        h(
          "div",
          { class: "wrap-chips" },
          rt
            ? [
                pill(
                  decides(rt)
                    ? "decides with " + rt.model
                    : sizes(rt)
                      ? "places " +
                        (rt.destinations || []).length +
                        " destinations by size"
                      : "tries " +
                        (rt.destinations || []).length +
                        " destinations, " +
                        (measures(rt) ? "by " + ordersBy(rt) : "in order"),
                ),
                fallbackPill(rt),
              ]
            : [pill("Removed from this organisation", "warn")],
        ),
        h("div", { style: { flex: 1 } }),
        rangePicker(ctx, since),
        rt && canEdit
          ? h(
              "button",
              {
                class: "btn",
                title: "Test this router with sample prompts",
                onClick: async (e) => {
                  const button = e.currentTarget;
                  button.disabled = true;
                  try {
                    report(ctx, await api.checkRouter(ctx.orgID, rt.alias));
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
        rt && canEdit
          ? h(
              "button",
              {
                class: "btn",
                onClick: () =>
                  api.models().then((r) =>
                    editRouter(
                      ctx,
                      rt,
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

  for (const banner of detailBanners(rep, rt, placed)) wrap.append(banner);

  // "Placed" leads because every share below it is meaningless without it: a
  // router that sent its whole traffic to one model over three requests is not
  // a router that has stopped deciding, it is a router nobody has used yet.
  wrap.append(
    h(
      "div",
      { class: "grid grid-4" },
      stat(
        "Placed",
        compact(placed),
        null,
        placed ? "requests to this router" : "no requests in this window",
      ),
      stat(
        rt && measures(rt)
          ? "Best ranked"
          : rt && !decides(rt)
            ? "First choice"
            : "Decided",
        pct(rep.chose, placed),
        null,
        chosenNote(rt, rep),
      ),
      // What the router added to the wait, which is a generation on one that
      // reads the request and the destinations that did not answer on one
      // that tries them. Both are time the client spent before the model that
      // answered had been asked anything.
      stat(
        rt && !decides(rt) ? "Lost to failover" : "Deciding cost",
        ms(rep.decision_median_ms || 0),
        null,
        `p95 ${ms(rep.decision_p95_ms || 0)}, added per request`,
      ),
      stat(
        "Spend",
        money(rep.total ? rep.total.cost_micros || 0 : 0, ""),
        currency,
        "destinations' cost, decisions included",
      ),
    ),
  );

  // The split. It is the reason this screen exists: it is the only figure that
  // distinguishes a router doing its job from an expensive way of hard-coding
  // one destination, and both produce nothing but successful requests.
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
          h("h2", {}, "Where it sent them"),
          h("div", { class: "spacer" }),
          placed ? pill(`${compact(placed)} placed`) : null,
        ),
        h(
          "div",
          { class: "card-body" },
          (rep.destinations || []).length
            ? barList(rep.destinations, {
                label: (d) => d.alias || "(no model)",
                value: (d) => d.requests,
              })
            : h("div", { class: "empty" }, "Nothing placed in this window."),
        ),
      ),
      h(
        "div",
        { class: "card" },
        h(
          "div",
          { class: "card-head" },
          h("h2", {}, "What it cost per destination"),
        ),
        h(
          "div",
          { class: "card-body" },
          (rep.destinations || []).length
            ? barList(rep.destinations, {
                label: (d) => d.alias || "(no model)",
                value: (d) => d.cost_micros,
                currency,
                colorIndex: 2,
              })
            : h("div", { class: "empty" }, "Nothing charged in this window."),
        ),
      ),
    ),
  );

  if ((rep.destinations || []).length) {
    wrap.append(
      h(
        "div",
        { class: "card", style: { marginTop: "16px" } },
        h("div", { class: "card-head" }, h("h2", {}, "Each destination")),
        table(
          [
            {
              label: "Model",
              cell: (d) =>
                h("span", { class: "mono" }, d.alias || "(no model)"),
            },
            {
              label: "Requests",
              shrink: true,
              cell: (d) => num(d.requests),
            },
            {
              label: "Share",
              shrink: true,
              cell: (d) => pct(d.requests, placed),
            },
            {
              label: "Tokens",
              shrink: true,
              cell: (d) => compact(d.input_tokens + d.output_tokens),
            },
            {
              label: "Spend",
              shrink: true,
              cell: (d) => money(d.cost_micros, currency),
            },
            // The reason a cheap destination is not always the right one. A
            // router that halved the bill and doubled what a developer waits
            // for has not obviously helped anybody.
            {
              label: "Median wait",
              shrink: true,
              cell: (d) => ms(d.ttft_median_ms),
            },
            {
              label: "Refused",
              shrink: true,
              cell: (d) =>
                d.refused
                  ? pill(num(d.refused), "warn")
                  : h("span", { class: "faint" }, "-"),
            },
            {
              label: "Failed",
              shrink: true,
              cell: (d) =>
                d.failed
                  ? pill(num(d.failed), "bad")
                  : h("span", { class: "faint" }, "-"),
            },
          ],
          rep.destinations,
          {},
        ),
      ),
    );
  }

  if (rt) {
    wrap.append(
      h(
        "div",
        { class: "card", style: { marginTop: "16px" } },
        h(
          "div",
          { class: "card-head" },
          h("h2", {}, decides(rt) ? "What it was told" : "How it is written"),
        ),
        h(
          "div",
          { class: "card-body" },
          facts([
            rt.description ? ["Description", rt.description] : null,
            [
              "Mode",
              decides(rt) ? pill("instruction", "accent") : pill(modeOf(rt)),
            ],
            decides(rt)
              ? ["Runs on", h("span", { class: "mono" }, rt.model)]
              : null,
            decides(rt) ? null : ["In the order of", ordersBy(rt)],
            [
              decides(rt)
                ? "Chooses between"
                : sizes(rt)
                  ? "Places between"
                  : measures(rt)
                    ? "Tries"
                    : "Tries in order",
              h(
                "div",
                { class: "wrap-chips" },
                // Numbered only where the number is the configuration. On a
                // measured router the order here is the tie-break and nothing
                // more, and printing 1. 2. 3. beside it would be this screen
                // stating a running order it does not know. On a size router
                // the ceiling is the configuration, so it goes on the chip:
                // the list without it says nothing about where anything goes.
                (rt.destinations || []).map((alias, i) => {
                  if (sizes(rt)) {
                    const ceiling = ceilingOf(rt, alias);
                    return h(
                      "span",
                      { class: "pill mono" },
                      ceiling
                        ? `${alias} ≤ ${tokens(ceiling)}`
                        : `${alias} - anything larger`,
                    );
                  }
                  return h(
                    "span",
                    { class: "pill mono" },
                    (decides(rt) || measures(rt) ? "" : i + 1 + ". ") + alias,
                  );
                }),
              ),
            ],
            [
              decides(rt) ? "Cannot decide" : "Cannot place it",
              sizes(rt)
                ? "the next larger destination takes it; if none can, the " +
                  "client gets the last one's answer"
                : !decides(rt)
                  ? "all destinations have been tried, and the client gets the " +
                    "last one's answer"
                  : rt.fallback
                    ? `the request goes to ${rt.fallback} without a decision`
                    : "the request is refused",
            ],
            [
              "How a client reaches it",
              h("span", { class: "mono" }, `"model": "${rt.alias}"`),
            ],
          ]),
          // The instruction is the router, so it is shown whole rather than
          // clamped to a row: it is the one field somebody reading this screen
          // has to be able to compare against where the traffic went. A
          // fallback router has none - the list above is the whole of it.
          decides(rt)
            ? h(
                "pre",
                {
                  class: "key-value",
                  style: {
                    whiteSpace: "pre-wrap",
                    userSelect: "text",
                    marginTop: "12px",
                  },
                },
                rt.prompt || "",
              )
            : null,
        ),
      ),
    );
  }

  return wrap;
}

// detailBanners are the things worth saying before any of the numbers, because
// each of them changes how the numbers should be read.
function detailBanners(rep, rt, placed) {
  const out = [];
  if (!rt) {
    out.push(
      h(
        "div",
        { class: "banner banner-warn" },
        "This router no longer exists. Below is the traffic it placed " +
          "before. Clients still using it are told the model does not exist.",
      ),
    );
  }
  if (!placed) {
    out.push(
      h(
        "div",
        { class: "banner banner-info" },
        "No requests used this router in this window. Clients reach it by " +
          "using its alias as the model name, so either nobody has the alias " +
          "or nobody called.",
      ),
    );
    return out;
  }
  const chained = rt && !decides(rt);
  const ranked = rt && measures(rt);
  const banded = rt && sizes(rt);
  // A router that never gets its first answer is the failure this screen
  // exists for. It is not visible in any request, because every one of them
  // was answered - by something else.
  if (rep.chose === 0) {
    out.push(
      h(
        "div",
        { class: "banner banner-bad" },
        banded
          ? "No request in this window was served by the destination its " +
              "size was meant for. Each one waited for it to fail and was " +
              "served by a larger one. Check the destinations."
          : ranked
          ? "The destination this router ranked first never answered in this " +
              "window. Each request waited for it to fail and was served " +
              "further down. The ranking is not working."
          : chained
            ? "The first destination never answered in this window. Each " +
              "request waited for it to fail and was served further down. " +
              "Check it."
            : "This router made no decision in this window. It still costs a " +
              "generation per request. Check it and the model it decides with.",
      ),
    );
  } else if (rep.fell_back > 0 && rep.fell_back / placed >= 0.1) {
    out.push(
      h(
        "div",
        { class: "banner banner-warn" },
        banded
          ? `${pct(rep.fell_back, placed)} of these requests were served by a ` +
              "larger destination after the one for their size failed. Not an " +
              "error, but it costs more and is reported nowhere else."
          : ranked
          ? `${pct(rep.fell_back, placed)} of these requests were not served ` +
              "by the destination ranked first. Each waited for it to fail " +
              "first: this is the cost of a wrong ranking."
          : chained
            ? `${pct(rep.fell_back, placed)} of these requests were served ` +
              "further down the chain, after the ones before failed."
            : `${pct(rep.fell_back, placed)} of these requests were placed ` +
              "without a decision, but still paid for the attempt.",
      ),
    );
  }
  if (rep.errored > 0) {
    out.push(
      h(
        "div",
        { class: "banner banner-bad" },
        chained
          ? `${num(rep.errored)} request${rep.errored === 1 ? "" : "s"} ran out of ` +
              "destinations: every model failed, and the client got the last " +
              "one's answer."
          : `${num(rep.errored)} request${rep.errored === 1 ? " was" : "s were"} refused ` +
              "because this router could not place them and has no fallback. " +
              "The clients were only told that a guardrail refused them.",
      ),
    );
  }
  // One destination taking everything, on enough traffic to mean it. Not an
  // error, and worth a sentence: it is what a hard-coded alias looks like.
  const destinations = rep.destinations || [];
  if (!chained && rep.chose > 0 && destinations.length === 1 && placed >= 20) {
    out.push(
      h(
        "div",
        { class: "banner banner-warn" },
        `Every request went to ${destinations[0].alias}. The router costs a ` +
          "generation per request for the same result as naming that model " +
          "directly.",
      ),
    );
  }
  return out;
}

// chosenNote says what the headline share actually counted, which is not the
// same sentence for the kinds of router even though it is the same number: the
// router got what it wanted.
function chosenNote(rt, rep) {
  if (!rep.chose) {
    if (rt && measures(rt)) return "what it ranked first answered nothing";
    return rt && !decides(rt)
      ? "the first destination answered nothing"
      : "no decision was made";
  }
  if (rt && measures(rt)) {
    return `${num(rep.chose)} answered by the destination it ranked first`;
  }
  return rt && !decides(rt)
    ? `${num(rep.chose)} answered by the first destination`
    : `${num(rep.chose)} chosen by reading the request`;
}

function identity(rt) {
  const destinations = (rt.destinations || []).length;
  const models = `${destinations} model` + (destinations === 1 ? "" : "s");
  if (decides(rt)) return `decides with ${rt.model}, between ${models}`;
  if (sizes(rt)) return `places between ${models}, by the size of the request`;
  if (measures(rt)) return `tries ${models}, by ${ordersBy(rt)}`;
  return `tries ${models} in order, first that answers`;
}

function pct(part, whole) {
  if (!whole) return "-";
  return ((100 * (part || 0)) / whole).toFixed(1) + "%";
}

/* -------------------------------------------------------------- editing one */

// MODES are the five things a router can choose with, as the dialog offers
// them: a mark, a name, and the whole of what the mode does with the list of
// destinations. One list, because the tiles that pick a mode and the row that
// reports the one picked have to say the same thing about it.
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

function modeNamed(value) {
  return MODES.find((m) => m.value === value) || MODES[0];
}

function editRouter(ctx, existing, chatModels) {
  const err = h("div");
  const creating = !existing;
  // No mode among the defaults: a new router has none until one of the tiles
  // is clicked, and a default here would be the answer to the one question
  // this dialog asks before any other.
  const rt = existing || {
    alias: "",
    model: "",
    prompt: "",
    destinations: [],
    fallback: "",
    description: "",
  };

  const alias = h("input", {
    class: "input mono",
    placeholder: "simple-router",
    value: rt.alias,
    disabled: !creating,
  });
  const description = h("input", {
    class: "input",
    placeholder: "Keeps the large model for the work that needs it",
    value: rt.description || "",
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
      h("option", { value: m.alias, selected: m.alias === rt.model }, m.alias),
    ),
  );
  const prompt = h("textarea", { class: "input", rows: "8" });
  prompt.value = rt.prompt || PROMPT_EXAMPLE;

  // What chooses. It is asked first and on its own, because it decides what
  // the rest of the form means: the same list of models is a menu to choose
  // from on one kind of router, an order of preference on the next and a set
  // of size bands on the third. As a select halfway down the form it was a
  // question answered after the fields it governs, most of which were hidden
  // until it had been.
  let chosen = creating ? null : modeOf(rt);
  const reads = () => chosen === "instruction";
  // Whether the ceilings are the configuration. A size router's list of
  // destinations means nothing on its own: without a number beside each,
  // nothing says where anything goes.
  const bySize = () => chosen === "size";
  // Whether the running order is something this form sets. On the two measured
  // modes the list below is still worth ordering - it is what breaks a tie
  // between destinations the gateway cannot tell apart - but it is no longer
  // the whole configuration, and the hint has to stop saying that it is.
  const ranks = () => chosen === "latency" || chosen === "least-busy";

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
  // The way back to the five. Changing the mode keeps the form filled in: the
  // alias, the destinations and their order mean something under every mode,
  // and what the new mode has no use for is hidden rather than discarded, so
  // going to look at the other tiles costs nothing.
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
    // router, the first thing left to type; on one being changed, the control
    // that opened the tiles. Before the dialog is in the document neither
    // does anything, which is what the first choice of an edit wants.
    if (creating && !alias.value) alias.focus();
    else change.focus();
  };
  const modeField = h(
    "div",
    { class: "field" },
    h("label", {}, "What chooses"),
    tiles,
    chosenRow,
  );

  // The destinations, as checkboxes over the catalogue rather than a text
  // field. The list is the authority on where this router can send a prompt,
  // so choosing from what exists is the only way to write one that cannot
  // name a model the deployment does not have.
  //
  // They are kept in an array rather than a set because on a fallback router
  // the order is the entire configuration, and it has to be something
  // somebody can see and change rather than a side effect of which box was
  // ticked first.
  const picked = [...(rt.destinations || [])];
  const fallback = h("select", { class: "select" });
  const syncFallback = () => {
    const previous = fallback.value || rt.fallback || "";
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

  // The order, shown only when it means something. A fallback router whose
  // destinations were ticked in the wrong order is a deployment serving every
  // request out of its second choice, and nothing else on this screen would
  // say so.
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
            ? pill("tried first", "good")
            : h("span", { class: "faint" }, "if the ones above fail"),
          h("div", { style: { flex: 1 } }),
          h(
            "button",
            {
              class: "btn btn-sm",
              type: "button",
              title: "Try this one earlier",
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
              title: "Try this one later",
              disabled: i === picked.length - 1,
              onClick: () => move(i, 1),
            },
            "\u2193",
          ),
        ),
      ),
    );
  };

  // A ceiling per destination, for a size router. Kept here rather than read
  // off the inputs, so unticking a destination and ticking it again does not
  // lose the number that was typed against it.
  const ceilings = { ...(rt.ceilings || {}) };
  const ceilingList = h("div", { class: "stack", style: { gap: "4px" } });
  const ceilingHint = h("div", { class: "hint" });
  // What each row says beside its box, kept by destination. The words follow
  // the numbers rather than the list of destinations, so they are written on
  // their own: rebuilding the rows to say them would take the cursor out of
  // the box somebody is typing into, a digit at a time.
  const notes = new Map();
  const syncCeilingNotes = () => {
    for (const [alias, note] of notes) {
      note.textContent = ceilings[alias]
        ? "tokens at most"
        : "takes what is larger than every ceiling";
    }
    // The control plane refuses a router without exactly one unbounded
    // destination. Saying so here is what stops somebody filling in every box
    // and being told no at the last step.
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
            const raw = box.value.trim().toLowerCase();
            const n = raw.endsWith("k")
              ? Number(raw.slice(0, -1)) * 1000
              : Number(raw);
            if (raw === "" || !Number.isFinite(n) || n <= 0) {
              delete ceilings[alias];
            } else {
              ceilings[alias] = Math.round(n);
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
  const ceilingField = h(
    "div",
    { class: "field" },
    h("label", {}, "Up to this many input tokens"),
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
          // The description is what the deciding model is actually told about
          // this destination, so it is shown here rather than left to be
          // discovered as a router choosing between bare names.
          m.description
            ? h("div", { class: "hint" }, m.description)
            : h(
                "div",
                { class: "hint" },
                "No description, so the router sees only the alias. Add one " +
                  "on the model.",
              ),
        ),
      );
    }),
  );
  syncFallback();
  renderOrder();
  renderCeilings();

  // The fields only one kind of router has. They are hidden rather than
  // disabled, because a form that offers an instruction to a router that reads
  // nothing is a form inviting somebody to write one and wonder where it went.
  const field = (label, control, hint) =>
    h(
      "div",
      { class: "field" },
      h("label", {}, label),
      control,
      hint ? h("div", { class: "hint" }, hint) : null,
    );
  const modelField = field(
    "Runs on",
    model,
    // Short, but not the short version of a different sentence: where this
    // model runs is the one thing about it somebody can get wrong without
    // anything on this screen going red.
    "A small, fast model. It reads every request before any filter, so " +
      "keep it inside the cluster.",
  );
  const fallbackField = field(
    "When it cannot decide",
    fallback,
    // Which answer is right is a property of the deployment, not of this
    // form, so the hint gives the two cases rather than a recommendation.
    "Fall back if the router saves money. Refuse if it keeps prompts " +
      "inside the cluster.",
  );
  const promptField = field(
    "Instruction",
    prompt,
    "Name kinds of request, not models. The destinations and their " +
      "descriptions are added for you.",
  );
  const orderHint = h("div", { class: "hint" });
  const orderField = h(
    "div",
    { class: "field" },
    h("label", {}, "In this order"),
    order,
    orderHint,
  );
  const destinationsLabel = h("label", {}, "Chooses between");
  const destinationsHint = h("div", { class: "hint" });
  const costBanner = h("div", { class: "banner banner-info" });

  const syncMode = () => {
    // Nothing below the tiles until one of them is chosen, Save included:
    // there is nothing to save yet, and a Save button beside five unanswered
    // tiles is one inviting somebody to skip the question.
    rest.hidden = chosen === null;
    saveButton.hidden = chosen === null;
    saveButton.disabled = chosen === null;
    if (chosen === null) return;
    for (const el of [modelField, fallbackField, promptField]) {
      el.hidden = !reads();
    }
    ceilingField.hidden = !bySize();
    // A size router's order is the ceilings, so the order list would be a
    // second answer to the same question - and one this form does not use.
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
    // Each variant says only what this mode does with the list. That a key
    // allowed the router is allowed everywhere it sends is the same sentence
    // in all three, so it is said once, in the subtitle.
    destinationsHint.textContent = reads()
      ? "At least two."
      : bySize()
        ? "At least two, each with the largest request it should be given."
        : "At least two, tried until one answers.";
    if (bySize()) {
      costBanner.textContent =
        "Costs nothing: it counts the request's tokens without reading " +
        "them. It cannot tell a hard request from an easy one of the same " +
        "length.";
      renderCeilings();
      return;
    }
    costBanner.textContent = reads()
      ? "Every request waits for the decision, which is charged to the same " +
        "budgets."
      : ranks()
        ? "Costs nothing itself; it ranks by what each gateway process " +
          "measures. A request pays for every destination that fails before " +
          "one answers."
        : "Costs nothing itself. A request pays for every destination that " +
          "fails before one answers.";
  };

  // Everything the mode governs, in one wrapper so the dialog can hold it back
  // until there is a mode. The alias is in here too: a router is named for
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
            ? "Lowercase letters, digits and hyphens. Clients use it as the " +
                "model name."
            : "Clients use this name, so it cannot change.",
        ),
      ),
      h("div", { class: "field" }, h("label", {}, "Description"), description),
    ),
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
        // addresses no router, so the write would not reach the handler that
        // could say so.
        const problem = aliasProblem(alias.value.trim());
        if (problem) {
          showError(err, problem);
          alias.focus();
          return;
        }
        button.disabled = true;
        try {
          await api.putRouter(ctx.orgID, alias.value.trim(), {
            mode: chosen,
            // A router that reads nothing carries nothing that would decide
            // for it. Sending what the form still holds would save a deciding
            // model on a router that never asks it anything, which is a thing
            // somebody would later read as the reason its traffic goes where
            // it goes.
            model: reads() ? model.value : "",
            prompt: reads() ? prompt.value : "",
            destinations: [...picked],
            ceilings: bySize()
              ? Object.fromEntries(
                  picked
                    .filter((alias) => ceilings[alias])
                    .map((alias) => [alias, ceilings[alias]]),
                )
              : {},
            fallback: reads() ? fallback.value : "",
            description: description.value.trim(),
          });
          close();
          toast("Router saved", "good");
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
    title: creating ? "New router" : `Edit ${rt.alias}`,
    subtitle: creating
      ? "Choose how it picks first; the rest of the form depends on it. A " +
        "key allowed to use this router can reach all its destinations."
      : "A key allowed to use this router can reach all its destinations.",
    body: h("form", {}, err, modeField, rest),
    actions: (dismiss) => [
      h("button", { class: "btn", onClick: dismiss }, "Cancel"),
      saveButton,
    ],
  });

  // The keyboard starts on the tiles. The dialog's own rule - the first field
  // somebody can type in - finds nothing on a new router, because until a mode
  // is chosen there is nothing to type in.
  if (creating) tiles.firstChild.focus();
}

// PROMPT_EXAMPLE is a starting point rather than a default. It is the shape a
// routing instruction has to have: name the kinds of request rather than the
// models, because what each model is for is added from the catalogue - and say
// which way to err, because the decision is made in one word by a small model
// and it will be wrong sometimes.
const PROMPT_EXAMPLE = `Choose the model that suits this request.

Send small, self-contained requests to the cheapest capable model: a rename, a \
syntax question, a short edit to one file, a commit message.

Send requests needing design, several steps at once, or judgement about a \
system to a model that can reason.

Send anything containing a client's name, an account number, personal details \
or a credential to a model inside the cluster, whatever else it is asking for. \
That takes priority.

If you are unsure, choose the more capable model rather than the cheaper one.`;
