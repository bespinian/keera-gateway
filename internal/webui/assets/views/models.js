// The model catalogue.
//
// A model is an API contract shared by every tenant, so only an operator can
// change one; everybody else reads it, because a developer needs to know what
// the models are.

import { api } from "../api.js";
import {
  h,
  table,
  modal,
  confirm,
  toast,
  pill,
  money,
  num,
  compact,
  ms,
  icon,
  icons,
  copyText,
  rowLink,
  providerMark,
  showError,
} from "../ui.js";

export async function modelsView(ctx) {
  const models = (await api.models()).data || [];
  const canEdit = ctx.state.me.can_edit_catalogue;
  // The hosted endpoints this binary knows. They only fill in a form, so a
  // deployment that cannot reach them still gets a working catalogue screen.
  const providers = canEdit ? await presets() : [];
  ctx.setSubtitle(`${models.length} model${models.length === 1 ? "" : "s"}`);

  const head = h(
    "div",
    { class: "row", style: { marginBottom: "16px" } },
    h(
      "div",
      { class: "muted" },
      "Clients use a model's alias, not the backend model id or URL. So the " +
        "backend can change without any client changes.",
    ),
    h("div", { style: { flex: 1 } }),
    canEdit
      ? h(
          "button",
          {
            class: "btn btn-primary",
            onClick: () => editModel(ctx, null, providers),
          },
          icon(icons.plus),
          "New model",
        )
      : null,
  );

  const rows = table(
    [
      {
        // The alias opens the model's own screen: whether it is being used, by
        // whom, how fast it is answering and what it has been failing with. None
        // of that is configuration, which is what the rest of this row is.
        label: "Alias",
        sortKey: (m) => m.alias,
        cell: (m) =>
          h(
            "div",
            { class: "stack" },
            h(
              "span",
              { class: "mono" },
              rowLink(
                ctx,
                "/models/" + encodeURIComponent(m.alias),
                m.alias,
                "This model's traffic and latency",
              ),
            ),
            h(
              "span",
              { class: "faint", style: { fontSize: "11.5px" } },
              "serves " + m.backend_model,
            ),
          ),
      },
      {
        label: "Kind",
        shrink: true,
        sortKey: (m) => m.kind,
        cell: (m) => pill(m.kind),
      },
      {
        // The provider rather than the addresses it explains. What a row is
        // read for is whether a prompt sent to this alias leaves the cluster,
        // not which URL it sits behind - that is one click away in the model's
        // own dialog. So it is a pill, and it sorts, because grouping the
        // hosted models is the same question asked of the whole list.
        //
        // A model on an inference plane of your own gets a dash and not a
        // claim: no provider is not the same as inside the cluster, since
        // nothing stops an address of your own from pointing anywhere.
        label: "Provider",
        shrink: true,
        sortKey: (m) => m.provider || "",
        cell: (m) =>
          m.provider
            ? h(
                "span",
                {
                  title:
                    "Hosted provider: prompts to this model leave your " +
                    "infrastructure",
                },
                // The provider's own mark, where this build has one. It is
                // read faster than the word beside it down a column of them,
                // which is the whole of what it is for - so the word stays: a
                // logo is not a label, and a provider this build has no mark
                // for has to read the same as one it does.
                pill([providerMark(m.provider), m.provider], "warn"),
              )
            : h("span", { class: "faint" }, "-"),
      },
      {
        label: "Context",
        num: true,
        shrink: true,
        sortKey: (m) => m.max_context || null,
        sortDir: "desc",
        cell: (m) =>
          m.max_context
            ? compact(m.max_context)
            : h("span", { class: "faint" }, "-"),
      },
      {
        label: "Price / Mtok",
        num: true,
        shrink: true,
        // By what a prompt costs, which is the number a catalogue is read for.
        sortKey: (m) => m.input_micros_per_mtok || null,
        sortDir: "desc",
        cell: (m) =>
          m.input_micros_per_mtok || m.output_micros_per_mtok
            ? h(
                "span",
                { class: "nowrap muted" },
                `${money(m.input_micros_per_mtok, "")} in · ${money(m.output_micros_per_mtok, "")} out`,
              )
            : h("span", { class: "faint" }, "not billed"),
      },
      {
        // Enabled says an operator turned it on. Health says the inference plane
        // actually answers, and - the one that matters - that a tool call comes
        // back as a tool call. They are not the same claim and the panel used to
        // only make the first.
        label: "Status",
        shrink: true,
        sortKey: (m) => (m.enabled ? 1 : 0),
        cell: (m) =>
          h(
            "div",
            { class: "stack" },
            m.enabled ? pill("Enabled", "good") : pill("Disabled", "warn"),
            canEdit ? healthCell(m) : null,
          ),
      },
      {
        label: "",
        shrink: true,
        cell: (m) => {
          // A member gets the same row opened for reading. What a model is
          // configured to do is what their requests to it will meet, so the
          // answer to "why does this one cost that much" or "how large a request
          // may I send" is here rather than in a message to an operator.
          if (!canEdit) {
            return h(
              "button",
              {
                class: "btn btn-sm",
                title: "What this model is configured to do",
                onClick: () => viewModel(ctx, m),
              },
              "View",
            );
          }
          return h(
            "div",
            { class: "row-tight" },
            h(
              "button",
              {
                class: "btn btn-sm",
                title: "What this model serves, and on what",
                onClick: () => editModel(ctx, m, providers),
              },
              "Edit",
            ),
            // A model the catalogue file declares is applied again on every
            // start, so removing it here would last until the next one.
            m.managed
              ? null
              : h(
                  "button",
                  {
                    class: "btn btn-sm btn-danger",
                    onClick: () =>
                      confirm({
                        title: `Remove ${m.alias}?`,
                        body:
                          "Clients that still use this model will get 404s. " +
                          "Disable it instead to keep its reports.",
                        confirmLabel: "Remove model",
                        danger: true,
                        onConfirm: async () => {
                          await api.deleteModel(m.alias);
                          toast("Model removed", "good");
                          ctx.reload();
                        },
                      }),
                  },
                  "Remove",
                ),
          );
        },
      },
    ],
    models,
    {
      emptyTitle: "The catalogue is empty",
      emptyBody:
        "Every inference request is refused until a model exists. Add " +
        "one and check it before handing out keys.",
      // The backend model and the endpoints are searched as well as the alias,
      // the endpoints even though no column shows them any more: "which models
      // still point at the old cluster" is a question asked of this screen
      // whenever one is being moved, and the rows that come back are the
      // answer whether or not the URL that matched is on them.
      search: (m) =>
        [
          m.alias,
          m.backend_model,
          m.kind,
          m.provider || "",
          ...(m.backends || []),
        ].join(" "),
      searchLabel: "models",
    },
  );

  return h("div", {}, head, rows);
}


// openModel is this model's dialog opened from somewhere other than the
// catalogue - the model's own screen, which links here rather than growing a
// second copy of the same form. Which of the two dialogs it is depends on the
// reader, exactly as it does on the row.
export async function openModel(ctx, m) {
  if (!ctx.state.me.can_edit_catalogue) return viewModel(ctx, m);
  return editModel(ctx, m, await presets());
}

// viewModel is the model as a member meets it: what it is called, what it
// serves, how large a request may be, and what a token costs.
//
// It is not the edit form with its fields disabled. Half of that form is where
// the model points - the endpoints and the credential - and the control plane
// sends neither to anybody but an operator, so a disabled form would mostly be
// empty boxes with no explanation for being empty. What is left is what a
// member is actually held to, and it reads better as an answer than as a field
// they cannot fill in.
function viewModel(ctx, m) {
  modal({
    title: `Model - ${m.alias}`,
    subtitle:
      "How this model is set up. My access shows whether your keys can " +
      "use it.",
    body: h(
      "div",
      {},
      readRow(
        "Alias",
        h("span", { class: "mono" }, m.alias),
        "The name clients use. The backend behind it can change without " +
          "client changes.",
      ),
      readRow("Kind", pill(m.kind), surfaceOf(m.kind)),
      readRow(
        "What it is for",
        m.description
          ? h("span", {}, m.description)
          : h("span", { class: "muted" }, "nothing written down"),
        "Listed on /api/v1/models and shown to routers choosing a model.",
      ),
      readRow(
        "Model it serves",
        h("span", { class: "mono" }, m.backend_model),
        "The model the backend runs.",
      ),
      h(
        "div",
        { class: "field-row" },
        readRow(
          "Context window",
          m.max_context
            ? h(
                "span",
                {},
                `${compact(m.max_context)} tokens`,
                h("span", { class: "hint-origin" }, ` - ${num(m.max_context)}`),
              )
            : h("span", { class: "muted" }, "not advertised"),
          "Advertised to clients. A request that cannot fit is refused.",
        ),
        readRow(
          "Status",
          m.enabled ? pill("Enabled", "good") : pill("Disabled", "warn"),
          m.enabled
            ? "This does not mean the backend is answering right now."
            : "Refused as if the model did not exist.",
        ),
      ),
      readRow(
        `Price per million tokens (${ctx.currency})`,
        m.input_micros_per_mtok || m.output_micros_per_mtok
          ? h(
              "span",
              { class: "nowrap" },
              `${money(m.input_micros_per_mtok, "")} in · ` +
                `${money(m.output_micros_per_mtok, "")} out`,
            )
          : h("span", { class: "muted" }, "not billed"),
        "What requests to this model count against your budgets." +
          (m.kind === "chat"
            ? " Clients resend the whole conversation each turn, so input " +
              "is charged again."
            : ""),
      ),
    ),
    actions: (close) => [
      h("button", { class: "btn btn-primary", onClick: close }, "Close"),
    ],
  });
}

function readRow(label, value, hint) {
  return h(
    "div",
    { class: "field" },
    h("label", {}, label),
    h("div", {}, value),
    hint ? h("div", { class: "hint" }, hint) : null,
  );
}

// surfaceOf names the endpoint a kind answers on. "chat" is the kind; the thing
// a developer has to get right is the path their client posts to.
function surfaceOf(kind) {
  switch (kind) {
    case "chat":
      return (
        "Answers /api/v1/chat/completions, /api/v1/messages for an " +
        "Anthropic-shaped client and /api/v1/responses for Codex."
      );
    case "completion":
      return "Answers /api/v1/completions.";
    case "embedding":
      return "Answers /api/v1/embeddings.";
    default:
      return null;
  }
}

// healthCell is the check button and, once it has run, its verdict in place.
// The result is deliberately not cached anywhere: a check is a statement about
// the inference plane right now, and a stale green pill is worse than none.
function healthCell(m) {
  const slot = h("span");
  const run = h(
    "button",
    {
      class: "btn btn-sm btn-quiet",
      title: "Send a test request to this model's backend",
      onClick: async () => {
        run.disabled = true;
        slot.replaceChildren(
          h(
            "span",
            { class: "faint nowrap" },
            h("span", { class: "blip" }),
            " checking…",
          ),
        );
        try {
          const probe = await api.checkModel(m.alias);
          slot.replaceChildren(verdict(probe));
        } catch (ex) {
          slot.replaceChildren(
            h("span", { class: "pill pill-bad" }, "check failed"),
          );
          toast(ex.message, "bad");
        } finally {
          run.disabled = false;
        }
      },
    },
    "Check",
  );
  return h(
    "div",
    { class: "row-tight", style: { marginTop: "4px" } },
    run,
    slot,
  );
}

// verdict is the pill, and the whole report behind it. The pill has to be
// readable at a glance and the report has to be complete, so they are not the
// same thing.
function verdict(probe) {
  const tone = probe.ok
    ? (probe.warnings || []).length
      ? "warn"
      : "good"
    : "bad";
  const label = probe.ok
    ? (probe.warnings || []).length
      ? "OK, with notes"
      : "Healthy"
    : probe.tool_call_as_text
      ? "Tool calls broken"
      : probe.reachable
        ? "Answering, unusable"
        : "Unreachable";
  return h(
    "button",
    {
      class: "pill pill-" + tone + " pill-button",
      title: "The full report",
      onClick: () => report(probe),
    },
    h("span", { class: "dot" }),
    label,
  );
}

function report(probe) {
  const lines = [
    ["Backend", probe.backend || "-"],
    ["Reachable", probe.reachable ? "yes" : "no"],
    ["HTTP status", probe.status || "-"],
    ["Streamed", probe.streamed ? "yes" : "no"],
    ["Tool call in `tool_calls`", probe.tool_calls ? "yes" : "no"],
    ["Model the backend served", probe.served_model || "-"],
    ["First token", probe.ttft_ms ? ms(probe.ttft_ms) : "-"],
    ["Total", probe.total_ms ? ms(probe.total_ms) : "-"],
  ];

  modal({
    wide: true,
    title: `Check - ${probe.alias}`,
    subtitle: probe.ok
      ? "The model answered and produced a usable tool call."
      : "A coding agent cannot use this model as it is.",
    body: h(
      "div",
      {},
      probe.error
        ? h("div", { class: "banner banner-bad" }, probe.error)
        : h(
            "div",
            { class: "banner banner-good" },
            "Reachable, streaming, and the tool call came back in a form " +
              "clients can use.",
          ),
      (probe.warnings || []).map((wmsg) =>
        h("div", { class: "banner banner-warn" }, wmsg),
      ),
      h(
        "div",
        { class: "table-wrap", style: { marginTop: "12px" } },
        h(
          "table",
          {},
          h(
            "tbody",
            {},
            lines.map(([k, v]) =>
              h(
                "tr",
                {},
                h("td", { class: "muted" }, k),
                h("td", { class: "mono" }, String(v)),
              ),
            ),
          ),
        ),
      ),
      probe.sample
        ? h(
            "div",
            { style: { marginTop: "12px" } },
            h("div", { class: "hint" }, "What the model produced:"),
            h(
              "div",
              { class: "code" },
              h(
                "button",
                {
                  class: "btn btn-sm code-copy",
                  title: "Copy",
                  "aria-label": "Copy",
                  onClick: () => copyText(probe.sample),
                },
                icon(icons.copy),
              ),
              h("pre", {}, h("code", {}, probe.sample)),
            ),
          )
        : null,
      h(
        "div",
        { class: "hint", style: { marginTop: "14px" } },
        "A check goes straight to the backend with the gateway's own " +
          "credentials. It is not rate-limited, budgeted or billed, but it " +
          "does run one real request.",
      ),
    ),
  });
}

const KINDS = ["chat", "completion", "embedding"];

// What a per-customer endpoint carries in place of the part only the
// deployment knows. It is the same text internal/catalog/providers.go writes.
const PRODUCT_ID = "{product_id}";

// endpointOf is the address a provider's model is actually reached at: its
// endpoint for the providers that serve everybody at one, and that endpoint
// with the deployment's own product id written in for the ones that do not.
//
// Null while such a provider has no product id yet - there is no address to
// show, and nothing to call a default.
function endpointOf(p, state) {
  if (!p.needs_product_id) return p.endpoint;
  const id = ((state && state.productID) || "").trim();
  return id ? p.endpoint.replace(PRODUCT_ID, id) : null;
}

// aliasFor is the alias a provider's model is offered under. An alias is
// rendered into the client configurations `keera connect` prints, which quote
// none of it, so it is lowercase letters, digits and interior hyphens - and a
// model id is none of those things: it may carry the publisher's name, capital
// letters and dots. This is a suggestion the operator types over, so the job
// is only to offer something the form will accept.
function aliasFor(id) {
  const name = String(id || "").split("/").pop();
  return name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

// productIDOf reads a product id back out of a stored model's address, because
// that is the only place it is kept: it is part of a URL, not a field of a
// model. An address that does not match the provider's template is the
// operator's own, and gives none back.
function productIDOf(p, backends) {
  if (!p || !p.needs_product_id) return "";
  const [head, tail] = p.endpoint.split(PRODUCT_ID);
  const url = String(backends || "").split("\n")[0].trim();
  if (!url.startsWith(head) || !url.endsWith(tail)) return "";
  const id = url.slice(head.length, url.length - tail.length);
  return /^[A-Za-z0-9_-]+$/.test(id) ? id : "";
}

// What a provider's table fills in. Each of these keeps a line under its input
// naming where the value came from, and - once it has been edited - the value
// it replaced, for the hosted cases that still show these fields: a model this
// build cannot price, and one priced in the wrong currency.
const PRESET_FIELDS = [
  {
    key: "backendModel",
    label: "model id",
    fromModel: true,
    of: (p, m) => (m ? m.id : null),
  },
  {
    key: "description",
    label: "description",
    fromModel: true,
    // The one default that is prose. Two things follow from that: the line
    // under the field cannot quote what an edit replaced, the way it quotes a
    // price - and the field is never disabled, because a provider's sentence
    // says what the model is and only this deployment knows what its alias is
    // for. It is a starting point, and the point of it is being edited.
    prose: true,
    of: (p, m) => (m && m.description ? m.description : null),
  },
  {
    key: "backends",
    label: "endpoint",
    of: (p, m, state) => endpointOf(p, state),
  },
  {
    key: "maxContext",
    label: "context window",
    fromModel: true,
    numeric: true,
    of: (p, m) => (m && m.max_context ? String(m.max_context) : null),
  },
  { key: "apiKeyEnv", label: "credential variable", of: (p) => p.api_key_env },
  {
    key: "priceIn",
    label: "list input price",
    fromModel: true,
    numeric: true,
    of: (p, m) => (m ? String(m.input_micros_per_mtok / 1e6) : null),
  },
  {
    key: "priceOut",
    label: "list output price",
    fromModel: true,
    numeric: true,
    of: (p, m) => (m ? String(m.output_micros_per_mtok / 1e6) : null),
  },
  {
    key: "priceCached",
    label: "list cached-input price",
    fromModel: true,
    numeric: true,
    of: (p, m) =>
      m && m.cached_input_micros_per_mtok
        ? String(m.cached_input_micros_per_mtok / 1e6)
        : null,
  },
];

function editModel(ctx, existing, providers) {
  const m = existing || { kind: "chat", enabled: true, backends: [] };
  const err = h("div");
  // A model the catalogue file declares belongs to the file: the control plane
  // refuses to change one, because the next start would apply the file over any
  // change made here. The form still shows it, and still takes a credential -
  // that is the one field no catalogue file carries.
  const locked = !!(existing && existing.managed);

  const alias = h("input", {
    class: "input",
    value: m.alias || "",
    placeholder: "keera-code",
    disabled: !!existing,
  });
  const kind = h("select", { class: "select", disabled: locked });
  const backendModel = h("input", {
    class: "input",
    value: m.backend_model || "",
    placeholder: "keera-code",
    disabled: locked,
  });
  const backends = h("textarea", {
    class: "input",
    rows: "3",
    disabled: locked,
    placeholder: "http://keera-engine:8000/v1",
  });
  backends.value = (m.backends || []).join("\n");
  const maxContext = h("input", {
    class: "input",
    type: "number",
    min: "0",
    value: m.max_context || "",
    placeholder: "0",
    disabled: locked,
  });
  const description = h("input", {
    class: "input",
    value: m.description || "",
    placeholder: "A fast local model for short edits and everyday questions",
    disabled: locked,
  });
  const apiKeyEnv = h("input", {
    class: "input",
    value: m.api_key_env || "",
    placeholder: "KEERA_UPSTREAM_KEY",
    disabled: locked,
  });
  const priceIn = priceInput(m.input_micros_per_mtok, locked);
  const priceOut = priceInput(m.output_micros_per_mtok, locked);
  const priceCached = priceInput(m.cached_input_micros_per_mtok, locked);
  // The fields a provider can fill in, by the names PRESET_FIELDS knows them by.
  const fields = {
    kind,
    backendModel,
    description,
    backends,
    maxContext,
    apiKeyEnv,
    priceIn,
    priceOut,
    priceCached,
  };
  setKinds(kind, KINDS, m.kind || "chat");

  // The credential itself. It is write-only in both directions: the control
  // plane never sends one back, so an empty field means "leave it as it is"
  // rather than "there is none", and the placeholder is the only status the
  // panel can show.
  const canStore = ctx.state.me.can_store_credentials;
  const apiKey = h("input", {
    class: "input",
    type: "password",
    autocomplete: "off",
    value: "",
    placeholder: canStore
      ? m.has_api_key
        ? "•••••••• stored - type to replace"
        : "sk-…"
      : "set KEERA_SECRET_KEY to store a key here",
    disabled: !canStore,
  });
  const clearKey =
    m.has_api_key && canStore ? h("input", { type: "checkbox" }) : null;
  const enabled = h("input", {
    type: "checkbox",
    checked: m.enabled !== false,
    disabled: locked,
  });

  // What the form is doing, for the parts of it that have to agree: which
  // provider's defaults are in the fields, whether a hosted endpoint was
  // chosen at all, whether the operator has named the alias themselves, and
  // which of the provider's answers this entry had already written over before
  // the dialog opened - those stay the operator's to edit.
  const state = {
    // Which tile is pressed: an inference plane of your own, one of the
    // providers, or none at all on a new model - which is what holds the rest
    // of the form back until the question is answered.
    place: null,
    preset: null,
    hosted: false,
    aliasDirty: !!existing,
    descriptionDirty: !!existing,
    overridden: new Set(),
    // The one part of a per-customer endpoint the provider cannot answer for.
    // It is not stored on the model - it is part of the address - so it is
    // read back out of that address when an existing model is opened.
    productID: "",
  };
  alias.addEventListener("input", () => {
    state.aliasDirty = true;
  });
  // For the reason the alias is tracked: both are offered rather than decided,
  // so once somebody has written one, changing the model underneath it must
  // not write the table's answer back over theirs.
  description.addEventListener("input", () => {
    state.descriptionDirty = true;
  });

  const marks = {};
  for (const f of PRESET_FIELDS) {
    marks[f.key] = h("div", { class: "hint-origin", hidden: true });
    fields[f.key].addEventListener("input", () =>
      paintPresets(state.preset, fields, marks, state),
    );
  }

  // Everything that describes the backend rather than the alias. It lives in
  // one group so that choosing a hosted provider can take it off the screen:
  // somebody pointing a model at Anthropic is not deciding what a backend URL
  // is, and being asked to look at one is what made that job feel harder than
  // it is.
  const envField = h(
    "div",
    { class: "field" },
    h("label", {}, "Upstream key variable"),
    apiKeyEnv,
    h(
      "div",
      { class: "hint" },
      canStore
        ? "Read from the gateway's environment instead. A key stored above " +
            "takes priority."
        : "The name of a variable, not the secret, e.g. ANTHROPIC_API_KEY. " +
            "It is read at startup, so changes need a restart.",
    ),
    marks.apiKeyEnv,
  );
  const machinery = h(
    "div",
    {},
    h(
      "div",
      { class: "field" },
      h("label", {}, "Backend model"),
      backendModel,
      h(
        "div",
        { class: "hint" },
        "The model name the backend serves, e.g. vLLM's --served-model-name.",
      ),
      marks.backendModel,
    ),
    h(
      "div",
      { class: "field" },
      h("label", {}, "Backends"),
      backends,
      h(
        "div",
        { class: "hint" },
        "One base URL per line. Several are used in turn.",
      ),
      marks.backends,
    ),
    h(
      "div",
      { class: "field-row" },
      h("div", { class: "field" }, h("label", {}, "Kind"), kind),
      h(
        "div",
        { class: "field" },
        h("label", {}, "Context window"),
        maxContext,
        h(
          "div",
          { class: "hint" },
          "Advertised to clients. A request that cannot fit is refused.",
        ),
        marks.maxContext,
      ),
    ),
    h(
      "div",
      { class: "field-row" },
      h(
        "div",
        { class: "field" },
        h("label", {}, `Input price / Mtok (${ctx.currency})`),
        priceIn,
        marks.priceIn,
      ),
      h(
        "div",
        { class: "field" },
        h("label", {}, `Output price / Mtok (${ctx.currency})`),
        priceOut,
        marks.priceOut,
      ),
    ),
    // On its own row rather than squeezed beside the two above, because it is
    // the one price that needs a sentence next to it: left blank it is not a
    // rate of nothing, it is no rate at all, and those tokens are then charged
    // at the input price.
    h(
      "div",
      { class: "field-row" },
      h(
        "div",
        { class: "field" },
        h("label", {}, `Cached input price / Mtok (${ctx.currency})`),
        priceCached,
        h(
          "div",
          { class: "hint" },
          "What a hosted provider charges for prompt tokens served from " +
            "its cache. If blank, they are charged at the full input " +
            "price. Self-hosted models do not need it.",
        ),
        marks.priceCached,
      ),
    ),
    // Where the credential comes from is only a detail while the field above
    // can take one. Without KEERA_SECRET_KEY it is the whole answer to "where
    // does my key go", so it does not belong behind a fold - it is put next to
    // the disabled field instead.
    canStore ? envField : null,
  );
  // Where that group sits: inline while the model points at an inference plane
  // of your own, gone once a provider has filled it in.
  const slot = h("div", {}, machinery);
  // Where the product id sits, for the one provider that asks for one: just
  // above the API key, which is the other half of the same answer.
  const productSlot = h("div", {});

  const hosted = locked
    ? null
    : hostedFields(ctx, providers, {
        fields,
        marks,
        state,
        slot,
        productSlot,
        machinery,
        alias,
        // An entry that already exists has answered the question, even when
        // the answer is "an inference plane of my own": its tile is the one
        // pressed, and the form below it is filled in.
        existing: !!existing,
        // Whether this is already a hosted model, which is a thing the entry
        // says rather than a thing to guess from its backend URL.
        initial: existing ? presetOf(providers, existing) : null,
        // A tile has been clicked, so there is a form to fill in now.
        onPick: () => reveal(),
      });

  // Everything the tiles govern, in one wrapper the dialog can hold back until
  // one of them is clicked. On a new model that is most of the form: where it
  // runs decides whether these fields are questions at all, or a provider's
  // answers folded away.
  const rest = h(
    "div",
    {},
    h(
      "div",
      { class: "field" },
      h("label", {}, "Alias"),
      alias,
      h(
        "div",
        { class: "hint" },
        "The name clients use. Changing it breaks them.",
      ),
    ),
    // Under the alias, and outside the machinery below, for two reasons. It
    // is the one field on this form addressed to a reader rather than to the
    // inference plane, so it belongs with the name rather than among the
    // addresses and prices - and the machinery is folded away for a hosted
    // model whose provider answered for it, which is the model a router most
    // needs described.
    h(
      "div",
      { class: "field" },
      h("label", {}, "Description"),
      description,
      h(
        "div",
        { class: "hint" },
        "One sentence: what it is good at and where it runs. Routers " +
          "use this to choose a model.",
      ),
      marks.description,
    ),
    productSlot,
    // Above the fold below it, because it is a question rather than an
    // answer: a hosted model takes a provider, a model and a key, and the key
    // is the only one of the three that is not on the screen already.
    h(
      "div",
      { class: "field" },
      h("label", {}, "API key"),
      apiKey,
      h(
        "div",
        { class: "hint" },
        canStore
          ? "The key the gateway sends to the backend. Stored encrypted " +
              "and never shown again. Leave empty to keep the stored key."
          : "KEERA_SECRET_KEY is not set, so a key cannot be stored here. " +
              "Set it and restart the gateway, or name a variable below.",
      ),
      canStore ? null : command("openssl rand -hex 32"),
      clearKey
        ? h(
            "label",
            { class: "row-tight", style: { marginTop: "6px" } },
            clearKey,
            "Remove the stored key",
          )
        : null,
    ),
    canStore ? null : envField,
    slot,
    h(
      "div",
      { class: "field" },
      h("label", { class: "row-tight" }, enabled, "Enabled"),
      h(
        "div",
        { class: "hint" },
        "When off, requests are refused as if the model did not exist. " +
          "Its history is kept.",
      ),
    ),
  );

  // Save is built out here rather than in the footer's callback, because the
  // tiles turn it on: there is nothing to save before one is clicked, and a
  // Create button beside four unanswered tiles is one inviting somebody to
  // skip the question. It closes the dialog through the handle modal hands
  // back.
  let close;
  const saveButton = h(
    "button",
    {
      class: "btn btn-primary",
      onClick: async (e) => {
        const list = locked
          ? m.backends || []
          : backends.value
              .split("\n")
              .map((s) => s.trim())
              .filter(Boolean);
        // Said on its own, because for this provider an empty backend is
        // not a field left blank - it is the one question the form asked
        // instead of showing a URL.
        const preset = state.hosted ? state.preset : null;
        if (
          preset &&
          preset.p.needs_product_id &&
          !state.overridden.has("backends") &&
          !list.length
        ) {
          showError(
            err,
            `${preset.p.name} gives each customer their own address. ` +
              "Fill in the product id.",
          );
          return;
        }
        if (!alias.value.trim() || !backendModel.value.trim() || !list.length) {
          // Under a provider the only way to get here is `custom`: the table
          // answers both of those fields for a model it knows, and a model it
          // does not is named and addressed inside the fold.
          showError(
            err,
            state.hosted
              ? "Enter the alias, and the model id and endpoint under " +
                  "Advanced options."
              : "Enter an alias, a backend model and at least one backend.",
          );
          return;
        }
        e.target.disabled = true;
        try {
          // Absent unless the operator typed one or asked for the stored
          // one to go: sending "" on every save would wipe it.
          let credential;
          if (clearKey && clearKey.checked) credential = "";
          else if (apiKey.value) credential = apiKey.value;

          const declaration = locked
            ? {
                alias: m.alias,
                kind: m.kind,
                backends: list,
                backend_model: m.backend_model,
                provider: m.provider || "",
                max_context: m.max_context || 0,
                description: m.description || "",
                api_key_env: m.api_key_env || "",
                input_micros_per_mtok: m.input_micros_per_mtok || 0,
                output_micros_per_mtok: m.output_micros_per_mtok || 0,
                cached_input_micros_per_mtok:
                  m.cached_input_micros_per_mtok || 0,
                enabled: m.enabled !== false,
              }
            : {
                alias: alias.value.trim(),
                kind: kind.value,
                backends: list,
                backend_model: backendModel.value.trim(),
                // What the fields below are: this provider's answers, or
                // nobody's but the operator's. Saving it is what lets the
                // next edit of this model tell the two apart.
                provider:
                  state.hosted && state.preset ? state.preset.p.name : "",
                max_context: parseInt(maxContext.value, 10) || 0,
                description: description.value.trim(),
                api_key_env: apiKeyEnv.value.trim(),
                input_micros_per_mtok: micros(priceIn.value),
                output_micros_per_mtok: micros(priceOut.value),
                cached_input_micros_per_mtok: micros(priceCached.value),
                enabled: enabled.checked,
              };
          await api.putModel(declaration.alias, {
            ...declaration,
            ...(credential === undefined ? {} : { api_key: credential }),
          });
          close();
          toast("Catalogue updated", "good");
          ctx.reload();
        } catch (ex) {
          showError(err, ex.message);
          e.target.disabled = false;
        }
      },
    },
    !existing ? "Create model" : locked ? "Save key" : "Save model",
  );

  const reveal = () => {
    rest.hidden = false;
    saveButton.hidden = false;
    saveButton.disabled = false;
  };
  // Held back only where there is a choice to make: a managed entry has no
  // tiles, nor has a build that knows no providers, and a model that already
  // exists has answered.
  const waiting = !!hosted && !state.place;
  rest.hidden = waiting;
  saveButton.hidden = waiting;
  saveButton.disabled = waiting;

  close = modal({
    wide: true,
    title: existing ? `${locked ? "" : "Edit "}${m.alias}` : "New model",
    subtitle: waiting
      ? "Start with where it runs. A hosted provider fills in the " +
        "endpoint, context window and prices. For self-hosted, you enter " +
        "them."
      : null,
    body: h("form", { onSubmit: (e) => e.preventDefault() }, err, hosted, rest),
    actions: (dismiss) => [
      h("button", { class: "btn", onClick: dismiss }, "Cancel"),
      saveButton,
    ],
  });

  // The keyboard starts on the tiles. The dialog's own rule - the first field
  // somebody can type in - finds nothing on a new model, because until a tile
  // is clicked there is nothing below them to type in.
  if (waiting) hosted.querySelector("button.tile").focus();
}

// presets reads the hosted endpoints this build knows. It is a convenience, so
// it degrades to none rather than taking the screen down with it.
async function presets() {
  try {
    return (await api.providers()).data || [];
  } catch {
    return [];
  }
}

// setKinds rebuilds the kind list. A provider serves the surfaces it serves,
// and offering a model a kind its endpoint has none of only moves the refusal
// from the form to the save.
function setKinds(select, kinds, selected) {
  const list = kinds && kinds.length ? kinds : KINDS;
  select.replaceChildren(...list.map((k) => h("option", { value: k }, k)));
  select.value = list.includes(selected) ? selected : list[0];
}

// presetOf says which provider's row of the table a stored model sits on, and
// which of that provider's models it serves. A model naming no provider, or one
// naming a provider this build has since dropped, sits on none: its fields are
// then nobody's but the operator's, which is exactly how the form treats them.
function presetOf(providers, m) {
  const p = (providers || []).find((x) => x.name === m.provider);
  if (!p) return null;
  return {
    p,
    m: (p.models || []).find((mm) => mm.id === m.backend_model) || null,
  };
}

// providerAnswers says which of the fields below the chosen provider answers
// for. Those are not the operator's to edit: the value they typed would be the
// one that stops matching the provider they named, and nothing would say so.
//
// The rest are nobody else's to fill. A model this build has no entry for has
// no context window and no prices in the table, so it has none to show; and a
// provider quoting a currency this deployment does not account in has two
// prices that are the wrong number until somebody converts them.
function providerAnswers(p, known, currency) {
  return {
    backends: true,
    apiKeyEnv: true,
    // The description is deliberately not here. The table fills it in like
    // everything else, and then it is the operator's: a provider's sentence
    // says what the model is, and what this deployment means by the alias is
    // the part a router actually needs and only they can write.
    //
    // A provider serving one surface leaves nothing to choose. One serving
    // several does, and which of them a model is has to be somebody's answer.
    kind: (p.kinds || []).length === 1,
    backendModel: !!known,
    maxContext: !!known,
    priceIn: !!known && p.currency === currency,
    priceOut: !!known && p.currency === currency,
    // A provider that publishes no cached-input rate has not answered this
    // one: it is left live, and left blank those tokens are charged at the
    // input price.
    priceCached:
      !!known &&
      !!known.cached_input_micros_per_mtok &&
      p.currency === currency,
  };
}

// hostedFields is the top of the form: where the model runs. Every place it
// can run is a tile - an inference plane of your own, and each hosted endpoint
// Keera Gateway already knows - because a provider is recognised by its mark
// before its name is read, and a tile has room for the sentence an <option>
// had to leave out.
//
// It is asked first and on its own, the way a filter's mode is: the answer
// decides what the rest of the form means, and a Create button beside an
// unanswered question is one inviting somebody to skip it. Once a tile is
// clicked it collapses to a single row with the way back to the others, so the
// choice stays on the screen without the other tiles staying with it.
//
// Choosing a provider is meant to be the whole job - a provider, a model, a
// key - so everything it implies is filled in, disabled and folded away under
// Advanced options. The fold exists rather than nothing at all because a model
// being edited still has to be readable: what it serves, what it costs and
// where it goes are most of why the dialog gets opened.
//
// What the table does not answer stays live inside the fold - see
// providerAnswers - and the banner above names it, because a field somebody
// has to fill in is now behind a fold they have to open. An entry that had
// already written over one of the answers keeps it live too: a catalogue file
// pointing `provider: anthropic` at an egress proxy is not this form's to take
// back.
function hostedFields(ctx, providers, form) {
  if (!providers || !providers.length) return null;
  const {
    fields,
    marks,
    state,
    slot,
    productSlot,
    machinery,
    alias,
    existing,
    initial,
    onPick,
  } = form;

  const note = h("div");
  // The places a model can run, in the order the tiles offer them: your own
  // inference plane first, because it is the one Keera exists for, then the
  // providers in the table's own order. A place with no `p` is that first one,
  // and having no provider table behind it is the whole difference.
  //
  // Each carries one line: what a provider is, in its own table's words, and
  // what an inference plane of your own is in the panel's. The caveats - the
  // prices, what an endpoint does not do - are the provider's note, which the
  // form shows the moment the tile is clicked.
  const places = [
    {
      p: null,
      name: "Self-hosted",
      what: "Your own inference server, such as vLLM, Ollama or Keera Engine.",
    },
    ...providers.map((p) => ({ p, name: p.name, what: p.summary })),
  ];
  // A place's mark: the provider's own logo where this build has one, a cloud
  // where it does not, and a rack of machines for a plane of your own. Drawn
  // afresh each time rather than kept, because the tile and the row that
  // replaces it show the same mark and one node cannot be in both.
  const markOf = (place) =>
    place.p
      ? providerMark(place.p.name) || icon(icons.cloud)
      : icon(icons.selfHosted);

  const model = h("select", { class: "select" });
  // Some providers serve every customer at their own address. For those the
  // endpoint under Advanced options is not an answer the table can give, so
  // the missing part of it is asked for instead - right above the API key,
  // because it is the other thing the same console page hands you, and
  // because both are per-customer where the provider and the model are not.
  const productID = h("input", {
    class: "input",
    value: "",
    placeholder: "100234",
    autocomplete: "off",
  });
  const productIDField = h(
    "div",
    { class: "field", hidden: true },
    h("label", {}, "Product ID"),
    productID,
    h(
      "div",
      { class: "hint" },
      "Not a secret: it is part of the address. Find it in the Infomaniak " +
        "console under AI Tools.",
    ),
  );
  // What follows a hosted provider: which of its models this alias serves, and
  // what is worth knowing about the endpoint before that is answered.
  const block = h(
    "div",
    { hidden: true },
    h("div", { class: "field" }, h("label", {}, "Model"), model),
    note,
  );
  // Its own place in the form, far from the block above, so hiding that block
  // is not what hides it - see choose().
  if (productSlot) productSlot.replaceChildren(productIDField);
  const summary = h("summary", {}, "Advanced options");
  const fold = h("details", { class: "fold" }, summary);

  const chosen = () => (state.place ? state.place.p : null);

  // A provider's warnings are about its endpoint, so they are worth reading
  // before a model is picked rather than after.
  const paintNote = () => {
    const p = chosen();
    if (!p) return;
    // Asked for only where there is a hole in the address to fill. The value
    // is kept while it is hidden, so going back to the tiles and returning
    // does not lose what somebody typed.
    productIDField.hidden = !p.needs_product_id;
    const lines = [];
    if (p.note) lines.push(h("div", { class: "banner banner-warn" }, p.note));
    if (p.currency !== ctx.currency) {
      lines.push(
        h(
          "div",
          { class: "banner banner-warn" },
          `Priced in ${p.currency}, but this deployment uses ` +
            `${ctx.currency}. Convert the prices under Advanced options, or ` +
            `budgets for this model will be wrong.`,
        ),
      );
    }
    if (state.preset && !state.preset.m) {
      lines.push(
        h(
          "div",
          { class: "banner banner-info" },
          "This build does not know that model. Enter its context window " +
            "and prices under Advanced options. Zero means free; blank means " +
            "unpriced. A blank cached-input price means the input price.",
        ),
      );
    }
    note.replaceChildren(...lines);
  };

  // The address is built from the product id rather than typed, so it is kept
  // in step with it. An entry that wrote its own address keeps it: that one is
  // the operator's, and a product id cannot be read out of it.
  const syncEndpoint = () => {
    const p = chosen();
    if (!p || !p.needs_product_id || state.overridden.has("backends")) return;
    fields.backends.value = endpointOf(p, state) || "";
  };
  productID.addEventListener("input", () => {
    state.productID = productID.value.trim();
    syncEndpoint();
    paintPresets(state.preset, fields, marks, state);
  });

  // Which fields the provider is answering for right now, or null while the
  // model points at an inference plane of your own - where every one of them is
  // the operator's.
  const answered = () => {
    const { p, m } = state.preset || {};
    if (!state.hosted || !p) return null;
    const fixed = providerAnswers(p, m, ctx.currency);
    for (const key of state.overridden) fixed[key] = false;
    return fixed;
  };

  // Disabled is the whole of how this is enforced in the panel, and it is
  // enough: the control plane takes whatever it is sent, because a catalogue
  // file and the CLI have always been allowed to override a provider's table
  // and this form is not the place to start refusing them.
  const paintLocks = () => {
    const fixed = answered();
    for (const [key, el] of Object.entries(fields)) {
      el.disabled = !!(fixed && fixed[key]);
    }
    return fixed;
  };

  // Where the machinery sits: inline while the model points at an inference
  // plane of your own, folded away once a provider has answered for it, and
  // off the screen between choosing a provider and choosing a model, when
  // there is nothing to read or fill in.
  //
  // `open` is true or false to state the fold, undefined to leave it as the
  // operator left it, and "auto" for a fold appearing: start closed, and leave
  // one already on the screen as it was.
  const paintMachinery = (open) => {
    const fixed = paintLocks();
    if (state.hosted && !state.preset) {
      slot.replaceChildren();
      return;
    }
    if (!fixed) {
      slot.replaceChildren(machinery);
      return;
    }
    if (open === "auto") open = slot.contains(fold) && fold.open;
    fold.replaceChildren(summary, machinery);
    if (open !== undefined) fold.open = open;
    slot.replaceChildren(fold);
  };

  const fillModels = () => {
    const p = chosen();
    model.replaceChildren(
      ...p.models.map((mm, i) => h("option", { value: String(i) }, mm.id)),
      // A model released after this binary was built is still one this
      // catalogue entry can serve; it just has to be named and priced by hand.
      h("option", { value: "other" }, "custom"),
    );
    // The first of them, rather than a "choose a model" nobody would leave
    // standing: picking a provider then fills the form in, and the list is
    // there to change the one answer it guessed at.
    model.value = "0";
  };

  const applyModel = () => {
    const p = chosen();
    const known =
      model.value === "other" ? null : p.models[Number(model.value)];
    state.preset = { p, m: known };
    // The table has just been written into every field it answers for, so
    // nothing here is an override any more.
    state.overridden.clear();
    for (const f of PRESET_FIELDS) {
      // Every field the table answers for is written over, because the model
      // has just changed underneath them. Except a description somebody has
      // written: that one is a sentence, and nothing here is worth losing one.
      if (f.key === "description" && state.descriptionDirty) continue;
      const def = f.of(p, known, state);
      if (def !== null) fields[f.key].value = def;
      else if (f.fromModel) fields[f.key].value = "";
    }
    // The endpoint is the one default that can be half an answer: a provider
    // serving each customer at their own address has none to write until the
    // product id is in. Clearing it is what keeps the previous provider's out.
    syncEndpoint();
    setKinds(fields.kind, p.kinds, fields.kind.value);
    // The alias is the one thing the provider cannot know, so it is offered
    // rather than decided: the model's own name until somebody types over it.
    if (!state.aliasDirty) alias.value = known ? aliasFor(known.id) : "";
    paintMachinery("auto");
    paintPresets(state.preset, fields, marks, state);
    paintNote();
  };

  model.addEventListener("change", applyModel);

  const tiles = h(
    "div",
    { class: "tiles" },
    places.map((place) =>
      h(
        "button",
        {
          class: "tile",
          type: "button",
          "aria-pressed": "false",
          onClick: () => choose(place),
        },
        h("span", { class: "tile-mark" }, markOf(place)),
        h("span", { class: "tile-name" }, place.name),
        place.what ? h("span", { class: "tile-what" }, place.what) : null,
      ),
    ),
  );
  const chosenRow = h("div", { class: "tile-chosen", hidden: true });
  // What to weigh before clicking one of them, and only then: once a provider
  // is chosen its own note says the same thing in its own words, and over the
  // self-hosted row it is a caution about a choice that was not made.
  const caution = h(
    "div",
    { class: "hint" },
    "With a hosted provider, prompts leave your infrastructure. Read " +
      "docs/providers.md first, and set budgets for the teams that use it.",
  );
  const showTiles = (show) => {
    tiles.hidden = !show;
    caution.hidden = !show;
    chosenRow.hidden = show;
  };
  // The way back to the tiles. What is in the fields stays there until another
  // tile is clicked: going to look at the others costs nothing.
  const change = h(
    "button",
    {
      class: "btn btn-sm",
      type: "button",
      onClick: () => {
        showTiles(true);
        tiles.firstChild.focus();
      },
    },
    "Change",
  );

  // paint is the choice as the form shows it: which tile is pressed, the row
  // that stands in for them all, and whether there is a model to pick below
  // it. It changes nothing else, which is what lets an entry being edited open
  // on its own provider without a default being written over anything.
  const paint = (place) => {
    tiles.childNodes.forEach((tile, i) => {
      tile.setAttribute("aria-pressed", String(places[i] === place));
    });
    chosenRow.replaceChildren(
      markOf(place),
      h(
        "div",
        { class: "stack", style: { gap: "1px" } },
        h("span", { class: "tile-name" }, place.name),
        place.what ? h("span", { class: "tile-what" }, place.what) : null,
      ),
      h("div", { class: "spacer" }),
      change,
    );
    showTiles(false);
    block.hidden = !place.p;
  };

  // choose is a tile being clicked, which is more than painting it: where the
  // model runs has changed, so the model list is rebuilt and nothing is filled
  // in again until a model is named. The values already in the fields stay -
  // they are the operator's now, and clearing a form somebody has filled in is
  // never the helpful reading. With the provider goes the reason any of them
  // was disabled.
  const choose = (place) => {
    // The tile that is already pressed is the way back from the tiles and
    // nothing more. Rebuilding the list under it would throw away the model
    // this entry names, which is what somebody went to look at the others for.
    if (place === state.place) {
      paint(place);
      change.focus();
      return;
    }
    state.place = place;
    state.hosted = !!place.p;
    state.preset = null;
    state.overridden.clear();
    paint(place);
    if (place.p) {
      // Its first model comes with it: a provider, a model and a key is the
      // whole job, and two of the three are answered by the click.
      fillModels();
      applyModel();
    } else {
      productIDField.hidden = true;
      paintMachinery();
      setKinds(fields.kind, KINDS, fields.kind.value);
      paintPresets(null, fields, marks, state);
      note.replaceChildren();
    }
    if (onPick) onPick();
    // Where the cursor lands is the point of asking this first: the model
    // list, which is the next question a provider asks, or the alias, which is
    // the first thing left to type for a plane of your own.
    if (place.p) model.focus();
    else if (!alias.value && !alias.disabled) alias.focus();
    else change.focus();
  };

  // A model that already names a provider opens on that provider's tile, with
  // its own stored values untouched: an entry may override anything the table
  // filled in, and the one thing this form must not do is write a default back
  // over an override on the way to changing a description.
  if (initial) {
    state.place = places.find((place) => place.p === initial.p);
    state.hosted = true;
    fillModels();
    model.value = initial.m
      ? String(initial.p.models.indexOf(initial.m))
      : "other";
    state.preset = initial;
    // Read back out of the stored address before anything is compared against
    // the table, because for this provider the address is not the table's
    // until the product id is in hand.
    state.productID = productIDOf(initial.p, fields.backends.value);
    productID.value = state.productID;
    for (const f of PRESET_FIELDS) {
      const def = f.of(initial.p, initial.m, state);
      if (def !== null && !sameValue(f, fields[f.key].value, def)) {
        state.overridden.add(f.key);
      }
    }
    // An address that gave no product id back is not this provider's template
    // at all - an egress proxy, say - so it stays the operator's to edit, the
    // way any other value they wrote over the table's does.
    if (initial.p.needs_product_id && !state.productID) {
      state.overridden.add("backends");
    }
    // A description that is still this provider's own follows the model, so
    // pointing an alias at a different one brings that model's sentence with
    // it. One somebody has written does not: it is the entry's, and an entry's
    // own value is the thing this form must never take back. An empty one is
    // neither - there is nothing there to protect, so it takes the next
    // model's default like a new entry would.
    state.descriptionDirty =
      state.overridden.has("description") &&
      fields.description.value.trim() !== "";
    // A kind outside what the provider serves is somebody's deliberate answer
    // and stays one; narrowing the list would silently change it to the first
    // surface on it.
    if ((initial.p.kinds || []).includes(fields.kind.value)) {
      setKinds(fields.kind, initial.p.kinds, fields.kind.value);
    } else {
      state.overridden.add("kind");
    }
    paintPresets(initial, fields, marks, state);
    paint(state.place);
    paintMachinery("auto");
    paintNote();
  } else if (existing) {
    // An entry naming no provider is one that runs on a plane of your own.
    // That is an answer, so its tile is pressed and the form below it is the
    // form for it - nothing is reset, because there is nothing to choose.
    state.place = places[0];
    paint(state.place);
  }

  return h(
    "div",
    { class: "field" },
    h("label", {}, "Where the model runs"),
    tiles,
    chosenRow,
    caution,
    block,
  );
}

// paintPresets keeps the line under each filled-in field true: which provider
// the value came from while it still holds, and what it replaced once it does
// not. It is the whole of the explanation a disabled field gets, so it is
// written for one: "anthropic's endpoint" says why the box cannot be typed in.
function paintPresets(preset, fields, marks, state) {
  for (const f of PRESET_FIELDS) {
    const line = marks[f.key];
    const def = preset ? f.of(preset.p, preset.m, state) : null;
    // An empty line is not the same as a short one: the field has to close up
    // again when there is no default behind it.
    line.hidden = def === null;
    if (def === null) {
      line.replaceChildren();
      continue;
    }
    line.replaceChildren(
      sameValue(f, fields[f.key].value, def)
        ? `${preset.p.name}'s ${f.label}.`
        : f.prose
          ? `Replaces ${preset.p.name}'s own ${f.label}.`
          : `Overrides ${preset.p.name}'s ${f.label} of ${def}.`,
    );
  }
}

// sameValue reports whether a field still holds what the provider's table put
// there. Numbers are compared as numbers, because 5 and 5.0 are one price.
function sameValue(f, now, def) {
  now = String(now).trim();
  return f.numeric ? Number(now) === Number(def) : now === def;
}

// command is a line somebody has to run somewhere this panel cannot reach. It
// carries a copy button because the alternative is retyping 64 hex characters
// of somebody's own generated key by hand.
function command(text) {
  return h(
    "div",
    { class: "code", style: { marginTop: "6px" } },
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

function priceInput(v, disabled) {
  return h("input", {
    class: "input",
    type: "number",
    min: "0",
    step: "0.01",
    placeholder: "0.00",
    value: v ? v / 1e6 : "",
    disabled: !!disabled,
  });
}

function micros(v) {
  const n = parseFloat(v);
  return Number.isFinite(n) && n > 0 ? Math.round(n * 1e6) : 0;
}
