// Connecting a coding agent to Keera Gateway.
//
// This screen exists because the last mile is where a gateway is actually
// adopted or quietly abandoned: an organisation has models and keys, and a
// developer still has to work out which URL, which model and which file. It
// answers that for each client the deployment supports, with every model the
// developer's key may call already filled in - so the thing they copy is the
// thing that works, not a template they have to fill in and get wrong once.

import { api, chatTargets } from "../api.js";
import {
  h,
  icon,
  icons,
  copyText,
  empty,
  go,
  compact,
  isAdmin,
} from "../ui.js";

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

/** fill substitutes one model's placeholders. It has to agree with
 *  connect.fill on the server. */
function fill(text, m) {
  const { context, output } = limits((m && m.max_context) || 0);
  const alias = (m && m.alias) || "";
  return String(text || "")
    .replaceAll("{{alias}}", alias)
    .replaceAll("{{name}}", title(alias))
    .replaceAll("{{context}}", String(context))
    .replaceAll("{{output}}", String(output));
}

/** render fills in a client's block for a key's models, the default first. It
 *  has to agree with connect.Render on the server. */
function render(c, base, models) {
  return fill(c.template, models[0])
    .replaceAll("{{base}}", String(base || "").replace(/\/+$/, ""))
    .replaceAll("{{models}}", models.map((m) => fill(c.entry, m)).join(",\n"))
    .replaceAll("{{aliases}}", models.map((m) => m.alias).join(", "));
}

// One fetch per page load. A catalogue that changed under a panel already open
// is picked up on the next reload, which is the same bargain every other list
// on this screen makes.
let catalogue;

/** loadClients reads the client catalogue and merges the panel's prose into it.
 *
 *  Every entry comes back with the same shape the screens here expect: a
 *  `config(base, models)` that renders the block, and `run`/`note` that build
 *  the paragraphs either side of it. So no screen rendering one has to know
 *  which client it is looking at. */
export function loadClients() {
  if (!catalogue) {
    catalogue = api.connect().then(
      (res) =>
        (res.data || []).map((c) => ({
          ...c,
          path: c.path || null,
          config: (base, models) => render(c, base, models),
          run: (models) => prose(fill(c.run, models[0])),
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

/** keyModels is the chat models and routers the key may call, in the
 *  organisation's order. A key from /v1/access carries them resolved. */
export function keyModels(targets, allowed) {
  return targets.filter((m) => (allowed || []).includes(m.alias));
}

/** usableKeys is the reader's own keys that work from a client or the
 *  playground. A subscription key works only from Claude Code signed in to a
 *  Claude plan, which `keera connect claude-code --subscription` sets up. */
export function usableKeys(a) {
  return (a.keys || []).filter(
    (k) => k.state === "active" && k.kind !== "subscription",
  );
}

export async function connectView(ctx) {
  // Only the reader's own keys: the configuration is for their machine.
  const [clients, a] = await Promise.all([loadClients(), api.access()]);
  ctx.setSubtitle(
    "Pi, OpenCode, Claude Code or any OpenAI-compatible client, pointed at " +
      "this gateway",
  );
  const keys = usableKeys(a);
  if (!keys.length) return noKey(ctx, a, "A client connects with");
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

  // Every key here is in the reader's own organisation, so one read covers
  // them all.
  const targets = await chatTargets(keys[0].org_id);

  const remembered = localStorage.getItem("keera.connect.key");
  const clientKey =
    localStorage.getItem("keera.connect.client") || clients[0].key;
  const state = {
    key: keys.find((k) => k.id === remembered) || keys[0],
    client: clients.find((c) => c.key === clientKey) || clients[0],
    base: defaultBase(ctx),
  };

  // Only what depends on the key or the client is rebuilt when they change:
  // the picker is not re-created, so the select keeps its focus.
  const models = h("div", { class: "hint" });
  const steps = h("div");
  const redraw = () => {
    const usable = keyModels(targets, state.key.allowed_models);
    models.replaceChildren(...modelList(usable));
    steps.replaceChildren(
      usable.length
        ? instructions(ctx, state, usable)
        : h(
            "div",
            { class: "card" },
            empty(
              "This key can use no chat model",
              "Its guardrails allow no model that serves chat. Choose another " +
                "key, or ask an administrator.",
            ),
          ),
    );
  };

  const key = h(
    "select",
    {
      class: "select",
      onChange: (e) => {
        state.key = keys.find((k) => k.id === e.target.value) || keys[0];
        localStorage.setItem("keera.connect.key", state.key.id);
        redraw();
      },
    },
    keys.map((k) =>
      h(
        "option",
        { value: k.id, selected: k.id === state.key.id },
        `${k.name} - ${k.prefix}…`,
      ),
    ),
  );

  // The key is the only choice on this screen. The models follow from it, and
  // the gateway address is not a decision: it is already in the block below,
  // and an operator whose panel is published under another name declares it
  // with KEERA_PUBLIC_URL so that every panel and `keera connect` agree.
  const picker = h(
    "div",
    { class: "card card-body", style: { marginBottom: "16px" } },
    h(
      "div",
      { class: "field", style: { marginBottom: 0 } },
      h("label", {}, "API key"),
      key,
      models,
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
              redraw();
            },
          },
          c.label,
        ),
      ),
    ),
  );

  redraw();
  return h(
    "div",
    {},
    h(
      "div",
      { class: "muted", style: { marginBottom: "16px" } },
      "Pick one of your keys and a client, then copy the result. It sets up " +
        "every model the key can use.",
    ),
    picker,
    tabs,
    steps,
  );
}

/** modelList names the models a configuration sets up, with their windows. */
function modelList(models) {
  if (!models.length) return [];
  return [
    "This key can use ",
    ...models.flatMap((m, i) => [
      i ? ", " : "",
      h("code", {}, m.alias),
      m.router
        ? " (router)"
        : m.max_context
          ? ` (${compact(m.max_context)} context)`
          : "",
    ]),
    ". The first is the default.",
  ];
}

/** noKey explains why the reader has no key to use, and where to get one.
 *  lead says what the key is for, as in "A client connects with". */
export function noKey(ctx, a, lead) {
  if (a.anonymous || (a.operator_key && !(a.keys || []).length)) {
    return h(
      "div",
      { class: "card" },
      empty(
        "The operator key has no API keys",
        `${lead} one of your own API keys. Sign in with your identity ` +
          "provider to use yours.",
      ),
    );
  }
  const canIssue = isAdmin(ctx);
  return h(
    "div",
    { class: "card" },
    empty(
      "You have no active API key",
      canIssue
        ? h(
            "span",
            {},
            `${lead} one of your own keys. Issue one on `,
            h("a", { href: "/keys", onClick: go(ctx, "/keys") }, "API keys"),
            ".",
          )
        : `${lead} one of your own keys. Ask an administrator for a key ` +
            "in your name.",
    ),
  );
}

function instructions(ctx, state, models) {
  const { client, key, base } = state;
  const config = client.config(base, models);

  return h(
    "div",
    { class: "steps" },
    step(
      1,
      "Set your key",
      h(
        "div",
        {},
        h(
          "p",
          { class: "muted" },
          "Add the key ",
          h("strong", {}, key.name),
          " to your shell profile. It was shown only once, when it was " +
            "issued. If you no longer have it, rotate it under ",
          h("a", { href: "/keys", onClick: go(ctx, "/keys") }, "API keys"),
          ".",
        ),
        code(`export KEERA_API_KEY=${key.prefix}…`),
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
        client.note ? h("p", { class: "muted" }, client.note()) : null,
      ),
    ),

    step(3, "Run it", h("p", { class: "muted" }, client.run(models))),
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
function title(alias) {
  return alias
    .split(/[-_.\s]+/)
    .filter(Boolean)
    .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
    .join(" ");
}
