// Routers: the hook that chooses which model answers a request.
//
// A router that goes wrong still returns good answers, just from the wrong
// model, so its traffic looks like one that works. The split between its
// destinations is the one number that tells them apart.
//
// Clients reach a router by naming it; nothing on this screen points traffic
// at one.

import { api } from "../api.js";
import {
  h,
  table,
  modal,
  pill,
  stat,
  rowLink,
  compact,
  num,
  money,
  amount,
  moneyStat,
  ms,
  icon,
  icons,
  aliasProblem,
  takenProblem,
  toast,
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
  empty,
  dash,
  trafficSummary,
  formModal,
  detailHead,
  busyButton,
  banner,
} from "../ui.js";
import { barList } from "../chart.js";
import { orgNameOf } from "./orgs.js";
import { facts } from "./detail.js";

export async function routersView(ctx) {
  const since = currentRange();
  const [res, models] = await Promise.all([
    api.routers(ctx.orgID, { stats: true, since }),
    api.models(ctx.orgID).then((r) => r.data || []),
  ]);
  const routers = res.data || [];
  const stats = res.stats || {};
  // A subscription model only answers its caller's own sign-in, so a router
  // can neither decide with it nor send there.
  const readers = models.filter(canDecide);
  const canEdit = isAdmin(ctx);
  ctx.setSubtitle(`${routers.length} ${plural(routers.length, "router")}`);

  const head = listHead(
    null,
    [rangePicker(ctx, since)],
    canEdit && {
      label: "Create router",
      onClick: () => editRouter(ctx, null, readers),
    },
  );

  const known = new Set(models.map((m) => m.alias));
  const rows = table(
    [
      {
        label: "Alias",
        sortKey: (rt) => rt.alias,
        cell: (rt) =>
          rowLink(
            ctx,
            "/routers/" + encodeURIComponent(rt.alias),
            h("strong", { class: "mono" }, rt.alias),
            "Where this router sent traffic",
          ),
      },
      // The mode comes first, because it changes what the other columns
      // mean: a fallback router's list is an order, not a menu.
      {
        label: "Mode",
        shrink: true,
        sortKey: (rt) => modeName(modeOf(rt)),
        cell: (rt) => modeCell(rt, known),
      },
      // Only the first few are named, so the row does not wrap.
      {
        label: "Destinations",
        shrink: true,
        sortKey: (rt) => (rt.destinations || []).length || null,
        sortDir: "desc",
        cell: (rt) => destinationsCell(rt, known),
      },
      // Whether it actually does what it is meant to.
      {
        label: "In this period",
        shrink: true,
        sortKey: (rt) => stats[rt.alias]?.requests || null,
        sortDir: "desc",
        cell: (rt) => trafficCell(stats[rt.alias]),
      },
      // Without this column the alias column would fill half the table.
      {
        label: "Description",
        sortKey: (rt) => rt.description || null,
        cell: (rt) =>
          rt.description
            ? h("span", { class: "muted clamp-2" }, rt.description)
            : dash(),
      },
      canEdit && {
        label: "Check",
        shrink: true,
        cell: (rt) => checkCell(ctx, rt),
      },
      canEdit && {
        label: "",
        shrink: true,
        cell: (rt) =>
          rowActions(ctx, rt.alias, {
            editTitle: "Edit this router",
            onEdit: () => editRouter(ctx, rt, readers),
            deleteTitle: "Delete this router",
            body:
              "Deleting it takes it off the organisation's allow-list. " +
              "It cannot be deleted while a project's or key's " +
              "allow-list names it, or while it is all the organisation's " +
              "list names; the error lists them. Clients still " +
              "using the alias will be told the model does not exist.",
            confirmLabel: "Delete router",
            remove: () => api.deleteRouter(ctx.orgID, rt.alias),
          }),
      },
    ].filter(Boolean),
    routers,
    {
      emptyTitle: "No routers yet",
      emptyBody:
        "Routers send your request to the model best suited to answer it. " +
        "Different routing strategies are available.",
    },
  );

  return h("div", {}, head, rows);
}

// decides reports whether this router chooses by reading the request. With
// one destination there is nothing to choose, so the gateway does not ask.
function decides(rt) {
  return modeOf(rt) === "instruction" && (rt.destinations || []).length > 1;
}

// A router saved before modes existed has none, and was an instruction router.
function modeOf(rt) {
  return rt.mode || "instruction";
}

function modeName(mode) {
  return (MODES.find((m) => m.value === mode) || { name: mode }).name;
}

// measures reports whether the gateway works out this router's order. Then
// the order is a reading of the last few minutes on one replica, so screens
// must not show it numbered.
function measures(rt) {
  const mode = modeOf(rt);
  return mode === "latency" || mode === "least-busy";
}

// sizes reports whether this router chooses by request size. It reads the
// request like an instruction router, but costs nothing like the others.
function sizes(rt) {
  return modeOf(rt) === "size";
}

// ceilingOf is the largest request, in estimated tokens, for a destination,
// or null for the one that takes what the rest will not.
function ceilingOf(rt, alias) {
  const n = (rt.ceilings || {})[alias];
  return n > 0 ? n : null;
}

// tokens renders a ceiling as it is typed, such as '32k'.
function tokens(n) {
  return n >= 1000 && n % 1000 === 0 ? n / 1000 + "k" : String(n);
}

// ordersBy says, as a clause, how a router orders its destinations. It is the
// only thing that tells the modes that read nothing apart.
function ordersBy(rt) {
  return ordersByMode(modeOf(rt));
}

// ordersByMode is ordersBy for the check, which reports only the mode.
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

// modeCell shows the mode and, on an instruction router, the model it reads
// with, which has no column of its own.
function modeCell(rt, known) {
  if (sizes(rt)) {
    return h(
      "span",
      { title: "places each request by " + ordersBy(rt) },
      pill(modeName("size"), "good"),
    );
  }
  if (!decides(rt)) {
    return h(
      "span",
      { title: "tries its destinations in the order of " + ordersBy(rt) },
      pill(modeName(modeOf(rt))),
    );
  }
  return h(
    "div",
    { class: "row-tight" },
    pill(modeName("instruction"), "accent"),
    known.has(rt.model)
      ? h("span", { class: "mono muted" }, rt.model)
      : h(
          "div",
          { class: "row-tight" },
          h("span", { class: "mono muted" }, rt.model),
          pill("Missing", "bad"),
        ),
  );
}

// destinationsCell also counts the destinations the organisation no longer
// has. They are quietly left out of the choice, so without the count a broken
// router looks like one that works.
function destinationsCell(rt, known) {
  const destinations = rt.destinations || [];
  if (destinations.length === 0) {
    return dash();
  }
  const shown = destinations.slice(0, 3);
  const more = destinations.length - shown.length;
  const missing = destinations.filter((alias) => !known.has(alias)).length;
  return h(
    "div",
    { class: "row-tight" },
    h(
      "span",
      { class: "mono nowrap", title: destinations.join(", ") },
      shown.join(", "),
      more > 0 ? h("span", { class: "faint" }, ` +${more}`) : null,
    ),
    missing > 0 ? pill(`${missing} missing`, "bad") : null,
  );
}

// fallbackPill says what happens to a request the router cannot place. It is
// shown on the router's own screen, not in the list: whether to fall back or
// refuse depends on what the router is for.
//
// A router that tries its destinations has no such setting: when it fails,
// the client gets the last destination's answer.
function fallbackPill(rt) {
  if (sizes(rt)) {
    return h("span", { class: "muted nowrap" }, "the next destination up");
  }
  if (!decides(rt)) {
    return h("span", { class: "muted nowrap" }, "the last one's answer");
  }
  if (!rt.fallback) return pill("Refuse the request", "warn");
  return h("span", { class: "pill mono" }, "→ " + rt.fallback);
}

// trafficCell shows how many requests were placed and how many without a
// decision. The split is on the router's own screen.
function trafficCell(st) {
  if (!st || !st.requests) return trafficSummary(0);
  const undecided = (st.fell_back || 0) + (st.errored || 0);
  const rate = undecided / st.requests;
  return trafficSummary(
    st.requests,
    "placed",
    undecided > 0
      ? pill(
          `${pct(undecided, st.requests)} undecided`,
          rate >= 0.25 ? "bad" : "warn",
        )
      : null,
    [money(st.cost_micros), " spent, deciding and filters included"],
  );
}

// checkCell puts three sample prompts through the router and shows where each
// went. The result is not cached: a small model following prose may answer
// differently next time.
function checkCell(ctx, rt) {
  return checkButton(
    "Test this router with sample prompts",
    () => api.checkRouter(ctx.orgID, rt.alias),
    (probe) => verdict(ctx, probe),
  );
}

function verdict(ctx, probe) {
  const warned = (probe.warnings || []).length > 0;
  const tone = probe.ok ? (warned ? "warn" : "good") : "bad";
  const label = !probe.ok
    ? "Not usable"
    : warned
      ? "Working, with notes"
      : "Working";
  return {
    label,
    tone,
    title: "Where the router sent each sample",
    open: () => report(ctx, probe),
  };
}

// report shows each sample prompt and the model the router chose for it.
//
// There is no pass: only the reader can judge whether the choices suit their
// work. What is always wrong is every sample going the same way.
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
      probe.error ? banner("bad", probe.error) : null,
      (probe.warnings || []).map((warning) => banner("warn", warning)),
      facts([
        probe.model
          ? ["Runs on", h("span", { class: "mono" }, probe.model)]
          : null,
        ranked || banded ? ["In the order of", ordersByMode(probe.mode)] : null,
        [
          banded
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
                " - each request to this router pays a share of this on top",
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
                      ? pill("Yes", "good")
                      : pill(hop.status ? String(hop.status) : "No", "bad"),
                },
                {
                  label: "Took",
                  shrink: true,
                  cell: (hop) => (hop.ms ? ms(hop.ms) : dash()),
                },
                // Why a destination is where it is, on the modes where the
                // order is measured rather than written.
                ranked || banded
                  ? {
                      label: banded ? "Takes" : "Reading",
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
              // A sample decided at 35% went there by a hair. Absent when
              // the deciding model's backend serves no probabilities.
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
    ),
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Close"),
    ],
  });
}

/* ------------------------------------------------------------ one router */

export async function routerDetailView(ctx) {
  const since = currentRange();
  const res = await api.routerReport(ctx.orgID, ctx.param, since);
  const rep = res.report || {};
  const rt = res.router || null;
  const canEdit = isAdmin(ctx);
  const placed = rep.total ? rep.total.requests || 0 : 0;

  ctx.setTitle(ctx.param, rt ? identity(rt) : `no longer in ${orgNameOf(ctx)}`);

  const wrap = h(
    "div",
    {},
    detailHead(
      ctx,
      ["/routers", "Routers"],
      rt
        ? [
            pill(
              decides(rt)
                ? "Decides with " + rt.model
                : sizes(rt)
                  ? "Places " +
                    (rt.destinations || []).length +
                    " destinations by size"
                  : "Tries " +
                    (rt.destinations || []).length +
                    " destinations, " +
                    (measures(rt) ? "by " + ordersBy(rt) : "in order"),
            ),
            fallbackPill(rt),
          ]
        : pill(`Deleted from ${orgNameOf(ctx)}`, "warn"),
      [
        rangePicker(ctx, since),
        rt && canEdit
          ? busyButton(
              { title: "Test this router with sample prompts" },
              "Check",
              async () =>
                report(ctx, await api.checkRouter(ctx.orgID, rt.alias)),
            )
          : null,
        rt && canEdit
          ? h(
              "button",
              {
                class: "btn",
                title: "Edit this router",
                "aria-label": `Edit ${rt.alias}`,
                onClick: () =>
                  api
                    .models(ctx.orgID)
                    .then((r) =>
                      editRouter(ctx, rt, (r.data || []).filter(canDecide)),
                    ),
              },
              icon(icons.pencil),
            )
          : null,
      ],
    ),
  );

  for (const banner of detailBanners(rep, rt, placed)) wrap.append(banner);

  // "Placed" leads because the shares mean nothing without it: all of three
  // requests to one model is not a router that stopped deciding.
  wrap.append(
    h(
      "div",
      { class: "grid grid-4" },
      stat(
        "Placed",
        compact(placed),
        placed ? "requests to this router" : "no requests in this period",
      ),
      stat(
        rt && measures(rt)
          ? "Best ranked"
          : rt && !decides(rt)
            ? "First choice"
            : "Decided",
        pct(rep.chose, placed),
        chosenNote(rt, rep),
      ),
      // The wait the router added: a generation when it reads the request,
      // or the destinations that did not answer when it tries them.
      stat(
        rt && !decides(rt) ? "Lost to failover" : "Deciding cost",
        ms(rep.decision_median_ms || 0),
        `p95 ${ms(rep.decision_p95_ms || 0)}, added per request`,
      ),
      moneyStat(
        "Cost",
        rep.total ? rep.total.cost_micros || 0 : 0,
        "destinations' cost, decisions included",
      ),
    ),
  );

  // The split is the only figure that tells a working router from an
  // expensive way of hard-coding one destination.
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
                label: (d) => d.alias || "No model",
                value: (d) => d.requests,
              })
            : empty(
                "Nothing placed",
                "No request used this router in this period.",
              ),
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
                label: (d) => d.alias || "No model",
                value: (d) => d.cost_micros,
                cost: true,
                colorIndex: 2,
              })
            : empty(
                "Nothing charged",
                "No request through this router cost anything in this period.",
              ),
        ),
      ),
    ),
  );

  if ((rep.destinations || []).length) {
    wrap.append(
      h(
        "div",
        { style: { marginTop: "20px" } },
        sectionHead("Each destination"),
        table(
          [
            {
              label: "Model",
              cell: (d) => h("span", { class: "mono" }, d.alias || "No model"),
            },
            {
              label: "Requests",
              shrink: true,
              num: true,
              cell: (d) => num(d.requests),
            },
            {
              label: "Share",
              shrink: true,
              num: true,
              cell: (d) => pct(d.requests, placed),
            },
            {
              label: "Tokens",
              shrink: true,
              num: true,
              cell: (d) => compact(d.input_tokens + d.output_tokens),
            },
            {
              label: "Cost (CHF)",
              shrink: true,
              num: true,
              cell: (d) => amount(d.cost_micros),
            },
            // A cheaper destination that doubles the wait has not obviously
            // helped.
            {
              label: "Median wait",
              shrink: true,
              num: true,
              cell: (d) => ms(d.ttft_median_ms),
            },
            {
              label: "Refused",
              shrink: true,
              num: true,
              cell: (d) => (d.refused ? pill(num(d.refused), "warn") : dash()),
            },
            {
              label: "Failed",
              shrink: true,
              num: true,
              cell: (d) => (d.failed ? pill(num(d.failed), "bad") : dash()),
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
              decides(rt)
                ? pill(modeName("instruction"), "accent")
                : pill(modeName(modeOf(rt))),
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
                // Numbered only where the order is the configuration. On a
                // measured router it is only a tie-break. On a size router
                // the ceiling goes on the chip instead.
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
          // Shown whole, not clamped: the instruction is what readers compare
          // against where the traffic went.
          decides(rt)
            ? h(
                "div",
                { class: "prompt-text", style: { marginTop: "12px" } },
                rt.prompt || "",
              )
            : null,
        ),
      ),
    );
  }

  return wrap;
}

// detailBanners come before the numbers, because each changes how they read.
function detailBanners(rep, rt, placed) {
  const out = [];
  if (!rt) {
    out.push(
      banner(
        "warn",
        "This router no longer exists. Below is the traffic it placed " +
          "before. Clients still using it are told the model does not exist.",
      ),
    );
  }
  if (!placed) {
    out.push(
      banner(
        "info",
        "No requests used this router in this period. Clients reach it by " +
          "using its alias as the model name, so either nobody has the alias " +
          "or nobody called.",
      ),
    );
    return out;
  }
  const chained = rt && !decides(rt);
  const ranked = rt && measures(rt);
  const banded = rt && sizes(rt);
  // A router that never gets its first choice is invisible in the requests,
  // because each one was answered by something else.
  if (rep.chose === 0) {
    out.push(
      banner(
        "bad",
        banded
          ? "No request in this period was served by the destination its " +
              "size was meant for. Each one waited for it to fail and was " +
              "served by a larger one. Check the destinations."
          : ranked
            ? "The destination this router ranked first never answered in this " +
              "period. Each request waited for it to fail and was served " +
              "further down. The ranking is not working."
            : chained
              ? "The first destination never answered in this period. Each " +
                "request waited for it to fail and was served further down. " +
                "Check it."
              : "This router made no decision in this period. It still costs a " +
                "generation per request. Check it and the model it decides with.",
      ),
    );
  } else if (rep.fell_back > 0 && rep.fell_back / placed >= 0.1) {
    out.push(
      banner(
        "warn",
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
      banner(
        "bad",
        chained
          ? `${num(rep.errored)} ${plural(rep.errored, "request")} ran out of ` +
              "destinations: every model failed, and the client got the last " +
              "one's answer."
          : `${num(rep.errored)} ${plural(rep.errored, "request was", "requests were")} refused ` +
              "because this router could not place them and has no fallback. " +
              "The clients got a 503 saying the router could not choose.",
      ),
    );
  }
  // One destination taking everything, on enough traffic to mean it, is a
  // hard-coded alias in disguise.
  const destinations = rep.destinations || [];
  if (!chained && rep.chose > 0 && destinations.length === 1 && placed >= 20) {
    out.push(
      banner(
        "warn",
        `Every request went to ${destinations[0].alias}. The router costs a ` +
          "generation per request for the same result as naming that model " +
          "directly.",
      ),
    );
  }
  return out;
}

// chosenNote says what the headline share counted, in words that suit each
// kind of router.
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
  const models = `${destinations} ${plural(destinations, "model")}`;
  if (decides(rt)) return `decides with ${rt.model}, between ${models}`;
  if (sizes(rt)) return `places between ${models}, by the size of the request`;
  if (measures(rt)) return `tries ${models}, by ${ordersBy(rt)}`;
  return `tries ${models} in order, first that answers`;
}

/* -------------------------------------------------------------- editing one */

// MODES feeds both the tiles that pick a mode and the rows that name it, so
// they always agree.
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

// A decision model only answers choices, so it can decide but never be a
// destination.
function canDecide(m) {
  return (m.kind === "chat" || m.kind === "decision") && !m.subscription;
}

function editRouter(ctx, existing, readers) {
  const chatModels = readers.filter((m) => m.kind === "chat");
  const creating = !existing;
  // No default mode: the dialog asks for it first.
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
      readers.length ? "Choose a model" : "No chat model exists",
    ),
    readers.map((m) =>
      h(
        "option",
        { value: m.alias, selected: m.alias === rt.model },
        m.alias + (m.kind === "decision" ? " - decision model" : ""),
      ),
    ),
  );
  const prompt = h("textarea", { class: "input", rows: "8" });
  // A decision model answers a short question rather than following an
  // instruction, so it gets its own example.
  const exampleFor = (alias) =>
    readers.find((m) => m.alias === alias)?.kind === "decision"
      ? DECISION_EXAMPLE
      : PROMPT_EXAMPLE;
  prompt.value = rt.prompt || exampleFor(model.value);
  model.addEventListener("change", () => {
    if (prompt.value === PROMPT_EXAMPLE || prompt.value === DECISION_EXAMPLE) {
      prompt.value = exampleFor(model.value);
    }
  });

  // The mode is asked first, because it decides what the rest of the form
  // means: the same list is a menu, an order, or a set of size bands.
  let chosen = creating ? null : modeOf(rt);
  const reads = () => chosen === "instruction";
  const bySize = () => chosen === "size";
  // On the measured modes the order still breaks ties, but it is not the
  // whole configuration.
  const ranks = () => chosen === "latency" || chosen === "least-busy";

  // Changing the mode hides what the new mode does not use rather than
  // clearing it, so trying another tile loses nothing.
  const picker = modeTiles(MODES, chosen, (value) => {
    chosen = value;
    syncMode();
    // Focus the alias on a new router, the change control on an edit.
    // Before the dialog is in the document neither does anything, which is
    // right for an edit's first choice.
    if (creating && !alias.value) alias.focus();
    else picker.change.focus();
  });
  const { tiles, chosenRow, choose } = picker;
  const modeField = h(
    "div",
    { class: "field" },
    h("label", {}, "What chooses"),
    tiles,
    chosenRow,
  );

  // Checkboxes over the catalogue, so a router cannot name a model the
  // organisation does not have. An array, not a set: on a fallback router the
  // order is the configuration and has to be visible and changeable.
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

  // Shown only where the order matters. Ticked in the wrong order, a
  // fallback router serves everything from its second choice.
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

  // Kept here rather than read off the inputs, so unticking and ticking a
  // destination again keeps its ceiling.
  const ceilings = { ...(rt.ceilings || {}) };
  // Boxes holding something that is not a ceiling. Saving refuses them, as
  // `keera router` does, rather than reading them as "no ceiling".
  const badCeilings = new Set();
  const ceilingList = h("div", { class: "stack", style: { gap: "4px" } });
  const ceilingHint = h("div", { class: "hint" });
  // The note beside each box is updated on its own: rebuilding the rows would
  // take the cursor out of the box being typed into.
  const notes = new Map();
  const syncCeilingNotes = () => {
    for (const [alias, note] of notes) {
      note.textContent = ceilings[alias]
        ? "tokens at most"
        : "takes what is larger than every ceiling";
    }
    // The control plane needs exactly one destination without a ceiling.
    // Saying so here saves being refused at the last step.
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
            // The CLI's form: whole tokens, where 'k' means a thousand.
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
  const ceilingField = field(
    "Up to this many input tokens",
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
          // The description is what the deciding model is told about this
          // destination.
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

  // Fields only instruction routers have. Hidden rather than disabled, so
  // nobody writes an instruction that a router then ignores.
  const modelField = field(
    "Runs on",
    model,
    // Where this model runs can be wrong without anything here going red.
    "A small, fast model. It reads every request before any filter, so " +
      "keep it inside the cluster.",
  );
  const fallbackField = field(
    "When it cannot decide",
    fallback,
    // The right answer depends on the deployment, so the hint gives both.
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
  const orderField = field("In this order", order, orderHint);
  const destinationsLabel = h("label", {}, "Chooses between");
  const destinationsHint = h("div", { class: "hint" });
  const costBanner = banner("info");

  // Set once the dialog is up. The tiles turn it on.
  let saveButton = null;
  const syncMode = () => {
    // Nothing below the tiles, Save included, until one is chosen.
    rest.hidden = chosen === null;
    if (saveButton) {
      saveButton.hidden = chosen === null;
      saveButton.disabled = chosen === null;
    }
    if (chosen === null) return;
    for (const el of [modelField, fallbackField, promptField]) {
      el.hidden = !reads();
    }
    ceilingField.hidden = !bySize();
    // A size router's order is its ceilings.
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
    // What applies to every mode is said once, in the subtitle. One
    // destination is allowed in every mode, to give a model a lasting name.
    destinationsHint.textContent = reads()
      ? "With only one, every request goes there and nothing is asked."
      : bySize()
        ? "Each with the largest request it should be given."
        : "Tried until one answers. One is enough to give a model a lasting name.";
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

  // Held back until there is a mode. The alias is here too: a router is
  // easier to name once it is clear what it does.
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
          ? "Lowercase letters, digits and hyphens. Clients use it as the " +
              "model name."
          : "Clients use this name, so it cannot change.",
      ),
      field("Description", description),
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

  if (creating) syncMode();
  else choose(chosen);

  ({ button: saveButton } = formModal({
    wide: true,
    title: creating ? "Create router" : `Edit ${rt.alias}`,
    subtitle: creating
      ? "Choose how it picks first; the rest of the form depends on it. A " +
        "key allowed to use this router can reach all its destinations."
      : "A key allowed to use this router can reach all its destinations.",
    body: h("form", {}, modeField, rest),
    submitLabel: creating ? "Create router" : "Save",
    // Saved either way. The warning is about the organisation's allow-list,
    // and stays up long enough to read.
    done: (saved) => {
      const said = creating ? "Router created" : "Router saved";
      if (!saved || !saved.warning) return said;
      toast(
        said +
          ", but " +
          saved.warning +
          ". Change the list in the organisation's Guardrails.",
        "bad",
      );
      return "";
    },
    ctx,
    submit: async () => {
      // Checked here: an empty alias would not reach the handler that could
      // say so.
      const problem = aliasProblem(alias.value.trim());
      if (problem) {
        alias.focus();
        throw new Error(problem);
      }
      const bad = bySize() ? picked.find((a) => badCeilings.has(a)) : null;
      if (bad) {
        throw new Error(
          `The ceiling on ${bad} is not a number of tokens. Write 4000 ` +
            "or 4k, or leave it empty for no ceiling.",
        );
      }
      if (creating) {
        const taken = takenProblem(
          (await api.routers(ctx.orgID)).data,
          alias.value.trim(),
          "router",
        );
        if (taken) {
          alias.focus();
          throw new Error(taken);
        }
      }
      return api.putRouter(ctx.orgID, alias.value.trim(), {
        mode: chosen,
        // Leftovers from the instruction mode are not saved.
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
    },
  }));
  syncMode(); // again, now that Save is there
  // Until a mode is chosen there is no field to focus, so start on the tiles.
  if (creating) tiles.firstChild.focus();
}

// PROMPT_EXAMPLE shows the shape an instruction needs: name kinds of request,
// not models, because the catalogue adds what each model is for. And say which
// way to err, because a small model deciding in one word is sometimes wrong.
const PROMPT_EXAMPLE = `Choose the model that suits this request.

Send small, self-contained requests to the cheapest capable model: a rename, a \
syntax question, a short edit to one file, a commit message.

Send requests needing design, several steps at once, or judgement about a \
system to a model that can reason.

Send anything containing a client's name, an account number, personal details \
or a credential to a model inside the cluster, whatever else it is asking for. \
That takes priority.

If you are unsure, choose the more capable model rather than the cheaper one.`;

// A decision model is offered each destination by its description alone. See
// docs/gateway.md#decision-models.
const DECISION_EXAMPLE = "Which model should answer this request?";
