// Rate limits and budgets, as the guardrails dialog and a router's steps both
// show and edit them.

import { h, num, compact, money, field } from "../ui.js";

// PER is how a rate limit's period reads after a slash, as in 60 req/min.
const PER = { minute: "min", day: "day", month: "month" };

/** rateShort is one rate limit for a list of chips, such as "120 req/min". */
export function rateShort(rl) {
  const per = PER[rl.period || "minute"] || rl.period;
  const bits = [];
  if (rl.requests > 0) bits.push(`${num(rl.requests)} req/${per}`);
  if (rl.tokens > 0) bits.push(`${compact(rl.tokens)} tok/${per}`);
  return bits;
}

/** budgetShort is one budget on a card, such as "CHF 50.00 / month". */
export function budgetShort(b) {
  return `${money(b.budget_micros)} / ${b.period || "month"}`;
}

/** budgetText says one budget, such as "CHF 50.00 per month". */
export function budgetText(b) {
  return `${money(b.budget_micros)} per ${b.period || "month"}`;
}

/** rateFields edits a rate limit, counted per one of periods, the first
 *  being the default. hint goes under the period. read throws when neither
 *  count is set, and leaves out the one that is not. */
export function rateFields(cur, periods, hint) {
  const requests = numberInput(cur.requests, "60");
  const tokens = numberInput(cur.tokens, "200000");
  const period = periodSelect(periods, cur.period || periods[0]);
  return {
    el: h(
      "div",
      {},
      h(
        "div",
        { class: "field-row" },
        field("Requests", requests, "Empty for no limit on requests."),
        field("Tokens", tokens, "Empty for no limit on tokens."),
      ),
      field("Per", period, hint),
    ),
    read: () => {
      const out = { period: period.value };
      const r = whole(requests);
      const t = whole(tokens);
      if (r) out.requests = r;
      if (t) out.tokens = t;
      if (!r && !t)
        throw new Error("Set a number of requests or tokens, or both.");
      return out;
    },
  };
}

/** budgetFields edits a budget, as rateFields does a rate limit. hint goes
 *  under both fields, which share a row. */
export function budgetFields(cur, periods, hint) {
  const amount = amountInput(cur.budget_micros);
  const period = periodSelect(periods, cur.period || periods[0]);
  return {
    el: h(
      "div",
      {},
      h(
        "div",
        { class: "field-row" },
        field("Amount (CHF)", amount),
        field("Per", period),
      ),
      h("div", { class: "hint" }, hint),
    ),
    read: () => ({ budget_micros: amountOf(amount), period: period.value }),
  };
}

function periodSelect(periods, chosen) {
  return h(
    "select",
    { class: "select" },
    periods.map((p) =>
      h("option", { value: p, selected: p === chosen }, `per ${p}`),
    ),
  );
}

function numberInput(value, placeholder) {
  return h("input", {
    class: "input",
    type: "number",
    min: "0",
    step: "1",
    placeholder,
    value: value || "",
  });
}

/** whole reads a number input as a count, or 0 when it holds none. */
function whole(input) {
  const v = parseInt(input.value, 10);
  return Number.isFinite(v) && v > 0 ? v : 0;
}

/** amountInput is a budget's amount in francs. */
function amountInput(micros) {
  return h("input", {
    class: "input",
    type: "number",
    min: "0",
    step: "0.01",
    placeholder: "50",
    value: micros ? micros / 1e6 : "",
  });
}

/** amountOf reads an amountInput in micros, or throws when it is not above
 *  zero. */
function amountOf(input) {
  const v = parseFloat(input.value);
  if (!Number.isFinite(v) || v <= 0) {
    throw new Error("Set an amount above zero.");
  }
  return Math.round(v * 1e6);
}
