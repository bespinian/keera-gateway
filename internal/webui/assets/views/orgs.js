// Organisations - operator only. This is the multi-tenant screen: one row per
// customer, and the email domain that decides where their people land when they
// first sign in.

import { api } from "../api.js";
import {
  h,
  table,
  modal,
  toast,
  date,
  icon,
  icons,
  empty,
  showError,
} from "../ui.js";

export async function orgsView(ctx) {
  const orgs = (await api.orgs()).data || [];
  ctx.setSubtitle(`${orgs.length} organisation${orgs.length === 1 ? "" : "s"}`);

  const head = h(
    "div",
    { class: "row", style: { marginBottom: "16px" } },
    h(
      "div",
      { class: "muted" },
      "A dedicated or on-premises deployment has exactly one. It needs no " +
        "email domain, because everyone who signs in joins it.",
    ),
    h("div", { style: { flex: 1 } }),
    h(
      "button",
      { class: "btn btn-primary", onClick: () => newOrg(ctx) },
      icon(icons.plus),
      "New organisation",
    ),
  );

  const rows = table(
    [
      {
        label: "Organisation",
        cell: (o) =>
          h(
            "div",
            { class: "stack" },
            h("strong", {}, o.name),
            h(
              "span",
              { class: "faint mono", style: { fontSize: "11px" } },
              o.id,
            ),
          ),
      },
      {
        label: "Email domain",
        cell: (o) =>
          o.email_domain
            ? h("span", { class: "mono" }, o.email_domain)
            : h("span", { class: "faint" }, "not set"),
      },
      {
        label: "Created",
        shrink: true,
        cell: (o) => h("span", { class: "muted nowrap" }, date(o.created_at)),
      },
      {
        label: "",
        shrink: true,
        cell: (o) =>
          h(
            "div",
            { class: "row-tight" },
            h(
              "button",
              {
                class: "btn btn-sm",
                title: "This organisation's settings",
                onClick: () => orgSettings(ctx, o),
              },
              "Edit",
            ),
            // Not "Edit": this one does not show anything, it changes which
            // organisation every other screen in the panel is about.
            h(
              "button",
              {
                class: "btn btn-sm",
                title: "Show this organisation on the other screens",
                onClick: () => {
                  ctx.state.orgID = o.id;
                  localStorage.setItem("keera.org", o.id);
                  ctx.navigate("/");
                },
              },
              "Switch to",
            ),
            // Deleting your own organisation would take your user row and your
            // session with it, so the button is shown disabled rather than hidden:
            // an operator who came looking for it deserves the reason.
            o.id === ctx.state.me.org_id
              ? h(
                  "button",
                  {
                    class: "btn btn-sm btn-danger",
                    disabled: true,
                    title:
                      "You are signed in to this organisation. " +
                      "Delete it from another operator's account, or with the operator key.",
                  },
                  "Delete",
                )
              : h(
                  "button",
                  {
                    class: "btn btn-sm btn-danger",
                    onClick: () => deleteOrg(ctx, o),
                  },
                  "Delete",
                ),
          ),
      },
    ],
    orgs,
    {
      emptyTitle: "No organisations",
      emptyBody: "Create one per customer.",
    },
  );

  return h("div", {}, head, rows);
}

function newOrg(ctx) {
  const name = h("input", {
    class: "input",
    placeholder: "Example Bank",
    autofocus: true,
  });
  const domain = h("input", { class: "input", placeholder: "example.ch" });
  const err = h("div");
  modal({
    title: "New organisation",
    body: h(
      "form",
      { onSubmit: (e) => e.preventDefault() },
      err,
      h("div", { class: "field" }, h("label", {}, "Name"), name),
      h(
        "div",
        { class: "field" },
        h("label", {}, "Email domain"),
        domain,
        h(
          "div",
          { class: "hint" },
          "Optional. New users who sign in from this domain join here.",
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
            if (!name.value.trim()) return name.focus();
            e.target.disabled = true;
            try {
              const org = await api.createOrg(
                name.value.trim(),
                domain.value.trim(),
              );
              // The shell's copy of the tenant list feeds the org switcher in
              // the topbar, and reload() re-renders that without re-fetching
              // it. Appended rather than re-read: the list is ordered by
              // creation, so this one belongs at the end.
              (ctx.state.orgs = ctx.state.orgs || []).push(org);
              close();
              toast("Organisation created", "good");
              ctx.reload();
            } catch (ex) {
              showError(err, ex.message);
              e.target.disabled = false;
            }
          },
        },
        "Create",
      ),
    ],
  });
}

function orgSettings(ctx, org) {
  const domain = h("input", {
    class: "input",
    value: org.email_domain || "",
    placeholder: "example.ch",
  });
  const err = h("div");
  modal({
    title: `Settings - ${org.name}`,
    body: h(
      "div",
      {},
      err,
      h(
        "div",
        { class: "field" },
        h("label", {}, "Domain"),
        domain,
        h(
          "div",
          { class: "hint" },
          "Leave empty to clear it. Existing users stay where they are.",
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
              await api.setOrgDomain(org.id, domain.value.trim());
              close();
              toast("Domain saved", "good");
              ctx.reload();
            } catch (ex) {
              showError(err, ex.message);
              e.target.disabled = false;
            }
          },
        },
        "Save",
      ),
    ],
  });
}

// deleteOrg destroys a tenant. It is the most damaging thing the panel can do,
// so the modal loads the real blast radius before it lets anybody act on it,
// and then makes them type the organisation's name - an id is easy to click by
// mistake in a table of ten, a name is not.
function deleteOrg(ctx, org) {
  const err = h("div");
  const summary = h("div", { class: "muted" }, "Counting what it holds…");
  const name = h("input", {
    class: "input",
    placeholder: org.name,
    autocomplete: "off",
    disabled: true,
    onInput: () => {
      go.disabled = name.value.trim() !== org.name;
    },
  });
  const go = h(
    "button",
    {
      class: "btn btn-danger",
      disabled: true,
      onClick: async (e) => {
        const button = e.currentTarget;
        button.disabled = true;
        err.replaceChildren();
        try {
          const gone = await api.deleteOrg(org.id);
          // The shell's copy of the tenant list feeds the org switcher in the
          // topbar, and reload() re-renders that without re-fetching it.
          ctx.state.orgs = (ctx.state.orgs || []).filter(
            (o) => o.id !== org.id,
          );
          if (ctx.state.orgID === org.id) {
            const next = ctx.state.orgs[0];
            ctx.state.orgID = next ? next.id : "";
            if (next) localStorage.setItem("keera.org", next.id);
            else localStorage.removeItem("keera.org");
          }
          close();
          toast(`${gone.name || org.name} deleted`, "good");
          ctx.reload();
        } catch (ex) {
          showError(err, ex.message);
          button.disabled = false;
        }
      },
    },
    "Delete organisation",
  );

  const close = modal({
    title: `Delete ${org.name}?`,
    body: h(
      "form",
      { onSubmit: (e) => e.preventDefault() },
      err,
      h(
        "div",
        { class: "banner banner-bad" },
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
    actions: (dismiss) => [
      h("button", { class: "btn", onClick: dismiss }, "Cancel"),
      go,
    ],
  });

  // Loaded after the modal is up so the operator is not left staring at a
  // spinner, and failing softly: not knowing the counts is no reason to block a
  // deletion the typed name already confirms.
  Promise.all([
    api.teams(org.id).catch(() => ({ data: [] })),
    api.users(org.id).catch(() => ({ data: [] })),
    api.keys(org.id).catch(() => ({ data: [] })),
  ]).then(([teams, users, keys]) => {
    const counts = [
      [(teams.data || []).length, "team"],
      [(users.data || []).length, "user"],
      [(keys.data || []).length, "API key"],
    ];
    summary.replaceChildren(
      h("div", {}, "Deleting it removes:"),
      h(
        "ul",
        { class: "muted", style: { margin: "8px 0 0", paddingLeft: "20px" } },
        ...counts.map(([n, label]) =>
          h(
            "li",
            {},
            h("strong", {}, String(n)),
            ` ${label}${n === 1 ? "" : "s"}`,
          ),
        ),
        h("li", {}, "its guardrails and its budget counters"),
      ),
      h(
        "div",
        { class: "hint", style: { marginTop: "8px" } },
        "Everyone in it is signed out, and every key stops working at once.",
      ),
    );
    name.disabled = false;
    name.focus();
  });
}

/** chooseOrg is the wall four screens hit when an operator has the switcher on
 *  every organisation at once.
 *
 *  Teams, keys, people and filters each belong to one organisation, so those
 *  screens have nothing to show. The fix is the switcher in the topbar, and a
 *  screen that only names it leaves the reader hunting for it - so the picker
 *  is here, where the wall is.
 *
 *  what is the plural of whatever the screen holds, for the sentence. */
export function chooseOrg(ctx, what) {
  const orgs = ctx.state.orgs || [];
  if (!orgs.length) {
    return h(
      "div",
      { class: "card" },
      empty(
        "No organisations yet",
        "Create one first. The Overview screen guides you through setup.",
      ),
    );
  }

  const open = (id) => {
    ctx.state.orgID = id;
    localStorage.setItem("keera.org", id);
    ctx.reload();
  };

  // A handful of tenants is a row of buttons, which is one click. A deployment
  // with more than that gets the list it can actually work through.
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
