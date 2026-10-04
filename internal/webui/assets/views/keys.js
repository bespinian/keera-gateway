// API keys - issuing one, seeing what is live, renaming one, and taking one
// away.

import { api, chatTargets } from "../api.js";
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
  isAdmin,
  field,
} from "../ui.js";
import { openGuardrails, ceilingsFor, summarise } from "./guardrails.js";
import { chooseOrg, orgNameOf } from "./orgs.js";
import { oldestProject } from "./projects.js";
import { loadClients, code, defaultBase, keyModels } from "./connect.js";

export async function keysView(ctx) {
  // Keys, models and people all belong to one organisation, so an operator
  // looking at every organisation picks one before anything is read.
  if (!ctx.orgID && ctx.state.me.unrestricted)
    return chooseOrg(ctx, "API keys");

  const [keysRes, projectsRes, modelsRes, usersRes, clients] =
    await Promise.all([
      api.keys(ctx.orgID),
      api.projects(ctx.orgID).catch(() => ({ data: [] })),
      api.models(ctx.orgID).catch(() => ({ data: [] })),
      // Who a key can be attributed to. A deployment with no identity provider
      // has nobody here, and the field is left out rather than shown empty.
      api.users(ctx.orgID).catch(() => ({ data: [] })),
      // The client catalogue, so that the configuration handed over with a key
      // is the same one Connect a client hands out.
      loadClients().catch(() => []),
    ]);
  const keys = keysRes.data || [];
  const projects = projectsRes.data || [];
  const users = usersRes.data || [];
  const models = (modelsRes.data || []).filter((m) => m.enabled !== false);
  const projectNames = Object.fromEntries(projects.map((t) => [t.id, t.name]));
  const canEdit = isAdmin(ctx);
  // Only an administrator issues keys, so each has the project and guardrails
  // they chose. A member renames, rotates and revokes their own.
  const canRotateOwn = !canEdit && ctx.state.me.can_manage_own_keys;
  const currency = keysRes.currency || ctx.currency;

  const live = keys.filter((k) => stateOf(k) === "active").length;
  ctx.setSubtitle(`${live} active of ${keys.length}`);

  const head = h(
    "div",
    { class: "row", style: { marginBottom: "16px" } },
    h(
      "div",
      { class: "muted" },
      canEdit
        ? "A key is shown only once, when it is issued. It is not stored."
        : canRotateOwn
          ? "A key is shown only once, when it is issued. An administrator " +
            "issues keys and sets their guardrails. You can rename, rotate or " +
            "revoke your own."
          : "A key is shown only once, when it is issued. Open a key to see " +
            "its guardrails. An administrator sets them.",
    ),
    h("div", { style: { flex: 1 } }),
    canEdit
      ? h(
          "button",
          {
            class: "btn btn-primary",
            onClick: () => issueKey(ctx, projects, users, clients),
          },
          icon(icons.plus),
          "Issue key",
        )
      : null,
  );

  const rows = table(
    [
      {
        // The name opens the key's own screen. "Can this be revoked" is answered
        // by the columns here; "what has it actually been doing" is one request
        // at a time, and that is a screen rather than a cell.
        label: "Name",
        sortKey: (k) => k.name,
        cell: (k) =>
          h(
            "div",
            { class: "stack" },
            rowLink(
              ctx,
              "/keys/" + encodeURIComponent(k.id),
              k.name,
              "This key's traffic, spend and requests",
            ),
            h(
              "span",
              { class: "faint mono", style: { fontSize: "11px" } },
              k.prefix + "…",
            ),
            k.kind === "subscription"
              ? h(
                  "span",
                  {
                    title:
                      "For Claude Code signed in to a Claude plan. It reaches " +
                      "only the models that plan pays for.",
                  },
                  pill("Claude plan"),
                )
              : null,
          ),
      },
      {
        label: "Project",
        // Only a revoked key can have none: its project was deleted.
        sortKey: (k) =>
          k.project_id ? projectNames[k.project_id] || k.project_id : null,
        cell: (k) =>
          k.project_id
            ? h("span", {}, projectNames[k.project_id] || k.project_id)
            : h("span", { class: "faint" }, "deleted project"),
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
            : h("span", { class: "faint nowrap" }, "not this month"),
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
                k.subscription_micros
                  ? h(
                      "span",
                      {
                        class: "faint nowrap",
                        style: { fontSize: "11.5px" },
                        title:
                          "What the Claude plan paid for, at API prices. " +
                          "No budget is charged it.",
                      },
                      `${money(k.subscription_micros, currency)} on the plan`,
                    )
                  : null,
                planUsed(k.plan),
              )
            : h("span", { class: "faint" }, "-"),
      },
      {
        label: "Guardrails",
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
            : h(
                "span",
                { class: "faint" },
                k.project_id ? "project's" : `${orgNameOf(ctx, k.org_id)}'s`,
              );
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
                title: "Guardrails for this key",
                onClick: () =>
                  openKey(ctx, k, models, projectNames[k.project_id], canEdit),
              },
              "Guardrails",
            ),
            // Any key can be renamed, revoked ones too: their name still
            // heads their usage history.
            canRevoke(ctx, k)
              ? h(
                  "button",
                  {
                    class: "btn btn-sm",
                    title: "Rename this key",
                    "aria-label": `Rename ${k.name}`,
                    onClick: () => renameKey(ctx, k),
                  },
                  icon(icons.pencil),
                )
              : null,
            // An expired key can be rotated too. A member cannot issue a new
            // one, so this is how they get going again.
            canRevoke(ctx, k) && stateOf(k) !== "revoked"
              ? h(
                  "button",
                  {
                    class: "btn btn-sm",
                    title: "Replace this key with a new one",
                    onClick: () =>
                      confirm({
                        title: "Rotate this key?",
                        body:
                          `A new key replaces ${k.name}, with the same project, ` +
                          "person and guardrails. The old key stops working " +
                          "at once, so update its clients with the new one.",
                        confirmLabel: "Rotate",
                        onConfirm: async () => {
                          const created = await api.rotateKey(k.id);
                          showSecret(ctx, created, clients);
                        },
                      }),
                  },
                  "Rotate",
                )
              : null,
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
      emptyBody: canEdit
        ? "Issue one to get started. Usage and the audit log are tracked per key."
        : "An administrator issues keys. Usage and the audit log are tracked per key.",
      // This list grows with however finely the deployment slices its keys, so it
      // can get long. Searching it by name is how anybody actually arrives at a
      // row, since that is what the name is for.
      search: (k) =>
        [k.name, k.prefix, projectNames[k.project_id] || ""].join(" "),
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

// canRevoke reports whether the reader may rename, revoke or rotate this key. An administrator
// revokes any of their organisation's; a member revokes one attributed to
// themselves. A key attributed to nobody
// is the shared one an administrator issued for a pipeline, so it is nobody's to
// take away but theirs - which is why the id has to be there and match, rather
// than merely not belong to somebody else.
export function canRevoke(ctx, k) {
  if (isAdmin(ctx)) return true;
  return (
    !!ctx.state.me.can_manage_own_keys &&
    !!k.user_id &&
    k.user_id === ctx.state.me.user_id
  );
}

// revokeBody says what revoking this particular key would actually break, which
// is a different sentence for a key in daily use and one nobody has ever used.
export function revokeBody(k) {
  const base =
    `Clients using ${k.name} stop working within a second. ` +
    "You cannot undo this. To replace the key instead, rotate it.";
  if (!k.last_used_at) return base + " This key has not been used this month.";
  return base;
}

export function stateOf(k) {
  if (k.revoked_at) return "revoked";
  if (k.expires_at && new Date(k.expires_at) < new Date()) return "expired";
  return "active";
}

// openKey opens this one key's limits with its project's and its organisation's
// loaded as the ceiling above them.
//
// A member gets the same dialog with nothing to fill in. These limits are the
// answer to "why was my editor refused", and a key nobody may read is a key
// whose refusals are a message to an administrator.
export async function openKey(ctx, key, models, projectName, canEdit) {
  const ceilings = await ceilingsFor("key", {
    orgID: key.org_id,
    orgName: orgNameOf(ctx, key.org_id),
    projectID: key.project_id,
    projectName,
  });
  await openGuardrails(ctx, {
    scope: "key",
    id: key.id,
    name: key.name,
    models,
    orgID: key.org_id,
    ceilings,
    canEdit,
  });
}

// renameKey changes what a key is called and nothing else.
function renameKey(ctx, k) {
  const name = h("input", { class: "input", value: k.name, autofocus: true });
  const err = h("div");
  modal({
    title: `Rename ${k.name}`,
    subtitle:
      "The key keeps working as it is. The old name stays in the audit log.",
    body: h(
      "form",
      { onSubmit: (e) => e.preventDefault() },
      err,
      field("Name", name),
    ),
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Cancel"),
      h(
        "button",
        {
          class: "btn btn-primary",
          onClick: async (e) => {
            const next = name.value.trim();
            if (!next) return name.focus();
            if (next === k.name) return close();
            e.target.disabled = true;
            try {
              await api.renameKey(k.id, next);
              close();
              toast("Key renamed", "good");
              ctx.reload();
            } catch (ex) {
              showError(err, ex.message);
              e.target.disabled = false;
            }
          },
        },
        "Rename",
      ),
    ],
  });
}

// The expiries a key can be issued with, longest-lived last.
const EXPIRIES = [
  { value: "720h", label: "30 days", hours: 720 },
  { value: "2160h", label: "90 days", hours: 2160 },
  { value: "8760h", label: "1 year", hours: 8760 },
  { value: "", label: "Never" },
];

function issueKey(ctx, projects, users, clients) {
  // Every key is in a project, so with none there is nothing to issue yet.
  if (!projects.length) {
    const close = modal({
      title: "Create a project first",
      body: h(
        "div",
        { class: "muted" },
        "Every key is in a project, and this organisation has none.",
      ),
      actions: (dismiss) => [
        h("button", { class: "btn", onClick: dismiss }, "Cancel"),
        h(
          "button",
          {
            class: "btn btn-primary",
            onClick: () => {
              close();
              ctx.navigate("/projects");
            },
          },
          "Go to Projects",
        ),
      ],
    });
    return;
  }
  const name = h("input", {
    class: "input",
    placeholder: "alice-laptop",
    autofocus: true,
  });
  // The oldest project is picked, as the control plane does for a key that
  // names none.
  const first = oldestProject(projects);
  const project = h(
    "select",
    { class: "select" },
    projects.map((t) =>
      h("option", { value: t.id, selected: t.id === first }, t.name),
    ),
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
      field(
        "Name",
        name,
        "What the key is for. Reports and the audit log show it.",
      ),
      field(
        "Project",
        project,
        "The project's guardrails apply to this key, inside the " +
          "organisation's.",
      ),
      users.length
        ? field(
            "User",
            person,
            h(
              "span",
              {},
              "The key shows on their ",
              h("strong", {}, "My access"),
              " screen, and its usage counts as theirs.",
            ),
          )
        : null,
      field("Expires", expiry),
    ),
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Cancel"),
      h(
        "button",
        {
          class: "btn btn-primary",
          onClick: async (e) => {
            if (!name.value.trim()) return name.focus();
            const button = e.currentTarget;
            button.disabled = true;
            try {
              const created = await api.createKey({
                org_id: ctx.orgID || ctx.state.me.org_id,
                project_id: project.value,
                user_id: person.value,
                name: name.value.trim(),
                expires_in: expiry.value,
              });
              close();
              showSecret(ctx, created, clients);
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

/** issuedModels is the chat models and routers a key may call. An allow-list
 *  of null means every one. */
async function issuedModels(created) {
  const [targets, effective] = await Promise.all([
    chatTargets(created.org_id),
    api.effectiveGuardrails("key", created.id),
  ]);
  return effective.allowed_models == null
    ? targets
    : keyModels(targets, effective.allowed_models);
}

// showSecret is the only moment the key exists outside the developer's machine,
// so it is also the only useful moment to hand over the configuration that
// contains it. Sending somebody to Connect a client afterwards sends them to a
// screen where the key is gone forever and the file has a blank in it.
async function showSecret(ctx, created, clients) {
  // Read before the dialog opens. Failing to read them still shows the key,
  // which is what matters here.
  const chat =
    created.kind === "subscription"
      ? []
      : await issuedModels(created).catch(() => []);
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
    const client = h(
      "select",
      { class: "select" },
      clients.map((c) => h("option", { value: c.key }, c.label)),
    );
    const out = h("div");

    const draw = () => {
      const c = clients.find((x) => x.key === client.value) || clients[0];
      // The key is substituted in rather than left as an environment
      // reference: this is being handed over with the secret, once.
      const config = c
        .config(base, chat)
        .replaceAll(
          /\{env:KEERA_API_KEY\}|\$\{KEERA_API_KEY\}|\$KEERA_API_KEY/g,
          created.key,
        );
      // Filtered, because this is replaceChildren and not h: a client with no
      // note would otherwise put the string "null" under its config block.
      out.replaceChildren(
        ...[
          h(
            "p",
            { class: "muted" },
            c.path
              ? [
                  "Save this as ",
                  h("code", {}, c.path),
                  ". If the file exists, merge these settings into it. ",
                ]
              : "Add these to the developer's shell profile. ",
            "It sets up every model this key can use: ",
            chat.map((m) => m.alias).join(", "),
            ".",
          ),
          code(config),
          c.note ? h("p", { class: "muted" }, c.note()) : null,
        ].filter(Boolean),
      );
    };
    client.addEventListener("change", draw);
    draw();

    body.append(
      h(
        "div",
        { class: "field", style: { marginBottom: 0 } },
        h("label", {}, "Client"),
        client,
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

// planUsed is how much of their Claude plan a subscription key's holder has
// used, in the plan's two windows, as Anthropic last reported it.
function planUsed(plan) {
  if (!plan) return null;
  const parts = [];
  if (plan.five_hour != null)
    parts.push(`5h ${Math.round(plan.five_hour * 100)}%`);
  if (plan.seven_day != null)
    parts.push(`7d ${Math.round(plan.seven_day * 100)}%`);
  if (!parts.length) return null;
  const limited = plan.status === "rejected";
  return h(
    "span",
    {
      class: (limited ? "" : "faint ") + "nowrap",
      style: { fontSize: "11.5px" },
      title:
        "How much of the Claude plan is used: in the last five hours, and " +
        "in the last seven days.",
    },
    "plan " + parts.join(" · "),
    limited ? h("span", {}, " ", pill("limit reached", "warn")) : null,
  );
}
