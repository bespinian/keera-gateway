// The first-run checklist, ticked off from what the control plane has. It
// replaces the dashboard until the first request is served and, with card
// payments, until the organisation has had credit. A member gets a shorter
// guide, until one of their keys has made a request.

import {
  h,
  svg,
  icon,
  icons,
  pill,
  isAdmin,
  docsURL,
  brandMark,
  providerMark,
  clientMark,
} from "../ui.js";
import { addCredit } from "./billing.js";
import { openConnectClient, usableKeys } from "./connect.js";
import { activateKey, confirmRotate, openIssueKey } from "./keys.js";
import { providerName } from "./models.js";
import { newOrg, orgNameOf } from "./orgs.js";

/** The sidebar uses this too, to name the screen. */
export function firstRunDue(ctx, setup) {
  if (!isAdmin(ctx) || !setup) return false;
  if (setup.requests === 0) return true;
  // No credit to ask for while an operator looks at all organisations.
  return (
    !!ctx.state.me.payments && !!ctx.orgID && !setup.credits && !setup.invoiced
  );
}

export function firstRun(ctx, setup, credit, providers) {
  const operator = ctx.state.me.unrestricted;
  const admin = isAdmin(ctx);

  // Each step needs the ones before it.
  const steps = [
    {
      title: "Create your organisation",
      icon: icons.orgs,
      done: setup.orgs > 0,
      // No name to give before there is one, or for an operator looking at
      // all of them.
      body:
        ctx.orgID || ctx.state.me.org_id
          ? [
              "Your organisation ",
              h("strong", {}, orgNameOf(ctx)),
              " holds all your users, models, keys, etc.",
            ]
          : "Your organisation holds all your users, models, keys, etc.",
      // Only an operator can create one. An administrator already has theirs.
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

  // Only with card payments; invoiced organisations need no credit.
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

  // Last, as a request needs everything above it.
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

  return stepList(ctx, steps, providers);
}

export function memberRunDue(ctx, access) {
  return (
    !isAdmin(ctx) &&
    !!access &&
    !access.anonymous &&
    !access.operator_key &&
    !access.used
  );
}

/** An admin issues the key, so the first step is to get its secret: by
 *  activating the key, or rotating one already active. */
export function memberRun(ctx, access, providers) {
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
  return stepList(ctx, steps, providers);
}

/** For a member with no key to activate: an active key whose secret they may
 *  not have, or none. */
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

function stepList(ctx, steps, providers) {
  const nextIndex = steps.findIndex((s) => !s.done && !s.skipped);
  const button = (a, primary) =>
    h(
      "button",
      { class: "btn" + (primary ? " btn-primary" : ""), onClick: a.run },
      a.label,
    );

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
  return h("div", {}, overview(ctx, providers), list);
}

// The tools a developer connects, by their marks in ui.js.
const flowTools = [
  ["Claude Code", () => providerMark("anthropic")],
  ["OpenAI SDK", () => providerMark("openai")],
  ["OpenCode", () => clientMark("opencode")],
  // Keera Chat carries the same mark as the gateway.
  ["Keera Chat", () => brandMark(18)],
];

// Two frontier labs and two in Switzerland, from
// internal/catalog/providers.go.
const flowProviders = ["anthropic", "openai", "infomaniak", "cscs"];

const flowChecks = [
  ["API keys", icons.keys],
  ["Guardrails", icons.gate],
  ["Filters", icons.filters],
  ["Usage", icons.usage],
];

/** overview shows what the steps below set up: tools send requests through
 *  the gateway to the providers' models. Each provider shows its logo and one
 *  of its models, or its name when providers is empty. */
function overview(ctx, providers) {
  // A long model name is cut short on a narrow screen.
  const item = (mark, label) =>
    h("div", { class: "flow-item" }, mark, h("span", { title: label }, label));
  // The catalogue lists each provider's lead model first.
  const example = (name) =>
    (((providers || []).find((p) => p.name === name) || {}).models || [])[0];
  return h(
    "figure",
    { class: "flow" },
    h("div", { class: "flow-head" }, "Your tools"),
    h("div", { class: "flow-head flow-head-end" }, "Models"),
    h(
      "div",
      { class: "flow-list flow-tools" },
      flowTools.map(([label, mark]) => item(mark(), label)),
    ),
    flowLines(flowTools.length, false),
    h(
      "div",
      { class: "flow-gateway" },
      brandMark(40),
      h("strong", {}, "Keera Gateway"),
      h(
        "ul",
        {},
        flowChecks.map(([label, path]) => h("li", {}, icon(path), label)),
      ),
    ),
    flowLines(flowProviders.length, true),
    h(
      "div",
      { class: "flow-list flow-models" },
      flowProviders.map((p) => {
        const m = example(p);
        return item(
          providerMark(p),
          m ? m.display_name || m.id : providerName(p),
        );
      }),
    ),
    h(
      "figcaption",
      { class: "hint" },
      "Every request goes through the gateway. It checks the key and the " +
        "guardrails, runs the filters, then passes the request on to a model " +
        "and records the cost. Need more help? Read the ",
      h(
        "a",
        { href: docsURL(ctx), target: "_blank", rel: "noopener" },
        "documentation",
      ),
      ".",
    ),
  );
}

/** flowLines joins n equal rows to the gateway's edge. It stretches to the
 *  rows' height, so the strokes do not scale, to keep their width. */
function flowLines(n, out) {
  const height = n * 10;
  const mid = height / 2;
  return svg(
    "svg",
    {
      class: "flow-lines",
      viewBox: `0 0 100 ${height}`,
      preserveAspectRatio: "none",
      "aria-hidden": "true",
    },
    Array.from({ length: n }, (_, i) => {
      const y = i * 10 + 5;
      // The lines meet the gateway close together, not in one point.
      const g = mid + (y - mid) * 0.3;
      const [a, b] = out ? [g, y] : [y, g];
      return svg("path", {
        d: `M0 ${a} C50 ${a} 50 ${b} 100 ${b}`,
        "vector-effect": "non-scaling-stroke",
      });
    }),
  );
}
