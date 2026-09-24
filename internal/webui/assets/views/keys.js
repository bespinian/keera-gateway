// API keys - issuing one, seeing what is live, and taking one away.

import { api } from "../api.js";
import {
  h,
  table,
  modal,
  confirm,
  toast,
  pill,
  ago,
  date,
  money,
  num,
  icon,
  icons,
  copyText,
  rowLink,
  showError,
} from "../ui.js";
import { openGuardrails, ceilingsFor, summarise } from "./guardrails.js";
import { chooseOrg } from "./orgs.js";
import { loadClients, code, defaultBase, title } from "./connect.js";

export async function keysView(ctx) {
  const [keysRes, teamsRes, modelsRes, usersRes, clients] = await Promise.all([
    api.keys(ctx.orgID),
    api.teams(ctx.orgID).catch(() => ({ data: [] })),
    api.models().catch(() => ({ data: [] })),
    // Who a key can be attributed to. A deployment with no identity provider
    // has nobody here, and the field is left out rather than shown empty.
    api.users(ctx.orgID).catch(() => ({ data: [] })),
    // The client catalogue, so that the configuration handed over with a key
    // is the same one Connect a client hands out.
    loadClients().catch(() => []),
  ]);
  const keys = keysRes.data || [];
  const teams = teamsRes.data || [];
  const users = usersRes.data || [];
  const models = (modelsRes.data || []).filter((m) => m.enabled !== false);
  const teamName = Object.fromEntries(teams.map((t) => [t.id, t.name]));
  const canEdit = ctx.state.me.unrestricted || ctx.state.me.role === "admin";
  // A member issues keys for themselves and nobody else, so they get a button
  // and not the dialog above it: there is no person to choose and no team to
  // put it in, which leaves an alias and an expiry.
  const canIssueOwn = !canEdit && ctx.state.me.can_issue_own_key;
  const currency = keysRes.currency || ctx.currency;

  const live = keys.filter((k) => stateOf(k) === "active").length;
  ctx.setSubtitle(`${live} active of ${keys.length}`);

  if (!ctx.orgID && ctx.state.me.unrestricted)
    return chooseOrg(ctx, "API keys");

  const head = h(
    "div",
    { class: "row", style: { marginBottom: "16px" } },
    h(
      "div",
      { class: "muted" },
      canEdit
        ? "A key is shown only once, when it is issued. It is not stored."
        : canIssueOwn
          ? "A key is shown only once, when it is issued. You can issue your " +
            "own. An administrator sets its limits."
          : "A key is shown only once, when it is issued. Open a key to see " +
            "its limits. An administrator sets them.",
    ),
    h("div", { style: { flex: 1 } }),
    canEdit
      ? h(
          "button",
          {
            class: "btn btn-primary",
            onClick: () => issueKey(ctx, teams, models, users, clients),
          },
          icon(icons.plus),
          "Issue key",
        )
      : canIssueOwn
        ? h(
            "button",
            {
              class: "btn btn-primary",
              onClick: () => issueOwnKey(ctx, models, clients),
            },
            icon(icons.plus),
            "Issue my own key",
          )
        : null,
  );

  const rows = table(
    [
      {
        // The alias opens the key's own screen. "Can this be revoked" is answered
        // by the columns here; "what has it actually been doing" is one request
        // at a time, and that is a screen rather than a cell.
        label: "Alias",
        sortKey: (k) => k.alias,
        cell: (k) =>
          h(
            "div",
            { class: "stack" },
            rowLink(
              ctx,
              "/keys/" + encodeURIComponent(k.id),
              k.alias,
              "This key's traffic, spend and requests",
            ),
            h(
              "span",
              { class: "faint mono", style: { fontSize: "11px" } },
              k.prefix + "…",
            ),
          ),
      },
      {
        label: "Team",
        sortKey: (k) => (k.team_id ? teamName[k.team_id] || k.team_id : null),
        cell: (k) =>
          k.team_id
            ? h("span", {}, teamName[k.team_id] || k.team_id)
            : h("span", { class: "faint" }, "organisation-wide"),
      },
      {
        // Active first, because a list of keys is read to find the live ones.
        label: "Status",
        shrink: true,
        sortKey: (k) => ({ active: 0, expired: 1, revoked: 2 })[stateOf(k)],
        cell: (k) => {
          const s = stateOf(k);
          if (s === "revoked") return pill("Revoked", "bad");
          if (s === "expired") return pill("Expired", "warn");
          return pill("Active", "good");
        },
      },
      {
        // The question anyone actually has about a key is whether revoking it
        // would break somebody. Created and expires do not answer that; this
        // does, and a key nothing has touched is called out rather than left as
        // a date to work out for yourself.
        label: "Last used",
        shrink: true,
        sortKey: (k) =>
          k.last_used_at ? new Date(k.last_used_at).getTime() : null,
        sortDir: "desc",
        cell: (k) =>
          k.last_used_at
            ? h(
                "span",
                { class: "muted nowrap", title: k.last_used_at },
                ago(k.last_used_at),
              )
            : h(
                "span",
                { class: "faint nowrap" },
                stateOf(k) === "active" ? "never used" : "never",
              ),
      },
      {
        label: "This month",
        num: true,
        shrink: true,
        sortKey: (k) => k.requests || null,
        sortDir: "desc",
        cell: (k) =>
          k.requests
            ? h(
                "div",
                { class: "stack" },
                h("span", { class: "nowrap" }, num(k.requests), " req"),
                h(
                  "span",
                  { class: "faint nowrap", style: { fontSize: "11.5px" } },
                  money(k.spend_micros, currency),
                ),
              )
            : h("span", { class: "faint" }, "-"),
      },
      {
        label: "Limits",
        shrink: true,
        cell: (k) => {
          const bits = summarise(k.limits || {});
          if (k.limits && k.limits.budget_micros) {
            bits.push(
              `${money(k.limits.budget_micros, currency)}/${k.limits.budget_period || "month"}`,
            );
          }
          if (k.limits && k.limits.allowed_models) {
            bits.unshift(`${k.limits.allowed_models.length} model`);
          }
          return bits.length
            ? h(
                "span",
                { class: "muted nowrap", style: { fontSize: "11.5px" } },
                bits.join(" · "),
              )
            : h("span", { class: "faint" }, "team's");
        },
      },
      {
        label: "Expires",
        shrink: true,
        sortKey: (k) =>
          k.expires_at ? new Date(k.expires_at).getTime() : null,
        cell: (k) =>
          k.expires_at
            ? h("span", { class: "muted nowrap" }, date(k.expires_at))
            : h("span", { class: "faint" }, "never"),
      },
      {
        // A key's limits are worth reading whatever state it is in; only
        // revoking one is about a key that is still working.
        label: "",
        shrink: true,
        cell: (k) =>
          h(
            "div",
            { class: "row-tight" },
            h(
              "button",
              {
                class: "btn btn-sm",
                title: "Limits for this key",
                onClick: () =>
                  openKey(ctx, k, models, teamName[k.team_id], canEdit),
              },
              "Guardrails",
            ),
            canRevoke(ctx, k) && stateOf(k) === "active"
              ? h(
                  "button",
                  {
                    class: "btn btn-sm btn-danger",
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
      },
    ],
    keys,
    {
      emptyTitle: "No keys yet",
      emptyBody:
        "Issue one to get started. Usage and the audit log are tracked per key.",
      // This list grows with however finely the deployment slices its keys, so it
      // can get long. Searching it by alias is how anybody actually arrives at a
      // row, since that is what the alias is for.
      search: (k) => [k.alias, k.prefix, teamName[k.team_id] || ""].join(" "),
      searchLabel: "keys",
      // Off by default: a revoked key is nobody's working key any more, and the
      // list is read to find one that works. It stays one click away, because a
      // revoked key is still the answer to "what happened to mine". An expired
      // one is left in the list either way - it says so in its own row, and it
      // is not what this box is named after.
      toggles: [
        {
          label: "Show revoked",
          hidden: (k) => stateOf(k) === "revoked",
        },
      ],
    },
  );

  return h("div", {}, head, rows);
}

// canRevoke reports whether the reader may revoke this key. An administrator
// revokes any of their organisation's; a member revokes one attributed to
// themselves, on the same screen they issued it on. A key attributed to nobody
// is the shared one an administrator issued for a pipeline, so it is nobody's to
// take away but theirs - which is why the id has to be there and match, rather
// than merely not belong to somebody else.
export function canRevoke(ctx, k) {
  if (ctx.state.me.unrestricted || ctx.state.me.role === "admin") return true;
  return (
    !!ctx.state.me.can_revoke_own_key &&
    !!k.user_id &&
    k.user_id === ctx.state.me.user_id
  );
}

// revokeBody says what revoking this particular key would actually break, which
// is a different sentence for a key in daily use and one nobody has ever used.
export function revokeBody(k) {
  const base =
    `Clients using ${k.alias} stop working within a second. ` +
    "You cannot undo this. Issue a new key instead.";
  if (!k.last_used_at) return base + " This key has never been used.";
  return base;
}

function stateOf(k) {
  if (k.revoked_at) return "revoked";
  if (k.expires_at && new Date(k.expires_at) < new Date()) return "expired";
  return "active";
}

// openKey opens this one key's limits with its team's and its organisation's
// loaded as the ceiling above them.
//
// A member gets the same dialog with nothing to fill in. These limits are the
// answer to "why was my editor refused", and a key nobody may read is a key
// whose refusals are a message to an administrator.
async function openKey(ctx, key, models, teamName, canEdit) {
  const ceilings = await ceilingsFor("key", {
    orgID: key.org_id,
    orgName:
      ctx.state.me.org_name ||
      (ctx.state.orgs.find((o) => o.id === key.org_id) || {}).name,
    teamID: key.team_id,
    teamName,
  });
  await openGuardrails(ctx, {
    scope: "key",
    id: key.id,
    name: key.alias,
    models,
    orgID: key.org_id,
    ceilings,
    canEdit,
  });
}

// The expiries a key can be issued with, longest-lived last.
const EXPIRIES = [
  { value: "720h", label: "30 days", hours: 720 },
  { value: "2160h", label: "90 days", hours: 2160 },
  { value: "8760h", label: "1 year", hours: 8760 },
  { value: "", label: "Never" },
];

function issueKey(ctx, teams, models, users, clients) {
  const alias = h("input", {
    class: "input",
    placeholder: "my-secret-key",
    autofocus: true,
  });
  const team = h(
    "select",
    { class: "select" },
    h("option", { value: "" }, "No team - organisation-wide"),
    teams.map((t) => h("option", { value: t.id }, t.name)),
  );
  const person = h(
    "select",
    { class: "select" },
    h("option", { value: "" }, "Nobody in particular"),
    users.map((u) => h("option", { value: u.id }, u.email)),
  );
  // Each span carries the day it lands on: "90 days" is not a date anybody can
  // hold in their head, and the choice is easier to make beside the calendar.
  const expiry = h(
    "select",
    { class: "select" },
    EXPIRIES.map(({ value, label, hours }) =>
      h(
        "option",
        { value, selected: value === "2160h" },
        hours
          ? `${label} (${date(new Date(Date.now() + hours * 3600e3))})`
          : label,
      ),
    ),
  );
  const err = h("div");

  modal({
    title: "Issue an API key",
    body: h(
      "form",
      {},
      err,
      h("div", { class: "field" }, h("label", {}, "Alias"), alias),
      h(
        "div",
        { class: "field" },
        h("label", {}, "Team"),
        team,
        h(
          "div",
          { class: "hint" },
          "The team's guardrails apply to this key. Without a team, only " +
            "the organisation's apply.",
        ),
      ),
      users.length
        ? h(
            "div",
            { class: "field" },
            h("label", {}, "Person"),
            person,
            h(
              "div",
              { class: "hint" },
              "The key shows on their ",
              h("strong", {}, "My access"),
              " screen, and its usage counts as theirs.",
            ),
          )
        : null,
      h("div", { class: "field" }, h("label", {}, "Expires"), expiry),
    ),
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Cancel"),
      h(
        "button",
        {
          class: "btn btn-primary",
          onClick: async (e) => {
            if (!alias.value.trim()) return alias.focus();
            const button = e.currentTarget;
            button.disabled = true;
            try {
              const created = await api.createKey({
                org_id: ctx.orgID || ctx.state.me.org_id,
                team_id: team.value,
                user_id: person.value,
                alias: alias.value.trim(),
                expires_in: expiry.value,
              });
              close();
              showSecret(ctx, created, models, clients);
            } catch (ex) {
              showError(err, ex.message);
              button.disabled = false;
            }
          },
        },
        "Issue key",
      ),
    ],
  });
}

// issueOwnKey is the member's version: the same secret and the same
// hand-over, without the two fields they have no say in.
//
// It exists so that a developer's first key does not have to travel from an
// administrator to them through a chat message - the one moment the key is
// readable is the moment it is on the machine that will use it, and nobody
// else's screen ever has it.
function issueOwnKey(ctx, models, clients) {
  const who = (ctx.state.me.email || "").split("@")[0] || "my";
  const alias = h("input", {
    class: "input",
    placeholder: `${who}-work-laptop`,
    autofocus: true,
  });
  const expiry = h(
    "select",
    { class: "select" },
    h("option", { value: "720h" }, "30 days"),
    h("option", { value: "2160h", selected: true }, "90 days"),
    h("option", { value: "8760h" }, "1 year"),
    h("option", { value: "" }, "Never"),
  );
  const err = h("div");

  modal({
    title: "Issue your own API key",
    body: h(
      "form",
      {},
      err,
      h("div", { class: "field" }, h("label", {}, "Alias"), alias),
      h(
        "div",
        { class: "field" },
        h("label", {}, "Person"),
        h(
          "div",
          { class: "input", style: { background: "transparent" } },
          ctx.state.me.email || ctx.state.me.user_id,
        ),
        h(
          "div",
          { class: "hint" },
          "Keys you issue are always yours. Only an administrator can " +
            "issue keys for others.",
        ),
      ),
      h("div", { class: "field" }, h("label", {}, "Expires"), expiry),
    ),
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Cancel"),
      h(
        "button",
        {
          class: "btn btn-primary",
          onClick: async (e) => {
            if (!alias.value.trim()) return alias.focus();
            const button = e.currentTarget;
            button.disabled = true;
            try {
              // Neither the person nor the team is sent: the control plane
              // attributes the key to the caller and refuses a team from anybody
              // who is not an administrator, and a field the panel cannot honour
              // is worse than one it does not draw.
              const created = await api.createKey({
                org_id: ctx.orgID || ctx.state.me.org_id,
                alias: alias.value.trim(),
                expires_in: expiry.value,
              });
              close();
              showSecret(ctx, created, models, clients);
            } catch (ex) {
              showError(err, ex.message);
              button.disabled = false;
            }
          },
        },
        "Issue key",
      ),
    ],
  });
}

// showSecret is the only moment the key exists outside the developer's machine,
// so it is also the only useful moment to hand over the configuration that
// contains it. Sending somebody to Connect a client afterwards sends them to a
// screen where the key is gone forever and the file has a blank in it.
function showSecret(ctx, created, models, clients) {
  const chat = models.filter((m) => m.kind === "chat");
  const base = defaultBase(ctx) || location.origin + "/api";

  const secret = h("input", {
    class: "input key-value mono",
    readonly: true,
    value: created.key,
    "aria-label": "The issued API key",
    // Selecting on focus makes the manual path one click, which is the path
    // taken whenever the clipboard is unavailable - a panel served over plain
    // http on anything but localhost has no clipboard at all.
    onFocus: (e) => e.target.select(),
    onClick: (e) => e.target.select(),
  });

  const body = h(
    "div",
    {},
    h(
      "div",
      { class: "field" },
      h("label", {}, "The key"),
      h(
        "div",
        { class: "row-tight" },
        secret,
        h(
          "button",
          { class: "btn", onClick: () => copyText(created.key) },
          icon(icons.copy),
          "Copy",
        ),
      ),
      h(
        "div",
        { class: "hint" },
        "Set it as ",
        h("code", {}, "KEERA_API_KEY"),
        " in the developer's shell profile.",
      ),
    ),
  );

  if (chat.length && clients.length) {
    const model = h(
      "select",
      { class: "select" },
      chat.map((m) => h("option", { value: m.alias }, m.alias)),
    );
    const client = h(
      "select",
      { class: "select" },
      clients.map((c) => h("option", { value: c.key }, c.label)),
    );
    const out = h("div");

    const draw = () => {
      const c = clients.find((x) => x.key === client.value) || clients[0];
      const m = chat.find((x) => x.alias === model.value) || chat[0];
      // The key is substituted in rather than left as an environment
      // reference: this is being handed over with the secret, once.
      const config = c
        .config(base, m)
        .replaceAll(
          /\{env:KEERA_API_KEY\}|\$\{KEERA_API_KEY\}|\$KEERA_API_KEY/g,
          created.key,
        );
      // Filtered, because this is replaceChildren and not h: a client with no
      // note would otherwise put the string "null" under its config block.
      out.replaceChildren(
        ...[
          c.path
            ? h(
                "p",
                { class: "muted" },
                "Save this as ",
                h("code", {}, c.path),
                ". If the file exists, merge these settings into it. Then run ",
                h("code", {}, c.key),
                " and pick ",
                h("strong", {}, title(m.alias)),
                ".",
              )
            : h(
                "p",
                { class: "muted" },
                "Add these to the developer's shell profile. They set ",
                h("strong", {}, m.alias),
                " as the model.",
              ),
          code(config),
          c.note ? h("p", { class: "muted" }, c.note(m)) : null,
        ].filter(Boolean),
      );
    };
    model.addEventListener("change", draw);
    client.addEventListener("change", draw);
    draw();

    body.append(
      h(
        "div",
        { class: "field-row" },
        h(
          "div",
          { class: "field", style: { marginBottom: 0 } },
          h("label", {}, "Client"),
          client,
        ),
        h(
          "div",
          { class: "field", style: { marginBottom: 0 } },
          h("label", {}, "Model"),
          model,
        ),
      ),
      h("div", { style: { marginTop: "12px" } }, out),
      h(
        "div",
        { class: "hint", style: { marginTop: "12px" } },
        "This contains the key in plain text. For a file without it, use ",
        h("strong", {}, "Connect a client"),
        ", which reads it from the environment.",
      ),
    );
  }

  modal({
    wide: true,
    title: "Key issued",
    subtitle: "Shown only this once. It is not stored.",
    body,
    actions: (close) => [
      h(
        "button",
        {
          class: "btn btn-primary",
          onClick: () => {
            close();
            ctx.reload();
          },
        },
        "Done",
      ),
    ],
  });
}
