// Users: who can sign in, and what they may do.

import { api } from "../api.js";
import {
  h,
  table,
  toast,
  confirm,
  pill,
  ago,
  dateTime,
  icon,
  icons,
  iconButton,
  plural,
  field,
  rowLink,
  idpMark,
  listHead,
  dash,
  formModal,
} from "../ui.js";
import { chooseOrg, orgNameOf } from "./orgs.js";
import { userPasskeys, showPasskeyLink } from "./passkeys.js";

const ROLES = [
  {
    key: "member",
    label: "Member",
    note: "Sees their organisation's usage and rotates or revokes their own keys.",
  },
  {
    key: "admin",
    label: "Administrator",
    note: "Manages the organisation: users, projects, keys, guardrails, models, MCP servers, filters, routers and sandbox classes.",
  },
  {
    key: "operator",
    label: "Operator",
    note: "Creates organisations and works in any of them.",
  },
];

// What this screen may hand out. The operator role is not on it anywhere: it
// crosses organisations, so it comes from KEERA_OPERATORS or an operator group
// in the gateway's configuration and from nothing a panel can send. It is still
// shown in the Role column, because reading who has it is the point of the
// column.
const ASSIGNABLE = ROLES.filter((r) => r.key !== "operator");

export const TONE = { operator: "accent", admin: "good", member: "" };

// isPasskeyAccount mirrors authn.IsPasskeyAccount: the account signs in with
// passkeys, not through a directory.
const isPasskeyAccount = (u) =>
  (u.external_id || "").startsWith("keera:passkey:");

/** headline says how users get in and where their roles come from. */
function headline(me, canAssign) {
  if (!me.sso && me.passkeys)
    return (
      "Users sign in with a passkey. Add a user to get a set-up link " +
      "for them."
    );
  if (!me.sso)
    return (
      "Nobody can sign in until single sign-on is set up. Users added " +
      "here are linked to their identity when they first sign in."
    );
  if (canAssign) return null;
  return (
    "Roles come from a group in the identity provider, read at every " +
    "sign-in. Change them in the directory."
  );
}

/** signsInWith is the line under a person's address. */
export function signsInWith(u) {
  if (isPasskeyAccount(u)) return "signs in with a passkey";
  if (u.external_id) return "linked to " + u.external_id;
  return "not linked to an identity yet";
}

/** identityProvider is the logo and name of where a person signs in, or
 *  null before they are linked to an identity. */
function identityProvider(me, u) {
  if (isPasskeyAccount(u)) return [passkeyMark(), "Passkey"];
  // The provider is the part of the external ID before the first colon.
  const name = (u.external_id || "").split(":")[0];
  if (!name || name === "keera") return null;
  const p = (me.providers || []).find((x) => x.name === name);
  return [idpMark(name), p ? p.label : name];
}

function passkeyMark() {
  const svg = icon(icons.passkey);
  svg.classList.add("provider-mark");
  return svg;
}

export async function peopleView(ctx) {
  const canOperate = ctx.state.me.unrestricted;
  if (!ctx.orgID && canOperate) return chooseOrg(ctx, "Users");

  const users = (await api.users(ctx.orgID)).data || [];
  // Whether a role can be changed here at all. It cannot when a group decides
  // the administrator role: the directory reapplies its answer on the next
  // sign-in, so the screen says where roles come from rather than offering a
  // control that does not keep.
  const canAssign = !ctx.state.me.roles_from_directory;
  const active = users.filter((u) => !u.disabled_at).length;
  ctx.setSubtitle(
    active === users.length
      ? `${users.length} ${plural(users.length, "user")}`
      : `${active} active of ${users.length}`,
  );

  const head = listHead(headline(ctx.state.me, canAssign), null, {
    label: "Add user",
    onClick: () => addPerson(ctx),
  });

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
              rowLink(
                ctx,
                "/users/" + encodeURIComponent(u.id),
                u.email,
                "This user's traffic, cost and keys",
              ),
              u.disabled_at ? pill("Disabled", "bad") : null,
            ),
            h(
              "span",
              { class: "faint", style: { fontSize: "11px" } },
              signsInWith(u),
            ),
          ),
      },
      {
        label: "Identity provider",
        shrink: true,
        sortKey: (u) => {
          const p = identityProvider(ctx.state.me, u);
          return p ? p[1] : null;
        },
        cell: (u) => {
          const p = identityProvider(ctx.state.me, u);
          return p
            ? h("span", { class: "row nowrap", style: { gap: "6px" } }, ...p)
            : dash();
        },
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
        // The later of their last sign-in and their last request through one
        // of their keys: an agent working all week counts, not only the panel.
        label: "Last active",
        shrink: true,
        sortKey: (u) =>
          u.last_active_at ? new Date(u.last_active_at).getTime() : null,
        sortDir: "desc",
        cell: (u) =>
          u.last_active_at
            ? h(
                "span",
                {
                  class: "muted nowrap",
                  title:
                    dateTime(u.last_active_at) +
                    " - last sign-in or request through one of their keys",
                },
                ago(u.last_active_at),
              )
            : h("span", { class: "faint nowrap" }, "never"),
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
            { class: "muted nowrap", title: dateTime(u.created_at) },
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
                  title: "Let this user sign in again",
                  onClick: () => enablePerson(ctx, u),
                },
                "Enable",
              )
            : h(
                "button",
                {
                  class: "btn btn-sm btn-danger",
                  title: "Disable this user, for example when they leave",
                  onClick: () => disablePerson(ctx, u),
                },
                "Disable",
              );
          return h(
            "div",
            { class: "row nowrap", style: { gap: "8px" } },
            passkeyButton(ctx, u),
            canAssign && !u.disabled_at
              ? iconButton(
                  icons.pencil,
                  "Edit this user's role",
                  () => changeRole(ctx, u),
                  { label: `Edit the role of ${u.email}` },
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
      emptyTitle: "No users yet",
      emptyBody: "Users appear here the first time they sign in.",
      search: (u) =>
        [
          u.email,
          u.external_id || "",
          (identityProvider(ctx.state.me, u) || [])[1] || "",
          roleLabel(u.role),
          u.disabled_at ? "disabled" : "",
        ].join(" "),
      searchLabel: "users",
      // Off by default: someone disabled has left, and is kept only so their
      // usage and audit entries still have a name.
      toggles: [
        {
          label: "Show disabled",
          hidden: (u) => !!u.disabled_at,
        },
      ],
    },
  );

  return h("div", {}, head, rows);
}

export function roleLabel(role) {
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
  // Without a directory a passkey is the only way in, so it is the only
  // choice: the API refuses the other.
  const signIn = h(
    "select",
    { class: "select" },
    ctx.state.me.sso
      ? h(
          "option",
          { value: "sso", selected: true },
          "Their identity provider",
        )
      : null,
    h("option", { value: "passkey", selected: !ctx.state.me.sso }, "A passkey"),
  );

  formModal({
    title: "Add user",
    subtitle: canAssign
      ? ctx.state.me.passkeys
        ? "This reserves their role."
        : "This reserves their role. They still sign in through the identity provider."
      : `This adds them to ${orgNameOf(ctx)} before they first sign in. ` +
        "Their role comes from the directory.",
    body: h(
      "form",
      { onSubmit: (e) => e.preventDefault() },
      field("Email", email),
      ctx.state.me.passkeys
        ? field(
            "Signs in with",
            signIn,
            "A passkey account does not leave with the directory: disable " +
              "it here when they leave. You get a set-up link to send them.",
          )
        : null,
      // A line each: run together they read as one sentence.
      canAssign
        ? field(
            "Role",
            role,
            ASSIGNABLE.map((r) => h("div", {}, `${r.label}: ${r.note}`)),
          )
        : null,
    ),
    submitLabel: "Add user",
    done: "User added",
    ctx,
    submit: async (close) => {
      if (!email.value.trim()) {
        email.focus();
        return false;
      }
      // Left empty, the control plane picks the way this gateway signs
      // people in, as it does for `keera user add`.
      const user = await api.addUser(
        ctx.orgID || ctx.state.me.org_id,
        email.value.trim(),
        role.value,
        ctx.state.me.passkeys ? signIn.value : "",
      );
      close();
      if (user.passkey_link) showPasskeyLink(user.email, user.passkey_link);
    },
  });
}

export function changeRole(ctx, user) {
  const role = roleSelect(user.role);
  formModal({
    title: `Role for ${user.email}`,
    body: h(
      "form",
      { onSubmit: (e) => e.preventDefault() },
      field(
        "Role",
        role,
        "This signs them out everywhere, so the new role applies at once.",
      ),
    ),
    done: "Role saved",
    ctx,
    submit: () => api.setUserRole(user.id, role.value),
  });
}

export function disablePerson(ctx, user) {
  confirm({
    title: `Disable ${user.email}?`,
    body: h(
      "div",
      {},
      h("p", {}, "Use this when a user leaves. At once:"),
      h(
        "ul",
        {},
        h("li", {}, "they cannot sign in, and are signed out everywhere"),
        h("li", {}, "all their keys are revoked for good"),
        ctx.state.me.passkeys && isPasskeyAccount(user)
          ? h("li", {}, "their passkeys are removed")
          : null,
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
      const keys = `${res.revoked_keys} ${plural(res.revoked_keys, "key")}`;
      const sb = res.sandboxes || {};
      const stopped = (sb.terminated || 0) + (sb.suspended || 0);
      const sandboxes = stopped
        ? `, ${stopped} ${plural(stopped, "sandbox", "sandboxes")} stopped`
        : "";
      toast(
        res.warning
          ? `${user.email} disabled. ${res.warning}`
          : `${user.email} disabled; ${keys} revoked${sandboxes}`,
        res.warning ? "bad" : "good",
      );
      ctx.reload();
    },
  });
}

// Passkeys are for accounts no directory vouches for: a passkey account, or
// someone who has not signed in yet.
function passkeyButton(ctx, u) {
  if (!ctx.state.me.passkeys || u.disabled_at) return null;
  if (isPasskeyAccount(u))
    return h(
      "button",
      {
        class: "btn btn-sm",
        title: "This user's passkeys, and a new set-up link",
        onClick: () => userPasskeys(u),
      },
      "Passkeys",
    );
  if (u.external_id) return null;
  return h(
    "button",
    {
      class: "btn btn-sm",
      title: "Let this user sign in with a passkey instead",
      onClick: () => switchToPasskey(ctx, u),
    },
    "Use a passkey",
  );
}

function switchToPasskey(ctx, user) {
  confirm({
    title: `Let ${user.email} sign in with a passkey?`,
    body:
      "They will sign in with a passkey instead of the identity provider, and " +
      "you get a set-up link to send them. This cannot be undone here.",
    confirmLabel: "Get set-up link",
    onConfirm: async () => {
      const link = await api.passkeyLink(user.id);
      ctx.reload();
      showPasskeyLink(user.email, link);
    },
  });
}

export function enablePerson(ctx, user) {
  confirm({
    title: `Enable ${user.email}?`,
    body: isPasskeyAccount(user)
      ? "Their old keys stay revoked, and their passkeys are gone. Send them " +
        "a new set-up link from Passkeys."
      : "They can sign in again. Their old keys stay revoked, so they start " +
        "with none.",
    confirmLabel: "Enable",
    onConfirm: async () => {
      await api.enableUser(user.id);
      toast(`${user.email} can sign in again`, "good");
      ctx.reload();
    },
  });
}
