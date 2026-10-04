// The usage report: who spent what, grouped the way the question was asked.

import { api } from "../api.js";
import {
  h,
  table,
  num,
  compact,
  money,
  icon,
  icons,
  RANGES as SHARED_RANGES,
} from "../ui.js";
import { orgNameOf } from "./orgs.js";

const GROUPS = [
  { key: "project", label: "Project" },
  { key: "model", label: "Model" },
  // What sent the requests, which is the one grouping that says what is
  // pointed at this gateway rather than what it is pointed at. The Live map
  // draws the same rows; this is them added up.
  { key: "client", label: "Client" },
  { key: "key", label: "Key" },
  { key: "user", label: "User" },
  { key: "day", label: "Day" },
];

// Offered only across every organisation: inside one it is a single row.
const ORG_GROUP = { key: "org", label: "Organisation" };

// The shared windows plus a quarter, and a window of its own rather than the
// shared one: a chargeback is read over a month or a quarter, and carrying that
// back to the dashboard would leave somebody looking at a quarter of traffic
// where they expected this week's. The three it shares are not restated, so
// they cannot drift from the picker every other screen draws.
const RANGES = [
  ...SHARED_RANGES,
  { label: "90d", since: "2160h", long: "Last 90 days" },
];

export async function usageView(ctx) {
  const groups =
    !ctx.orgID && ctx.state.me.unrestricted ? [ORG_GROUP, ...GROUPS] : GROUPS;
  const stored = sessionStorage.getItem("keera.usage.group");
  const groupBy = groups.some((g) => g.key === stored) ? stored : "project";
  const since = sessionStorage.getItem("keera.usage.range") || "720h";

  const res = await api.usage(ctx.orgID, groupBy, since);
  const rows = res.data || [];
  // Every grouping resolves to a name, not only projects. A report grouped by key
  // or by user that shows raw ids is a report nobody can act on, and those two
  // are exactly what a manager asks for.
  const names = {
    project: res.project_names || {},
    key: res.key_names || {},
    user: res.user_names || {},
    client: res.client_names || {},
  };
  const total = rows.reduce((a, r) => a + r.cost_micros, 0);
  const requests = rows.reduce((a, r) => a + r.requests, 0);

  // Only served requests count here: a refused or failed one costs nothing.
  ctx.setSubtitle(
    `${num(requests)} served requests · ${money(total, res.currency)}`,
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
              sessionStorage.setItem("keera.usage.group", g.key);
              ctx.reload();
            },
          },
          g.label,
        ),
      ),
    ),
    h("div", { style: { flex: 1 } }),
    h(
      "div",
      { class: "seg" },
      RANGES.map((r) =>
        h(
          "button",
          {
            "aria-pressed": String(r.since === since),
            onClick: () => {
              sessionStorage.setItem("keera.usage.range", r.since);
              ctx.reload();
            },
          },
          r.label,
        ),
      ),
    ),
    // A chargeback is reconciled in a spreadsheet. A report that cannot leave
    // the panel is a report somebody re-types by hand.
    h(
      "button",
      {
        class: "btn btn-sm",
        style: { marginLeft: "8px" },
        title: "Download this report as CSV",
        onClick: () =>
          api.download("/v1/usage", {
            org_id: ctx.orgID,
            group_by: groupBy,
            since,
          }),
      },
      icon(icons.download),
      "CSV",
    ),
  );

  const label = (r) => {
    if (!r.group) {
      // A row with no group is real traffic: a key with no project, or a request
      // made with the operator key. Naming it beats a dash that reads as a bug.
      return h(
        "span",
        { class: "faint" },
        groupBy === "project"
          ? "No project"
          : groupBy === "user"
            ? "No user"
            : groupBy === "client"
              ? "Unidentified client"
              : "-",
      );
    }
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
    const named = names[groupBy] && names[groupBy][r.group];
    if (!named) {
      // An id with no name behind it is one whose row is gone - a deleted user,
      // a key from a purged org. Saying so beats showing a bare id.
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
      // The id under the name, except where they are the same string: an
      // unrecognised client is shown under the name it gave itself, and
      // printing that twice reads as a bug.
      named === r.group
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
      // The grouping column is given its width rather than measured from what
      // is in it: the same table is redrawn over every grouping, and a first
      // column that resizes under each of them makes the tabs above read as
      // different reports rather than one asked several ways.
      {
        label: groups.find((g) => g.key === groupBy).label,
        width: "36%",
        cell: label,
        sortKey: (r) =>
          groupBy === "org"
            ? orgNameOf(ctx, r.group)
            : (names[groupBy] && names[groupBy][r.group]) || r.group || "",
      },
      {
        label: "Served",
        width: "16%",
        num: true,
        cell: (r) => num(r.requests),
        sortKey: (r) => r.requests,
        sortDir: "desc",
      },
      {
        label: "Input Tokens",
        width: "16%",
        num: true,
        cell: (r) => compact(r.input_tokens),
        sortKey: (r) => r.input_tokens,
        sortDir: "desc",
      },
      {
        label: "Output Tokens",
        width: "16%",
        num: true,
        cell: (r) => compact(r.output_tokens),
        sortKey: (r) => r.output_tokens,
        sortDir: "desc",
      },
      {
        label: `Cost (${res.currency})`,
        width: "16%",
        num: true,
        cell: (r) => h("strong", {}, money(r.cost_micros, "")),
        sortKey: (r) => r.cost_micros,
        sortDir: "desc",
      },
    ],
    rows,
    {
      // Most used first. Days stay in date order, which is the question a
      // grouping by day asks.
      sortBy: groupBy === "day" ? null : "Served",
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
          h("div", { style: { flex: 1 } }),
          h("span", { class: "muted" }, `${num(requests)} served requests`),
          h("strong", {}, money(total, res.currency)),
        ),
      )
    : null;

  const note = h(
    "div",
    { class: "hint", style: { marginTop: "14px" } },
    "Cost uses the prices set on each model. Only served requests count; " +
      "refused and failed ones cost nothing and are in Requests. Requests " +
      "cancelled mid-stream are charged an estimate, and the log marks them.",
  );

  return h("div", {}, controls, body, foot, note);
}
