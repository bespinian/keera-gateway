// Tool calls: what an agent did through the MCP servers the gateway stands in
// front of. Shared by a session's own screen and the Models screen.
//
// Nothing about a call's content is kept, so a row says which tool, what came
// of it, how long it took and how much went each way - which is enough to see
// an agent looping on one tool, or sending four megabytes to a chat channel.

import { h, table, pill, num, ms, dateTime } from "../ui.js";

// OUTCOMES are the words a tool call ends in, and how each is drawn.
const OUTCOMES = {
  ok: ["answered", "good"],
  tool_error: ["tool failed", "warn"],
  error: ["no result", "bad"],
  denied: ["not allowed", "warn"],
  refused: ["refused by a filter", "warn"],
};

export function toolOutcome(outcome) {
  const [label, tone] = OUTCOMES[outcome] || [outcome, ""];
  return pill(label, tone);
}

// bytes is a size for a person: 812 B, 4.2 kB, 3.1 MB.
export function bytes(n) {
  if (n < 1000) return `${num(n)} B`;
  if (n < 1e6) return `${(n / 1e3).toFixed(1)} kB`;
  return `${(n / 1e6).toFixed(1)} MB`;
}

// toolCallTable lists tool calls, one row each.
export function toolCallTable(rows, opts = {}) {
  return table(
    [
      {
        label: "When",
        sortKey: (t) => t.ts,
        cell: (t) => h("span", { class: "nowrap muted" }, dateTime(t.ts)),
      },
      {
        label: "Tool",
        sortKey: (t) => `${t.server}/${t.tool}`,
        cell: (t) => h("span", { class: "mono" }, `${t.server}/${t.tool}`),
      },
      {
        label: "Outcome",
        sortKey: (t) => t.outcome,
        cell: (t) =>
          h(
            "div",
            { class: "stack" },
            toolOutcome(t.outcome),
            t.error
              ? h(
                  "span",
                  { class: "faint", style: { fontSize: "11.5px" } },
                  t.error,
                )
              : null,
          ),
      },
      {
        label: "Time",
        num: true,
        shrink: true,
        sortKey: (t) => t.latency_ms,
        cell: (t) => h("span", { class: "nowrap" }, ms(t.latency_ms)),
      },
      {
        label: "Sent",
        num: true,
        shrink: true,
        sortKey: (t) => t.arg_bytes,
        cell: (t) => h("span", { class: "nowrap" }, bytes(t.arg_bytes)),
      },
      {
        label: "Back",
        num: true,
        shrink: true,
        sortKey: (t) => t.result_bytes,
        cell: (t) => h("span", { class: "nowrap" }, bytes(t.result_bytes)),
      },
    ],
    rows,
    {
      sortBy: "When",
      sortDir: opts.sortDir || "desc",
      emptyTitle: "No tool calls",
      emptyBody: opts.emptyBody,
    },
  );
}
