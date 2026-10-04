// The first run.
//
// A fresh deployment has nothing in it, and every screen that needs an
// organisation points at a switcher that is not rendered until one exists.
// This is the way out: the three things that have to happen, in order, each
// with the button that does it, each ticking itself off from what the control
// plane already has.
//
// It replaces the dashboard only until the first request has been served.

import { h, icon, icons, pill, plural, isAdmin } from "../ui.js";
import { newOrg } from "./orgs.js";

export function firstRun(ctx, setup) {
  const operator = ctx.state.me.unrestricted;
  const admin = isAdmin(ctx);

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
        ? {
            label: "New organisation",
            run: () => newOrg(ctx, { switchTo: true }),
          }
        : null,
      skipped: !operator,
      by: "An operator",
    },
    {
      title: "Add a model",
      done: setup.models > 0,
      body:
        "Clients ask for a model by its alias. Until an enabled model " +
        "exists, every request is refused. A new organisation starts with " +
        "the models in the catalogue file, if the deployment has one.",
      action: admin
        ? { label: "Go to Models", run: () => ctx.navigate("/models") }
        : null,
      note: admin
        ? "Then run its check. It catches a backend that answers with text " +
          "instead of a tool call, which breaks coding agents."
        : null,
      skipped: !admin,
      by: "An administrator",
    },
    {
      title: "Issue a key and connect a client",
      done: setup.keys > 0,
      body:
        "Usage and limits are tracked per key. A key goes in the default " +
        "project unless you pick another. The panel gives you a ready " +
        "editor configuration with it.",
      action:
        setup.orgs > 0 && admin
          ? { label: "Go to API keys", run: () => ctx.navigate("/keys") }
          : null,
      skipped: !admin,
      by: "An administrator",
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
            ? h("p", { class: "hint" }, s.by + " does this one.")
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
        ? `This deployment has not served a request yet. ${remaining} ` +
            `${plural(remaining, "step")} to go.`
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
      h("code", {}, "keera project create"),
      ", ",
      h("code", {}, "keera key create"),
      ".",
    ),
  );
}
