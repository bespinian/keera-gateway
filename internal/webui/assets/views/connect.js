// My clients: the coding agents that call the gateway with the reader's keys,
// and how to add one. Connect client fills in the key's models, so what is
// copied works as it is.

import { api, chatTargets } from "../api.js";
import {
  h,
  icons,
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
  clientMark,
  providerMark,
  currentRange,
  rangePicker,
  listHead,
  code,
  remembered,
  remember,
} from "../ui.js";

// The control plane fills in the configuration blocks, at
// /control/v1/connect/render, with the same code `keera connect` runs, so the
// panel and the command line always agree. See internal/connect.
//
// The prose around a block comes with it. A span in backticks is shown as code.

// One fetch per page load. A changed catalogue shows after a reload.
let catalogue;

/** loadClients reads the client catalogue: what the picker offers, and the
 *  prose that does not depend on the models. */
function loadClients() {
  if (!catalogue) {
    catalogue = api.connect().then(
      (res) =>
        (res.data || []).map((c) => ({
          ...c,
          path: c.path || null,
          holdsKey: String(c.template || "").includes("{{key}}"),
        })),
      (err) => {
        // A failure is not cached, so a reload can try again.
        catalogue = null;
        throw err;
      },
    );
  }
  return catalogue;
}

/** The first of models becomes the default in every block. */
function renderFor(models) {
  return api.renderConnect(
    models.map((m) => ({
      alias: m.alias,
      name: m.display_name,
      max_context_tokens: m.max_context_tokens,
      backend_model: m.backend_model,
      provider: m.provider,
    })),
  );
}

/** A key from /v1/access carries its allowed models resolved. */
export function keyModels(targets, allowed) {
  return targets.filter((m) => (allowed || []).includes(m.alias));
}

/** usableKeys drops subscription keys: they work only from Claude Code
 *  signed in to a Claude plan, which `keera connect claude-code
 *  --subscription` sets up. */
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
  const head = listHead(
    "The clients that called the gateway with your keys, and which key " +
      "each one used. A client shows here after its first request.",
    [rangePicker(ctx, since)],
    canAdd && {
      label: "Connect client",
      onClick: (e) => addClient(ctx, catalogue, keys, e.currentTarget),
    },
  );

  const list = table(
    [
      {
        label: "Client",
        sortKey: (c) => c.label,
        cell: (c) =>
          h(
            "span",
            { class: c.client ? null : "muted" },
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
                `last ${ago(k.last_used_at)}`,
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
        sortKey: (c) => c.last_used_at,
        sortDir: "desc",
        cell: (c) =>
          h("span", { title: dateTime(c.last_used_at) }, ago(c.last_used_at)),
      },
    ],
    used,
    {
      emptyTitle: "No client has called in this window",
      emptyBody: canAdd
        ? "Use Connect client to set one up."
        : [
            "You need an ",
            h("a", { href: "/keys", onClick: go(ctx, "/keys") }, "API key"),
            " to set one up.",
          ],
    },
  );

  return h("div", { class: "stack" }, head, list);
}

/** openConnectClient opens Connect client from another screen. Without a key
 *  or a client to offer, it goes to My clients, which says why. created is a
 *  key whose secret was just shown, which the setup fills in. It resolves to
 *  whether the dialog opened. */
export async function openConnectClient(ctx, button, created = null) {
  button.disabled = true;
  let catalogue, a;
  try {
    [catalogue, a] = await Promise.all([loadClients(), api.access()]);
  } catch (err) {
    toast(err.message, "bad");
    return false;
  } finally {
    button.disabled = false;
  }
  let keys = usableKeys(a);
  if (created) keys = keys.filter((k) => k.id === created.id);
  if (!keys.length || !catalogue.length) {
    // The secret dialog stays open: leaving it would lose the secret.
    if (created) {
      toast("This key can set up no client here.", "bad");
      return false;
    }
    ctx.navigate("/clients");
    return false;
  }
  return addClient(ctx, catalogue, keys, button, created && created.key);
}

/** addClient asks which client to set up, then shows its setup for one of the
 *  reader's keys. secret, when given, replaces the placeholder. It resolves to
 *  whether the dialog opened. */
async function addClient(ctx, catalogue, keys, button, secret = null) {
  // Every key here is in the reader's own organisation, so one read covers
  // them all.
  button.disabled = true;
  let targets;
  try {
    targets = await chatTargets(keys[0].org_id);
  } catch (err) {
    toast(err.message, "bad");
    return false;
  } finally {
    button.disabled = false;
  }

  const last = remembered("keera.connect.key", { lasting: true });
  const state = {
    key: keys.find((k) => k.id === last) || keys[0],
    client: null,
    secret,
  };

  // Only what depends on the key or the client is rebuilt when they change,
  // so the select keeps its focus.
  const models = h("div", { class: "hint" });
  const steps = h("div");
  // Each key's blocks are read once. A later choice that is drawn first is
  // not overwritten by an earlier, slower answer.
  const rendered = new Map();
  let drawing = 0;
  const redraw = async () => {
    const usable = keyModels(targets, state.key.allowed_models);
    models.replaceChildren(...modelList(usable));
    if (!usable.length) {
      steps.replaceChildren(noModel());
      return;
    }
    const turn = ++drawing;
    const id = state.key.id;
    if (!rendered.has(id)) rendered.set(id, renderFor(usable));
    let res;
    try {
      res = await rendered.get(id);
    } catch (err) {
      rendered.delete(id);
      if (turn === drawing) toast(err.message, "bad");
      return;
    }
    if (turn !== drawing) return;
    const block = (res.data || []).find((b) => b.client === state.client.client);
    steps.replaceChildren(
      block ? instructions(ctx, state, block, res.placeholder) : "",
    );
  };

  const keySelect = h(
    "select",
    {
      class: "select",
      onChange: (e) => {
        state.key = keys.find((k) => k.id === e.target.value) || keys[0];
        remember("keera.connect.key", state.key.id, { lasting: true });
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
  // and the gateway address from the control plane, which builds it in one
  // place for every panel and `keera connect`.
  const setup = h(
    "div",
    { hidden: true },
    h("div", { class: "field" }, h("label", {}, "API key"), keySelect, models),
    steps,
  );

  const picker = modeTiles(
    catalogue.map((c) => ({
      value: c.client,
      icon: icons.terminal,
      mark: clientMarks[c.client],
      name: c.label,
      what: c.about,
    })),
    null,
    (value) => {
      state.client = catalogue.find((c) => c.client === value);
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
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Close"),
    ],
  });
  picker.tiles.firstChild.focus();
  return true;
}

const clientMarks = {
  "claude-code": () => providerMark("anthropic"),
  codex: () => providerMark("openai"),
  opencode: () => clientMark("opencode"),
  pi: () => clientMark("pi"),
  python: () => clientMark("python"),
};

function modelList(models) {
  if (!models.length) return [];
  return [
    "This key can use ",
    ...models.flatMap((m, i) => [
      i ? ", " : "",
      h("code", {}, m.alias),
      m.router
        ? " (router)"
        : m.max_context_tokens
          ? ` (${compact(m.max_context_tokens)} context)`
          : "",
    ]),
    ". The first is the default.",
  ];
}

export function noKey(ctx, a) {
  if (a.anonymous || (a.operator_key && !(a.keys || []).length)) {
    return h(
      "div",
      { class: "card" },
      empty(
        "You signed in with the operator key",
        "It is not a person, so it has no API keys, project or budget. Sign " +
          "in with your identity provider to see yours.",
      ),
    );
  }
  // A key issued without a user still works, but nothing links it to the
  // reader, so it is not counted here.
  const unlinked =
    "A key you already have may not be linked to you. It still works, but " +
    "it does not show here.";
  return h(
    "div",
    { class: "card" },
    empty(
      "You have no active API key",
      isAdmin(ctx)
        ? h(
            "span",
            {},
            "Issue one on ",
            h("a", { href: "/keys", onClick: go(ctx, "/keys") }, "API keys"),
            ". " + unlinked,
          )
        : "Ask an administrator for a key in your name. " + unlinked,
    ),
  );
}

export function noModel() {
  return empty(
    "This key can use no chat model",
    "Its guardrails allow no model that serves chat. Choose another key, or " +
      "ask an administrator.",
  );
}

function instructions(ctx, state, block, placeholder) {
  const { client, key, secret } = state;
  // A block that holds the key shows it by its prefix once its secret is
  // gone. It sits in a JSON or TOML string.
  const shown = JSON.stringify(secret || `${key.prefix}…`).slice(1, -1);
  const config = block.config.replaceAll(placeholder, shown);
  const rotate = [
    "It was shown only once, when it was activated. If you no longer have it, " +
      "rotate it under ",
    h("a", { href: "/keys", onClick: go(ctx, "/keys") }, "API keys"),
    ".",
  ];

  return h(
    "div",
    { class: "steps" },
    step(
      1,
      "Set your key",
      client.holdsKey
        ? h(
            "p",
            { class: "muted" },
            "The settings below hold the key ",
            h("strong", {}, key.name),
            ". ",
            secret
              ? "It is filled in, and this is the only time it is shown."
              : [
                  "Put it in place of ",
                  h("code", {}, `${key.prefix}…`),
                  ". ",
                  ...rotate,
                ],
          )
        : secret
          ? h(
              "div",
              {},
              h(
                "p",
                { class: "muted" },
                "Add the key ",
                h("strong", {}, key.name),
                " to your shell profile. This is the only time it is shown.",
              ),
              code(`export KEERA_API_KEY=${secret}`),
            )
          : h(
              "div",
              {},
              h(
                "p",
                { class: "muted" },
                "Add the key ",
                h("strong", {}, key.name),
                " to your shell profile. ",
                ...rotate,
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
                // A path outside the home directory is a new file, so there
                // is nothing to merge into.
                client.path.startsWith("~/")
                  ? ". If the file exists, merge these settings into it " +
                    "instead of replacing it."
                  : ".",
              ]
            : "Run this in a shell.",
        ),
        code(config),
        client.note ? h("p", { class: "muted" }, prose(client.note)) : null,
      ),
    ),

    step(3, "Run it", h("p", { class: "muted" }, prose(block.run))),
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

// A span in backticks is shown as code.
function prose(text) {
  return String(text || "")
    .split("`")
    .map((part, i) => (i % 2 ? h("code", {}, part) : part));
}
