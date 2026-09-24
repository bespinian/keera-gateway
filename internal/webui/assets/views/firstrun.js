// The first run.
//
// A fresh deployment has nothing in it, and every screen that needs an
// organisation points at a switcher that is not rendered until one exists.
// This is the way out: the four things that have to happen, in order, each
// with the button that does it, each ticking itself off from what the control
// plane already has.
//
// It replaces the dashboard only until the first request has been served.

import { api } from "../api.js";
import { h, icon, icons, modal, toast, pill, showError } from "../ui.js";

export function firstRun(ctx, setup) {
  const operator = ctx.state.me.unrestricted;

  // Steps are ordered by what the next one needs, which is also the order a
  // deployment actually comes up in.
  const steps = [
    {
      title: "Create an organisation",
      done: setup.orgs > 0,
      body:
        "One per customer in a shared deployment. A dedicated or " +
        "on-premises one has exactly one, and everyone who signs in joins it.",
      // Only an operator can create one. An administrator arrives at a
      // deployment that already has theirs, so for them this is already done.
      action: operator
        ? { label: "New organisation", run: () => newOrg(ctx) }
        : null,
      skipped: !operator,
    },
    {
      title: "Add a model",
      done: setup.models > 0,
      body:
        "Clients ask for a model by its alias. Until an enabled model " +
        "exists, every request is refused.",
      action: ctx.state.me.can_edit_catalogue
        ? { label: "Go to Models", run: () => ctx.navigate("/models") }
        : null,
      note: ctx.state.me.can_edit_catalogue
        ? "Then run its check. It catches a backend that answers with text " +
          "instead of a tool call, which breaks coding agents."
        : null,
      skipped: !ctx.state.me.can_edit_catalogue,
    },
    {
      title: "Create a team and set its guardrails",
      done: setup.teams > 0,
      body:
        "Usually one per department, so budgets and reports match how the " +
        "organisation works. Its guardrails apply to every key in it.",
      action:
        setup.orgs > 0
          ? { label: "Go to Teams", run: () => ctx.navigate("/teams") }
          : null,
    },
    {
      title: "Issue a key and connect a client",
      done: setup.keys > 0,
      body:
        "Usage and limits are tracked per key. The panel gives you a ready " +
        "editor configuration with it.",
      action:
        setup.orgs > 0
          ? { label: "Go to API keys", run: () => ctx.navigate("/keys") }
          : null,
    },
  ];

  const nextIndex = steps.findIndex((s) => !s.done && !s.skipped);

  const list = h(
    "div",
    { class: "steps" },
    steps.map((s, i) => {
      const current = i === nextIndex;
      return h(
        "div",
        {
          class:
            "step" +
            (s.done ? " step-done" : "") +
            (current ? " step-now" : ""),
        },
        h(
          "div",
          { class: "step-n" },
          s.done ? icon(icons.check) : String(i + 1),
        ),
        h(
          "div",
          { class: "step-body" },
          h(
            "div",
            { class: "row" },
            h("h2", { style: { marginBottom: "6px" } }, s.title),
            h("div", { style: { flex: 1 } }),
            s.done ? pill("Done", "good") : null,
          ),
          h("p", { class: "muted" }, s.body),
          s.note && !s.done ? h("p", { class: "hint" }, s.note) : null,
          !s.done && s.action
            ? h(
                "button",
                {
                  class: "btn " + (current ? "btn-primary" : ""),
                  onClick: s.action.run,
                },
                s.action.label,
              )
            : null,
          !s.done && !s.action && s.skipped
            ? h("p", { class: "hint" }, "An operator does this one.")
            : null,
        ),
      );
    }),
  );

  const remaining = steps.filter((s) => !s.done && !s.skipped).length;

  return h(
    "div",
    {},
    h(
      "div",
      { class: "banner banner-info", style: { marginBottom: "16px" } },
      remaining
        ? `This deployment has not served a request yet. ${remaining} step` +
            `${remaining === 1 ? "" : "s"} to go.`
        : "Everything is set up. The dashboard appears after the first " +
            "request.",
    ),
    list,
    h(
      "div",
      { class: "hint", style: { marginTop: "16px" } },
      "Also on the command line: ",
      h("code", {}, "keera org create"),
      ", ",
      h("code", {}, "keera team create"),
      ", ",
      h("code", {}, "keera key create"),
      ".",
    ),
  );
}

// newOrg is here rather than only on the Organisations screen because the whole
// point of this one is that somebody arriving at an empty deployment should not
// have to find the screen that unblocks every other screen.
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
      {},
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
            const button = e.currentTarget;
            button.disabled = true;
            try {
              const org = await api.createOrg(
                name.value.trim(),
                domain.value.trim(),
              );
              close();
              toast("Organisation created", "good");
              // Switch to it straight away: a checklist whose next step still
              // says "pick an organisation" has not moved anybody forward.
              ctx.state.orgs =
                (await api.orgs().catch(() => ({ data: [] }))).data || [];
              ctx.state.orgID = org.id;
              localStorage.setItem("keera.org", org.id);
              ctx.reload();
            } catch (ex) {
              showError(err, ex.message);
              button.disabled = false;
            }
          },
        },
        "Create",
      ),
    ],
  });
}
