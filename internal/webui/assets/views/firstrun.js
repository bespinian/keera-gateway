// The first run.
//
// A fresh deployment has nothing in it, and every screen that needs an
// organisation points at a switcher that is not rendered until one exists.
// This is the way out: the things that have to happen, in order, each
// with the button that does it, each ticking itself off from what the control
// plane already has.
//
// It replaces the dashboard until the first request has been served and, with
// card payments, until the organisation has had credit. A member gets a
// shorter guide of their own, until one of their keys has made a request.

import { h, icon, icons, pill, isAdmin } from "../ui.js";
import { addCredit } from "./billing.js";
import { openConnectClient, usableKeys } from "./connect.js";
import { activateKey, confirmRotate, openIssueKey } from "./keys.js";
import { newOrg, orgNameOf } from "./orgs.js";

/** firstRunDue reports whether the checklist replaces the dashboard, from the
 *  counts of /v1/setup. The sidebar asks too, to name the screen after it. */
export function firstRunDue(ctx, setup) {
  if (!isAdmin(ctx) || !setup) return false;
  if (setup.requests === 0) return true;
  // Credit is one organisation's, so there is none to ask for while an
  // operator looks at all of them.
  return (
    !!ctx.state.me.payments &&
    !!ctx.orgID &&
    !setup.credits &&
    !setup.invoiced
  );
}

export function firstRun(ctx, setup, credit) {
  const operator = ctx.state.me.unrestricted;
  const admin = isAdmin(ctx);

  // Steps are ordered by what the next one needs, which is also the order a
  // deployment actually comes up in.
  const steps = [
    {
      title: "Create your organisation",
      icon: icons.orgs,
      done: setup.orgs > 0,
      // Before there is one, or while an operator looks at all of them at
      // once, there is no name to give.
      body:
        ctx.orgID || ctx.state.me.org_id
          ? [
              "Your organisation ",
              h("strong", {}, orgNameOf(ctx)),
              " holds all your users, models, keys, etc.",
            ]
          : "Your organisation holds all your users, models, keys, etc.",
      // Only an operator can create one. An administrator arrives at a
      // deployment that already has theirs, so for them this is already done.
      action: operator
        ? {
            label: "Create organisation",
            run: () => newOrg(ctx, { switchTo: true }),
          }
        : null,
      skipped: !operator,
      by: "An operator",
    },
    {
      title: "Add a model",
      icon: icons.models,
      done: setup.models > 0,
      body:
        "Add an LLM from our model catalogue. It contains frontier models " +
        "and open-weight models hosted in Switzerland.",
      action: admin
        ? {
            label: "Open model catalogue",
            run: () => ctx.navigate("/catalogue"),
          }
        : null,
      skipped: !admin,
      by: "An administrator",
    },
    {
      title: "Issue an API key",
      icon: icons.keys,
      done: setup.keys > 0,
      body:
        "With an API key you can access your models from different agents " +
        "and clients.",
      action:
        setup.orgs > 0 && admin
          ? { label: "Issue API key", run: () => openIssueKey(ctx) }
          : null,
      skipped: !admin,
      by: "An administrator",
    },
  ];

  // Only a deployment that takes card payments asks for credit. An
  // organisation billed by invoice needs none.
  if (ctx.state.me.payments) {
    const account = credit && credit.account;
    const limited =
      ctx.state.me.org_limited && ctx.orgID === ctx.state.me.org_id;
    steps.push({
      title: "Add credit",
      icon: icons.billing,
      done: setup.credits > 0 || setup.invoiced,
      body:
        "With credit you pay for the model usage across the entire " +
        "catalogue." +
        (limited ? " Your first payment unlocks all features." : ""),
      action:
        account && admin
          ? { label: "Add credit", run: () => addCredit(ctx, credit) }
          : null,
      skipped: !admin,
      by: "An administrator",
    });
  }

  // Last, because a request needs everything above it.
  steps.push({
    title: "Send your first request",
    icon: icons.connect,
    done: setup.requests > 0,
    body:
      "Connect an agent or client with your API key, or try your " +
      "models in the playground.",
    action:
      setup.keys > 0 && admin
        ? {
            label: "Connect client",
            run: (e) => openConnectClient(ctx, e.currentTarget),
          }
        : null,
    also:
      setup.keys > 0 && admin
        ? { label: "Open playground", run: () => ctx.navigate("/playground") }
        : null,
    skipped: !admin,
    by: "An administrator",
  });

  return stepList(steps);
}

/** memberRunDue reports whether a member's own guide replaces the dashboard:
 *  until one of their keys has made a request. access is /v1/access. */
export function memberRunDue(ctx, access) {
  return (
    !isAdmin(ctx) &&
    !!access &&
    !access.anonymous &&
    !access.operator_key &&
    !access.used
  );
}

/** memberRun is the guide for a member: from signing in to a working editor.
 *  An administrator issues the key, so the member's first task is to hold its
 *  secret: by activating the key, or, for one the administrator activated, by
 *  rotating it. */
export function memberRun(ctx, access) {
  const keys = usableKeys(access);
  const key = keys[0];
  const pending = (access.keys || []).find((k) => k.state === "pending");

  const steps = [
    pending && !key
      ? {
          title: "Activate your API key",
          icon: icons.keys,
          done: false,
          body: [
            "An administrator issued you the key ",
            h("strong", {}, pending.name),
            ". Activate it to see its secret, once. For Claude Code, ",
            h("code", {}, "keera connect claude-code"),
            " activates it and sets Claude Code up in one step.",
          ],
          action: {
            label: "Activate key",
            run: (e) => activateKey(ctx, pending, e.currentTarget),
          },
        }
      : getKeyStep(keys, key, ctx),
    {
      title: "Send your first request",
      icon: icons.connect,
      done: false,
      body:
        "Connect an agent or client with your API key, or try your " +
        "models in the playground.",
      action: key
        ? {
            label: "Connect client",
            run: (e) => openConnectClient(ctx, e.currentTarget),
          }
        : null,
      also: key
        ? { label: "Open playground", run: () => ctx.navigate("/playground") }
        : null,
    },
  ];
  return stepList(steps);
}

/** getKeyStep is the first step of a member's guide when they have no key to
 *  activate: an active key whose secret they may not have, or none. */
function getKeyStep(keys, key, ctx) {
  return {
    title: "Get your API key",
    icon: icons.keys,
    done: keys.length > 0,
    body: !key
      ? "An administrator issues keys. Ask them for one in your name."
      : keys.length > 1
        ? "Your keys are ready. Their secrets were shown only to the " +
          "administrator who issued them. If you don't have one, rotate " +
          "it on API keys."
        : [
            "Your key ",
            h("strong", {}, key.name),
            " is ready. Its secret was shown only to the administrator " +
              "who issued it. If you don't have it, rotate the key to get a " +
              "new one.",
          ],
    // A key that is ready still needs its secret, so the button stays.
    keepAction: true,
    action: key
      ? keys.length > 1
        ? { label: "Go to API keys", run: () => ctx.navigate("/keys") }
        : {
            label: "Rotate key",
            run: () => confirmRotate(ctx, key),
          }
      : null,
  };
}

/** stepList draws a guide's steps, with the next one to do drawn out. */
function stepList(steps) {
  const nextIndex = steps.findIndex((s) => !s.done && !s.skipped);
  const button = (a, primary) =>
    h(
      "button",
      { class: "btn" + (primary ? " btn-primary" : ""), onClick: a.run },
      a.label,
    );

  return h(
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
        // The number rides on the icon, so the order still reads at a glance.
        h(
          "div",
          { class: "step-icon" },
          icon(s.icon),
          h(
            "span",
            { class: "step-n" },
            s.done ? icon(icons.check) : String(i + 1),
          ),
        ),
        h(
          "div",
          { class: "step-body" },
          h(
            "div",
            { class: "row" },
            h("h2", { style: { marginBottom: "6px" } }, s.title),
            h("div", { class: "spacer" }),
            s.done ? pill("Done", "good") : null,
          ),
          h("p", { class: "muted" }, s.body),
          (!s.done || s.keepAction) && s.action
            ? h(
                "div",
                { class: "row" },
                button(s.action, current && !s.done),
                s.also ? button(s.also, false) : null,
              )
            : null,
          !s.done && !s.action && s.skipped
            ? h("p", { class: "hint" }, s.by + " does this one.")
            : null,
        ),
      );
    }),
  );
}
