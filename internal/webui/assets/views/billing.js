// The bill for the deployment's own provider keys, and the credit an
// organisation pays for them in advance. See docs/billing.md.

import { api } from "../api.js";
import {
  h,
  table,
  num,
  compact,
  money,
  icon,
  icons,
  iconButton,
  stat,
  pill,
  confirm,
  toast,
  field,
  sectionHead,
  dateTime,
  plural,
  remembered,
  remember,
  formModal,
  banner,
  csvButton,
  capitalise,
} from "../ui.js";

// thisMonth is the current month as YYYY-MM, in UTC like the bill.
function thisMonth() {
  return new Date().toISOString().slice(0, 7);
}

// shift moves a YYYY-MM month by n months.
function shift(month, n) {
  const [y, m] = month.split("-").map(Number);
  return new Date(Date.UTC(y, m - 1 + n, 1)).toISOString().slice(0, 7);
}

function monthName(month) {
  const [y, m] = month.split("-").map(Number);
  return new Date(Date.UTC(y, m - 1, 1)).toLocaleDateString(undefined, {
    month: "long",
    year: "numeric",
    timeZone: "UTC",
  });
}

export async function billingView(ctx) {
  const operator = ctx.state.me.unrestricted;
  // Credit is one organisation's; an operator looking at all of them sees
  // only the bill.
  const credit =
    ctx.state.me.payments && !(operator && !ctx.orgID)
      ? await creditSection(ctx)
      : null;
  const bill = await billSection(ctx);
  return h("div", {}, credit, bill);
}

async function billSection(ctx) {
  const now = thisMonth();
  const stored = remembered("keera.billing.month");
  const month = stored && stored <= now ? stored : now;
  const res = await api.billing(ctx.orgID, month);
  const rows = res.data || [];
  // Without a line there is nothing to total.
  const total = rows.length ? res.total : null;
  const operator = ctx.state.me.unrestricted;
  const allOrgs = operator && !ctx.orgID;
  const payments = ctx.state.me.payments;

  if (!payments || allOrgs) {
    ctx.setSubtitle(total ? money(total.micros) : monthName(month));
  }

  const go = (m) => {
    remember("keera.billing.month", m);
    ctx.reload();
  };

  const controls = h(
    "div",
    { class: "row", style: { marginBottom: "16px" } },
    iconButton(icons.back, "Previous month", () => go(shift(month, -1))),
    h(
      "strong",
      { style: { minWidth: "130px", textAlign: "center" } },
      monthName(month),
    ),
    h(
      "button",
      {
        class: "btn btn-sm",
        title: "Next month",
        "aria-label": "Next month",
        disabled: month >= now,
        onClick: () => go(shift(month, 1)),
      },
      h(
        "span",
        { style: { display: "inline-flex", transform: "scaleX(-1)" } },
        icon(icons.back),
      ),
    ),
    h("div", { class: "spacer" }),
    csvButton("Download this bill as CSV", () =>
      api.download("/v1/billing", { org_id: ctx.orgID, month }),
    ),
  );

  const columns = [
    allOrgs
      ? {
          label: "Organisation",
          cell: (r) =>
            h(
              "div",
              { class: "stack" },
              r.org_name
                ? h("span", {}, r.org_name)
                : h("span", { class: "faint" }, "Deleted"),
              h(
                "span",
                { class: "mono faint", style: { fontSize: "11px" } },
                r.org_id,
              ),
            ),
          sortKey: (r) => r.org_name || r.org_id,
        }
      : null,
    {
      label: "Model",
      cell: (r) =>
        h(
          "div",
          { class: "stack" },
          h("span", { class: "mono" }, r.backend_model),
          h(
            "span",
            { class: "faint", style: { fontSize: "11px" } },
            r.provider,
          ),
        ),
      sortKey: (r) => `${r.provider}/${r.backend_model}`,
    },
    {
      label: "Calls",
      num: true,
      cell: (r) => num(r.calls),
      sortKey: (r) => r.calls,
      sortDir: "desc",
    },
    {
      label: "Input",
      num: true,
      cell: (r) => compact(r.input_tokens),
      sortKey: (r) => r.input_tokens,
      sortDir: "desc",
    },
    {
      label: "Cached",
      num: true,
      cell: (r) => compact(r.cached_input_tokens),
      sortKey: (r) => r.cached_input_tokens,
      sortDir: "desc",
    },
    {
      label: "Cache writes",
      num: true,
      cell: (r) => compact(r.cache_write_tokens),
      sortKey: (r) => r.cache_write_tokens,
      sortDir: "desc",
    },
    {
      label: "Output",
      num: true,
      cell: (r) => compact(r.output_tokens),
      sortKey: (r) => r.output_tokens,
      sortDir: "desc",
    },
    {
      label: "Amount",
      num: true,
      cell: (r) => h("strong", {}, money(r.micros)),
      sortKey: (r) => r.micros,
      sortDir: "desc",
    },
    payments
      ? {
          label: "From credit",
          num: true,
          cell: (r) => money(r.credit_micros),
          sortKey: (r) => r.credit_micros,
          sortDir: "desc",
        }
      : null,
  ].filter(Boolean);

  const body = table(columns, rows, {
    emptyTitle:
      month === now
        ? "Nothing billed this month"
        : `Nothing billed in ${monthName(month)}`,
    emptyBody:
      "Calls to models on this deployment's own provider keys appear here.",
  });

  const foot =
    total &&
    h(
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
          `${num(total.calls)} ${plural(total.calls, "call")}`,
        ),
        h("strong", {}, money(total.micros)),
      ),
    );

  return h(
    "div",
    {},
    payments ? sectionHead("Usage", null, { marginTop: "28px" }) : null,
    controls,
    body,
    foot,
  );
}

// chf reads an amount typed in francs as micros, or NaN.
function chf(input) {
  const v = Number(String(input).replace(",", ".").trim());
  return input === "" || !Number.isFinite(v)
    ? NaN
    : Math.round(v * 100) * 10000;
}

const kindLabels = {
  topup: "Payment",
  auto: "Automatic top-up",
  grant: "Granted",
};

const stateTones = { paid: "good", pending: "warn", failed: "bad" };

async function creditSection(ctx) {
  const res = await api.creditAccount(ctx.orgID);
  if (!res.payments) return null;
  const a = res.account;
  const recent = res.recent || [];
  const operator = ctx.state.me.unrestricted;
  ctx.setSubtitle(`${money(a.balance_micros)} credit`);

  // Back from PostFinance's payment page: say how it went.
  const params = new URLSearchParams(location.search);
  const returned = params.get("payment");
  let back = null;
  if (returned) {
    const p = recent.find((r) => r.id === returned);
    back = returnBanner(ctx, p, a);
    if (!p || p.state !== "pending") {
      remember("keera.billing.wait", null);
      history.replaceState({}, "", location.pathname);
    }
  }

  const low = a.balance_micros <= 0 && !a.invoiced;
  const stats = h(
    "div",
    { class: "grid grid-3" },
    stat(
      "Credit",
      money(a.balance_micros),
      a.invoiced
        ? "billed by invoice, no credit needed"
        : low
          ? "used up: calls are refused"
          : "paid in advance",
    ),
    stat(
      "Automatic top-up",
      a.topup_micros ? money(a.topup_micros) : "Off",
      a.topup_micros
        ? `when the credit falls below ${money(a.topup_below_micros)}`
        : "charges the saved card when credit runs low",
    ),
    stat(
      "Card",
      a.card ? a.card.label : "None",
      a.card
        ? a.card.expires
          ? `expires ${new Date(a.card.expires).toLocaleDateString(undefined, { month: "2-digit", year: "2-digit" })}`
          : "saved at PostFinance, not here"
        : "save one when you add credit",
    ),
  );

  const actions = h(
    "div",
    { class: "row", style: { margin: "16px 0" } },
    h(
      "button",
      { class: "btn btn-primary", onClick: () => addCredit(ctx, res) },
      icon(icons.plus),
      "Add credit",
    ),
    h(
      "button",
      { class: "btn", onClick: () => autoTopUp(ctx, a) },
      "Automatic top-up",
    ),
    a.card
      ? h(
          "button",
          {
            class: "btn",
            onClick: () =>
              confirm({
                title: "Forget the saved card?",
                body:
                  "PostFinance forgets it too, and the automatic top-up stops. " +
                  "You can save a card again the next time you add credit.",
                confirmLabel: "Forget card",
                danger: true,
                onConfirm: async () => {
                  await api.forgetCard(ctx.orgID);
                  toast("Card forgotten", "good");
                  ctx.reload();
                },
              }),
          },
          "Forget card",
        )
      : null,
    h("div", { class: "spacer" }),
    operator
      ? h("button", { class: "btn", onClick: () => grant(ctx) }, "Grant credit")
      : null,
    operator
      ? h(
          "button",
          {
            class: "btn",
            title: a.invoiced
              ? "Make this organisation pay in advance again"
              : "Let this organisation use the keys without credit, and bill it by invoice",
            onClick: async (e) => {
              const button = e.currentTarget;
              button.disabled = true;
              try {
                await api.updateCredit(ctx.orgID, { invoiced: !a.invoiced });
                toast(
                  a.invoiced ? "Pays in advance again" : "Billed by invoice",
                  "good",
                );
                ctx.reload();
              } catch (ex) {
                toast(ex.message, "bad");
                button.disabled = false;
              }
            },
          },
          a.invoiced ? "Stop invoicing" : "Bill by invoice",
        )
      : null,
  );

  const failed = a.topup_error
    ? banner(
        "warn",
        { style: { marginTop: "16px" } },
        `The last automatic top-up failed: ${a.topup_error} ` +
          "No other is tried until you save the automatic top-up again or add credit by hand.",
      )
    : null;

  const list = table(
    [
      {
        label: "When",
        cell: (p) => h("span", { class: "nowrap" }, dateTime(p.ts)),
        sortKey: (p) => p.ts,
        sortDir: "desc",
      },
      {
        label: "What",
        cell: (p) =>
          h(
            "div",
            { class: "stack" },
            h("span", {}, kindLabels[p.kind] || p.kind),
            p.note
              ? h(
                  "span",
                  { class: "faint", style: { fontSize: "11px" } },
                  p.note,
                )
              : null,
          ),
      },
      {
        label: "Credit",
        num: true,
        cell: (p) => h("strong", {}, money(p.micros)),
        sortKey: (p) => p.micros,
      },
      {
        label: "VAT",
        num: true,
        cell: (p) => (p.vat_micros ? money(p.vat_micros) : "-"),
      },
      {
        label: "Fee",
        num: true,
        cell: (p) => (p.fee_micros ? money(p.fee_micros) : "-"),
      },
      {
        label: "State",
        cell: (p) => pill(capitalise(p.state), stateTones[p.state]),
      },
      { label: "By", cell: (p) => h("span", { class: "muted" }, p.actor) },
    ],
    recent,
    {
      emptyTitle: "No payments yet",
      emptyBody: "Add credit to use the models on this deployment's keys.",
    },
  );

  return h(
    "div",
    {},
    back,
    stats,
    failed,
    actions,
    sectionHead("Payments", "the latest 20"),
    list,
  );
}

// returnBanner says how a payment went, for someone back from the payment
// page. A pending one is looked at again for a little while.
function returnBanner(ctx, p, a) {
  if (!p) return null;
  if (p.state === "paid") {
    return banner(
      "good",
      { style: { marginBottom: "16px" } },
      `Thank you: ${money(p.micros)} was added. The credit is now ${money(a.balance_micros)}.`,
    );
  }
  if (p.state === "failed") {
    return banner(
      "bad",
      { style: { marginBottom: "16px" } },
      `The payment did not go through: ${p.note || "nothing was charged"}.`,
    );
  }
  const tries = Number(remembered("keera.billing.wait") || 0);
  if (tries < 10) {
    remember("keera.billing.wait", String(tries + 1));
    const timer = setTimeout(() => ctx.reload(), 3000);
    ctx.onTeardown(() => clearTimeout(timer));
  } else {
    remember("keera.billing.wait", null);
    history.replaceState({}, "", location.pathname);
  }
  return banner(
    "info",
    { style: { marginBottom: "16px" } },
    "PostFinance is still confirming the payment. The credit is added as soon as it is through.",
  );
}

export function addCredit(ctx, res) {
  const a = res.account;
  const vat = Number(res.vat_percent || 0);
  const amount = h("input", {
    class: "input",
    type: "number",
    min: res.min_topup_micros / 1e6,
    max: res.max_topup_micros / 1e6,
    step: "1",
    value: "100",
    inputmode: "decimal",
  });
  const save = h("input", { type: "checkbox", checked: !a.card });
  const total = h("div", { class: "price-sum" });
  // The same sums as the server's vatOn and feeOn, to the centime.
  const vatOn = (m) => Math.round((m * vat) / 100 / 10000) * 10000;
  const feeOn = (m) =>
    res.fee_fixed_micros +
    Math.round(
      Math.max(Math.floor((m * res.fee_bp) / 10000), res.fee_min_micros) /
        10000,
    ) *
      10000;
  const showTotal = () => {
    const m = chf(amount.value);
    total.hidden = Number.isNaN(m);
    if (total.hidden) return;
    const fee = feeOn(m);
    total.replaceChildren(
      h("span", {}, "Transaction fee"),
      h("span", { class: "num" }, money(fee)),
      h("strong", {}, vat ? `Total, with ${vat}% VAT` : "Total"),
      h("strong", { class: "num" }, money(m + vatOn(m) + fee)),
    );
  };
  amount.addEventListener("input", showTotal);
  showTotal();
  formModal({
    title: "Add credit",
    subtitle:
      "You pay on PostFinance Checkout, by card, TWINT or PostFinance Pay.",
    body: h(
      "form",
      { onSubmit: (e) => e.preventDefault() },
      field("Credit in CHF", amount, total),
      h(
        "label",
        { class: "row-tight" },
        save,
        a.card
          ? "Save this card instead of the one saved now"
          : "Save the card for automatic top-ups",
      ),
    ),
    submitLabel: "Continue to payment",
    submit: async () => {
      const m = chf(amount.value);
      if (Number.isNaN(m)) {
        amount.focus();
        throw new Error("Enter an amount in francs.");
      }
      const started = await api.topUp(ctx.orgID, m, save.checked);
      remember("keera.billing.wait", null);
      location.assign(started.url);
      // The page is leaving. The dialog stays, with its button off, so the
      // payment cannot be started twice.
      return new Promise(() => {});
    },
  });
}

function autoTopUp(ctx, a) {
  const on = h("input", { type: "checkbox", checked: a.topup_micros > 0 });
  const below = h("input", {
    class: "input",
    type: "number",
    step: "1",
    inputmode: "decimal",
    value: a.topup_below_micros ? a.topup_below_micros / 1e6 : "50",
  });
  const amount = h("input", {
    class: "input",
    type: "number",
    step: "1",
    inputmode: "decimal",
    value: a.topup_micros ? a.topup_micros / 1e6 : "200",
  });
  const fields = h(
    "div",
    {},
    field("When the credit falls below (CHF)", below),
    field(
      "Charge the saved card (CHF)",
      amount,
      "VAT and a transaction fee are added on top.",
    ),
  );
  const sync = () => {
    for (const el of [below, amount]) el.disabled = !on.checked;
  };
  on.addEventListener("change", sync);
  sync();
  formModal({
    title: "Automatic top-up",
    body: h(
      "form",
      { onSubmit: (e) => e.preventDefault() },
      a.card
        ? null
        : banner(
            "warn",
            { style: { marginBottom: "12px" } },
            "There is no saved card yet. Add credit once with “Save the card” ticked.",
          ),
      h(
        "label",
        { class: "row-tight", style: { marginBottom: "12px" } },
        on,
        "Top up automatically",
      ),
      fields,
    ),
    done: "Automatic top-up saved",
    ctx,
    submit: async () => {
      const change = on.checked
        ? {
            topup_below_micros: chf(below.value),
            topup_micros: chf(amount.value),
          }
        : { topup_below_micros: 0, topup_micros: 0 };
      if (Number.isNaN(change.topup_below_micros)) {
        below.focus();
        throw new Error("Enter the amount to top up below, in francs.");
      }
      if (Number.isNaN(change.topup_micros)) {
        amount.focus();
        throw new Error("Enter the amount to charge, in francs.");
      }
      await api.updateCredit(ctx.orgID, change);
    },
  });
}

function grant(ctx) {
  const amount = h("input", {
    class: "input",
    type: "number",
    step: "1",
    inputmode: "decimal",
  });
  const note = h("input", { class: "input", placeholder: "Trial" });
  formModal({
    title: "Grant credit",
    body: h(
      "form",
      { onSubmit: (e) => e.preventDefault() },
      field("Credit in CHF", amount, "Below 0 takes credit away."),
      field("Why", note, "The organisation sees this in its payments."),
    ),
    submitLabel: "Grant",
    done: "Credit granted",
    ctx,
    submit: async () => {
      const m = chf(amount.value);
      if (Number.isNaN(m) || m === 0) {
        amount.focus();
        throw new Error("Enter an amount in francs, other than 0.");
      }
      if (!note.value.trim()) {
        note.focus();
        throw new Error("Say why, for the organisation's payments.");
      }
      await api.grantCredit(ctx.orgID, m, note.value.trim());
    },
  });
}
