// The organisation's models. Administrators manage them; everyone can read
// them.

import { api } from "../api.js";
import { orgNameOf } from "./orgs.js";
import { steps } from "./router-tree.js";
import {
  h,
  table,
  modal,
  toast,
  pill,
  listPrice,
  num,
  compact,
  ms,
  icon,
  icons,
  rowLink,
  providerMark,
  modelAliasProblem,
  takenProblem,
  locationName,
  releaseDay,
  readRow,
  go,
  crumb,
  currentRange,
  rangePicker,
  isAdmin,
  plural,
  field,
  rowActions,
  code,
  listHead,
  dash,
  formModal,
  banner,
  confirm,
} from "../ui.js";

// usersOfModel names the filters and routers that use a model. Either blocks
// the delete.
async function usersOfModel(m) {
  const [filters, routers] = await Promise.all([
    api.filters(m.org_id).then((r) => r.data || []),
    api.routers(m.org_id).then((r) => r.data || []),
  ]);
  const f = filters.filter((x) => x.model === m.alias).map((x) => x.alias);
  const r = routers
    .filter((x) =>
      steps(x.start).some(
        (s) =>
          (s.kind === "model" && s.ref === m.alias) ||
          (s.kind === "route" &&
            (s.model === m.alias || (s.destinations || []).includes(m.alias))),
      ),
    )
    .map((x) => x.alias);
  const users = [
    ...f.map((a) => `filter ${a}`),
    ...r.map((a) => `router ${a}`),
  ];
  if (!users.length) return "";
  return (
    ` It is used by ${users.join(", ")}, so it cannot be deleted until ` +
    "they are changed."
  );
}

// deleteWarning gives the same warnings as `keera model delete`.
async function deleteWarning(m) {
  return (
    "Clients that still use this model will be refused, and " +
    "guardrails that allow only this model stop allowing anything." +
    (await usersOfModel(m)) +
    (m.has_api_key ? " Its stored credential is removed." : "") +
    " Usage history and the audit log are kept."
  );
}

export async function deleteModel(ctx, m, done = () => ctx.reload()) {
  confirm({
    title: `Delete ${m.alias}?`,
    body: await deleteWarning(m),
    confirmLabel: "Delete model",
    danger: true,
    onConfirm: async () => {
      await api.deleteModel(m.org_id, m.alias);
      toast(`${m.alias} deleted`, "good");
      done();
    },
  });
}

function canAddModel(ctx) {
  return isAdmin(ctx) && !!ctx.orgID;
}

export async function modelsView(ctx) {
  const since = currentRange();
  const res = await api.models(ctx.orgID, { stats: true, since });
  const models = res.data || [];
  const stats = res.stats || {};
  const canAdd = canAddModel(ctx);
  // The hosted endpoints only fill in a form, so the screen works without
  // them.
  const providers = canAdd ? await presets() : [];
  ctx.setSubtitle(`${models.length} ${plural(models.length, "model")}`);

  // Models are added from the catalogue, which also offers self-hosted.
  const head = listHead(
    null,
    [
      rangePicker(ctx, since),
      !canAdd
        ? h(
            "a",
            {
              class: "btn",
              href: "/catalogue",
              title: "Every model the hosted providers offer",
              onClick: go(ctx, "/catalogue"),
            },
            "Model catalogue",
          )
        : null,
    ],
    canAdd && {
      label: "Add model",
      onClick: go(ctx, "/catalogue"),
    },
  );

  const rows = table(
    [
      {
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
            h("span", { class: "faint small" }, "serves " + servedName(m)),
          ),
      },
      {
        // The provider, not the URL: it says whether a prompt leaves the
        // cluster. Self-hosted gets a dash, as its address can point anywhere.
        label: "Provider",
        shrink: true,
        sortKey: (m) => providerName(m.provider),
        cell: (m) =>
          m.provider
            ? h(
                "span",
                {
                  title:
                    "Hosted provider: prompts to this model leave your " +
                    "infrastructure",
                },
                // The word stays, so a provider with no mark reads the same.
                pill(
                  [providerMark(m.provider), providerName(m.provider)],
                  "warn",
                ),
              )
            : dash(),
      },
      {
        label: "Location",
        shrink: true,
        sortKey: (m) => m.location || "",
        cell: (m) => locationCell(m.location),
      },
      {
        label: "Released",
        shrink: true,
        sortKey: (m) => m.release_date || null,
        sortDir: "desc",
        cell: (m) => releasedCell(m.release_date),
      },
      {
        label: "Context",
        num: true,
        shrink: true,
        sortKey: (m) => m.max_context_tokens || null,
        sortDir: "desc",
        cell: (m) =>
          m.max_context_tokens ? compact(m.max_context_tokens) : dash(),
      },
      {
        // Same median and window as the model's own screen.
        label: "First token",
        num: true,
        shrink: true,
        sortKey: (m) => stats[m.alias]?.ttft_median_ms || null,
        cell: (m) => ttftCell(stats[m.alias]),
      },
      {
        label: "Price CHF / MTok",
        num: true,
        shrink: true,
        // By input price.
        sortKey: (m) => m.input_micros_per_mtok || null,
        sortDir: "desc",
        cell: (m) =>
          m.input_micros_per_mtok || m.output_micros_per_mtok
            ? h(
                "span",
                { class: "nowrap muted" },
                `${listPrice(m.input_micros_per_mtok)} in · ${listPrice(m.output_micros_per_mtok)} out`,
              )
            : h("span", { class: "faint" }, "not billed"),
      },
      {
        // Enabled is the admin's switch; Check tests the backend.
        label: "Status",
        shrink: true,
        sortKey: (m) => (m.enabled ? 1 : 0),
        cell: (m) =>
          m.enabled ? pill("Enabled", "good") : pill("Disabled", "warn"),
      },
      {
        label: "",
        shrink: true,
        cell: (m) => {
          // Members can read the configuration too.
          if (!isAdmin(ctx)) {
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
          return rowActions(ctx, m.alias, {
            editTitle: "What this model serves, and on what",
            onEdit: () => editModel(ctx, m, providers),
            deleteTitle: "Delete this model",
            body: () => deleteWarning(m),
            confirmLabel: "Delete model",
            remove: () => api.deleteModel(m.org_id, m.alias),
          });
        },
      },
    ].filter(Boolean),
    models,
    {
      emptyTitle: "No models yet",
      emptyBody:
        "Every inference request is refused until a model exists. Add " +
        "one and check it before handing out keys.",
      // Endpoints are searchable though not shown, to find models that point
      // at an old cluster.
      search: (m) =>
        [
          m.alias,
          m.backend_model,
          m.display_name || "",
          m.kind,
          m.provider || "",
          providerName(m.provider),
          m.location || "",
          locationName(m.location),
          ...(m.backends || []),
        ].join(" "),
      searchLabel: "models",
      ctx,
      open: (m) => "/models/" + encodeURIComponent(m.alias),
    },
  );

  return h("div", {}, head, rows);
}

// catalogueView lists every model the hosted providers offer, and whether
// this organisation already serves it.
export async function catalogueView(ctx) {
  const canEdit = canAddModel(ctx);
  const [providers, models] = await Promise.all([
    presets(),
    ctx.orgID ? api.models(ctx.orgID).then((r) => r.data || []) : [],
  ]);

  const rows = providers.flatMap((p) =>
    (p.models || []).map((m) => ({
      p,
      m,
      // So a model is not added twice.
      aliases: models
        .filter((x) => x.provider === p.name && isModel(m, x.backend_model))
        .map((x) => x.alias),
    })),
  );
  ctx.setSubtitle(
    `${rows.length} ${plural(rows.length, "model")} from ` +
      `${providers.length} ${plural(providers.length, "provider")}`,
  );

  const price = (v) => (v ? h("span", { class: "nowrap" }, chf(v)) : dash());

  // Self-hosted models are in no provider's table, so they are added here.
  const head = h(
    "div",
    { class: "detail-head row" },
    crumb(ctx, "/models", "Models"),
    h("div", { class: "spacer" }),
    canEdit
      ? h(
          "button",
          {
            class: "btn",
            title:
              "A model on your own inference server, such as vLLM, " +
              "Ollama or Keera Engine",
            onClick: () => editModel(ctx, null, providers, null),
          },
          icon(icons.selfHosted),
          "Self-hosted model",
        )
      : null,
  );

  const list = table(
    [
      {
        label: "Model",
        sortKey: (r) => r.m.display_name || r.m.id,
        cell: (r) =>
          h(
            "div",
            { class: "stack" },
            r.m.display_name
              ? h("span", {}, r.m.display_name)
              : h("span", { class: "mono" }, r.m.id),
            r.m.description
              ? h("span", { class: "faint small" }, r.m.description)
              : null,
          ),
      },
      {
        label: "Provider",
        shrink: true,
        sortKey: (r) => providerName(r.p.name),
        cell: (r) => pill([providerMark(r.p.name), providerName(r.p.name)]),
      },
      {
        label: "Location",
        shrink: true,
        sortKey: (r) => r.p.location || "",
        cell: (r) => locationCell(r.p.location),
      },
      {
        label: "Released",
        shrink: true,
        sortKey: (r) => r.m.release_date || null,
        sortDir: "desc",
        cell: (r) => releasedCell(r.m.release_date),
      },
      {
        label: "Context",
        num: true,
        shrink: true,
        sortKey: (r) => r.m.max_context_tokens || null,
        sortDir: "desc",
        cell: (r) =>
          r.m.max_context_tokens ? compact(r.m.max_context_tokens) : dash(),
      },
      {
        label: "Input / MTok",
        num: true,
        shrink: true,
        sortKey: (r) => r.m.input_micros_per_mtok || null,
        cell: (r) => price(r.m.input_micros_per_mtok),
      },
      {
        label: "Cached input / MTok",
        num: true,
        shrink: true,
        sortKey: (r) => r.m.cached_input_micros_per_mtok || null,
        cell: (r) => price(r.m.cached_input_micros_per_mtok),
      },
      {
        label: "Cache write / MTok",
        num: true,
        shrink: true,
        sortKey: (r) => r.m.cache_write_micros_per_mtok || null,
        cell: (r) => price(r.m.cache_write_micros_per_mtok),
      },
      {
        label: "Output / MTok",
        num: true,
        shrink: true,
        sortKey: (r) => r.m.output_micros_per_mtok || null,
        cell: (r) => price(r.m.output_micros_per_mtok),
      },
      {
        label: "In use as",
        shrink: true,
        sortKey: (r) => r.aliases[0] || null,
        cell: (r) =>
          r.aliases.length
            ? h("div", { class: "stack" }, aliasLinks(ctx, r.aliases))
            : dash(),
      },
    ],
    rows,
    {
      pick: canEdit
        ? (r) => editModel(ctx, null, providers, { p: r.p, m: r.m })
        : null,
      pickTitle: `Add this model to ${orgNameOf(ctx)}`,
      emptyTitle: "No hosted models",
      emptyBody: "This build knows no hosted providers.",
      // Newest first: people usually look for new models.
      sortBy: "Released",
      sortDir: "desc",
      search: (r) =>
        [
          r.m.id,
          r.m.display_name || "",
          r.m.description,
          r.p.name,
          providerName(r.p.name),
          locationName(r.p.location),
        ].join(" "),
      searchLabel: "models",
      tile: (r) => catalogueTile(ctx, r),
      choices: [
        {
          label: "Locations",
          options: [...new Set(providers.map((p) => p.location))]
            .filter(Boolean)
            .map((l) => ({ value: l, label: locationName(l) })),
          test: (r, v) => r.p.location === v,
        },
        {
          label: "Providers",
          options: providers.map((p) => ({
            value: p.name,
            label: providerName(p.name),
          })),
          test: (r, v) => r.p.name === v,
        },
        {
          label: "Kinds",
          options: KINDS.filter((k) =>
            providers.some((p) => (p.kinds || []).includes(k)),
          ).map((k) => ({ value: k, label: kindName(k) })),
          test: (r, v) => (r.p.kinds || []).includes(v),
        },
        // A dropdown with one choice filters nothing.
      ].filter((c) => c.options.length > 1),
      // Models that need the organisation's own key are hidden by default.
      // Shown only when keys are mixed.
      toggles: [
        {
          label: "Show bring your own token",
          hidden: (r) => !r.p.platform_key,
        },
      ].filter(
        () =>
          providers.some((p) => p.platform_key) &&
          providers.some((p) => !p.platform_key),
      ),
    },
  );

  return h("div", {}, head, list);
}

function catalogueTile(ctx, r) {
  // A price may wrap here: a tile is narrow.
  const price = (v) => chf(v) || dash();
  const fact = (label, value) =>
    h("div", {}, h("dt", {}, label), h("dd", {}, value));
  return h(
    "article",
    { class: "model-tile" },
    h(
      "div",
      { class: "model-tile-head" },
      h(
        "span",
        { class: "tile-mark" },
        providerMark(r.p.name) || icon(icons.cloud),
      ),
      h(
        "div",
        { class: "stack" },
        h("span", { class: "small" }, providerName(r.p.name)),
        h(
          "span",
          { class: "faint small" },
          [
            r.p.location && locationName(r.p.location),
            r.m.release_date && releaseDay(r.m.release_date),
          ]
            .filter(Boolean)
            .join(" · "),
        ),
      ),
    ),
    h(
      "div",
      { class: "stack" },
      h(
        "span",
        { class: "model-tile-name" + (r.m.display_name ? "" : " mono") },
        r.m.display_name || r.m.id,
      ),
      r.m.display_name
        ? h("span", { class: "mono faint small" }, r.m.id)
        : null,
    ),
    r.m.description
      ? h("p", { class: "model-tile-what" }, r.m.description)
      : null,
    h(
      "dl",
      { class: "model-tile-facts", title: "Prices per million tokens" },
      fact(
        "Context",
        r.m.max_context_tokens ? compact(r.m.max_context_tokens) : dash(),
      ),
      fact("Input / MTok", price(r.m.input_micros_per_mtok)),
      fact("Output / MTok", price(r.m.output_micros_per_mtok)),
      fact("Cached input / MTok", price(r.m.cached_input_micros_per_mtok)),
      fact("Cache write / MTok", price(r.m.cache_write_micros_per_mtok)),
    ),
    r.aliases.length
      ? h(
          "div",
          { class: "small muted" },
          "In use as ",
          aliasLinks(ctx, r.aliases).map((link, i) => [i ? ", " : "", link]),
        )
      : null,
  );
}

function chf(micros) {
  return micros ? "CHF " + listPrice(micros) : null;
}

// aliasLinks links each alias a catalogue model is in use as.
function aliasLinks(ctx, aliases) {
  return aliases.map((a) =>
    h(
      "span",
      { class: "mono" },
      rowLink(ctx, "/models/" + encodeURIComponent(a), a),
    ),
  );
}

function locationCell(loc) {
  if (!loc) return dash();
  return h(
    "span",
    { class: "nowrap", title: `Prompts to this model go to: ${loc}` },
    locationName(loc),
  );
}

function releasedCell(day) {
  return day ? h("span", { class: "nowrap" }, releaseDay(day)) : dash();
}

function ttftCell(st) {
  if (!st?.ttft_median_ms) return dash();
  return h(
    "div",
    { class: "stack nowrap" },
    h("span", {}, ms(st.ttft_median_ms)),
    h("span", { class: "faint small" }, `p95 ${ms(st.ttft_p95_ms)}`),
  );
}

export async function openModel(ctx, m) {
  if (!isAdmin(ctx)) return viewModel(ctx, m);
  return editModel(ctx, m, await presets());
}

// viewModel is the model as a member sees it. Not a disabled edit form:
// members are not sent the endpoints or key, so it would be mostly empty.
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
      readRow("Kind", pill(kindName(m.kind)), surfaceOf(m.kind)),
      readRow(
        "What it is for",
        m.description
          ? h("span", {}, m.description)
          : h("span", { class: "muted" }, "nothing written down"),
        "Listed on /api/v1/models and shown to routers choosing a model.",
      ),
      readRow(
        "Model it serves",
        m.display_name
          ? h(
              "span",
              {},
              m.display_name,
              h("span", { class: "mono muted" }, ` - ${m.backend_model}`),
            )
          : h("span", { class: "mono" }, m.backend_model),
        "The model the backend runs.",
      ),
      h(
        "div",
        { class: "field-row" },
        readRow(
          "Location",
          h("span", {}, locationName(m.location)),
          "Where the model runs, and so where your prompts go.",
        ),
        readRow(
          "Released",
          m.release_date
            ? h("span", {}, releaseDay(m.release_date))
            : h("span", { class: "muted" }, "not known"),
          "The day the model came out.",
        ),
      ),
      h(
        "div",
        { class: "field-row" },
        readRow(
          "Context window",
          m.max_context_tokens
            ? h(
                "span",
                {},
                `${compact(m.max_context_tokens)} tokens`,
                h(
                  "span",
                  { class: "hint-origin" },
                  ` - ${num(m.max_context_tokens)}`,
                ),
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
        "Price per million tokens (CHF)",
        m.input_micros_per_mtok || m.output_micros_per_mtok
          ? h(
              "span",
              { class: "nowrap" },
              `${listPrice(m.input_micros_per_mtok)} in · ` +
                `${listPrice(m.output_micros_per_mtok)} out`,
            )
          : h("span", { class: "muted" }, "not billed"),
        m.subscription
          ? "What the API would have charged. No budget is charged, " +
              "because each person's Claude subscription pays."
          : "What requests to this model count against your budgets." +
              (m.kind === "chat"
                ? " Clients resend the whole conversation each turn, so " +
                  "input is charged again."
                : ""),
      ),
      m.long_prompt
        ? readRow(
            "Price for long prompts (CHF)",
            h(
              "span",
              { class: "nowrap" },
              `${listPrice(m.long_prompt.input_micros_per_mtok)} in · ` +
                `${listPrice(m.long_prompt.output_micros_per_mtok)} out`,
            ),
            `What a request costs instead when its prompt is over ` +
              `${num(m.long_prompt.above_tokens)} tokens.`,
          )
        : null,
      m.subscription
        ? readRow(
            "Paid by",
            pill("Claude subscription", "good"),
            ctx.state.me.claude_subscriptions
              ? [
                  "Only Claude Code signed in to a Claude plan can use it. " +
                    "Set it up with ",
                  h("code", {}, "keera connect claude-code --subscription"),
                  ".",
                ]
              : "This deployment does not take Claude subscriptions, so the " +
                  "gateway refuses this model.",
          )
        : null,
    ),
  });
}

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
    case "decision":
      return (
        "Answers no client. Routers and gate filters decide with it, " +
        "on its /v1/systemone endpoint."
      );
    default:
      return null;
  }
}

// checkModel shows the check report on the model's screen, which has no room
// for a verdict pill.
export async function checkModel(m) {
  try {
    report(await api.checkModel(m.org_id, m.alias), m.kind);
  } catch (ex) {
    toast(ex.message, "bad");
  }
}

// Only a chat check streams and asks for a tool call.
function report(probe, kind) {
  const chat = !kind || kind === "chat";
  const lines = [
    ["Backend", probe.backend || "-"],
    ["Reachable", probe.reachable ? "yes" : "no"],
    ["HTTP status", probe.status || "-"],
    ...(chat
      ? [
          ["Streamed", probe.streamed ? "yes" : "no"],
          [
            ["Tool call in ", h("code", {}, "tool_calls")],
            probe.tools_off
              ? "turned off on the backend"
              : probe.tool_calls
                ? "yes"
                : "no",
          ],
        ]
      : []),
    ["Model the backend served", probe.served_model || "-"],
    ["First token", probe.ttft_ms ? ms(probe.ttft_ms) : "-"],
    ["Total", probe.total_ms ? ms(probe.total_ms) : "-"],
  ];

  modal({
    wide: true,
    title: `Check - ${probe.alias}`,
    subtitle: probe.ok
      ? chat && !probe.tools_off
        ? "The model answered and produced a usable tool call."
        : "The model answered."
      : chat
        ? "A coding agent cannot use this model as it is."
        : "Clients cannot use this model as it is.",
    body: h(
      "div",
      {},
      probe.error
        ? banner("bad", probe.error)
        : banner(
            "good",
            chat && !probe.tools_off
              ? "Reachable, streaming, and the tool call came back in a form " +
                  "clients can use."
              : "Reachable, and the answer came back in a form clients can use.",
          ),
      (probe.warnings || []).map((wmsg) => banner("warn", wmsg)),
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
            code(probe.sample),
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

const KINDS = ["chat", "completion", "embedding", "decision"];

function kindName(kind) {
  return (
    {
      chat: "Chat",
      completion: "Completion",
      embedding: "Embedding",
      decision: "Decision",
    }[kind] || kind
  );
}

// The control plane sends only the lowercase key.
const PROVIDER_NAMES = {
  anthropic: "Anthropic",
  openai: "OpenAI",
  google: "Google",
  mistral: "Mistral",
  infomaniak: "Infomaniak",
  "stepping-stone": "stepping stone",
  phoeniqs: "Phoeniqs",
  cscs: "CSCS",
};

// An unknown provider shows its configured key.
export function providerName(name) {
  return PROVIDER_NAMES[name] || name || "";
}

export function servedName(m) {
  return m.display_name || m.backend_model;
}

// Must match internal/catalog/providers.go.
const PRODUCT_ID = "{product_id}";

// endpointOf fills in the product id where needed. Null while it is missing.
function endpointOf(p, state) {
  if (!p.needs_product_id) return p.endpoint;
  const id = ((state && state.productID) || "").trim();
  return id ? p.endpoint.replace(PRODUCT_ID, id) : null;
}

// aliasFor suggests an alias for a model id. Aliases go unquoted into the
// configs `keera connect` prints, so only lowercase letters, digits and inner
// hyphens.
function aliasFor(id) {
  const name = String(id || "")
    .split("/")
    .pop();
  return name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

// productIDOf reads the product id from a model's address, the only place it
// is kept. "" if the address does not match the template.
function productIDOf(p, backends) {
  if (!p || !p.needs_product_id) return "";
  const [head, tail] = p.endpoint.split(PRODUCT_ID);
  const url = String(backends || "")
    .split("\n")[0]
    .trim();
  if (!url.startsWith(head) || !url.endsWith(tail)) return "";
  const id = url.slice(head.length, url.length - tail.length);
  return /^[A-Za-z0-9_-]+$/.test(id) ? id : "";
}

// What a provider's table fills in. Each field gets a line under it naming
// where the value came from or what it replaced.
const PRESET_FIELDS = [
  {
    key: "backendModel",
    label: "model ID",
    fromModel: true,
    of: (p, m) => (m ? m.id : null),
    // A dated id is the same model, so it overrides nothing.
    same: (now, m) => isModel(m, now),
  },
  {
    key: "description",
    label: "description",
    fromModel: true,
    // Prose: the line under it cannot quote what an edit replaced. Never
    // locked, as only the organisation knows what the alias is for.
    prose: true,
    of: (p, m) => (m && m.description ? m.description : null),
  },
  {
    key: "displayName",
    label: "display name",
    fromModel: true,
    of: (p, m) => (m && m.display_name ? m.display_name : null),
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
    of: (p, m) =>
      m && m.max_context_tokens ? String(m.max_context_tokens) : null,
  },
  {
    key: "releaseDate",
    label: "release date",
    fromModel: true,
    of: (p, m) => (m && m.release_date ? m.release_date : null),
  },
  { key: "location", label: "location", of: (p) => p.location || null },
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
  {
    key: "priceWrite",
    label: "list cache-write price",
    fromModel: true,
    numeric: true,
    of: (p, m) =>
      m && m.cache_write_micros_per_mtok
        ? String(m.cache_write_micros_per_mtok / 1e6)
        : null,
  },
];

// pick is the catalogue's choice for a new model: a provider and one of its
// models, or null for self-hosted.
function editModel(ctx, existing, providers, pick) {
  const m = existing || { kind: "chat", enabled: true, backends: [] };
  const orgID = existing ? existing.org_id : ctx.orgID;

  const alias = h("input", {
    class: "input",
    value: m.alias || "",
    placeholder: "keera-code",
    disabled: !!existing,
  });
  const kind = h("select", { class: "select" });
  const backendModel = h("input", {
    class: "input",
    value: m.backend_model || "",
    placeholder: "keera-code",
  });
  const backends = h("textarea", {
    class: "input",
    rows: "3",
    placeholder: "http://keera-engine:8000/v1",
  });
  backends.value = (m.backends || []).join("\n");
  const maxContext = h("input", {
    class: "input",
    type: "number",
    min: "0",
    value: m.max_context_tokens || "",
    placeholder: "0",
  });
  const displayName = h("input", {
    class: "input",
    value: m.display_name || "",
    placeholder: "Qwen3 Coder Next",
  });
  const description = h("input", {
    class: "input",
    value: m.description || "",
    placeholder: "A fast local model for short edits and everyday questions",
  });
  const releaseDate = h("input", {
    class: "input",
    type: "date",
    value: m.release_date || "",
  });
  const location = h("input", {
    class: "input",
    value: m.location || "",
    placeholder: "from the backend URL",
    list: "model-locations",
  });
  const priceIn = priceInput(m.input_micros_per_mtok);
  const priceOut = priceInput(m.output_micros_per_mtok);
  const priceCached = priceInput(m.cached_input_micros_per_mtok);
  const priceWrite = priceInput(m.cache_write_micros_per_mtok);
  const fields = {
    kind,
    backendModel,
    displayName,
    description,
    backends,
    maxContext,
    releaseDate,
    location,
    priceIn,
    priceOut,
    priceCached,
    priceWrite,
  };
  setKinds(kind, KINDS, m.kind || "chat");

  // The key is never sent back, so empty means "keep it", not "none".
  const apiKey = h("input", {
    class: "input",
    type: "password",
    autocomplete: "off",
    value: "",
    placeholder: m.has_api_key ? "•••••••• stored - type to replace" : "sk-…",
  });
  const clearKey = m.has_api_key ? h("input", { type: "checkbox" }) : null;
  // Only Anthropic models can be paid by a Claude plan, which uses each
  // person's own Claude sign-in instead of a key.
  const subscription = h("input", {
    type: "checkbox",
    checked: !!m.subscription,
  });
  const enabled = h("input", {
    type: "checkbox",
    checked: m.enabled !== false,
  });

  // overridden holds the provider values this entry replaced. They stay
  // editable.
  const state = {
    place: null,
    preset: null,
    hosted: false,
    aliasDirty: !!existing,
    descriptionDirty: !!existing,
    overridden: new Set(),
    // Kept only in the address; read back from there for an existing model.
    productID: "",
  };
  alias.addEventListener("input", () => {
    state.aliasDirty = true;
  });
  // Like the alias, a written description survives picking another model.
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

  // basics stays visible for a self-hosted model; extras is folded under
  // Advanced options. A hosted model folds both. See layout.
  const basics = h(
    "div",
    {},
    field(
      "Backend URL",
      backends,
      "One per line. Several are used in turn.",
      marks.backends,
    ),
    h(
      "div",
      { class: "field-row" },
      field(
        "Backend model",
        backendModel,
        "The model name the backend serves, e.g. vLLM's --served-model-name.",
        marks.backendModel,
      ),
      field(
        "Context window",
        maxContext,
        "Advertised to clients. A request that cannot fit is refused.",
        marks.maxContext,
      ),
    ),
    h(
      "div",
      { class: "field-row" },
      field("Input price / Mtok (CHF)", priceIn, null, marks.priceIn),
      field("Output price / Mtok (CHF)", priceOut, null, marks.priceOut),
    ),
  );
  const extras = h(
    "div",
    {},
    h(
      "div",
      { class: "field-row" },
      field(
        "Display name",
        displayName,
        "The model's name for people.",
        marks.displayName,
      ),
      field("Kind", kind),
    ),
    h(
      "div",
      { class: "field-row" },
      field(
        "Location",
        location,
        "Where the model runs, such as ch or usa. Clients see it. Left " +
          "empty, a backend inside your network is onprem.",
        // Suggestions only: any short lowercase name is accepted.
        h(
          "datalist",
          { id: "model-locations" },
          ["onprem", "ch", "eu", "usa"].map((l) =>
            h("option", { value: l }, locationName(l)),
          ),
        ),
        marks.location,
      ),
      field(
        "Release date",
        releaseDate,
        "The day the model came out.",
        marks.releaseDate,
      ),
    ),
    // Blank means no rate, not a rate of zero: the input price applies.
    h(
      "div",
      { class: "field-row" },
      field(
        "Cached input price / Mtok (CHF)",
        priceCached,
        "For prompt tokens a hosted provider serves from its cache. If " +
          "blank, they are charged at the input price.",
        marks.priceCached,
      ),
      field(
        "Cache write price / Mtok (CHF)",
        priceWrite,
        "For prompt tokens a hosted provider writes to its cache. If " +
          "blank, they are charged at the input price.",
        marks.priceWrite,
      ),
    ),
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
  const slot = h("div", {});
  const summary = h("summary", {}, "Advanced options");
  const fold = h("details", { class: "fold" }, summary);
  const advanced = h("div", {});
  // layout places the groups: "self", "hosted" once a provider filled them
  // in, or null between choosing a provider and a model. `open` sets the
  // fold; undefined leaves it, "auto" keeps it closed unless already open.
  const layout = (how, open) => {
    if (open === "auto") open = advanced.contains(fold) && fold.open;
    if (!how) {
      slot.replaceChildren();
      advanced.replaceChildren();
      return;
    }
    const hosted = how === "hosted";
    slot.replaceChildren(...(hosted ? [] : [basics]));
    fold.replaceChildren(summary, ...(hosted ? [basics] : []), extras);
    if (open !== undefined) fold.open = open;
    advanced.replaceChildren(fold);
  };
  layout("self");
  // The product id sits just above the provider key.
  const productSlot = h("div", {});

  const hosted = hostedFields(ctx, providers, {
    fields,
    marks,
    state,
    layout,
    productSlot,
    alias,
    existing: !!existing,
    // Read from the entry, not guessed from its backend URL.
    initial: existing ? presetOf(providers, existing) : null,
    pick: existing ? null : pick,
  });

  const keyField = field(
    "Provider key",
    apiKey,
    "The key the gateway sends to the backend. Stored encrypted " +
      "and never shown again. Leave empty to keep the stored key.",
    clearKey
      ? h(
          "label",
          { class: "row-tight", style: { marginTop: "6px" } },
          clearKey,
          "Remove the stored key",
        )
      : null,
  );
  const subscriptionField = h(
    "div",
    { class: "field" },
    h(
      "label",
      { class: "row-tight" },
      subscription,
      "Paid by each person's Claude subscription",
    ),
    h(
      "div",
      { class: "hint" },
      "For Claude Team and Enterprise plans. Only Claude Code signed in to " +
        "the plan can use this model. The gateway forwards each person's " +
        "own sign-in and stores no key. The prices only show what the API " +
        "would have charged.",
    ),
  );
  const anthropicChosen = () =>
    state.hosted && !!state.place && state.place.p?.name === "anthropic";
  const paidByClaudePlan = () => anthropicChosen() && subscription.checked;
  // The form has no long-prompt price fields. A known model takes the
  // table's; any other keeps its own while its backend model is unchanged.
  const longPrompt = () => {
    const { m: known } = state.preset || {};
    if (state.hosted && known) return known.long_prompt || null;
    if (existing && backendModel.value.trim() === existing.backend_model)
      return existing.long_prompt || null;
    return null;
  };
  // The operator holds the key and the provider's endpoint and list prices
  // apply, so neither is asked.
  const onPlatformKey = () =>
    state.hosted && !!state.place && !!state.place.p?.platform_key;
  const platformNote = h(
    "div",
    { class: "field" },
    h("label", {}, "Provider key"),
    h(
      "div",
      { class: "hint" },
      "Provided by Keera directly. Use is billed to your organisation.",
    ),
  );
  // A model already paid by subscription still shows the box, so it can be
  // turned back.
  const offersSubscription =
    !!ctx.state.me.claude_subscriptions || !!m.subscription;
  const paintSubscription = () => {
    const platform = onPlatformKey() && !paidByClaudePlan();
    subscriptionField.hidden = !anthropicChosen() || !offersSubscription;
    keyField.hidden = paidByClaudePlan() || platform;
    platformNote.hidden = !platform;
    productSlot.hidden = platform;
  };
  subscription.addEventListener("change", paintSubscription);

  const rest = h(
    "div",
    {},
    field(
      "Alias",
      alias,
      "The name clients use. Changing it breaks them." +
        ` Only ${orgNameOf(ctx)} can use this model.`,
    ),
    // Kept outside the fold: routers choose by it.
    field(
      "Description",
      description,
      "One sentence: what it is good at and where it runs. Routers " +
        "use this to choose a model.",
      marks.description,
    ),
    productSlot,
    subscriptionField,
    slot,
    // The key stays above the fold: provider and model are already shown.
    keyField,
    platformNote,
    advanced,
  );

  paintSubscription();

  formModal({
    wide: true,
    title: existing ? `Edit ${m.alias}` : "Add model",
    body: h("form", { onSubmit: (e) => e.preventDefault() }, hosted, rest),
    submitLabel: existing ? "Save" : "Add model",
    done: existing ? "Model saved" : "Model added",
    // A new model comes from the catalogue; show it among the others instead
    // of reloading the catalogue.
    ctx: existing ? ctx : { ...ctx, reload: () => ctx.navigate("/models") },
    submit: async () => {
      const list = backends.value
        .split("\n")
        .map((s) => s.trim())
        .filter(Boolean);
      const preset = state.hosted ? state.preset : null;
      // The control plane sets the endpoint itself.
      const platform = onPlatformKey() && !paidByClaudePlan();
      if (platform && preset) list.splice(0, list.length, preset.p.endpoint);
      // Where a provider needs a product id, an empty backend means the id
      // is missing.
      if (
        preset &&
        !platform &&
        preset.p.needs_product_id &&
        !state.overridden.has("backends") &&
        !list.length
      ) {
        throw new Error(
          `${providerName(preset.p.name)} gives each customer their own ` +
            "address. Fill in the product ID.",
        );
      }
      if (!alias.value.trim() || !backendModel.value.trim() || !list.length) {
        // Under a provider, only for an unknown model, whose fields are
        // folded.
        throw new Error(
          state.hosted
            ? "Enter the alias, and the backend model and backends under " +
                "Advanced options."
            : "Enter an alias, a backend model and at least one backend.",
        );
      }
      const problem =
        modelAliasProblem(alias.value.trim()) ||
        (!existing &&
          takenProblem(
            (await api.models(orgID)).data,
            alias.value.trim(),
            "model",
          ));
      if (problem) {
        alias.focus();
        throw new Error(problem);
      }
      // Unset unless a key was typed or removal asked for: "" wipes it.
      const paidByPlan = paidByClaudePlan();
      let credential;
      if (paidByPlan || platform) credential = undefined;
      else if (clearKey && clearKey.checked) credential = "";
      else if (apiKey.value) credential = apiKey.value;

      const declaration = {
        alias: alias.value.trim(),
        kind: kind.value,
        backends: list,
        backend_model: backendModel.value.trim(),
        // Lets the next edit tell provider values from the administrator's.
        provider: state.hosted && state.preset ? state.preset.p.name : "",
        max_context_tokens: parseInt(maxContext.value, 10) || 0,
        display_name: displayName.value.trim(),
        description: description.value.trim(),
        release_date: releaseDate.value,
        // Left empty, the control plane fills in the provider's, or onprem.
        location: location.value.trim().toLowerCase(),
        // Empty is sent as unset, so an unknown model is refused rather than
        // saved unbilled.
        input_micros_per_mtok: statedMicros(priceIn.value),
        output_micros_per_mtok: statedMicros(priceOut.value),
        cached_input_micros_per_mtok: toMicros(priceCached.value),
        cache_write_micros_per_mtok: toMicros(priceWrite.value),
        long_prompt: longPrompt(),
        subscription: paidByPlan,
        enabled: enabled.checked,
      };
      try {
        await api.putModel(orgID, declaration.alias, {
          ...declaration,
          ...(credential === undefined ? {} : { api_key: credential }),
        });
      } catch (ex) {
        // A backend outside the network needs a location, which may be
        // folded.
        if (/location/i.test(ex.message) && advanced.contains(fold)) {
          fold.open = true;
          setTimeout(() => location.focus());
        }
        throw ex;
      }
    },
  });
}

// presets returns none on error rather than failing the screen.
async function presets() {
  try {
    return (await api.providers()).data || [];
  } catch {
    return [];
  }
}

// setKinds offers only the kinds the provider serves; others are refused on
// save.
function setKinds(select, kinds, selected) {
  const list = kinds && kinds.length ? kinds : KINDS;
  select.replaceChildren(
    ...list.map((k) => h("option", { value: k }, kindName(k))),
  );
  select.value = list.includes(selected) ? selected : list[0];
}

// presetOf finds a stored model's provider and model in the table, or null.
function presetOf(providers, m) {
  const p = (providers || []).find((x) => x.name === m.provider);
  if (!p) return null;
  return {
    p,
    m: (p.models || []).find((mm) => isModel(mm, m.backend_model)) || null,
  };
}

// isModel matches by id or by dated snapshot id.
function isModel(mm, id) {
  return mm.id === id || (!!mm.snapshot && mm.snapshot === id);
}

// providerAnswers says which fields the provider fills in. They are locked,
// as a typed value would quietly stop matching the provider.
function providerAnswers(p, known) {
  return {
    backends: true,
    location: true,
    releaseDate: !!known && !!known.release_date,
    displayName: !!known && !!known.display_name,
    // No description on purpose: only the organisation knows what the alias
    // is for, and routers need that.
    kind: (p.kinds || []).length === 1,
    backendModel: !!known,
    maxContext: !!known,
    priceIn: !!known,
    priceOut: !!known,
    // No published rate leaves the field open. Blank means the input price.
    priceCached: !!known && !!known.cached_input_micros_per_mtok,
    priceWrite: !!known && !!known.cache_write_micros_per_mtok,
  };
}

// hostedFields is the top of the form: where the model runs. A provider, a
// model and a key should be the whole job, so what the provider implies is
// filled in, locked and folded away. A value an entry already overrode, such
// as an egress proxy address, stays editable.
function hostedFields(ctx, providers, form) {
  if (!providers || !providers.length) return null;
  const {
    fields,
    marks,
    state,
    layout,
    productSlot,
    alias,
    existing,
    initial,
    pick,
  } = form;

  const note = h("div");
  // A place with no `p` is self-hosted.
  const places = [
    {
      p: null,
      name: "Self-hosted",
      what: "Your own inference server, such as vLLM, Ollama or Keera Engine.",
    },
    ...providers.map((p) => ({
      p,
      name: providerName(p.name),
      what: p.summary,
    })),
  ];
  const markOf = (place) =>
    place.p
      ? providerMark(place.p.name) || icon(icons.cloud)
      : icon(icons.selfHosted);

  const model = h("select", { class: "select" });
  // Some providers give each customer their own address, so the missing part
  // is asked for.
  const productID = h("input", {
    class: "input",
    value: "",
    placeholder: "100234",
    autocomplete: "off",
  });
  const productIDField = field(
    "Product ID",
    productID,
    "Not a secret: it is part of the address. Find it in the Infomaniak " +
      "console under AI Tools.",
  );
  productIDField.hidden = true;
  const block = h("div", { hidden: true }, field("Model", model), note);
  if (productSlot) productSlot.replaceChildren(productIDField);

  const chosen = () => (state.place ? state.place.p : null);

  const paintNote = () => {
    const p = chosen();
    if (!p) return;
    productIDField.hidden = !p.needs_product_id;
    const lines = [];
    if (state.preset && !state.preset.m) {
      lines.push(
        banner(
          "info",
          "This build does not know that model. Enter its context window " +
            "and prices under Advanced options. Blank or zero means no price. " +
            "A cached-input price of zero means the input price.",
        ),
      );
    }
    note.replaceChildren(...lines);
  };

  // An entry that wrote its own address keeps it.
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

  // The fields the provider fills in now, or null for self-hosted.
  const answered = () => {
    const { p, m } = state.preset || {};
    if (!state.hosted || !p) return null;
    const fixed = providerAnswers(p, m);
    for (const key of state.overridden) fixed[key] = false;
    // On the operator's key the control plane sets endpoint and prices.
    if (p.platform_key) {
      fixed.backends = true;
      for (const key of ["priceIn", "priceOut", "priceCached", "priceWrite"])
        fixed[key] = !!m;
    }
    return fixed;
  };

  // Only the panel locks fields. The control plane accepts overrides from
  // template files and the CLI.
  const paintLocks = () => {
    const fixed = answered();
    for (const [key, el] of Object.entries(fields)) {
      el.disabled = !!(fixed && fixed[key]);
    }
    return fixed;
  };

  const paintMachinery = (open) => {
    const fixed = paintLocks();
    if (state.hosted && !state.preset) layout(null);
    else layout(fixed ? "hosted" : "self", open);
  };

  // Read before any provider writes over the field.
  const unknownModel =
    initial && !initial.m ? fields.backendModel.value.trim() : null;
  const fillModels = () => {
    const p = chosen();
    model.replaceChildren(
      ...p.models.map((mm, i) =>
        h(
          "option",
          { value: String(i), title: mm.id },
          mm.display_name || mm.id,
        ),
      ),
      // Other models are added with the CLI or a template file; one being
      // edited keeps its own entry.
      ...(unknownModel && p === initial.p
        ? [h("option", { value: "other" }, unknownModel)]
        : []),
    );
    // Picking a provider fills the form at once.
    model.value = "0";
  };

  const applyModel = () => {
    const p = chosen();
    const known =
      model.value === "other" ? null : p.models[Number(model.value)];
    state.preset = { p, m: known };
    // The table overwrites every field, so nothing is an override any more.
    state.overridden.clear();
    for (const f of PRESET_FIELDS) {
      // Except a description somebody wrote.
      if (f.key === "description" && state.descriptionDirty) continue;
      const def = f.of(p, known, state);
      if (def !== null) fields[f.key].value = def;
      else if (f.fromModel) fields[f.key].value = "";
    }
    // Clears the endpoint until a product id is in.
    syncEndpoint();
    setKinds(fields.kind, p.kinds, fields.kind.value);
    if (!state.aliasDirty) alias.value = known ? aliasFor(known.id) : "";
    paintMachinery("auto");
    paintPresets(state.preset, fields, marks, state);
    paintNote();
  };

  model.addEventListener("change", applyModel);

  // choose sets where a new model runs, with the catalogue's model or the
  // provider's first.
  const choose = (place, mm) => {
    state.place = place;
    state.hosted = !!place.p;
    if (place.p) {
      fillModels();
      const i = mm ? place.p.models.findIndex((x) => x.id === mm.id) : -1;
      if (i >= 0) model.value = String(i);
      applyModel();
    } else {
      // The control plane works the location out from the backend URL.
      paintMachinery();
    }
  };

  // An existing hosted model keeps its stored values: never write a default
  // over an override.
  if (initial) {
    state.place = places.find((place) => place.p === initial.p);
    state.hosted = true;
    fillModels();
    model.value = initial.m
      ? String(initial.p.models.indexOf(initial.m))
      : "other";
    state.preset = initial;
    // Before comparing: the table needs the product id to build the address.
    state.productID = productIDOf(initial.p, fields.backends.value);
    productID.value = state.productID;
    for (const f of PRESET_FIELDS) {
      const def = f.of(initial.p, initial.m, state);
      if (def !== null && !sameValue(f, fields[f.key].value, def, initial.m)) {
        state.overridden.add(f.key);
      }
    }
    // An address with no product id, such as an egress proxy, is an override.
    if (initial.p.needs_product_id && !state.productID) {
      state.overridden.add("backends");
    }
    // A written description stays when another model is picked; an empty
    // one follows the provider.
    state.descriptionDirty =
      state.overridden.has("description") &&
      fields.description.value.trim() !== "";
    // A kind the provider does not serve was chosen on purpose; narrowing the
    // list would change it.
    if ((initial.p.kinds || []).includes(fields.kind.value)) {
      setKinds(fields.kind, initial.p.kinds, fields.kind.value);
    } else {
      state.overridden.add("kind");
    }
    paintPresets(initial, fields, marks, state);
    paintMachinery("auto");
    paintNote();
  } else if (existing) {
    // Self-hosted: nothing to choose, so nothing is reset.
    state.place = places[0];
  } else {
    const picked = pick && places.find((x) => x.p && x.p.name === pick.p.name);
    choose(picked || places[0], pick && pick.m);
  }
  block.hidden = !state.place.p;

  // Moving a model elsewhere makes a new model, so this is shown, not asked.
  const place = state.place;
  return h(
    "div",
    { class: "field" },
    h("label", {}, "Where the model runs"),
    h(
      "div",
      { class: "tile-chosen" },
      markOf(place),
      h(
        "div",
        { class: "stack", style: { gap: "1px" } },
        h("span", { class: "tile-name" }, place.name),
        place.what ? h("span", { class: "tile-what" }, place.what) : null,
      ),
    ),
    block,
  );
}

// paintPresets writes the line under each filled-in field.
function paintPresets(preset, fields, marks, state) {
  for (const f of PRESET_FIELDS) {
    const line = marks[f.key];
    const def = preset ? f.of(preset.p, preset.m, state) : null;
    line.hidden = def === null;
    if (def === null) {
      line.replaceChildren();
      continue;
    }
    const who = providerName(preset.p.name);
    line.replaceChildren(
      sameValue(f, fields[f.key].value, def, preset.m)
        ? `${who}'s ${f.label}.`
        : f.prose
          ? `Replaces ${who}'s own ${f.label}.`
          : `Overrides ${who}'s ${f.label} of ${def}.`,
    );
  }
}

// sameValue compares numbers as numbers, because 5 and 5.0 are one price.
function sameValue(f, now, def, m) {
  now = String(now).trim();
  if (f.same) return f.same(now, m);
  return f.numeric ? Number(now) === Number(def) : now === def;
}

function priceInput(v) {
  return h("input", {
    class: "input",
    type: "number",
    min: "0",
    step: "0.01",
    placeholder: "0.00",
    value: v ? v / 1e6 : "",
  });
}

function toMicros(v) {
  const n = parseFloat(v);
  return Number.isFinite(n) && n > 0 ? Math.round(n * 1e6) : 0;
}

// statedMicros is null for an empty field.
function statedMicros(v) {
  return String(v).trim() === "" ? null : toMicros(v);
}
