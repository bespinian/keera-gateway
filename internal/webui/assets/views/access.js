// My access: the developer's own keys, budgets, refusals and models, so they
// need not ask an administrator. It shows only the reader's own keys, even for
// an administrator. Revoking is the one write, so a leaked key can be stopped
// at once.

import { api } from "../api.js";
import {
  h,
  replace,
  table,
  meter,
  ago,
  date,
  dateTime,
  money,
  num,
  empty,
  plural,
  sectionHead,
  dash,
  banner,
} from "../ui.js";
import { oneLine, statusMeaning } from "../status.js";
import {
  activateButton,
  canRevoke,
  confirmRevoke,
  prefixOf,
  statePill,
} from "./keys.js";
import { noKey } from "./connect.js";

// Each level as the developer would name it.
const SCOPE_NAMES = {
  org: "Your organisation",
  project: "Your project",
  key: "This key",
};

function whose(sc) {
  return SCOPE_NAMES[sc.type] || sc.type;
}

export async function accessView(ctx) {
  const a = await api.access();

  // anonymous is the operator key sent as a header. In the panel it signs in
  // as a stand-in user instead, which the server marks as operator_key.
  if (a.anonymous) return noKey(ctx, a);

  const keys = a.keys || [];
  const live = keys.filter((k) => k.state === "active");
  const refusals = a.refusals || [];
  ctx.setSubtitle(summary(live));

  const wrap = h("div", {});

  // A recent refusal is why people open this screen, so it comes first.
  const recent = refusals.filter(
    (f) => Date.now() - new Date(f.created_at).getTime() < 86400000,
  );
  if (recent.length) {
    wrap.append(
      banner(
        "warn",
        { style: { marginBottom: "16px" } },
        `${num(recent.length)} ${plural(recent.length, "request was", "requests were")} refused or failed in the ` +
          `last 24 hours: ${statusMeaning(recent[0].status)[0].toLowerCase()}` +
          `${recent.some((f) => f.status !== recent[0].status) ? ", among others" : ""}. ` +
          "Details are at the bottom of this screen.",
      ),
    );
  }

  // The same card as on My clients and in the playground, so the operator key
  // and a missing key read the same on all three.
  if (!keys.length) wrap.append(noKey(ctx, a));

  // Revoked keys are hidden by default: people come here to see which keys
  // work. The box resets on each visit, since it was ticked to check one thing.
  const cards = h("div", {});
  let showRevoked = false;
  function drawCards() {
    const shown = showRevoked
      ? keys
      : keys.filter((k) => k.state !== "revoked");
    replace(
      cards,
      shown.length
        ? shown.map((k) => keyCard(k, ctx))
        : h(
            "div",
            { class: "card" },
            empty(
              "None of your keys work",
              "All of them are revoked. Tick the box above to see them, or ask " +
                "an administrator for a new key.",
            ),
          ),
    );
  }

  // The keys are cards, not table rows, so this draws table()'s toolbar itself.
  if (keys.some((k) => k.state === "revoked")) {
    wrap.append(
      h(
        "div",
        { class: "toolbar" },
        h(
          "label",
          { class: "row-tight" },
          h("input", {
            type: "checkbox",
            checked: showRevoked,
            onChange: (e) => {
              showRevoked = e.target.checked;
              drawCards();
            },
          }),
          "Show revoked",
        ),
        h("div", { class: "spacer" }),
      ),
    );
  }

  if (keys.length) {
    drawCards();
    wrap.append(cards);
  }

  if (refusals.length) {
    wrap.append(refusalsCard(refusals, keys));
  }

  return wrap;
}

function summary(live) {
  if (!live.length) return "No active key";
  const spend = live.reduce((a, k) => a + (k.spend_micros || 0), 0);
  return (
    `${live.length} active ${plural(live.length, "key")} · ` +
    `${money(spend)} this month`
  );
}

function keyCard(k, ctx) {
  const facts = h(
    "div",
    { class: "row", style: { gap: "24px", flexWrap: "wrap" } },
    fact(
      "Project",
      k.project_name ||
        k.project_id ||
        h("span", { class: "faint" }, "No project"),
    ),
    fact(
      "Last used",
      k.last_used_at
        ? h("span", { title: dateTime(k.last_used_at) }, ago(k.last_used_at))
        : h("span", { class: "faint" }, "not this month"),
    ),
    fact(
      "Expires",
      k.expires_at
        ? h("span", {}, date(k.expires_at))
        : h("span", { class: "faint" }, "never"),
    ),
    fact(
      "This month",
      h(
        "span",
        {},
        `${num(k.requests || 0)} ${plural(k.requests || 0, "request")} · `,
        money(k.spend_micros),
      ),
    ),
  );

  const body = h("div", { class: "card-body" }, facts);

  // Outermost limit first, the order they apply in.
  const bound = (k.scopes || []).filter(hasLimit);
  if (bound.length) {
    body.append(
      h(
        "div",
        { class: "stack", style: { gap: "10px", marginTop: "16px" } },
        bound.map((sc) => scopeRow(sc)),
      ),
    );
  } else {
    body.append(
      h(
        "p",
        { class: "muted", style: { marginTop: "16px", marginBottom: 0 } },
        "No budget or rate limit applies to this key.",
      ),
    );
  }

  const allowed = k.allowed_models || [];
  body.append(
    h(
      "div",
      { style: { marginTop: "16px" } },
      h("div", { class: "stat-label" }, "Models this key can call"),
      allowed.length
        ? h(
            "div",
            {
              class: "row-tight",
              style: { flexWrap: "wrap", marginTop: "6px" },
            },
            allowed.map((name) => h("span", { class: "pill mono" }, name)),
          )
        : h(
            "p",
            { class: "muted", style: { marginBottom: 0 } },
            "None. Ask an administrator which model to use.",
          ),
    ),
  );

  const prompts = (k.scopes || []).filter((sc) => sc.system_prompt);
  if (prompts.length) {
    body.append(promptsSection(prompts));
  }

  const filtering = (k.scopes || []).filter((sc) => (sc.filters || []).length);
  if (filtering.length) {
    body.append(filtersSection(filtering));
  }

  return h(
    "div",
    { class: "card", style: { marginBottom: "16px" } },
    h(
      "div",
      { class: "card-head" },
      h(
        "div",
        { class: "stack" },
        h("h2", {}, k.name),
        h(
          "span",
          { class: "faint mono", style: { fontSize: "11px" } },
          prefixOf(k),
        ),
      ),
      h("div", { class: "spacer" }),
      statePill(k.state),
      // A pending key is always the reader's to activate.
      k.state === "pending" ? activateButton(ctx, k, "btn btn-sm") : null,
      (k.state === "active" || k.state === "pending") && canRevoke(ctx, k)
        ? h(
            "button",
            {
              class: "btn btn-sm btn-danger",
              title: "Stop this key from working",
              onClick: () => confirmRevoke(k, ctx.reload),
            },
            "Revoke",
          )
        : null,
    ),
    body,
  );
}

// The prompts are quoted in full, in the order sent, because a developer
// cannot see them in their own traffic.
function promptsSection(prompts) {
  return h(
    "div",
    { style: { marginTop: "16px" } },
    h("div", { class: "stat-label" }, "Added before your messages"),
    h(
      "div",
      { class: "stack", style: { gap: "10px", marginTop: "6px" } },
      prompts.map((sc) =>
        h(
          "div",
          { class: "stack", style: { gap: "4px" } },
          h(
            "span",
            { class: "muted small" },
            `${whose(sc)}${sc.name ? ` (${sc.name})` : ""} sends:`,
          ),
          h("div", { class: "prompt-text" }, sc.system_prompt),
        ),
      ),
    ),
    h(
      "div",
      { class: "hint", style: { marginTop: "8px" } },
      "Added to every chat request and charged as input tokens. An " +
        "administrator sets it.",
    ),
  );
}

// Filters can change or stop a request unseen, so they are named here. The
// details are on the Filters screen, which every member can read.
function filtersSection(scopes) {
  return h(
    "div",
    { style: { marginTop: "16px" } },
    h(
      "div",
      { class: "stat-label" },
      "Filters check your requests before they are sent",
    ),
    h(
      "div",
      { class: "stack", style: { gap: "4px", marginTop: "6px" } },
      scopes.map((sc) =>
        h(
          "span",
          { class: "muted", style: { fontSize: "12px" } },
          `${whose(sc)}${sc.name ? ` (${sc.name})` : ""} runs `,
          sc.filters.map((alias, i) => [
            i > 0 ? ", " : null,
            h("span", { class: "mono" }, alias),
          ]),
        ),
      ),
    ),
    h(
      "div",
      { class: "hint", style: { marginTop: "8px" } },
      "Each filter checks your request first: some remove secrets, some " +
        "only allow or refuse it, and some run in shadow and change nothing. " +
        "Filters that use a model add latency and cost, and if an enforcing " +
        "one refuses or cannot run, nothing is sent and the error names it.",
    ),
  );
}

function fact(label, value) {
  return h(
    "div",
    { class: "stack" },
    h("div", { class: "stat-label" }, label),
    h("div", {}, value),
  );
}

function hasLimit(sc) {
  return (sc.budgets || []).length > 0 || (sc.rates || []).length > 0;
}

function scopeRow(sc) {
  const who = whose(sc);

  const head = h(
    "div",
    { class: "row" },
    h("strong", {}, who),
    sc.name ? h("span", { class: "muted" }, sc.name) : null,
    h("div", { class: "spacer" }),
    rateChips(sc.rates || []),
  );

  const row = h("div", { class: "stack", style: { gap: "6px" } }, head);

  for (const b of sc.budgets || []) {
    const frac = b.spend_micros / b.budget_micros;
    const tone = frac >= 1 ? "bad" : frac >= 0.8 ? "warn" : "";
    row.append(
      h(
        "div",
        { class: "row-tight" },
        meter(frac, tone),
        h(
          "span",
          { class: "muted nowrap small" },
          `${Math.round(frac * 100)}%`,
        ),
      ),
      h(
        "span",
        { class: "muted small" },
        `${money(b.spend_micros)} of ${money(b.budget_micros)} per ${b.period}`,
        b.resets_at ? ` · resets ${date(b.resets_at)}` : "",
        frac >= 1 ? " · used up, so requests are refused until it resets" : "",
      ),
    );
  }
  return row;
}

// A daily or monthly limit only starts again with the period, so what is used
// of it is shown too.
function rateChips(rates) {
  const chips = [];
  for (const r of rates) {
    const per = r.period === "minute" ? "min" : r.period;
    const chip = (limit, used, unit) =>
      r.period === "minute"
        ? `${num(limit)} ${unit}/${per}`
        : `${num(used || 0)} of ${num(limit)} ${unit}/${per}`;
    if (r.requests > 0) chips.push(chip(r.requests, r.requests_used, "req"));
    if (r.tokens > 0) chips.push(chip(r.tokens, r.tokens_used, "tok"));
  }
  if (!chips.length) return null;
  return h("span", { class: "muted nowrap small" }, chips.join(" · "));
}

// Refusals are recorded like any request, so this is the same log the
// dashboard counts from.
function refusalsCard(refusals, keys) {
  const keyName = Object.fromEntries(keys.map((k) => [k.id, k.name]));

  const head = sectionHead("Recent refusals and failures", "last 7 days", {
    marginTop: "24px",
  });

  const rows = table(
    [
      {
        label: "When",
        shrink: true,
        cell: (f) =>
          h(
            "span",
            { class: "muted nowrap", title: dateTime(f.created_at) },
            ago(f.created_at),
          ),
      },
      {
        label: "Reason",
        cell: (f) => {
          const [label, why] = statusMeaning(f.status);
          // The gateway's own message first, because it names the limit and
          // its number. The standing explanation is the fallback.
          return h(
            "div",
            { class: "stack" },
            h("strong", {}, label),
            h(
              "span",
              {
                class: "muted small",
                title: f.error || why,
              },
              f.error ? oneLine(f.error, 160) : why,
            ),
          );
        },
      },
      {
        label: "Model",
        shrink: true,
        cell: (f) => (f.alias ? h("span", { class: "mono" }, f.alias) : dash()),
      },
      {
        label: "Key",
        shrink: true,
        cell: (f) =>
          h("span", { class: "muted nowrap" }, keyName[f.key_id] || f.key_id),
      },
      {
        label: "Status",
        num: true,
        shrink: true,
        cell: (f) => h("span", { class: "mono faint" }, String(f.status)),
      },
    ],
    refusals,
    {
      emptyTitle: "Nothing refused or failed",
      emptyBody: "Every request your keys made in the last week was answered.",
    },
  );

  return h(
    "div",
    {},
    head,
    rows,
    h(
      "div",
      { class: "hint", style: { marginTop: "12px" } },
      "Budgets and rate limits reset on their own. For a model you may not " +
        "use, or one with no backend, ask an administrator.",
    ),
  );
}
