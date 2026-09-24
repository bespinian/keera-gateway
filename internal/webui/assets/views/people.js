// People: who can sign in, and what they may do.

import { api } from "../api.js";
import {
  h,
  table,
  modal,
  toast,
  confirm,
  pill,
  ago,
  icon,
  icons,
  showError,
} from "../ui.js";
import { chooseOrg } from "./orgs.js";

const ROLES = [
  {
    key: "member",
    label: "Member",
    note: "Sees their organisation's usage. Changes nothing.",
  },
  {
    key: "admin",
    label: "Administrator",
    note: "Manages the organisation: teams, guardrails, keys, people.",
  },
  {
    key: "operator",
    label: "Operator",
    note: "Works across organisations and manages the model catalogue.",
  },
];

// What this screen may hand out. The operator role is not on it anywhere: it
// crosses organisations, so it comes from KEERA_OPERATORS or an operator group
// in the gateway's configuration and from nothing a panel can send. It is still
// shown in the Role column, because reading who has it is the point of the
// column.
const ASSIGNABLE = ROLES.filter((r) => r.key !== "operator");

const TONE = { operator: "accent", admin: "good", member: "" };

export async function peopleView(ctx) {
  const users = (await api.users(ctx.orgID)).data || [];
  // Whether a role can be changed here at all. It cannot when a group decides
  // the administrator role: the directory reapplies its answer on the next
  // sign-in, so the screen says where roles come from rather than offering a
  // control that does not keep.
  const canAssign = !ctx.state.me.roles_from_directory;
  const canOperate = ctx.state.me.unrestricted;
  ctx.setSubtitle(
    `${users.length} ${users.length === 1 ? "person" : "people"}`,
  );

  if (!ctx.orgID && canOperate) return chooseOrg(ctx, "Users");

  const head = h(
    "div",
    { class: "row", style: { marginBottom: "16px" } },
    h(
      "div",
      { class: "muted" },
      !ctx.state.me.sso
        ? "Nobody can sign in until single sign-on is set up. People added " +
            "here are linked to their identity when they first sign in."
        : canAssign
          ? "Roles set here stay until you change them. The operator role is " +
            "set in the gateway's configuration."
          : "Roles come from a group in the identity provider, read at every " +
            "sign-in. Change them in the directory.",
    ),
    h("div", { style: { flex: 1 } }),
    h(
      "button",
      { class: "btn btn-primary", onClick: () => addPerson(ctx) },
      icon(icons.plus),
      "Add person",
    ),
  );

  const rows = table(
    [
      {
        label: "Email",
        sortKey: (u) => u.email,
        cell: (u) =>
          h(
            "div",
            { class: "stack" },
            h(
              "div",
              { class: "row", style: { gap: "8px" } },
              h("strong", {}, u.email),
              u.disabled_at ? pill("Disabled", "bad") : null,
            ),
            u.external_id
              ? h(
                  "span",
                  { class: "faint", style: { fontSize: "11px" } },
                  "linked to " + u.external_id,
                )
              : h(
                  "span",
                  { class: "faint", style: { fontSize: "11px" } },
                  "not linked to an identity yet",
                ),
          ),
      },
      {
        // Widest role first: the question asked of this screen is who has more
        // than they should, not who has the least.
        label: "Role",
        shrink: true,
        sortKey: (u) => ({ operator: 0, admin: 1, member: 2 })[u.role] ?? 3,
        cell: (u) => pill(roleLabel(u.role), TONE[u.role] || ""),
      },
      {
        label: "Added",
        shrink: true,
        sortKey: (u) =>
          u.created_at ? new Date(u.created_at).getTime() : null,
        sortDir: "desc",
        cell: (u) =>
          h(
            "span",
            { class: "muted nowrap", title: u.created_at },
            ago(u.created_at),
          ),
      },
      {
        label: "",
        shrink: true,
        cell: (u) => {
          if (u.id === ctx.state.me.user_id) {
            return h("span", { class: "faint" }, "you");
          }
          // An operator holds that role because the configuration says so, and
          // the next sign-in would put it back, so there is nothing to edit.
          if (u.role === "operator") {
            return h(
              "span",
              { class: "faint nowrap" },
              "from the configuration",
            );
          }
          // Turning somebody off works the same whether roles come from the
          // directory or from here: the directory cannot revoke their keys.
          const toggle = u.disabled_at
            ? h(
                "button",
                {
                  class: "btn btn-sm",
                  title: "Let this person sign in again",
                  onClick: () => enablePerson(ctx, u),
                },
                "Enable",
              )
            : h(
                "button",
                {
                  class: "btn btn-sm btn-danger",
                  title: "Disable this person, for example when they leave",
                  onClick: () => disablePerson(ctx, u),
                },
                "Disable",
              );
          return h(
            "div",
            { class: "row nowrap", style: { gap: "8px" } },
            canAssign && !u.disabled_at
              ? h(
                  "button",
                  {
                    class: "btn btn-sm",
                    title: "This person's role",
                    onClick: () => changeRole(ctx, u),
                  },
                  "Edit",
                )
              : !canAssign
                ? h(
                    "span",
                    { class: "faint nowrap" },
                    "role from the directory",
                  )
                : null,
            toggle,
          );
        },
      },
    ],
    users,
    {
      emptyTitle: "Nobody yet",
      emptyBody: "Users appear here the first time they sign in.",
      search: (u) =>
        [
          u.email,
          u.external_id || "",
          roleLabel(u.role),
          u.disabled_at ? "disabled" : "",
        ].join(" "),
      searchLabel: "users",
    },
  );

  return h("div", {}, head, rows);
}

function roleLabel(role) {
  const r = ROLES.find((x) => x.key === role);
  return r ? r.label : role;
}

function roleSelect(current) {
  return h(
    "select",
    { class: "select" },
    ASSIGNABLE.map((r) =>
      h("option", { value: r.key, selected: r.key === current }, r.label),
    ),
  );
}

function addPerson(ctx) {
  const email = h("input", {
    class: "input",
    type: "email",
    placeholder: "first.last@example.ch",
    autofocus: true,
  });
  const canAssign = !ctx.state.me.roles_from_directory;
  const role = roleSelect("member");
  const err = h("div");

  modal({
    title: "Add a person",
    subtitle: canAssign
      ? "This reserves their role. They still sign in through the identity provider."
      : "This adds them to this organisation before they first sign in. " +
        "Their role comes from the directory.",
    body: h(
      "form",
      { onSubmit: (e) => e.preventDefault() },
      err,
      h("div", { class: "field" }, h("label", {}, "Email"), email),
      canAssign
        ? h(
            "div",
            { class: "field" },
            h("label", {}, "Role"),
            role,
            // A line each. Joined into one run of prose they read as a single
            // sentence about nobody in particular.
            h(
              "div",
              { class: "hint" },
              ASSIGNABLE.map((r) => h("div", {}, `${r.label}: ${r.note}`)),
            ),
          )
        : null,
    ),
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Cancel"),
      h(
        "button",
        {
          class: "btn btn-primary",
          onClick: async (e) => {
            if (!email.value.trim()) return email.focus();
            e.target.disabled = true;
            try {
              await api.inviteUser(
                ctx.orgID || ctx.state.me.org_id,
                email.value.trim().toLowerCase(),
                role.value,
              );
              close();
              toast("Person added", "good");
              ctx.reload();
            } catch (ex) {
              showError(err, ex.message);
              e.target.disabled = false;
            }
          },
        },
        "Add",
      ),
    ],
  });
}

function changeRole(ctx, user) {
  const role = roleSelect(user.role);
  const err = h("div");
  modal({
    title: `Role for ${user.email}`,
    body: h(
      "div",
      {},
      err,
      h(
        "div",
        { class: "field" },
        h("label", {}, "Role"),
        role,
        h(
          "div",
          { class: "hint" },
          "This signs them out everywhere, so the new role applies at once.",
        ),
      ),
    ),
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Cancel"),
      h(
        "button",
        {
          class: "btn btn-primary",
          onClick: async (e) => {
            e.target.disabled = true;
            try {
              await api.setUserRole(user.id, role.value);
              close();
              toast("Role updated", "good");
              ctx.reload();
            } catch (ex) {
              showError(err, ex.message);
              e.target.disabled = false;
            }
          },
        },
        "Save role",
      ),
    ],
  });
}

function disablePerson(ctx, user) {
  confirm({
    title: `Disable ${user.email}?`,
    body: h(
      "div",
      {},
      h("p", {}, "Use this when someone leaves. At once:"),
      h(
        "ul",
        {},
        h("li", {}, "they cannot sign in, and are signed out everywhere"),
        h("li", {}, "all their keys are revoked for good"),
        ctx.state.me.sandboxes
          ? h(
              "li",
              {},
              "their agent sandboxes are terminated, and their own are suspended",
            )
          : null,
      ),
      h(
        "p",
        {},
        "Usage history and the audit log are kept. You can enable them again, " +
          "but their old keys stay revoked.",
      ),
    ),
    confirmLabel: "Disable",
    danger: true,
    onConfirm: async () => {
      const res = await api.disableUser(user.id);
      const keys =
        res.revoked_keys === 1 ? "1 key" : `${res.revoked_keys} keys`;
      toast(
        res.warning
          ? `${user.email} disabled. ${res.warning}`
          : `${user.email} disabled; ${keys} revoked`,
        res.warning ? "bad" : "good",
      );
      ctx.reload();
    },
  });
}

function enablePerson(ctx, user) {
  confirm({
    title: `Enable ${user.email}?`,
    body:
      "They can sign in again. Their old keys stay revoked, so they start " +
      "with none.",
    confirmLabel: "Enable",
    onConfirm: async () => {
      await api.enableUser(user.id);
      toast(`${user.email} can sign in again`, "good");
      ctx.reload();
    },
  });
}
