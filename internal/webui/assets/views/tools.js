// Tool calls an agent made through the gateway's MCP servers, for a session's
// screen and the MCP screen.
//
// No call content is kept. Tool, outcome, time and bytes are enough to spot
// an agent looping on one tool.

import { h, table, pill, ms, bytes, dateTime, ago, capitalise } from "../ui.js";

const OUTCOMES = {
  ok: ["Answered", "good"],
  tool_error: ["Tool failed", "warn"],
  error: ["No result", "bad"],
  denied: ["Not allowed", "warn"],
  refused: ["Refused by a filter", "warn"],
  input_required: ["Asked for input", ""],
};

function toolOutcome(outcome) {
  // An unknown outcome is still shown as a word.
  const [label, tone] = OUTCOMES[outcome] || [
    capitalise(outcome).replace(/_/g, " "),
    "",
  ];
  return pill(label, tone);
}

export function toolCallTable(rows, opts = {}) {
  return table(
    [
      {
        label: "When",
        sortKey: (t) => t.created_at,
        cell: (t) =>
          h(
            "span",
            { class: "muted nowrap", title: dateTime(t.created_at) },
            ago(t.created_at),
          ),
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
            t.error ? h("span", { class: "faint small" }, t.error) : null,
          ),
      },
      {
        label: "Took",
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
