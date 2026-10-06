// My clients: the coding agents that call Keera Gateway with the reader's
// keys, and how to add one.
//
// Connect client exists because the last mile is where a gateway is actually
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
  table,
  rowLink,
  num,
  plural,
  ago,
  dateTime,
  toast,
  modeTiles,
  modal,
  providerMark,
  currentRange,
  rangePicker,
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

export async function clientsView(ctx) {
  const since = currentRange();
  // Only the reader's own keys: the setup is for their machine.
  const [catalogue, a, res] = await Promise.all([
    loadClients(),
    api.access(),
    api.clients(since),
  ]);
  const used = res.data || [];
  const keys = usableKeys(a);
  ctx.setSubtitle(
    `${used.length} ${plural(used.length, "client")} called with your keys`,
  );

  const canAdd = keys.length > 0 && catalogue.length > 0;
  const head = h(
    "div",
    { class: "detail-head" },
    h(
      "div",
      { class: "muted" },
      "The clients that called the gateway with your keys, and which key " +
        "each one used. A client shows here after its first request.",
    ),
    h(
      "div",
      { class: "row", style: { flexWrap: "wrap" } },
      h("div", { style: { flex: 1 } }),
      rangePicker(ctx, since),
      canAdd
        ? h(
            "button",
            {
              class: "btn btn-primary",
              onClick: (e) => addClient(ctx, catalogue, keys, e.currentTarget),
            },
            icon(icons.plus),
            "Connect client",
          )
        : null,
    ),
  );

  const list = table(
    [
      {
        label: "Client",
        sortKey: (c) => c.label,
        cell: (c) =>
          h(
            "span",
            { class: c.key ? null : "muted" },
            h("strong", {}, c.label),
          ),
      },
      {
        label: "Keys",
        cell: (c) =>
          c.keys.flatMap((k, i) => [
            i ? ", " : "",
            rowLink(
              ctx,
              "/keys/" + encodeURIComponent(k.id),
              k.name || k.id,
              `${num(k.requests)} ${plural(k.requests, "request")}, ` +
                `last ${ago(k.last_used)}`,
            ),
          ]),
      },
      {
        label: "Requests",
        num: true,
        shrink: true,
        sortKey: (c) => c.requests,
        sortDir: "desc",
        cell: (c) => num(c.requests),
      },
      {
        label: "Last used",
        shrink: true,
        sortKey: (c) => c.last_used,
        sortDir: "desc",
        cell: (c) =>
          h("span", { title: dateTime(c.last_used) }, ago(c.last_used)),
      },
    ],
    used,
    {
      emptyTitle: "No client has called in this window",
      emptyBody: canAdd
        ? "Use Connect client to set one up."
        : "You need an API key to set one up.",
    },
  );

  return h(
    "div",
    { class: "stack" },
    head,
    // Without a key there is nothing to add, so the reason comes first.
    keys.length ? null : noKey(ctx, a, "A client connects with"),
    list,
  );
}

/** addClient asks which client to set up, then shows its setup for one of the
 *  reader's keys. */
async function addClient(ctx, catalogue, keys, button) {
  // Every key here is in the reader's own organisation, so one read covers
  // them all.
  button.disabled = true;
  let targets;
  try {
    targets = await chatTargets(keys[0].org_id);
  } catch (err) {
    toast(err.message, "bad");
    return;
  } finally {
    button.disabled = false;
  }

  const remembered = localStorage.getItem("keera.connect.key");
  const state = {
    key: keys.find((k) => k.id === remembered) || keys[0],
    client: null,
    base: defaultBase(ctx),
  };

  // Only what depends on the key or the client is rebuilt when they change,
  // so the select keeps its focus.
  const models = h("div", { class: "hint" });
  const steps = h("div");
  const redraw = () => {
    const usable = keyModels(targets, state.key.allowed_models);
    models.replaceChildren(...modelList(usable));
    steps.replaceChildren(
      usable.length
        ? instructions(ctx, state, usable)
        : empty(
            "This key can use no chat model",
            "Its guardrails allow no model that serves chat. Choose another " +
              "key, or ask an administrator.",
          ),
    );
  };

  const keySelect = h(
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
        k.name || k.prefix,
      ),
    ),
  );

  // The key is the only choice after the client. The models follow from it,
  // and the gateway address is already in the setup: an operator whose panel
  // is published under another name sets KEERA_PUBLIC_URL, so that every
  // panel and `keera connect` agree.
  const setup = h(
    "div",
    { hidden: true },
    h("div", { class: "field" }, h("label", {}, "API key"), keySelect, models),
    steps,
  );

  const picker = modeTiles(
    catalogue.map((c) => ({
      value: c.key,
      icon: icons.terminal,
      mark: clientMarks[c.key],
      name: c.label,
      what: c.about,
    })),
    null,
    (value) => {
      state.client = catalogue.find((c) => c.key === value);
      setup.hidden = false;
      redraw();
      picker.change.focus();
    },
  );

  modal({
    wide: true,
    title: "Connect client",
    subtitle:
      "Choose a client, then copy its setup. It sets up every model " +
      "your key can use.",
    body: h(
      "div",
      {},
      h(
        "div",
        { class: "field" },
        h("label", {}, "Client"),
        picker.tiles,
        picker.chosenRow,
      ),
      setup,
    ),
    actions: (close) => [h("button", { class: "btn", onClick: close }, "Done")],
  });
  picker.tiles.firstChild.focus();
}

// clientMarks are the vendors' own logos, for the clients that have one here.
const clientMarks = {
  "claude-code": () => providerMark("anthropic"),
  openai: () => providerMark("openai"),
};

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
