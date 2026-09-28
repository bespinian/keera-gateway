// Connecting a coding agent to Keera Gateway.
//
// This screen exists because the last mile is where a gateway is actually
// adopted or quietly abandoned: an operator has models and keys, and a
// developer still has to work out which URL, which model and which file. It
// answers that for each client the deployment supports, with the model the
// developer picked already substituted in - so the thing they copy is the thing
// that works, not a template they have to fill in and get wrong once.

import { api, chatTargets } from "../api.js";
import { h, icon, icons, copyText, empty, go } from "../ui.js";
import { chooseOrg } from "./orgs.js";

// The configuration blocks come from the control plane at /control/v1/connect
// - the same catalogue `keera connect` reads, so the panel and the command
// line cannot hand a developer two versions of the same file. See
// internal/connect.
//
// The prose around a block comes with it. A span in backticks is something to
// type or a name to find, and is shown as code.

// The limits every block states, which connect.Limits on the server decides
// the same way. A config that named a window the gateway does not give is a
// coding agent that fills its context and is refused with a 413 on the turn
// after; a config that named none leaves the client guessing, and the guess is
// the same 128k for a model with a sixteenth of that.
const DEFAULT_CONTEXT = 128000;
const MAX_OUTPUT = 16384;
const MIN_OUTPUT = 1024;

/** limits turns a model's advertised window into the two numbers a block
 *  states. It has to agree with connect.Limits on the server. */
function limits(maxContext) {
  const window = maxContext > 0 ? maxContext : DEFAULT_CONTEXT;
  return {
    context: window,
    output: Math.max(MIN_OUTPUT, Math.min(MAX_OUTPUT, Math.floor(window / 4))),
  };
}

/** fill renders one of the catalogue's templates. It has to agree with
 *  connect.Render on the server, which does the same substitutions. */
function fill(template, base, alias, maxContext) {
  const { context, output } = limits(maxContext || 0);
  return String(template || "")
    .replaceAll("{{base}}", String(base || "").replace(/\/+$/, ""))
    .replaceAll("{{alias}}", alias)
    .replaceAll("{{name}}", title(alias))
    .replaceAll("{{context}}", String(context))
    .replaceAll("{{output}}", String(output));
}

// One fetch per page load. A catalogue that changed under a panel already open
// is picked up on the next reload, which is the same bargain every other list
// on this screen makes.
let catalogue;

/** loadClients reads the client catalogue and merges the panel's prose into it.
 *
 *  Every entry comes back with the same shape the screens here expect: a
 *  `config(base, model)` that renders the block, and `run`/`note` that build
 *  the paragraphs either side of it. So no screen rendering one has to know
 *  which client it is looking at. */
export function loadClients() {
  if (!catalogue) {
    catalogue = api.connect().then(
      (res) =>
        (res.data || []).map((c) => ({
          ...c,
          path: c.path || null,
          config: (base, m) => fill(c.template, base, m.alias, m.max_context),
          run: (m) => prose(fill(c.run, "", m.alias)),
          note: c.note ? () => prose(c.note) : null,
        })),
      (err) => {
        // A failure is not cached: the panel's Reload button, and the next
        // visit to a screen that needs this, have to be able to try again.
        catalogue = null;
        throw err;
      },
    );
  }
  return catalogue;
}

export async function connectView(ctx) {
  if (!ctx.orgID && ctx.state.me.unrestricted)
    return chooseOrg(ctx, "Connect a client");

  const [clients, models] = await Promise.all([
    loadClients(),
    chatTargets(ctx.orgID),
  ]);
  ctx.setSubtitle(
    "Pi, OpenCode, Claude Code or any OpenAI-compatible client, pointed at " +
      "this gateway",
  );

  if (!models.length) {
    return h(
      "div",
      { class: "card" },
      empty(
        "Nothing to connect to yet",
        "A coding agent needs an enabled chat model. An administrator can " +
          "add one under Models.",
      ),
    );
  }
  if (!clients.length) {
    return h(
      "div",
      { class: "card" },
      empty(
        "No client catalogue",
        "The gateway returned no clients, which should not happen. An " +
          "OpenAI-compatible client needs only the base URL and a key, and " +
          "My access has both.",
      ),
    );
  }

  const remembered = localStorage.getItem("keera.connect.model");
  const chosen = models.find((m) => m.alias === remembered) || models[0];
  const clientKey =
    localStorage.getItem("keera.connect.client") || clients[0].key;

  const state = {
    model: chosen,
    client: clients.find((c) => c.key === clientKey) || clients[0],
    base: defaultBase(ctx),
  };

  // Only the instructions are rebuilt when the model or the client changes:
  // the picker above them is not re-created, so the select keeps its focus.
  const steps = h("div");
  const render = () => steps.replaceChildren(instructions(ctx, state));

  const model = h(
    "select",
    {
      class: "select",
      onChange: (e) => {
        state.model =
          models.find((m) => m.alias === e.target.value) || models[0];
        localStorage.setItem("keera.connect.model", state.model.alias);
        render();
      },
    },
    models.map((m) =>
      h(
        "option",
        { value: m.alias, selected: m.alias === state.model.alias },
        m.router
          ? `${m.alias} - router`
          : m.max_context
            ? `${m.alias} - ${compactContext(m.max_context)} context`
            : m.alias,
      ),
    ),
  );

  // The model is the only choice on this screen. The gateway address had a
  // field next to this one, and it is gone rather than shown disabled: it is
  // not a decision, so a control for it - even one that refuses the keystroke -
  // only invites a developer to look for the way to change it. It is already
  // in the block below, which is where they need to read it, and an operator
  // whose panel is published under another name declares it with
  // KEERA_PUBLIC_URL so that every panel and `keera connect` agree.
  const picker = h(
    "div",
    { class: "card card-body", style: { marginBottom: "16px" } },
    h(
      "div",
      { class: "field", style: { marginBottom: 0 } },
      h("label", {}, "Model"),
      model,
      h(
        "div",
        { class: "hint" },
        "This is an alias. If it is pointed at another model, your editor " +
          "setup stays the same.",
      ),
    ),
  );

  const tabs = h(
    "div",
    { class: "row", style: { marginBottom: "16px" } },
    h(
      "div",
      { class: "seg" },
      clients.map((c) =>
        h(
          "button",
          {
            "aria-pressed": String(c.key === state.client.key),
            onClick: () => {
              state.client = c;
              localStorage.setItem("keera.connect.client", c.key);
              for (const el of tabs.querySelectorAll("button")) {
                el.setAttribute(
                  "aria-pressed",
                  String(el.textContent === c.label),
                );
              }
              render();
            },
          },
          c.label,
        ),
      ),
    ),
  );

  render();
  return h(
    "div",
    {},
    h(
      "div",
      { class: "muted", style: { marginBottom: "16px" } },
      "Pick a model and a client, then copy the result. It is ready to use " +
        "as is.",
    ),
    picker,
    tabs,
    steps,
  );
}

function instructions(ctx, state) {
  const { client, model, base } = state;
  const config = client.config(base || location.origin + "/api", model);

  return h(
    "div",
    { class: "steps" },
    step(
      1,
      "Get a key",
      h(
        "div",
        {},
        h(
          "p",
          { class: "muted" },
          "Budgets, rate limits and the audit log are tracked per key. A key " +
            "is shown only once, when it is issued.",
        ),
        h(
          "p",
          { class: "muted" },
          "Issue one under ",
          h(
            "a",
            {
              href: "/keys",
              onClick: go(ctx, "/keys"),
            },
            "API keys",
          ),
          ", or ask an administrator. Then add it to your shell profile:",
        ),
        code("export KEERA_API_KEY=keera_sk_…"),
      ),
    ),

    step(
      2,
      client.path ? "Configure " + client.label : "Point it at the gateway",
      h(
        "div",
        {},
        h(
          "p",
          { class: "muted" },
          client.path
            ? [
                "Save this as ",
                h("code", {}, client.path),
                ". If the file exists, merge these settings into it instead " +
                  "of replacing it.",
              ]
            : "Add these to your shell profile, so every client on the " +
                "machine picks them up.",
        ),
        code(config),
        client.note ? h("p", { class: "muted" }, client.note(model)) : null,
      ),
    ),

    step(3, "Run it", h("p", { class: "muted" }, client.run(model))),
  );
}

function step(n, title, body) {
  return h(
    "div",
    { class: "step" },
    h("div", { class: "step-n" }, String(n)),
    h(
      "div",
      { class: "step-body" },
      h("h2", { style: { marginBottom: "6px" } }, title),
      body,
    ),
  );
}

// code renders something to be copied, with the button that copies it. Every
// block on this screen is meant to leave the panel and land in a file, so none
// of them is shown without one.
export function code(text) {
  return h(
    "div",
    { class: "code" },
    h(
      "button",
      {
        class: "btn btn-sm code-copy",
        title: "Copy",
        "aria-label": "Copy",
        onClick: () => copyText(text),
      },
      icon(icons.copy),
    ),
    h("pre", {}, h("code", {}, text)),
  );
}

/** defaultBase is the gateway as the control plane believes a client sees it.
 *
 *  The fallback is this page's own origin plus the inference prefix, because
 *  the panel and the inference API are one listener: whatever address reached
 *  this screen reaches the gateway too. */
export function defaultBase(ctx) {
  const declared = ctx.state.me.gateway_url;
  if (declared) return declared.replace(/\/+$/, "");
  return location.origin + "/api";
}

/** prose renders the catalogue's prose, with each span in backticks as code. */
function prose(text) {
  return String(text || "")
    .split("`")
    .map((part, i) => (i % 2 ? h("code", {}, part) : part));
}

/** title turns an alias into the label a model picker shows: keera-speed → Keera Speed. */
export function title(alias) {
  return alias
    .split(/[-_.\s]+/)
    .filter(Boolean)
    .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
    .join(" ");
}

function compactContext(n) {
  return n >= 1000 ? Math.round(n / 1000) + "k" : String(n);
}
