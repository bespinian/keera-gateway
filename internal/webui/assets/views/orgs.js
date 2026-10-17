// Organisations, for the operator only: one row per customer, and the email
// domain that decides where their people land on first sign-in.

import { api } from "../api.js";
import {
  h,
  table,
  modal,
  date,
  icons,
  iconButton,
  empty,
  plural,
  field,
  pill,
  listHead,
  formModal,
  banner,
} from "../ui.js";

export async function orgsView(ctx) {
  const orgs = (await api.orgs()).data || [];
  ctx.setSubtitle(`${orgs.length} ${plural(orgs.length, "organisation")}`);

  const head = listHead(
    "A dedicated or on-premises deployment has exactly one. It needs no " +
      "email domain, because everyone who signs in joins it.",
    null,
    { label: "Create organisation", onClick: () => newOrg(ctx) },
  );

  const rows = table(
    [
      {
        label: "Organisation",
        sortKey: (o) => o.name,
        cell: (o) =>
          h(
            "div",
            { class: "stack" },
            h(
              "div",
              { class: "row-tight" },
              h("strong", {}, o.name),
              o.limited ? pill("Limited", "warn") : null,
            ),
            h(
              "span",
              { class: "faint mono", style: { fontSize: "11px" } },
              o.id,
            ),
          ),
      },
      {
        label: "Email domain",
        sortKey: (o) => o.email_domain || null,
        cell: (o) =>
          o.email_domain
            ? h("span", { class: "mono" }, o.email_domain)
            : h("span", { class: "faint" }, "not set"),
      },
      {
        label: "Created",
        shrink: true,
        sortKey: (o) =>
          o.created_at ? new Date(o.created_at).getTime() : null,
        sortDir: "desc",
        cell: (o) => h("span", { class: "muted nowrap" }, date(o.created_at)),
      },
      {
        label: "",
        shrink: true,
        cell: (o) =>
          h(
            "div",
            { class: "row-tight" },
            o.limited
              ? h(
                  "button",
                  {
                    class: "btn btn-sm",
                    title:
                      "Open models and MCP servers inside this deployment's network, and sandboxes",
                    onClick: () => liftLimit(ctx, o),
                  },
                  "Lift limit",
                )
              : null,
            iconButton(
              icons.pencil,
              "Edit this organisation",
              () => orgSettings(ctx, o),
              { label: `Edit ${o.name}` },
            ),
            // Not an edit: it switches the organisation every screen shows.
            h(
              "button",
              {
                class: "btn btn-sm",
                title: `Show ${o.name} on the other screens`,
                onClick: async () => {
                  await ctx.setOrg(o.id);
                  ctx.navigate("/");
                },
              },
              "Switch to",
            ),
            iconButton(
              icons.trash,
              "Delete this organisation",
              () =>
                o.id === ctx.state.me.org_id
                  ? cannotDeleteOwn(o)
                  : deleteOrg(ctx, o),
              { danger: true, label: `Delete ${o.name}` },
            ),
          ),
      },
    ],
    orgs,
    {
      emptyTitle: "No organisations yet",
      emptyBody: "Create one per customer.",
      sortBy: "Created",
      sortDir: "desc",
      search: (o) => [o.name, o.id, o.email_domain || ""].join(" "),
      searchLabel: "organisations",
    },
  );

  return h("div", {}, head, rows);
}

// newOrg asks for a new organisation. switchTo moves the panel to it, so the
// first-run checklist's next step does not still say "pick one".
export function newOrg(ctx, { switchTo = false } = {}) {
  const name = h("input", {
    class: "input",
    placeholder: "Example Bank",
    autofocus: true,
  });
  const domain = h("input", { class: "input", placeholder: "example.ch" });
  formModal({
    title: "Create organisation",
    body: h(
      "form",
      { onSubmit: (e) => e.preventDefault() },
      field("Name", name),
      field(
        "Email domain",
        domain,
        "Optional. New users who sign in from this domain join here.",
      ),
    ),
    submitLabel: "Create organisation",
    done: "Organisation created",
    ctx,
    submit: async () => {
      if (!name.value.trim()) {
        name.focus();
        return false;
      }
      const org = await api.createOrg(name.value.trim(), domain.value.trim());
      // The topbar's switcher reads the shell's copy of the list, which
      // reload() does not fetch again. It is ordered by creation.
      (ctx.state.orgs = ctx.state.orgs || []).push(org);
      if (switchTo) await ctx.setOrg(org.id);
    },
  });
}

// liftLimit gives a self-signed-up organisation what it would otherwise get
// with its first payment.
function liftLimit(ctx, org) {
  formModal({
    title: `Lift the limit of ${org.name}`,
    body: h(
      "div",
      {},
      h(
        "p",
        {},
        ctx.state.me.payments
          ? "Somebody created this organisation by signing up. Until it buys " +
              "credit, it can only use models billed from its credit and " +
              "services on public addresses."
          : "Somebody created this organisation by signing up. Until the " +
              "limit is lifted, it can only use services on public addresses " +
              "with keys of its own.",
      ),
      h(
        "p",
        {},
        "Lifting the limit opens the models and MCP servers inside this " +
          "deployment's network, and sandboxes" +
          (ctx.state.me.payments || !ctx.state.me.billing
            ? ". "
            : ", and models on this deployment's provider keys. ") +
          "Only do this for a customer you know.",
      ),
    ),
    submitLabel: "Lift limit",
    done: "Limit lifted",
    ctx,
    submit: async () => {
      const saved = await api.liftOrgLimit(org.id);
      const i = (ctx.state.orgs || []).findIndex((o) => o.id === org.id);
      if (i >= 0) ctx.state.orgs[i] = saved;
    },
  });
}

function orgSettings(ctx, org) {
  const name = h("input", { class: "input", value: org.name });
  const domain = h("input", {
    class: "input",
    value: org.email_domain || "",
    placeholder: "example.ch",
  });
  formModal({
    title: `Edit ${org.name}`,
    body: h(
      "form",
      { onSubmit: (e) => e.preventDefault() },
      field("Name", name, "Must differ from every other organisation's name."),
      field(
        "Email domain",
        domain,
        "Leave empty to clear it. Existing users stay where they are.",
      ),
    ),
    done: "Organisation saved",
    ctx,
    submit: async () => {
      if (!name.value.trim()) {
        name.focus();
        return false;
      }
      const saved = await api.updateOrg(
        org.id,
        name.value.trim(),
        domain.value.trim(),
      );
      // The topbar's switcher reads the shell's copy of the list, which
      // reload() does not fetch again.
      const i = (ctx.state.orgs || []).findIndex((o) => o.id === org.id);
      if (i >= 0) ctx.state.orgs[i] = saved;
      if (org.id === ctx.state.me.org_id) ctx.state.me.org_name = saved.name;
    },
  });
}

// cannotDeleteOwn explains why the operator's own organisation cannot be
// deleted here: it would take their user and session with it. The button opens
// this rather than being hidden, so the operator learns why and what to do.
function cannotDeleteOwn(org) {
  modal({
    title: `Delete ${org.name}?`,
    body: h(
      "div",
      { class: "muted" },
      `You are signed in to ${org.name}, so you cannot delete it here. ` +
        "Delete it as an operator in another organisation, or run ",
      h("code", {}, "keera org delete"),
      " with ",
      h("code", {}, "KEERA_OPERATOR_KEY"),
      " set.",
    ),
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Close"),
    ],
  });
}

// deleteOrg is the most damaging thing the panel can do, so the modal shows
// what would go and asks for the organisation's name to be typed.
function deleteOrg(ctx, org) {
  const summary = h("div", { class: "muted" }, "Counting what it holds…");
  const name = h("input", {
    class: "input",
    placeholder: org.name,
    autocomplete: "off",
    disabled: true,
    onInput: () => {
      button.disabled = name.value.trim() !== org.name;
    },
  });
  const { button } = formModal({
    title: `Delete ${org.name}?`,
    body: h(
      "form",
      { onSubmit: (e) => e.preventDefault() },
      banner(
        "bad",
        "This cannot be undone. Usage history and the audit log are kept " +
          "for reports. Everything else is deleted.",
      ),
      summary,
      h(
        "div",
        { class: "field", style: { marginTop: "16px" } },
        h("label", {}, "Type ", h("strong", {}, org.name), " to confirm"),
        name,
      ),
    ),
    submitLabel: "Delete organisation",
    danger: true,
    done: (gone) => `${gone.name || org.name} deleted`,
    ctx,
    submit: async () => {
      const gone = await api.deleteOrg(org.id);
      // The topbar's switcher reads the shell's copy of the list, which
      // reload() does not fetch again.
      ctx.state.orgs = (ctx.state.orgs || []).filter((o) => o.id !== org.id);
      if (ctx.state.orgID === org.id) {
        await ctx.setOrg(ctx.state.orgs[0]?.id || "");
      }
      return gone;
    },
  });
  button.disabled = true;

  // Loaded after the modal is up, and failing softly: the typed name already
  // confirms the deletion.
  Promise.all([
    api.projects(org.id).catch(() => ({ data: [] })),
    api.users(org.id).catch(() => ({ data: [] })),
    api.keys(org.id).catch(() => ({ data: [] })),
  ]).then(([projects, users, keys]) => {
    const counts = [
      [(projects.data || []).length, "project"],
      [(users.data || []).length, "user"],
      [(keys.data || []).length, "API key"],
    ];
    summary.replaceChildren(
      h("div", {}, "Deleting it removes:"),
      h(
        "ul",
        { class: "muted", style: { margin: "8px 0 0", paddingLeft: "20px" } },
        ...counts.map(([n, label]) =>
          h("li", {}, h("strong", {}, String(n)), ` ${plural(n, label)}`),
        ),
        h("li", {}, "its guardrails and its budget counters"),
        h(
          "li",
          {},
          "its models and their stored provider keys, MCP servers, filters, " +
            "routers, sandbox classes and the record of its finished sandboxes",
        ),
      ),
      h(
        "div",
        { class: "hint", style: { marginTop: "8px" } },
        "Everyone in it is signed out, and every key stops working at once. " +
          "Usage history and the audit log are kept.",
      ),
    );
    name.disabled = false;
    name.focus();
  });
}

/** orgNameOf is the name of orgID, or else of the organisation in view. The
 *  fallback is for a name the panel does not have; start says it opens a
 *  sentence. */
export function orgNameOf(ctx, orgID, { start = false } = {}) {
  const id = orgID || ctx.orgID || ctx.state.me.org_id;
  const name =
    (id === ctx.state.me.org_id && ctx.state.me.org_name) ||
    ((ctx.state.orgs || []).find((o) => o.id === id) || {}).name;
  if (name) return name;
  return start ? "This organisation" : "this organisation";
}

/** chooseOrg is shown on a screen that needs one organisation while the
 *  operator views all of them. It carries a picker so nobody hunts for the
 *  switcher. what is the plural of what the screen holds. */
export function chooseOrg(ctx, what) {
  const orgs = ctx.state.orgs || [];
  if (!orgs.length) {
    return h(
      "div",
      { class: "card" },
      empty(
        "No organisations yet",
        "Create one first. The Dashboard guides you through setup.",
      ),
    );
  }

  const open = async (id) => {
    await ctx.setOrg(id);
    ctx.reload();
  };

  // A few organisations get a row of buttons, more get a list.
  let picker;
  if (orgs.length <= 8) {
    picker = h(
      "div",
      {
        class: "wrap-chips",
        style: { justifyContent: "center", marginTop: "14px" },
      },
      orgs.map((o) =>
        h("button", { class: "btn", onClick: () => open(o.id) }, o.name),
      ),
    );
  } else {
    const select = h(
      "select",
      {
        class: "select",
        style: { width: "auto" },
        "aria-label": "Organisation",
      },
      h("option", { value: "" }, "Choose an organisation"),
      orgs.map((o) => h("option", { value: o.id }, o.name)),
    );
    picker = h(
      "div",
      {
        class: "row",
        style: { justifyContent: "center", marginTop: "14px" },
      },
      select,
      h(
        "button",
        {
          class: "btn btn-primary",
          onClick: () => {
            if (select.value) open(select.value);
          },
        },
        "Open",
      ),
    );
  }

  return h(
    "div",
    { class: "card" },
    h(
      "div",
      { class: "empty" },
      h("strong", {}, "Pick an organisation"),
      `${what} belong to one organisation, but the switcher above shows all ` +
        "of them.",
      picker,
    ),
  );
}
