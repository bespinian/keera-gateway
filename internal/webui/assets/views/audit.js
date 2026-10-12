// The audit log: who changed what.
//
// This is the compliance artefact of the whole product - the answer to "prove
// nothing left the cluster and show me who changed the guardrail". So it is
// filterable, it pages past its first screenful, and it comes out as a file. A
// log that can only show its most recent two hundred entries is one nobody can
// answer a question with.

import { api } from "../api.js";
import {
  h,
  table,
  pill,
  ago,
  dateTime,
  modal,
  icon,
  icons,
  empty,
  num,
  plural,
  RANGES as SHARED_RANGES,
  rangePicker,
} from "../ui.js";

const TONES = {
  "key.create": "accent",
  "key.revoke": "bad",
  "key.rotate": "warn",
  "guardrail.put": "warn",
  "filter.put": "warn",
  // Removing a filter is the entry a compliance reader is looking for: from
  // here on, requests that were being redacted are not.
  "filter.delete": "bad",
  "filter.check": "",
  "model.delete": "bad",
  "model.check": "",
  "auth.sign_in": "good",
  "auth.directory_check": "warn",
};

// What each action is called on screen. The wire keeps `noun.verb` because
// that is what filters and exports are written against, but a reader of this
// log should meet the same words the rest of the panel uses - somebody who has
// only ever seen "Guardrails" should not have to work out that `guardrail.put`
// is the same thing.
//
// An action with no entry here falls back to its wire id, which is what a
// deployment running ahead of this panel will show. That is the right failure:
// an unrecognised entry is still legible, and still filterable.
const LABELS = {
  "auth.sign_in": "Signed in",
  "auth.cli_token": "Signed in from the command line",
  "auth.directory_check": "Changed by the directory",
  "auth.switch_org": "Switched organisation",
  "org.create": "Organisation created",
  "org.update": "Organisation changed",
  "org.delete": "Organisation deleted",
  "project.create": "Project created",
  "project.update": "Project changed",
  "project.delete": "Project deleted",
  "user.create": "User added",
  "user.set_role": "User role changed",
  "user.disable": "User disabled",
  "user.enable": "User enabled",
  "key.create": "API key issued",
  "key.rename": "API key renamed",
  "key.revoke": "API key revoked",
  "key.rotate": "API key rotated",
  "passkey.create": "Passkey added",
  "passkey.delete": "Passkey removed",
  "user.passkey_link": "Passkey set-up link created",
  "guardrail.put": "Guardrails changed",
  "guardrail.allow_model": "Model allowed by the organisation",
  "model.put": "Model saved",
  "model.check": "Model checked",
  "model.delete": "Model deleted",
  "model.credential.set": "Model credential set",
  "model.credential.clear": "Model credential removed",
  "mcp_server.put": "MCP server saved",
  "mcp_server.delete": "MCP server deleted",
  "mcp_server.credential.set": "MCP server credential set",
  "mcp_server.credential.clear": "MCP server credential removed",
  "filter.put": "Filter saved",
  "filter.check": "Filter checked",
  "filter.delete": "Filter deleted",
  "router.put": "Router saved",
  "router.check": "Router checked",
  "router.delete": "Router deleted",
  "sandbox.create": "Sandbox created",
  "sandbox.extend": "Sandbox extended",
  "sandbox.terminate": "Sandbox terminated",
  "sandbox.suspend": "Sandbox suspended",
  "sandbox.resume": "Sandbox resumed",
  "sandbox.attach": "Sandbox attached to",
  "sandbox_class.put": "Sandbox class saved",
  "sandbox_class.delete": "Sandbox class deleted",
  "billing.account.update": "Billing settings changed",
  "billing.card.remove": "Saved card removed",
  "billing.topup": "Top-up started",
  "billing.grant": "Credit granted",
  "billing.payment.paid": "Payment received",
  "billing.payment.failed": "Payment failed",
};

/** label is what to show for an action id. */
function label(action) {
  return LABELS[action] || action;
}

// The shared windows plus everything there is, and a window of its own rather
// than the shared one: the audit log is searched over a period somebody names,
// not over whatever the dashboard was last showing. The three it shares are not
// restated, so they cannot drift from the picker every other screen draws.
const RANGES = [
  ...SHARED_RANGES,
  { label: "All", since: "", long: "Everything" },
];

const PAGE = 100;

export async function auditView(ctx) {
  // The filter lives in the session rather than the URL: it is a reading
  // position, and it should survive stepping over to another screen and back.
  const filter = {
    action: sessionStorage.getItem("keera.audit.action") || "",
    actor: sessionStorage.getItem("keera.audit.actor") || "",
    since: sessionStorage.getItem("keera.audit.since") ?? "720h",
  };

  const res = await api.audit({
    org_id: ctx.orgID,
    limit: PAGE,
    action: filter.action,
    actor: filter.actor,
    since: filter.since,
  });
  const entries = res.data || [];
  const facets = res.filters || { actions: [], actors: [] };

  ctx.setSubtitle(describe(filter, entries.length));

  const set = (key, value) => {
    sessionStorage.setItem("keera.audit." + key, value);
    ctx.reload();
  };

  const head = h(
    "div",
    { class: "section-head" },
    h("div", { style: { flex: 1 } }),
    rangePicker(ctx, filter.since, {
      ranges: RANGES,
      key: "keera.audit.since",
    }),
    h(
      "button",
      {
        class: "btn",
        title: "Download all matching entries as CSV",
        onClick: () =>
          api.download("/v1/audit", {
            org_id: ctx.orgID,
            action: filter.action,
            actor: filter.actor,
            since: filter.since,
          }),
      },
      icon(icons.download),
      "CSV",
    ),
  );

  // The filters sit on their own row under the head, as on Requests and
  // Sessions, with Clear beside them once one is set.
  const active = filter.action || filter.actor;
  const filters = h(
    "div",
    { class: "row", style: { margin: "12px 0", flexWrap: "wrap" } },
    choose("Action", "All actions", facets.actions, filter.action, label, (v) =>
      set("action", v),
    ),
    choose(
      "User",
      "All users",
      facets.actors,
      filter.actor,
      (a) => a,
      (v) => set("actor", v),
    ),
    active
      ? h(
          "button",
          {
            class: "btn btn-quiet btn-sm",
            onClick: () => {
              sessionStorage.removeItem("keera.audit.action");
              sessionStorage.removeItem("keera.audit.actor");
              ctx.reload();
            },
          },
          "Clear",
        )
      : null,
  );

  const list = entries.length ? auditTable(entries) : null;
  const body =
    list ||
    h(
      "div",
      { class: "card" },
      active
        ? empty(
            "Nothing matches these filters",
            filter.since
              ? "Clear the filters, or pick a longer period."
              : "Clear the filters.",
          )
        : filter.since
          ? empty(
              "Nothing recorded in this period",
              "Pick a longer period. Changes made in the panel, the CLI or " +
                "the API show up here.",
            )
          : empty(
              "Nothing recorded yet",
              "Changes made in the panel, the CLI or the API show up here.",
            ),
    );

  const wrap = h("div", {}, head, filters, body);

  // Paging adds to the one table rather than replacing it, so somebody reading
  // backwards through a day keeps everything they have already read on the
  // screen.
  if (entries.length === PAGE) {
    let before = res.next_before;
    const more = h(
      "button",
      {
        class: "btn",
        onClick: async () => {
          more.disabled = true;
          more.textContent = "Loading…";
          try {
            const next = await api.audit({
              org_id: ctx.orgID,
              limit: PAGE,
              before,
              action: filter.action,
              actor: filter.actor,
              since: filter.since,
            });
            const rows = next.data || [];
            entries.push(...rows);
            list.redraw();
            before = next.next_before;
            if (rows.length < PAGE) {
              moreWrap.replaceChildren(
                h("div", { class: "hint" }, "That is the whole log."),
              );
              return;
            }
          } catch (ex) {
            moreWrap.append(
              h("div", { class: "banner banner-bad" }, ex.message),
            );
          } finally {
            more.disabled = false;
            more.textContent = `Load ${PAGE} more`;
          }
        },
      },
      `Load ${PAGE} more`,
    );
    const moreWrap = h(
      "div",
      { style: { marginTop: "12px", textAlign: "center" } },
      more,
    );
    wrap.append(moreWrap);
  }

  if (!ctx.state.me.sso) {
    wrap.append(
      h(
        "div",
        { class: "banner banner-warn", style: { marginTop: "16px" } },
        "No identity provider is set up, so changes show as " +
          '"operator@localhost" or "operator key", not as a person. Set up ' +
          "single sign-on to see names here.",
      ),
    );
  }
  return wrap;
}

// choose is one filter. Like the request log's, it is left out when it has
// fewer than two values to pick from, unless one is set and needs clearing.
function choose(aria, all, values, selected, name, onPick) {
  if (!selected && values.length < 2) return null;
  const options =
    values.includes(selected) || !selected ? values : [selected, ...values];
  return h(
    "select",
    {
      class: "select",
      style: { width: "auto" },
      "aria-label": aria,
      onChange: (e) => onPick(e.target.value),
    },
    h("option", { value: "" }, all),
    options.map((v) =>
      h("option", { value: v, selected: v === selected }, name(v)),
    ),
  );
}

function describe(filter, count) {
  const bits = [
    `${count === PAGE ? PAGE + "+" : num(count)} ${plural(count, "entry", "entries")}`,
  ];
  const range = RANGES.find((r) => r.since === filter.since);
  if (range) bits.push(range.since ? `last ${range.label}` : "all time");
  if (filter.action) bits.push(label(filter.action));
  if (filter.actor) bits.push(filter.actor);
  return bits.join(" · ");
}

function auditTable(entries) {
  return table(
    [
      {
        label: "When",
        shrink: true,
        cell: (e) =>
          h(
            "span",
            { class: "muted nowrap", title: dateTime(e.ts) },
            ago(e.ts),
          ),
      },
      {
        label: "Who",
        // The operator key has no identity. Sent as a header it is logged as
        // "operator key"; in the panel it signs in as operator@localhost.
        cell: (e) =>
          e.actor === "operator key" || e.actor === "operator@localhost"
            ? h(
                "span",
                { class: "muted" },
                e.actor,
                h(
                  "span",
                  { class: "faint" },
                  e.actor === "operator key"
                    ? " (no identity)"
                    : " (operator key)",
                ),
              )
            : h("strong", {}, e.actor),
      },
      {
        label: "Action",
        shrink: true,
        cell: (e) => pill(label(e.action), TONES[e.action] || ""),
      },
      {
        label: "Target",
        cell: (e) =>
          e.target_id
            ? h(
                "span",
                { class: "mono muted", style: { fontSize: "12px" } },
                e.target_id,
              )
            : h("span", { class: "faint" }, "-"),
      },
      {
        label: "",
        shrink: true,
        cell: (e) =>
          e.detail
            ? h(
                "button",
                {
                  class: "btn btn-sm",
                  title: "Show details",
                  onClick: () =>
                    modal({
                      title: label(e.action),
                      subtitle: `${e.actor} · ${dateTime(e.ts)}`,
                      body: h(
                        "pre",
                        {
                          class: "key-value",
                          style: { whiteSpace: "pre-wrap", userSelect: "text" },
                        },
                        JSON.stringify(e.detail, null, 2),
                      ),
                    }),
                },
                "View",
              )
            : null,
      },
    ],
    entries,
    { emptyTitle: "Nothing here" },
  );
}
