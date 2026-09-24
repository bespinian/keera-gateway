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
  modal,
  icon,
  icons,
  empty,
  RANGES as SHARED_RANGES,
} from "../ui.js";

const TONES = {
  "key.create": "accent",
  "key.revoke": "bad",
  "guardrail.put": "warn",
  "filter.put": "warn",
  // Removing a filter is the entry a compliance reader is looking for: from
  // here on, requests that were being redacted are not.
  "filter.delete": "bad",
  "filter.check": "",
  "model.delete": "bad",
  "model.check": "",
  "auth.sign_in": "good",
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
  "org.create": "Organisation created",
  "org.update": "Organisation changed",
  "org.delete": "Organisation deleted",
  "team.create": "Team created",
  "user.put": "User added",
  "user.set_role": "User role changed",
  "key.create": "API key issued",
  "key.revoke": "API key revoked",
  "guardrail.put": "Guardrails changed",
  "model.put": "Model saved",
  "model.check": "Model checked",
  "model.delete": "Model deleted",
  "filter.put": "Filter saved",
  "filter.check": "Filter checked",
  "filter.delete": "Filter deleted",
  "router.put": "Router saved",
  "router.check": "Router checked",
  "router.delete": "Router deleted",
  "sandbox.create": "Sandbox created",
  "sandbox.extend": "Sandbox extended",
  "sandbox.terminate": "Sandbox terminated",
  "sandbox.attach": "Sandbox attached to",
  "sandbox_class.put": "Sandbox class saved",
  "sandbox_class.delete": "Sandbox class deleted",
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

  const controls = h(
    "div",
    { class: "row", style: { marginBottom: "16px" } },
    h(
      "select",
      {
        class: "select",
        style: { width: "auto" },
        "aria-label": "Action",
        onChange: (e) => set("action", e.target.value),
      },
      h("option", { value: "" }, "Every action"),
      facets.actions.map((a) =>
        h("option", { value: a, selected: a === filter.action }, label(a)),
      ),
    ),
    h(
      "select",
      {
        class: "select",
        style: { width: "auto" },
        "aria-label": "Who",
        onChange: (e) => set("actor", e.target.value),
      },
      h("option", { value: "" }, "Anybody"),
      facets.actors.map((a) =>
        h("option", { value: a, selected: a === filter.actor }, a),
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
            "aria-pressed": String(r.since === filter.since),
            onClick: () => set("since", r.since),
          },
          r.label,
        ),
      ),
    ),
    h(
      "button",
      {
        class: "btn btn-sm",
        style: { marginLeft: "8px" },
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

  const active = filter.action || filter.actor;
  const body = entries.length
    ? auditTable(entries)
    : h(
        "div",
        { class: "card" },
        empty(
          active ? "Nothing matches this filter" : "Nothing recorded yet",
          active
            ? "Widen the range, or clear the filter."
            : "Changes made in the panel, the CLI or the API show up here.",
        ),
      );

  const wrap = h("div", {}, controls, body);

  // Paging appends rather than replacing, so somebody reading backwards
  // through a day keeps everything they have already read on the screen.
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
            wrap.insertBefore(auditTable(rows), moreWrap);
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
          '"operator key", not as a person. Set up single sign-on to see ' +
          "names here.",
      ),
    );
  }
  return wrap;
}

function describe(filter, count) {
  const bits = [`${count === 100 ? "100+" : count} entries`];
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
          h("span", { class: "muted nowrap", title: e.ts }, ago(e.ts)),
      },
      {
        label: "Who",
        cell: (e) =>
          e.actor === "operator key"
            ? h(
                "span",
                { class: "muted" },
                "operator key",
                h("span", { class: "faint" }, " (no identity)"),
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
                      title: e.action,
                      subtitle: `${e.actor} · ${new Date(e.ts).toLocaleString()}`,
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
