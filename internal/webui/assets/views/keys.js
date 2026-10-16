// API keys - issuing one, seeing what is live, renaming one, and taking one
// away.

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
  dateTime,
  money,
  num,
  icons,
  iconButton,
  rowLink,
  isAdmin,
  field,
  secretField,
  currentRange,
  rangePicker,
  RANGES,
  listHead,
  plural,
  pct,
  dash,
  formModal,
  rangeOf,
} from "../ui.js";
import { openGuardrails, ceilingsFor, summarise } from "./guardrails.js";
import { orgNameOf } from "./orgs.js";
import { oldestProject } from "./projects.js";
import { openConnectClient } from "./connect.js";

export async function keysView(ctx) {
  const since = currentRange();
  const range = rangeOf(since, RANGES[1]);
  const [keysRes, projectsRes, modelsRes, usersRes] = await Promise.all([
    api.keys(ctx.orgID, { since }),
    api.projects(ctx.orgID).catch(() => ({ data: [] })),
    api.models(ctx.orgID).catch(() => ({ data: [] })),
    // Who a key can be attributed to. A deployment with no identity provider
    // has nobody here, and the field is left out rather than shown empty.
    api.users(ctx.orgID).catch(() => ({ data: [] })),
  ]);
  const keys = keysRes.data || [];
  const projects = projectsRes.data || [];
  const users = usersRes.data || [];
  const models = (modelsRes.data || []).filter((m) => m.enabled !== false);
  const projectNames = Object.fromEntries(projects.map((t) => [t.id, t.name]));
  const userEmails = Object.fromEntries(users.map((u) => [u.id, u.email]));
  const canEdit = isAdmin(ctx);
  // Only an administrator issues keys, so each has the project and guardrails
  // they chose. A member renames, rotates and revokes their own.

  const live = keys.filter((k) => stateOf(k) === "active").length;
  ctx.setSubtitle(`${live} active of ${keys.length}`);

  const head = listHead(
    canEdit
      ? null
      : "A key is shown only once, when it is issued. An administrator " +
          "issues keys and sets their guardrails. You can rename, rotate or " +
          "revoke your own.",
    rangePicker(ctx, since),
    canEdit
      ? {
          label: "Issue key",
          // A disabled person cannot be given a key, so they are not offered.
          onClick: () =>
            issueKey(ctx, projects, users.filter((u) => !u.disabled_at)),
        }
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
              "This key's traffic, cost and requests",
            ),
            h(
              "span",
              { class: "faint mono", style: { fontSize: "11px" } },
              prefixOf(k),
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
            : h("span", { class: "faint" }, "Deleted project"),
      },
      // A deployment with no identity provider has nobody to attribute a key
      // to, so the column would only ever say so.
      users.length
        ? {
            label: "User",
            sortKey: (k) =>
              k.user_id ? userEmails[k.user_id] || k.user_id : null,
            cell: (k) => userCell(ctx, k, userEmails),
          }
        : null,
      {
        // Active first, because a list of keys is read to find the live ones.
        label: "Status",
        shrink: true,
        sortKey: (k) =>
          ({ active: 0, pending: 1, expired: 2, revoked: 3 })[stateOf(k)],
        cell: (k) => statePill(stateOf(k)),
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
                { class: "muted nowrap", title: dateTime(k.last_used_at) },
                ago(k.last_used_at),
              )
            : h("span", { class: "faint nowrap" }, `not ${inPeriod(since)}`),
      },
      {
        label: range.long,
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
                  { class: "faint nowrap small" },
                  money(k.spend_micros),
                ),
                k.subscription_micros
                  ? h(
                      "span",
                      {
                        class: "faint nowrap small",
                        title:
                          "What the Claude plan paid for, at API prices. " +
                          "No budget is charged it.",
                      },
                      `${money(k.subscription_micros)} on the plan`,
                    )
                  : null,
                planUsed(k.plan),
              )
            : dash(),
      },
      {
        label: "Guardrails",
        shrink: true,
        cell: (k) => {
          const bits = summarise(k.limits || {});
          if (k.limits && k.limits.budget_micros) {
            bits.push(
              `${money(k.limits.budget_micros)}/${k.limits.budget_period || "month"}`,
            );
          }
          if (k.limits && k.limits.allowed_models) {
            const n = k.limits.allowed_models.length;
            bits.unshift(n ? `${n} ${plural(n, "model")}` : "No models");
          }
          return bits.length
            ? h("span", { class: "muted nowrap small" }, bits.join(" · "))
            : h("span", { class: "faint" }, "Inherited");
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
              ? iconButton(
                  icons.pencil,
                  "Rename this key",
                  () => renameKey(ctx, k),
                  { label: `Rename ${k.name}` },
                )
              : null,
            canActivate(ctx, k) && stateOf(k) === "pending"
              ? activateButton(ctx, k, "btn btn-sm")
              : null,
            // An expired key can be rotated too. A member cannot issue a new
            // one, so this is how they get going again.
            canRevoke(ctx, k) && canRotate(k)
              ? h(
                  "button",
                  {
                    class: "btn btn-sm",
                    title: "Replace this key with a new one",
                    onClick: () => confirmRotate(ctx, k),
                  },
                  "Rotate",
                )
              : null,
            canRevoke(ctx, k) && isLive(k)
              ? h(
                  "button",
                  {
                    class: "btn btn-sm btn-danger",
                    onClick: () =>
                      confirmRevoke(k, ctx.reload, inPeriod(since)),
                  },
                  "Revoke",
                )
              : null,
          ),
      },
    ].filter(Boolean),
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
        [
          k.name,
          k.prefix,
          projectNames[k.project_id] || "",
          userEmails[k.user_id] || "",
        ].join(" "),
      searchLabel: "keys",
      choices: [
        {
          label: "Projects",
          options: byLabel(
            keys.filter((k) => k.project_id),
            (k) => k.project_id,
            (k) => projectNames[k.project_id] || k.project_id,
          ),
          test: (k, v) => k.project_id === v,
        },
        {
          label: "Users",
          options: [
            ...byLabel(
              keys.filter((k) => k.user_id),
              (k) => k.user_id,
              (k) => userEmails[k.user_id] || k.user_id,
            ),
            // The shared keys an administrator issued for a pipeline.
            ...(keys.some((k) => !k.user_id)
              ? [{ value: NOBODY, label: NO_USER }]
              : []),
          ],
          test: (k, v) => (k.user_id || NOBODY) === v,
        },
        // A dropdown with one choice filters nothing.
      ].filter((c) => c.options.length > 1),
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

// userCell is whose key k is. Only an administrator has a user's page to open.
function userCell(ctx, k, userEmails) {
  if (!k.user_id) return h("span", { class: "faint" }, NO_USER);
  const email = userEmails[k.user_id] || k.user_id;
  return isAdmin(ctx)
    ? rowLink(
        ctx,
        "/users/" + encodeURIComponent(k.user_id),
        email,
        "This user's keys and usage",
      )
    : h("span", {}, email);
}

// NOBODY is the user filter's value for a key attributed to nobody. No user id
// looks like it.
const NOBODY = "-";

// NO_USER is what every screen shows for a key attributed to nobody.
export const NO_USER = "No user";

// inPeriod is the period last_used_at covers when the keys were read with
// since, worded to follow "used" or "not".
export function inPeriod(since) {
  return `in the ${rangeOf(since, RANGES[1]).long.toLowerCase()}`;
}

// byLabel is the distinct values of rows as dropdown options, sorted by label.
function byLabel(rows, value, label) {
  const seen = new Map();
  for (const r of rows) seen.set(value(r), label(r));
  return [...seen]
    .map(([v, l]) => ({ value: v, label: l }))
    .sort((a, b) => a.label.localeCompare(b.label));
}

// canRevoke reports whether the reader may rename, revoke or rotate this key. An administrator
// revokes any of their organisation's; a member revokes one attributed to
// themselves. A key attributed to nobody
// is the shared one an administrator issued for a pipeline, so it is nobody's to
// take away but theirs - which is why the id has to be there and match, rather
// than merely not belong to somebody else.
export function canRevoke(ctx, k) {
  if (isAdmin(ctx)) return true;
  return !!k.user_id && k.user_id === ctx.state.me.user_id;
}

// revokeBody says what revoking this particular key would actually break, which
// is a different sentence for a key in daily use and one nobody has used.
// period is the span last_used_at covers, which depends on how the keys were
// read: My access reads this month.
function revokeBody(k, period = "this month") {
  const base =
    `Clients using ${k.name} stop working within a second. ` +
    "You cannot undo this. To replace the key instead, rotate it.";
  if (!k.last_used_at) return base + ` This key has not been used ${period}.`;
  return base;
}

// confirmRevoke asks before revoking a key, then runs done. Every screen that
// shows a live key offers this, and it should read the same on each.
export function confirmRevoke(k, done, period) {
  confirm({
    title: k.name ? `Revoke ${k.name}?` : "Revoke this key?",
    body: revokeBody(k, period),
    confirmLabel: "Revoke",
    danger: true,
    onConfirm: async () => {
      await api.revokeKey(k.id);
      toast("Key revoked", "good");
      done();
    },
  });
}

// statePill is a key's state as the panel shows it everywhere.
export function statePill(state) {
  if (state === "revoked") return pill("Revoked", "bad");
  if (state === "expired") return pill("Expired", "warn");
  if (state === "pending") return pill("Not activated", "warn");
  return pill("Active", "good");
}

// stateOf is the same word store.KeyInfo.State gives on the server.
export function stateOf(k) {
  if (k.revoked_at) return "revoked";
  if (k.expires_at && new Date(k.expires_at) < new Date()) return "expired";
  if (k.pending) return "pending";
  return "active";
}

// isLive is a key that works, or will once it is activated: what revoking
// takes away.
export function isLive(k) {
  const state = stateOf(k);
  return state === "active" || state === "pending";
}

// canRotate is whether a key has a secret to replace. A pending key has none
// yet, so it is activated instead.
export function canRotate(k) {
  const state = stateOf(k);
  return state !== "revoked" && state !== "pending";
}

// prefixOf is how a key is shown where its secret would be.
export function prefixOf(k) {
  return k.pending ? "no secret yet" : k.prefix + "…";
}

// canActivate reports whether the reader may activate this pending key: the
// person it is for, or an administrator, who could rotate it anyway. A member
// who may not rotate their own keys can still activate one: it was left for
// them.
export function canActivate(ctx, k) {
  return isAdmin(ctx) || (!!k.user_id && k.user_id === ctx.state.me.user_id);
}

// activateKey gives a pending key its secret and shows it, once. button is
// held disabled meanwhile.
export async function activateKey(ctx, k, button) {
  button.disabled = true;
  try {
    const created = await api.activateKey(k.id);
    showSecret(ctx, created, null, "Key activated");
  } catch (ex) {
    toast(ex.message, "bad");
    button.disabled = false;
  }
}

// activateButton is the button that activates a pending key.
export function activateButton(ctx, k, cls = "btn") {
  return h(
    "button",
    {
      class: cls + " btn-primary",
      title: "Create this key's secret and see it once",
      onClick: (e) => activateKey(ctx, k, e.currentTarget),
    },
    "Activate",
  );
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

// confirmRotate asks before replacing a key with a new one, then shows the new
// secret. done gets the new key once it exists; a screen about the old key
// uses it to move to the new one.
export function confirmRotate(ctx, k, { done } = {}) {
  confirm({
    title: k.name ? `Rotate ${k.name}?` : "Rotate this key?",
    body:
      `A new key replaces ${k.name}, with the same project, user and ` +
      "guardrails. The old key stops working at once, so update its clients " +
      "with the new one.",
    confirmLabel: "Rotate",
    danger: true,
    onConfirm: async () => {
      const created = await api.rotateKey(k.id);
      if (done) done(created);
      showSecret(ctx, created);
    },
  });
}

// renameKey changes what a key is called and nothing else.
export function renameKey(ctx, k) {
  const name = h("input", { class: "input", value: k.name, autofocus: true });
  formModal({
    title: `Rename ${k.name}`,
    subtitle:
      "The key keeps working as it is. The old name stays in the audit log.",
    body: h(
      "form",
      { onSubmit: (e) => e.preventDefault() },
      field("Name", name),
    ),
    submitLabel: "Rename",
    done: "Key renamed",
    ctx,
    submit: async (close) => {
      const next = name.value.trim();
      if (!next) {
        name.focus();
        return false;
      }
      if (next === k.name) {
        close();
        return false;
      }
      await api.renameKey(k.id, next);
    },
  });
}

// The expiries a key can be issued with, longest-lived last. "date" asks for
// the last day the key works.
const EXPIRIES = [
  { value: "720h", label: "30 days", hours: 720 },
  { value: "2160h", label: "90 days", hours: 2160 },
  { value: "8760h", label: "1 year", hours: 8760 },
  { value: "date", label: "Custom date" },
  { value: "", label: "Never" },
];

// localDay is a day as a date input writes it, in local time.
function localDay(d) {
  const pad = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

// endOfDay is the last second of a YYYY-MM-DD day in local time, so a key
// picked to expire on a day still works all of that day.
function endOfDay(day) {
  const [y, m, d] = day.split("-").map(Number);
  return new Date(y, m - 1, d, 23, 59, 59);
}

// openIssueKey opens the form from another screen, which has not read the
// projects and people it offers. An operator looking at every organisation
// picks one on the keys screen first.
export async function openIssueKey(ctx) {
  if (!ctx.orgID && ctx.state.me.unrestricted) return ctx.navigate("/keys");
  const [projectsRes, usersRes] = await Promise.all([
    api.projects(ctx.orgID).catch(() => ({ data: [] })),
    api.users(ctx.orgID).catch(() => ({ data: [] })),
  ]);
  issueKey(
    ctx,
    projectsRes.data || [],
    (usersRes.data || []).filter((u) => !u.disabled_at),
  );
}

// issueKey asks for a new key. last holds the project and expiry of the key
// issued just before, when the form is opened again with "Issue another".
//
// The key is the reader's own unless they pick someone else, as `keera key
// create` does: a key that belongs to nobody shows on nobody's My access or My
// clients, so the person who issued it cannot set a client up with it. The
// user is not carried over to "Issue another": a key given to the wrong person
// quietly counts as theirs.
function issueKey(ctx, projects, users, last = {}) {
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
          "Go to projects",
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
  const first = projects.some((t) => t.id === last.project)
    ? last.project
    : oldestProject(projects);
  const project = h(
    "select",
    { class: "select" },
    projects.map((t) =>
      h("option", { value: t.id, selected: t.id === first }, t.name),
    ),
  );
  const me = ctx.state.me.user_id;
  const person = h(
    "select",
    { class: "select" },
    h("option", { value: "" }, NO_USER),
    users.map((u) =>
      h("option", { value: u.id, selected: u.id === me }, u.email),
    ),
  );
  // A key for somebody else is theirs to activate by default, so its secret
  // is only ever on their screen. The administrator can still take it now,
  // for a person who will be handed it some other way.
  const activation = h(
    "select",
    { class: "select" },
    h("option", { value: "them" }, "They activate it and see it once"),
    h("option", { value: "me" }, "I see it now, and hand it over"),
  );
  const activationField = field(
    "Secret",
    activation,
    "Until the key is activated it has no secret, and does not work.",
  );
  const forSomebodyElse = () => person.value !== "" && person.value !== me;
  activationField.hidden = !forSomebodyElse();
  person.addEventListener("change", () => {
    activationField.hidden = !forSomebodyElse();
  });
  // Each span carries the day it lands on: "90 days" is not a date anybody can
  // hold in their head, and the choice is easier to make beside the calendar.
  const expiry = h(
    "select",
    { class: "select" },
    EXPIRIES.map(({ value, label, hours }) =>
      h(
        "option",
        { value, selected: value === (last.expiry ?? "2160h") },
        hours
          ? `${label} (${date(new Date(Date.now() + hours * 3600e3))})`
          : label,
      ),
    ),
  );
  const today = localDay(new Date());
  const lastDay = h("input", {
    class: "input",
    type: "date",
    min: today,
    value:
      last.lastDay >= today
        ? last.lastDay
        : localDay(new Date(Date.now() + 2160 * 3600e3)),
  });
  const lastDayField = field(
    "Last day",
    lastDay,
    "The key works until the end of this day.",
  );
  lastDayField.hidden = expiry.value !== "date";
  expiry.addEventListener("change", () => {
    lastDayField.hidden = expiry.value !== "date";
  });
  formModal({
    title: "Issue key",
    body: h(
      "form",
      {},
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
      users.length ? activationField : null,
      field("Expires", expiry),
      lastDayField,
    ),
    submitLabel: "Issue key",
    submit: async (close) => {
      if (!name.value.trim()) {
        name.focus();
        return false;
      }
      let lifetime = { expires_in: expiry.value };
      if (expiry.value === "date") {
        if (!lastDay.value) {
          lastDay.focus();
          return false;
        }
        lifetime = { expires_at: endOfDay(lastDay.value) };
      }
      const created = await api.createKey({
        org_id: ctx.orgID || ctx.state.me.org_id,
        project_id: project.value,
        user_id: person.value,
        name: name.value.trim(),
        pending: forSomebodyElse() && activation.value === "them",
        ...lifetime,
      });
      close();
      const again = () =>
        issueKey(ctx, projects, users, {
          project: project.value,
          expiry: expiry.value,
          lastDay: lastDay.value,
        });
      if (created.pending) {
        const holder = users.find((u) => u.id === created.user_id);
        showPending(ctx, created, holder ? holder.email : "", again);
      } else {
        showSecret(ctx, created, again);
      }
    },
  });
}

// showSecret shows a key's secret, the only time it can be seen. again, when
// given, opens the form for another key like this one.
//
// The secret is never in the panel again, so a key of the reader's own is set
// up from here: Connect a client fills it into the setup it shows.
function showSecret(ctx, created, again, title = "Key issued") {
  const body = field("The key", secretField(created.key, "The API key"));
  const own =
    created.user_id &&
    created.user_id === ctx.state.me.user_id &&
    created.kind !== "subscription";

  modal({
    title,
    subtitle: "Shown only this once. It is not stored.",
    body,
    actions: (close) => [
      own
        ? h(
            "button",
            {
              class: "btn",
              title: "Set up a client with this key",
              onClick: (e) => {
                const button = e.currentTarget;
                openConnectClient(ctx, button, created).then((opened) => {
                  if (!opened) return;
                  close();
                  ctx.reload();
                });
              },
            },
            "Connect a client",
          )
        : null,
      again
        ? h(
            "button",
            {
              class: "btn",
              title: "Same project and expiry",
              onClick: () => {
                close();
                ctx.reload();
                again();
              },
            },
            "Issue another",
          )
        : null,
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

// showPending says what happens to a key left for its holder to activate.
function showPending(ctx, created, email, again) {
  modal({
    title: "Key issued",
    subtitle: "It has no secret yet, so nobody has seen it.",
    body: h(
      "p",
      { class: "muted" },
      email ? h("strong", {}, email) : "Its user",
      " activates it on their Dashboard or ",
      h("strong", {}, "My access"),
      ", or with ",
      h("code", {}, "keera connect claude-code"),
      ", and sees its secret once. Until then it does not work.",
    ),
    actions: (close) => [
      h(
        "button",
        {
          class: "btn",
          title: "Same project and expiry",
          onClick: () => {
            close();
            ctx.reload();
            again();
          },
        },
        "Issue another",
      ),
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
  if (plan.five_hour != null) parts.push(`5h ${pct(plan.five_hour, 1)}`);
  if (plan.seven_day != null) parts.push(`7d ${pct(plan.seven_day, 1)}`);
  if (!parts.length) return null;
  const limited = plan.status === "rejected";
  return h(
    "span",
    {
      class: (limited ? "" : "faint ") + "nowrap small",
      title:
        "How much of the Claude plan is used: in the last five hours, and " +
        "in the last seven days.",
    },
    "plan " + parts.join(" · "),
    limited ? h("span", {}, " ", pill("Limit reached", "warn")) : null,
  );
}
