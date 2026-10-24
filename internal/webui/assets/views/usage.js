// The usage report: who spent what, grouped the way the question was asked.

import { api } from "../api.js";
import {
  h,
  table,
  compact,
  money,
  amount,
  plural,
  rangePicker,
  remembered,
  remember,
  RANGES as SHARED_RANGES,
  csvButton,
} from "../ui.js";
import { lineChart } from "../chart.js";
import { orgNameOf } from "./orgs.js";

const GROUPS = [
  { key: "project", label: "Project" },
  { key: "model", label: "Model" },
  // What sent the requests. The Live map draws the same rows.
  { key: "client", label: "Client" },
  { key: "key", label: "Key" },
  { key: "user", label: "User" },
  { key: "day", label: "Day" },
];

// Offered only across every organisation: inside one it is a single row.
const ORG_GROUP = { key: "org", label: "Organisation" };

// The shared windows plus a quarter, as a chargeback is read over a month or
// a quarter. Kept apart from the shared choice, so a quarter does not carry
// over to the dashboard.
const RANGES = [
  ...SHARED_RANGES,
  { label: "90d", since: "2160h", long: "Last 90 days" },
];

export async function usageView(ctx) {
  const groups =
    !ctx.orgID && ctx.state.me.unrestricted ? [ORG_GROUP, ...GROUPS] : GROUPS;
  const stored = remembered("keera.usage.group");
  const groupBy = groups.some((g) => g.key === stored) ? stored : "project";
  const since = remembered("keera.usage.range") || "720h";

  const res = await api.usage(ctx.orgID, groupBy, since);
  const rows = res.data || [];
  // Every grouping resolves to a name: a report by key or user with raw ids is
  // one nobody can act on.
  const names = {
    project: res.project_names || {},
    key: res.key_names || {},
    user: res.user_names || {},
    client: res.client_names || {},
  };
  // A user is shown by name when they have one, with the address under it.
  const fullNames = groupBy === "user" ? res.user_full_names || {} : {};
  const nameOf = (id) => fullNames[id] || names[groupBy]?.[id];
  const total = rows.reduce((a, r) => a + r.cost_micros, 0);
  const requests = rows.reduce((a, r) => a + r.requests, 0);

  // Only served requests are counted, but the cost adds up every row: a
  // refused or failed one can still have paid for a filter or router call.
  ctx.setSubtitle(
    `${compact(requests)} served ${plural(requests, "request")} · ${money(total)}`,
  );

  const controls = h(
    "div",
    { class: "row", style: { marginBottom: "16px" } },
    h(
      "div",
      { class: "seg" },
      groups.map((g) =>
        h(
          "button",
          {
            "aria-pressed": String(g.key === groupBy),
            onClick: () => {
              remember("keera.usage.group", g.key);
              ctx.reload();
            },
          },
          g.label,
        ),
      ),
    ),
    h("div", { class: "spacer" }),
    rangePicker(ctx, since, { ranges: RANGES, key: "keera.usage.range" }),
    // A chargeback is reconciled in a spreadsheet.
    csvButton("Download this report as CSV", () =>
      api.download("/v1/usage", {
        org_id: ctx.orgID,
        group_by: groupBy,
        since,
      }),
    ),
  );

  // A row with no group is real traffic: a key with no project, or the
  // operator key. A dash would read as a bug.
  const nothing =
    groupBy === "project"
      ? "No project"
      : groupBy === "user"
        ? "No user"
        : groupBy === "client"
          ? "Unidentified client"
          : "-";

  // The busiest groups each get a line and the rest share one, so the chart
  // stays readable over hundreds of keys.
  const lineName = (l) => {
    if (l.other) return "Other";
    if (groupBy === "day") return "Served";
    if (!l.group) return nothing;
    if (l.org_id) return `${l.group} · ${orgNameOf(ctx, l.org_id)}`;
    if (groupBy === "org") return orgNameOf(ctx, l.group);
    if (groupBy === "model") return l.group;
    return nameOf(l.group) || "deleted";
  };
  const chartData = res.chart || {};
  const chart = h(
    "div",
    { class: "card", style: { marginBottom: "16px" } },
    h("div", { class: "card-head" }, h("h2", {}, "Served requests")),
    h(
      "div",
      { class: "card-body" },
      lineChart(
        chartData.at,
        (chartData.lines || []).map((l, i) => ({
          label: lineName(l),
          color: l.other ? "var(--faint)" : `var(--s${(i % 6) + 1})`,
          dashed: l.other,
          counts: l.counts,
        })),
        { bucket: chartData.bucket },
      ),
    ),
  );

  const label = (r) => {
    if (!r.group) return h("span", { class: "faint" }, nothing);
    if (r.org_id) {
      // Across every organisation, two of them can each have a model of the
      // same alias, so the row names whose it is.
      return h(
        "div",
        { class: "stack" },
        h("span", { class: "mono" }, r.group),
        h(
          "span",
          { class: "faint", style: { fontSize: "11px" } },
          orgNameOf(ctx, r.org_id),
        ),
      );
    }
    if (groupBy === "org") {
      return h(
        "div",
        { class: "stack" },
        h("span", {}, orgNameOf(ctx, r.group)),
        h(
          "span",
          { class: "mono faint", style: { fontSize: "11px" } },
          r.group,
        ),
      );
    }
    if (groupBy === "model" || groupBy === "day") {
      return h("span", { class: "mono" }, r.group);
    }
    const named = nameOf(r.group);
    if (!named) {
      // An id with no name is one whose row is gone, such as a deleted user.
      return h(
        "span",
        { class: "stack" },
        h("span", { class: "faint" }, "deleted"),
        h(
          "span",
          { class: "mono faint", style: { fontSize: "11px" } },
          r.group,
        ),
      );
    }
    return h(
      "div",
      { class: "stack" },
      h("span", {}, named),
      // Under the name, a user's address, or the id, unless it is the name
      // itself, as for an unrecognised client.
      groupBy === "user"
        ? fullNames[r.group]
          ? h(
              "span",
              { class: "faint", style: { fontSize: "11px" } },
              names.user[r.group],
            )
          : null
        : named === r.group
          ? null
          : h(
              "span",
              { class: "mono faint", style: { fontSize: "11px" } },
              r.group,
            ),
    );
  };

  const body = table(
    [
      // A fixed width, so the first column does not jump when the grouping
      // changes.
      {
        label: groups.find((g) => g.key === groupBy).label,
        width: "36%",
        cell: label,
        sortKey: (r) =>
          groupBy === "org"
            ? orgNameOf(ctx, r.group)
            : nameOf(r.group) || r.group || "",
      },
      {
        label: "Requests",
        width: "16%",
        num: true,
        cell: (r) => compact(r.requests),
        sortKey: (r) => r.requests,
        sortDir: "desc",
      },
      {
        label: "Input tokens",
        width: "16%",
        num: true,
        cell: (r) => compact(r.input_tokens),
        sortKey: (r) => r.input_tokens,
        sortDir: "desc",
      },
      {
        label: "Output tokens",
        width: "16%",
        num: true,
        cell: (r) => compact(r.output_tokens),
        sortKey: (r) => r.output_tokens,
        sortDir: "desc",
      },
      {
        label: "Cost (CHF)",
        width: "16%",
        num: true,
        cell: (r) => h("strong", {}, amount(r.cost_micros)),
        sortKey: (r) => r.cost_micros,
        sortDir: "desc",
      },
    ],
    rows,
    {
      // Most used first. Days stay in date order, which is the question a
      // grouping by day asks.
      sortBy: groupBy === "day" ? null : "Requests",
      sortDir: "desc",
      emptyTitle: "Nothing in this period",
      emptyBody: "Usage appears here as soon as a request is served.",
    },
  );

  const foot = rows.length
    ? h(
        "div",
        { class: "card", style: { marginTop: "12px" } },
        h(
          "div",
          { class: "card-body row" },
          h("strong", {}, "Total"),
          h("div", { class: "spacer" }),
          h(
            "span",
            { class: "muted" },
            `${compact(requests)} served ${plural(requests, "request")}`,
          ),
          h("strong", {}, money(total)),
        ),
      )
    : null;

  const note = h(
    "div",
    { class: "hint", style: { marginTop: "14px" } },
    "Cost uses the prices set on each model. Only served requests are " +
      "counted, but a refused or failed one can still cost what its filters " +
      "and router spent. Requests cancelled mid-stream are charged an " +
      "estimate, and the log marks them.",
  );

  return h("div", {}, controls, chart, body, foot, note);
}
