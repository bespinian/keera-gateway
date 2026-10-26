// Rate limits and budgets, for the guardrails dialog and a router's steps.

import { h, num, compact, money, date, meter, field } from "../ui.js";

// As in 60 req/min.
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

/** budgetText is one budget, such as "CHF 50.00 per month". */
export function budgetText(b) {
  return `${money(b.budget_micros)} per ${b.period || "month"}`;
}

/** budgetUse is a meter that warns from 80%, the amounts, and when it
 *  resets. full says what happens once it is spent. */
export function budgetUse(
  b,
  full = "used up, so requests are refused until it resets",
) {
  return use(
    b.spend_micros / b.budget_micros,
    `${money(b.spend_micros)} of ${budgetText(b)}`,
    b.resets_at ? ` · resets ${date(b.resets_at)}` : "",
    full,
  );
}

/** rateUse is a rate limit per day or month, shown as budgetUse shows a
 *  budget. */
export function rateUse(r, full) {
  const parts = [];
  let frac = 0;
  for (const [limit, used, unit] of [
    [r.requests, r.requests_used || 0, "requests"],
    [r.tokens, r.tokens_used || 0, "tokens"],
  ]) {
    if (!(limit > 0)) continue;
    parts.push(`${num(used)} of ${num(limit)} ${unit}`);
    frac = Math.max(frac, used / limit);
  }
  return use(frac, `${parts.join(", ")} per ${r.period}`, "", full);
}

function use(frac, text, resets, full) {
  const tone = frac >= 1 ? "bad" : frac >= 0.8 ? "warn" : "";
  return h(
    "div",
    { class: "stack", style: { gap: "6px" } },
    h(
      "div",
      { class: "row-tight" },
      meter(frac, tone),
      h("span", { class: "muted nowrap small" }, `${Math.round(frac * 100)}%`),
    ),
    h(
      "span",
      { class: "muted small" },
      text,
      resets,
      frac >= 1 ? ` · ${full}` : "",
    ),
  );
}

/** rateFields edits a rate limit; the first of periods is the default.
 *  read throws when neither count is set, and leaves out the one that is
 *  not. */
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

/** budgetFields edits a budget, like rateFields. hint goes under both
 *  fields. */
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

/** whole returns 0 for an empty input. */
function whole(input) {
  const v = parseInt(input.value, 10);
  return Number.isFinite(v) && v > 0 ? v : 0;
}

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

/** amountOf returns micros, or throws when not above zero. */
function amountOf(input) {
  const v = parseFloat(input.value);
  if (!Number.isFinite(v) || v <= 0) {
    throw new Error("Set an amount above zero.");
  }
  return Math.round(v * 1e6);
}
