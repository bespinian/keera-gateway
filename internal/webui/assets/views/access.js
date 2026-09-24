// My access - the developer's own screen.
//
// Every other screen here is shaped for whoever administers the deployment.
// This one answers the four questions that are otherwise a message to an
// administrator: are my keys still alive, how much budget is left, why was I
// refused, and what am I allowed to call.
//
// It shows only what belongs to the person reading it. An administrator sees
// their own keys, not the organisation's, which are two screens away.
//
// Revoking is the one write here, because this is where the person is when
// they need it: a key pasted somewhere it should not be is killed by whoever
// pasted it, in the second that takes.

import { api } from "../api.js";
import {
  h,
  replace,
  table,
  pill,
  meter,
  ago,
  date,
  dateTime,
  money,
  num,
  empty,
  confirm,
  toast,
} from "../ui.js";
import { canRevoke, revokeBody } from "./keys.js";

// REASONS turns the status a request was refused with into the sentence the
// person it happened to would use. The status is the record; this is what it
// meant to them.
const REASONS = {
  400: ["Malformed request", "The gateway could not read the request."],
  402: [
    "Budget spent",
    "A budget that covers this key is used up for this period.",
  ],
  403: ["Not permitted", "A guardrail refused the key."],
  404: [
    "Model not available",
    "This key may not use this model, or no backend serves it.",
  ],
  413: ["Request too large", "The prompt is over the gateway's size limit."],
  429: ["Rate limit", "Over one of the per-minute limits above."],
  503: ["No backend", "The model exists but nothing serves it."],
};

function reason(status) {
  if (REASONS[status]) return REASONS[status];
  if (status >= 500) return ["Backend error", "The backend did not answer."];
  return ["Refused", "The gateway did not forward this request."];
}

// Each level of the hierarchy as the person held by it would name it. They know
// they are in an organisation and a team; the ids are for quoting to whoever
// administers them.
const SCOPE_NAMES = {
  org: "Your organisation",
  team: "Your team",
  key: "This key",
};

function whose(sc) {
  return SCOPE_NAMES[sc.type] || sc.type;
}

export async function accessView(ctx) {
  const a = await api.access();
  const currency = a.currency || ctx.currency;

  if (a.anonymous) {
    return h(
      "div",
      { class: "card" },
      empty(
        "You are signed in with the operator key",
        "The operator key is not a person, so it has no keys, team or " +
          "budget. Sign in with your identity provider to see yours.",
      ),
    );
  }

  const keys = a.keys || [];
  const live = keys.filter((k) => k.state === "active");
  const refusals = a.refusals || [];
  ctx.setSubtitle(summary(live, currency));

  const wrap = h("div", {});

  // A refusal in the last day is the reason somebody opens this screen, so it
  // is the first thing on it rather than a table further down.
  const recent = refusals.filter(
    (f) => Date.now() - new Date(f.ts).getTime() < 86400000,
  );
  if (recent.length) {
    wrap.append(
      h(
        "div",
        { class: "banner banner-warn", style: { marginBottom: "16px" } },
        `${num(recent.length)} request${recent.length === 1 ? " was" : "s were"} refused in the ` +
          `last 24 hours: ${reason(recent[0].status)[0].toLowerCase()}` +
          `${recent.some((f) => f.status !== recent[0].status) ? ", among others" : ""}. ` +
          "Details are at the bottom of this screen.",
      ),
    );
  }

  if (!keys.length) {
    // The way out of this state depends on whether the reader can issue one
    // themselves. Telling somebody to go and ask an administrator when there
    // is a button two screens away would be the worse of the two answers.
    wrap.append(
      h(
        "div",
        { class: "card" },
        empty(
          "No key is linked to you",
          ctx.state.me.can_issue_own_key
            ? h(
                "span",
                {},
                "Issue one on ",
                h(
                  "a",
                  {
                    href: "/keys",
                    onClick: (e) => {
                      e.preventDefault();
                      ctx.navigate("/keys");
                    },
                  },
                  "API keys",
                ),
                ". A key you already have may not be linked to you. It still " +
                  "works, but it does not show here.",
              )
            : "A key is linked to a person when it is issued. Yours may not be " +
                "linked to you. It still works, but it does not show here. Ask " +
                "an administrator for a new key in your name.",
        ),
      ),
    );
  }

  // Off by default, for the reason it is off on the API keys screen: this
  // screen is read to find out whether a key still works, and a revoked one is
  // the answer to a different question. The box is only there when there is a
  // revoked key to bring back, since one that filters nothing is noise.
  //
  // The state is this render's, not the browser's: the reader ticked it to
  // check one thing, and the next time they open the screen they are back to
  // the keys they can actually use.
  const cards = h("div", {});
  let showRevoked = false;
  function drawCards() {
    const shown = showRevoked
      ? keys
      : keys.filter((k) => k.state !== "revoked");
    replace(
      cards,
      shown.length
        ? shown.map((k) => keyCard(k, currency, ctx))
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

  if (keys.length && keys.some((k) => k.state === "revoked")) {
    wrap.append(
      h(
        "div",
        { class: "row", style: { marginBottom: "12px" } },
        h("div", { style: { flex: 1 } }),
        h(
          "label",
          { class: "row-tight faint", style: { fontSize: "12px" } },
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

  wrap.append(
    h(
      "div",
      { class: "hint", style: { marginTop: "16px" } },
      "Your editor sends to ",
      h("code", {}, a.gateway_url || "the gateway"),
      ". Ready-made configuration is on ",
      h(
        "a",
        {
          href: "/connect",
          onClick: (e) => {
            e.preventDefault();
            ctx.navigate("/connect");
          },
        },
        "Connect a client",
      ),
      ". Each response reports what is left in the ",
      h("code", {}, "X-Keera-Budget-Remaining"),
      " and ",
      h("code", {}, "X-RateLimit-Remaining-Requests"),
      " headers.",
    ),
  );

  return wrap;
}

function summary(live, currency) {
  if (!live.length) return "no active key";
  const spend = live.reduce((a, k) => a + (k.spend_micros || 0), 0);
  return (
    `${live.length} active key${live.length === 1 ? "" : "s"} · ` +
    `${money(spend, currency)} this month`
  );
}

// keyCard is one key and everything that decides whether it works.
function keyCard(k, currency, ctx) {
  const tone =
    { active: "good", expired: "warn", revoked: "bad" }[k.state] || "";

  const facts = h(
    "div",
    { class: "row", style: { gap: "24px", flexWrap: "wrap" } },
    fact(
      "Team",
      k.team_name || h("span", { class: "faint" }, "organisation-wide"),
    ),
    fact(
      "Last used",
      k.last_used_at
        ? h("span", { title: k.last_used_at }, ago(k.last_used_at))
        : h("span", { class: "faint" }, "never"),
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
        num(k.requests || 0),
        " req · ",
        money(k.spend_micros, currency),
      ),
    ),
  );

  const body = h("div", { class: "card-body" }, facts);

  // The limits, outermost first: that is the order they apply in, and the order
  // the question "which one stopped me" is answered in.
  const bound = (k.scopes || []).filter(hasLimit);
  if (bound.length) {
    body.append(
      h(
        "div",
        { class: "stack", style: { gap: "10px", marginTop: "16px" } },
        bound.map((sc) => scopeRow(sc, currency)),
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

  if (k.max_output_tokens) {
    body.append(
      h(
        "p",
        { class: "hint", style: { marginTop: "12px" } },
        `Output is capped at ${num(k.max_output_tokens)} tokens. ` +
          "A request for more is lowered to the cap, not refused.",
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
        h("h2", {}, k.alias),
        h(
          "span",
          { class: "faint mono", style: { fontSize: "11px" } },
          k.prefix + "…",
        ),
      ),
      h("div", { class: "spacer", style: { flex: 1 } }),
      pill(k.state[0].toUpperCase() + k.state.slice(1), tone),
      // Only a key that still works can be taken away, and only the state
      // above says whether this one does.
      k.state === "active" && canRevoke(ctx, k)
        ? h(
            "button",
            {
              class: "btn btn-sm btn-danger",
              title: "Stop this key from working",
              onClick: () =>
                confirm({
                  title: "Revoke this key?",
                  body: revokeBody(k),
                  confirmLabel: "Revoke",
                  danger: true,
                  onConfirm: async () => {
                    await api.revokeKey(k.id);
                    toast("Key revoked", "good");
                    ctx.reload();
                  },
                }),
            },
            "Revoke",
          )
        : null,
    ),
    body,
  );
}

// promptsSection is the wording put ahead of this key's messages, in the order
// it is sent.
//
// It is the one thing on this screen that a developer cannot work out from
// their own traffic: text nobody in their editor wrote is prepended to every
// chat request they make, and until it is here the only ways to find out were
// to ask an administrator or to notice the model behaving oddly and guess. It
// is quoted rather than summarised, because "your team sets a system prompt"
// answers nothing.
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
            { class: "muted", style: { fontSize: "11.5px" } },
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

// filtersSection names what reads this key's requests on the way out.
//
// It belongs on this screen for the same reason the system prompt does, and
// more urgently: a filter does not add to what the developer wrote, it changes
// it or stops it. What reaches the model is not what left their editor - or
// nothing did - and no amount of reading their own traffic would ever tell them
// so. Only the aliases are here; the mode and the instruction are on the Filters
// screen, which every member can read for the same reason they can read the
// system prompt that is put ahead of their messages.
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
      "Each filter is a small model that checks your request first. Some " +
        "remove secrets or client data, some only decide whether it may be " +
        "sent, and some run in shadow and change nothing. The Filters screen " +
        "shows which. All add latency and cost. If an enforcing filter " +
        "refuses or cannot run, nothing is sent and the error names it.",
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
  return sc.budget_micros > 0 || sc.rpm > 0 || sc.tpm > 0;
}

// scopeRow is one level of the hierarchy: what it allows, what is left of it,
// and when it lifts. A developer who can see this does not have to ask.
function scopeRow(sc, currency) {
  const who = whose(sc);

  const head = h(
    "div",
    { class: "row" },
    h("strong", {}, who),
    sc.name ? h("span", { class: "muted" }, sc.name) : null,
    h("div", { style: { flex: 1 } }),
    rateChips(sc),
  );

  const row = h("div", { class: "stack", style: { gap: "6px" } }, head);

  if (sc.budget_micros > 0) {
    const frac = sc.spend_micros / sc.budget_micros;
    const tone = frac >= 1 ? "bad" : frac >= 0.8 ? "warn" : "";
    row.append(
      h(
        "div",
        { class: "row-tight" },
        meter(frac, tone),
        h(
          "span",
          { class: "muted nowrap", style: { fontSize: "11.5px" } },
          `${Math.round(frac * 100)}%`,
        ),
      ),
      h(
        "span",
        { class: "muted", style: { fontSize: "11.5px" } },
        `${money(sc.spend_micros, "")} of ${money(sc.budget_micros, currency)} per ${sc.period}`,
        sc.resets_at ? ` · resets ${date(sc.resets_at)}` : "",
        frac >= 1 ? " · used up, so requests are refused until it resets" : "",
      ),
    );
  }
  return row;
}

function rateChips(sc) {
  const chips = [];
  if (sc.rpm > 0) chips.push(`${num(sc.rpm)} req/min`);
  if (sc.tpm > 0) chips.push(`${num(sc.tpm)} tok/min`);
  if (!chips.length) return null;
  return h(
    "span",
    { class: "muted nowrap", style: { fontSize: "11.5px" } },
    chips.join(" · "),
  );
}

// refusalsCard is the answer to "my agent stopped working this afternoon". The
// gateway records a refusal the way it records a completed request, so this is
// the same log the dashboard counts from rather than a second account of it.
function refusalsCard(refusals, keys) {
  const keyAlias = Object.fromEntries(keys.map((k) => [k.id, k.alias]));

  const head = h(
    "div",
    { class: "row", style: { margin: "24px 0 12px" } },
    h("h2", {}, "Recent refusals"),
    h("div", { style: { flex: 1 } }),
    h("span", { class: "muted", style: { fontSize: "11.5px" } }, "last 7 days"),
  );

  const rows = table(
    [
      {
        label: "When",
        shrink: true,
        cell: (f) =>
          h(
            "span",
            { class: "muted nowrap", title: dateTime(f.ts) },
            ago(f.ts),
          ),
      },
      {
        label: "Reason",
        cell: (f) => {
          const [label, why] = reason(f.status);
          // The gateway's own sentence when there is one, because it names the
          // limit that bound and the number behind it - "your team is over its
          // rate limit of 60 requests per minute" is a different morning from
          // "rate limit". The standing explanation is the fallback.
          return h(
            "div",
            { class: "stack" },
            h("strong", {}, label),
            h(
              "span",
              {
                class: "muted",
                style: { fontSize: "11.5px" },
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
        cell: (f) =>
          f.alias
            ? h("span", { class: "mono" }, f.alias)
            : h("span", { class: "faint" }, "-"),
      },
      {
        label: "Key",
        shrink: true,
        cell: (f) =>
          h("span", { class: "muted nowrap" }, keyAlias[f.key_id] || f.key_id),
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
      emptyTitle: "Nothing has been refused",
      emptyBody:
        "Every request your keys have made in the last week was forwarded.",
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

// oneLine folds a gateway or backend message onto one row of a table. Both can
// run to several sentences, and a cell holding all of them is a table nobody
// can scan; the whole of it is on the row's title.
function oneLine(text, max) {
  const flat = text.replace(/\s+/g, " ").trim();
  return flat.length > max ? flat.slice(0, max - 1) + "\u2026" : flat;
}
