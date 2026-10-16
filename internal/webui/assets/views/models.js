// The organisation's models.
//
// Every model belongs to one organisation, and its administrators manage it.
// Everybody else reads the list, because a developer needs to know what the
// models are.

import { api } from "../api.js";
import { orgNameOf } from "./orgs.js";
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
  checkButton,
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
} from "../ui.js";

// usersOfModel names the filters and routers that use a model, for the
// removal warning. Removing it does not stop them.
async function usersOfModel(m) {
  const [filters, routers] = await Promise.all([
    api.filters(m.org_id).then((r) => r.data || []),
    api.routers(m.org_id).then((r) => r.data || []),
  ]);
  const f = filters.filter((x) => x.model === m.alias).map((x) => x.alias);
  const r = routers
    .filter(
      (x) => x.model === m.alias || (x.destinations || []).includes(m.alias),
    )
    .map((x) => x.alias);
  let text = "";
  if (f.length)
    text += ` The filters ${f.join(", ")} run on it and will refuse every request they cover.`;
  if (r.length) text += ` The routers ${r.join(", ")} will lose it.`;
  return text;
}

// canAddModel reports whether the reader may add a model to the organisation
// in view.
function canAddModel(ctx) {
  return isAdmin(ctx) && !!ctx.orgID;
}

export async function modelsView(ctx) {
  const since = currentRange();
  const res = await api.models(ctx.orgID, { stats: true, since });
  const models = res.data || [];
  const stats = res.stats || {};
  const canAdd = canAddModel(ctx);
  // The hosted endpoints this binary knows. They only fill in a form, so a
  // deployment that cannot reach them still gets a working catalogue screen.
  const providers = canAdd ? await presets() : [];
  ctx.setSubtitle(`${models.length} ${plural(models.length, "model")}`);

  // A model is added from the catalogue, which also offers a self-hosted one.
  // Everyone else can still browse it.
  const head = listHead(
    null,
    [
      rangePicker(ctx, since),
      !canAdd
        ? h(
            "a",
            {
              class: "btn",
              href: "/provider-models",
              title: "Every model the hosted providers offer",
              onClick: go(ctx, "/provider-models"),
            },
            "Model catalogue",
          )
        : null,
    ],
    canAdd && {
      label: "Add model",
      onClick: go(ctx, "/provider-models"),
    },
  );

  const rows = table(
    [
      {
        // The alias opens the model's own screen: whether it is being used, by
        // whom, how fast it is answering and what it has been failing with.
        // None of that is configuration, which is what the rest of this row is.
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
                // The provider's own mark, where this build has one. It is
                // read faster than the word beside it down a column of them,
                // which is the whole of what it is for - so the word stays: a
                // logo is not a label, and a provider this build has no mark
                // for has to read the same as one it does.
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
        sortKey: (m) => m.max_context || null,
        sortDir: "desc",
        cell: (m) => (m.max_context ? compact(m.max_context) : dash()),
      },
      {
        // The same median as on the model's own screen, over the same window.
        label: "First token",
        num: true,
        shrink: true,
        sortKey: (m) => stats[m.alias]?.ttft_median_ms || null,
        cell: (m) => ttftCell(stats[m.alias]),
      },
      {
        label: "Price / Mtok (CHF)",
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
                `${listPrice(m.input_micros_per_mtok)} in · ${listPrice(m.output_micros_per_mtok)} out`,
              )
            : h("span", { class: "faint" }, "not billed"),
      },
      {
        // Enabled says an administrator turned it on. Health says the inference
        // plane actually answers, and - the one that matters - that a tool call
        // comes back as a tool call. They are not the same claim and the panel
        // used to only make the first.
        label: "Status",
        shrink: true,
        sortKey: (m) => (m.enabled ? 1 : 0),
        cell: (m) =>
          m.enabled ? pill("Enabled", "good") : pill("Disabled", "warn"),
      },
      isAdmin(ctx) && {
        label: "Check",
        shrink: true,
        cell: (m) => healthCell(m),
      },
      {
        label: "",
        shrink: true,
        cell: (m) => {
          // A member gets the same row opened for reading. What a model is
          // configured to do is what their requests to it will meet, so the
          // answer to "why does this one cost that much" or "how large a
          // request may I send" is here rather than in a message to an
          // administrator.
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
            // The same warnings `keera model delete` gives.
            body: async () =>
              "Clients that still use this model will be refused, and " +
              "guardrails that allow only this model stop allowing anything." +
              (await usersOfModel(m)) +
              (m.has_api_key ? " Its stored credential is removed." : "") +
              " Usage history and the audit log are kept.",
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
      // The backend model and the endpoints are searched as well as the alias,
      // the endpoints even though no column shows them any more: "which models
      // still point at the old cluster" is a question asked of this screen
      // whenever one is being moved, and the rows that come back are the
      // answer whether or not the URL that matched is on them.
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
    },
  );

  return h("div", {}, head, rows);
}

// providerModelsView lists every model the hosted providers offer, with what
// it costs and whether this organisation already serves it. Everyone can read
// it. It is also where an administrator adds a model: a row opens the form
// with the provider and the model already picked, and the button above opens
// it for a self-hosted one.
export async function providerModelsView(ctx) {
  const canEdit = canAddModel(ctx);
  const [providers, models] = await Promise.all([
    presets(),
    ctx.orgID ? api.models(ctx.orgID).then((r) => r.data || []) : [],
  ]);

  const rows = providers.flatMap((p) =>
    (p.models || []).map((m) => ({
      p,
      m,
      // The aliases that already serve this model, so it is not added twice
      // by accident.
      aliases: models
        .filter((x) => x.provider === p.name && isModel(m, x.backend_model))
        .map((x) => x.alias),
    })),
  );
  ctx.setSubtitle(
    `${rows.length} ${plural(rows.length, "model")} from ` +
      `${providers.length} ${plural(providers.length, "provider")}`,
  );

  const price = (v) =>
    v ? h("span", { class: "nowrap" }, "CHF " + listPrice(v)) : dash();

  // A model on your own inference server is in no provider's table, so it is
  // added from here rather than from a row.
  const head = h(
    "div",
    { class: "detail-head" },
    crumb(ctx, "/models", "Models"),
    canEdit
      ? h(
          "div",
          { class: "row", style: { justifyContent: "flex-end" } },
          h(
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
          ),
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
        sortKey: (r) => r.m.max_context || null,
        sortDir: "desc",
        cell: (r) => (r.m.max_context ? compact(r.m.max_context) : dash()),
      },
      {
        label: "Input / Mtok",
        num: true,
        shrink: true,
        sortKey: (r) => r.m.input_micros_per_mtok || null,
        cell: (r) => price(r.m.input_micros_per_mtok),
      },
      {
        label: "Cached input / Mtok",
        num: true,
        shrink: true,
        sortKey: (r) => r.m.cached_input_micros_per_mtok || null,
        cell: (r) => price(r.m.cached_input_micros_per_mtok),
      },
      {
        label: "Cache write / Mtok",
        num: true,
        shrink: true,
        sortKey: (r) => r.m.cache_write_micros_per_mtok || null,
        cell: (r) => price(r.m.cache_write_micros_per_mtok),
      },
      {
        label: "Output / Mtok",
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
            ? h(
                "div",
                { class: "stack" },
                r.aliases.map((a) =>
                  h(
                    "span",
                    { class: "mono" },
                    rowLink(ctx, "/models/" + encodeURIComponent(a), a),
                  ),
                ),
              )
            : dash(),
      },
      canEdit
        ? {
            label: "",
            shrink: true,
            cell: (r) =>
              h(
                "button",
                {
                  class: "btn btn-sm",
                  title: `Add this model to ${orgNameOf(ctx)}`,
                  onClick: () =>
                    editModel(ctx, null, providers, { p: r.p, m: r.m }),
                },
                icon(icons.plus),
                "Add",
              ),
          }
        : null,
    ].filter(Boolean),
    rows,
    {
      emptyTitle: "No hosted models",
      emptyBody: "This build knows no hosted providers.",
      // Newest first: the model somebody is looking for is usually a new one.
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
      choices: [
        {
          label: "Providers",
          options: providers.map((p) => ({
            value: p.name,
            label: providerName(p.name),
          })),
          test: (r, v) => r.p.name === v,
        },
        {
          label: "Locations",
          options: [...new Set(providers.map((p) => p.location))]
            .filter(Boolean)
            .map((l) => ({ value: l, label: locationName(l) })),
          test: (r, v) => r.p.location === v,
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
      toggles: [
        {
          label: "Only models not in use",
          test: (r) => !r.aliases.length,
        },
      ],
    },
  );

  return h("div", {}, head, list);
}

// locationCell is where a model runs, with the code it is stored as on hover.
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

// ttftCell is the median wait for the first token, and the p95 under it.
// A model with no answered request in the window gets a dash.
function ttftCell(st) {
  if (!st?.ttft_median_ms) return dash();
  return h(
    "div",
    { class: "stack nowrap" },
    h("span", {}, ms(st.ttft_median_ms)),
    h("span", { class: "faint small" }, `p95 ${ms(st.ttft_p95_ms)}`),
  );
}

// openModel is this model's dialog opened from somewhere other than the
// catalogue - the model's own screen, which links here rather than growing a
// second copy of the same form. Which of the two dialogs it is depends on the
// reader, exactly as it does on the row.
export async function openModel(ctx, m) {
  if (!isAdmin(ctx)) return viewModel(ctx, m);
  return editModel(ctx, m, await presets());
}

// viewModel is the model as a member meets it: what it is called, what it
// serves, how large a request may be, and what a token costs.
//
// It is not the edit form with its fields disabled. Half of that form is where
// the model points - the endpoints and the credential - and the control plane
// sends neither to anybody but an administrator, so a disabled form would
// mostly be empty boxes with no explanation for being empty. What is left is
// what a member is actually held to, and it reads better as an answer than as a
// field they cannot fill in.
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
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Close"),
    ],
  });
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
    case "decision":
      return (
        "Answers no client. Routers and gate filters decide with it, " +
        "on its /v1/systemone endpoint."
      );
    default:
      return null;
  }
}

// healthCell is the check button and, once it has run, its verdict in place.
function healthCell(m) {
  return checkButton(
    "Send a test request to this model's backend",
    () => api.checkModel(m.org_id, m.alias),
    (probe) => verdict(probe, m.kind),
  );
}

// checkModel runs the check and opens its report, for the model's own screen,
// which has room for the report but not for a verdict pill.
export async function checkModel(m) {
  try {
    report(await api.checkModel(m.org_id, m.alias), m.kind);
  } catch (ex) {
    toast(ex.message, "bad");
  }
}

// verdict is the pill, and the whole report behind it. The pill has to be
// readable at a glance and the report has to be complete, so they are not the
// same thing.
function verdict(probe, kind) {
  const tone = probe.ok
    ? (probe.warnings || []).length
      ? "warn"
      : "good"
    : "bad";
  const label = probe.ok
    ? probe.tools_off
      ? "No tool calls"
      : (probe.warnings || []).length
        ? "Working, with notes"
        : "Working"
    : probe.tool_call_as_text
      ? "Tool calls broken"
      : probe.reachable
        ? "Answering, unusable"
        : "Unreachable";
  return {
    label,
    tone,
    title: "The full report",
    open: () => report(probe, kind),
  };
}

// Only a chat check streams and asks for a tool call. A completion or
// embedding check sends one plain request, so the report leaves those out.
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
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Close"),
    ],
  });
}

const KINDS = ["chat", "completion", "embedding", "decision"];

// kindName is a kind as a person reads it. The value stays lowercase on the
// wire and in catalogue files.
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

// PROVIDER_NAMES are the hosted providers as they write their own names. The
// control plane only sends the lowercase key.
const PROVIDER_NAMES = {
  anthropic: "Anthropic",
  openai: "OpenAI",
  infomaniak: "Infomaniak",
  "stepping-stone": "stepping stone",
  phoeniqs: "Phoeniqs",
  cscs: "CSCS",
};

// providerName is a hosted provider's display name. One this build does not
// know keeps its key, which is still the name it was configured with.
function providerName(name) {
  return PROVIDER_NAMES[name] || name || "";
}

// servedName is what a model serves, by its name for people when it has one.
export function servedName(m) {
  return m.display_name || m.backend_model;
}

// What a per-customer endpoint carries in place of the part only the
// organisation knows. It is the same text internal/catalog/providers.go writes.
const PRODUCT_ID = "{product_id}";

// endpointOf is the address a provider's model is actually reached at: its
// endpoint for the providers that serve everybody at one, and that endpoint
// with the organisation's own product id written in for the ones that do not.
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
// letters and dots. This is a suggestion the administrator types over, so the
// job is only to offer something the form will accept.
function aliasFor(id) {
  const name = String(id || "")
    .split("/")
    .pop();
  return name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

// productIDOf reads a product id back out of a stored model's address, because
// that is the only place it is kept: it is part of a URL, not a field of a
// model. An address that does not match the provider's template is the
// administrator's own, and gives none back.
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

// What a provider's table fills in. Each of these keeps a line under its input
// naming where the value came from, and - once it has been edited - the value
// it replaced, for the hosted case that still shows these fields: a model this
// build cannot price.
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
    // The one default that is prose. Two things follow from that: the line
    // under the field cannot quote what an edit replaced, the way it quotes a
    // price - and the field is never disabled, because a provider's sentence
    // says what the model is and only this organisation knows what its alias
    // is for. It is a starting point, and the point of it is being edited.
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
    of: (p, m) => (m && m.max_context ? String(m.max_context) : null),
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

// pick is where a new model runs, as the catalogue offers it: a provider and
// one of its models, or null for a self-hosted one.
function editModel(ctx, existing, providers, pick) {
  const m = existing || { kind: "chat", enabled: true, backends: [] };
  // Whose model this is: an existing one keeps its owner, and a new one is the
  // organisation's in view.
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
    value: m.max_context || "",
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
  // The fields a provider can fill in, by the names PRESET_FIELDS knows them
  // by.
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

  // The credential itself. It is write-only in both directions: the control
  // plane never sends one back, so an empty field means "leave it as it is"
  // rather than "there is none", and the placeholder is the only status the
  // panel can show.
  const apiKey = h("input", {
    class: "input",
    type: "password",
    autocomplete: "off",
    value: "",
    placeholder: m.has_api_key ? "•••••••• stored - type to replace" : "sk-…",
  });
  const clearKey = m.has_api_key ? h("input", { type: "checkbox" }) : null;
  // Only Anthropic's models can be paid by a Claude plan. Such a model is sent
  // each person's own Claude sign-in, so it takes no key of its own.
  const subscription = h("input", {
    type: "checkbox",
    checked: !!m.subscription,
  });
  const enabled = h("input", {
    type: "checkbox",
    checked: m.enabled !== false,
  });

  // What the form is doing, for the parts of it that have to agree: which
  // provider's defaults are in the fields, whether a hosted endpoint was chosen
  // at all, whether the administrator has named the alias themselves, and which
  // of the provider's answers this entry had already written over before the
  // dialog opened - those stay the administrator's to edit.
  const state = {
    // Where the model runs: an inference plane of your own, or one of the
    // providers.
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

  // The fields that describe the backend rather than the alias, in two groups.
  // basics is what a self-hosted model cannot do without, so it stays on the
  // screen for one. extras has a default or does not matter to most models,
  // so it is folded away under Advanced options. A hosted model's provider
  // answers both, so both are folded away for it. See layout.
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
      h(
        "div",
        { class: "field" },
        h("label", {}, "Location"),
        location,
        // Suggestions, not a fixed list: any short lowercase name is taken.
        h(
          "datalist",
          { id: "model-locations" },
          ["onprem", "ch", "usa"].map((l) =>
            h("option", { value: l }, locationName(l)),
          ),
        ),
        h(
          "div",
          { class: "hint" },
          "Where the model runs, such as ch or usa. Clients see it. Left " +
            "empty, a backend inside your network is onprem.",
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
    // Left blank, these are not a rate of nothing but no rate at all: those
    // tokens are then charged at the input price.
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
  // Where the two groups sit. slot holds basics inline, advanced holds the
  // fold.
  const slot = h("div", {});
  const summary = h("summary", {}, "Advanced options");
  const fold = h("details", { class: "fold" }, summary);
  const advanced = h("div", {});
  // layout puts the groups in place: "self" for a model on your own inference
  // server, "hosted" once a provider has answered for it, and null between
  // choosing a provider and a model, when there is nothing to show.
  //
  // `open` is true or false to set the fold, undefined to leave it as it was,
  // and "auto" for a fold appearing: closed, unless it was already open.
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
  // Where the product id sits, for the one provider that asks for one: just
  // above the provider key, which is the other half of the same answer.
  const productSlot = h("div", {});

  const hosted = hostedFields(ctx, providers, {
    fields,
    marks,
    state,
    layout,
    productSlot,
    alias,
    existing: !!existing,
    // Whether this is already a hosted model, which is a thing the entry
    // says rather than a thing to guess from its backend URL.
    initial: existing ? presetOf(providers, existing) : null,
    pick: existing ? null : pick,
  });

  const keyField = h(
    "div",
    { class: "field" },
    h("label", {}, "Provider key"),
    apiKey,
    h(
      "div",
      { class: "hint" },
      "The key the gateway sends to the backend. Stored encrypted " +
        "and never shown again. Leave empty to keep the stored key.",
    ),
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
  // The form has no fields for a long prompt's prices. A model the table
  // knows takes the table's, and any other keeps its own while it names the
  // same backend model.
  const longPrompt = () => {
    const { m: known } = state.preset || {};
    if (state.hosted && known) return known.long_prompt || null;
    if (existing && backendModel.value.trim() === existing.backend_model)
      return existing.long_prompt || null;
    return null;
  };
  // The operator holds the key for this provider. The model is sent to the
  // provider's own endpoint at its list prices, so those are not asked.
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
  // A deployment without Claude subscriptions offers none. A model that is
  // one still shows the box, so it can be turned back into an ordinary one.
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

  // Everything below where the model runs: that decides whether these fields
  // are questions at all, or a provider's answers folded away.
  const rest = h(
    "div",
    {},
    field(
      "Alias",
      alias,
      "The name clients use. Changing it breaks them." +
        ` Only ${orgNameOf(ctx)} can use this model.`,
    ),
    // Under the alias, and outside the backend fields below, for two
    // reasons. It is the one field on this form addressed to a reader rather
    // than to the inference plane, so it belongs with the name - and the
    // backend fields are folded away for a hosted model whose provider
    // answered for them, which is the model a router most needs described.
    field(
      "Description",
      description,
      "One sentence: what it is good at and where it runs. Routers " +
        "use this to choose a model.",
      marks.description,
    ),
    productSlot,
    subscriptionField,
    // Above the fold, because it is a question rather than an answer: a
    // hosted model takes a provider, a model and a key, and the key is the
    // only one of the three that is not on the screen already.
    slot,
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
    ctx,
    submit: async () => {
      const list = backends.value
        .split("\n")
        .map((s) => s.trim())
        .filter(Boolean);
      // Said on its own, because for this provider an empty backend is not a
      // field left blank - it is the one question the form asked instead of
      // showing a URL.
      const preset = state.hosted ? state.preset : null;
      // The control plane sets the endpoint itself.
      const platform = onPlatformKey() && !paidByClaudePlan();
      if (platform && preset) list.splice(0, list.length, preset.p.endpoint);
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
        // Under a provider the only way to get here is editing a model the
        // table does not know: the table answers both of those fields for one
        // it does, and the other is named and addressed inside the fold.
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
      // Absent unless the administrator typed one or asked for the stored one
      // to go: sending "" on every save would wipe it.
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
        // What the fields below are: this provider's answers, or nobody's but
        // the administrator's. Saving it is what lets the next edit of this
        // model tell the two apart.
        provider: state.hosted && state.preset ? state.preset.p.name : "",
        max_context: parseInt(maxContext.value, 10) || 0,
        display_name: displayName.value.trim(),
        description: description.value.trim(),
        release_date: releaseDate.value,
        // Left empty, the control plane fills in the provider's, or onprem.
        location: location.value.trim().toLowerCase(),
        // Left empty, these two are sent as unset, so a model the price table
        // does not know is refused rather than saved unbilled.
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
        // A backend outside the network needs a location, and that field is
        // folded away. Focused once the error is shown.
        if (/location/i.test(ex.message) && advanced.contains(fold)) {
          fold.open = true;
          setTimeout(() => location.focus());
        }
        throw ex;
      }
    },
  });
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
  select.replaceChildren(
    ...list.map((k) => h("option", { value: k }, kindName(k))),
  );
  select.value = list.includes(selected) ? selected : list[0];
}

// presetOf says which provider's row of the table a stored model sits on, and
// which of that provider's models it serves. A model naming no provider, or one
// naming a provider this build has since dropped, sits on none: its fields are
// then nobody's but the administrator's, which is exactly how the form treats
// them.
function presetOf(providers, m) {
  const p = (providers || []).find((x) => x.name === m.provider);
  if (!p) return null;
  return {
    p,
    m: (p.models || []).find((mm) => isModel(mm, m.backend_model)) || null,
  };
}

// isModel reports whether id names a model of the table: by its id, or by the
// dated id its maker also names it by.
function isModel(mm, id) {
  return mm.id === id || (!!mm.snapshot && mm.snapshot === id);
}

// providerAnswers says which of the fields below the chosen provider answers
// for. Those are not the administrator's to edit: the value they typed would be
// the one that stops matching the provider they named, and nothing would say
// so.
//
// The rest are nobody else's to fill. A model this build has no entry for has
// no context window and no prices in the table, so it has none to show.
function providerAnswers(p, known) {
  return {
    backends: true,
    location: true,
    releaseDate: !!known && !!known.release_date,
    displayName: !!known && !!known.display_name,
    // The description is deliberately not here. The table fills it in like
    // everything else, and then it is the administrator's: a provider's
    // sentence says what the model is, and what this organisation means by the
    // alias is the part a router actually needs and only they can write.
    //
    // A provider serving one surface leaves nothing to choose. One serving
    // several does, and which of them a model is has to be somebody's answer.
    kind: (p.kinds || []).length === 1,
    backendModel: !!known,
    maxContext: !!known,
    priceIn: !!known,
    priceOut: !!known,
    // A provider that publishes no cached-input rate has not answered this
    // one: it is left live, and left blank those tokens are charged at the
    // input price.
    priceCached: !!known && !!known.cached_input_micros_per_mtok,
    // The same for a provider that charges nothing extra for a cache write.
    priceWrite: !!known && !!known.cache_write_micros_per_mtok,
  };
}

// hostedFields is the top of the form: where the model runs, as one row with
// the provider's mark. It is chosen before the form opens - in the catalogue
// for a new model, and when it was added for an existing one - so it is shown
// here and not asked.
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
// already written over one of the answers keeps it live too: an entry
// pointing `provider: anthropic` at an egress proxy is not this form's to take
// back.
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
  // The places a model can run. A place with no `p` is your own inference
  // plane, and having no provider table behind it is the whole difference.
  // Each carries one line: what a provider is, in its own table's words, and
  // what an inference plane of your own is in the panel's.
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
  // A place's mark: the provider's own logo where this build has one, a cloud
  // where it does not, and a rack of machines for a plane of your own.
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
  // a word when this build does not know the one picked.
  const block = h("div", { hidden: true }, field("Model", model), note);
  // Its own place in the form, just above the API key, far from the block
  // above.
  if (productSlot) productSlot.replaceChildren(productIDField);

  const chosen = () => (state.place ? state.place.p : null);

  const paintNote = () => {
    const p = chosen();
    if (!p) return;
    // Asked for only where there is a hole in the address to fill.
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

  // The address is built from the product id rather than typed, so it is kept
  // in step with it. An entry that wrote its own address keeps it: that one is
  // the administrator's, and a product id cannot be read out of it.
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
  // the administrator's.
  const answered = () => {
    const { p, m } = state.preset || {};
    if (!state.hosted || !p) return null;
    const fixed = providerAnswers(p, m);
    for (const key of state.overridden) fixed[key] = false;
    // On the operator's key, the control plane sets the endpoint and the list
    // prices whatever is sent, so they cannot be overridden here either.
    if (p.platform_key) {
      fixed.backends = true;
      for (const key of ["priceIn", "priceOut", "priceCached", "priceWrite"])
        fixed[key] = !!m;
    }
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

  // Lays the fields out for where the model runs. Between choosing a provider
  // and choosing a model there is nothing to read or fill in.
  const paintMachinery = (open) => {
    const fixed = paintLocks();
    if (state.hosted && !state.preset) layout(null);
    else layout(fixed ? "hosted" : "self", open);
  };

  // The stored model of an entry the table does not know, read before any
  // provider writes over the field.
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
      // Only the table's models are offered. A model this build does not know
      // is added with the CLI or a catalogue file, and one being edited keeps
      // its own entry here so the dialog can still show it.
      ...(unknownModel && p === initial.p
        ? [h("option", { value: "other" }, unknownModel)]
        : []),
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

  // choose settles where a new model runs. A provider comes with the model
  // picked in the catalogue, or its first one: a provider, a model and a key is
  // the whole job, and two of the three are answered already.
  const choose = (place, mm) => {
    state.place = place;
    state.hosted = !!place.p;
    if (place.p) {
      fillModels();
      const i = mm ? place.p.models.findIndex((x) => x.id === mm.id) : -1;
      if (i >= 0) model.value = String(i);
      applyModel();
    } else {
      // The location is left empty: the control plane works it out from the
      // backend URL.
      paintMachinery();
    }
  };

  // A model that already names a provider opens on that provider, with its
  // own stored values untouched: an entry may override anything the table
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
      if (def !== null && !sameValue(f, fields[f.key].value, def, initial.m)) {
        state.overridden.add(f.key);
      }
    }
    // An address that gave no product id back is not this provider's template
    // at all - an egress proxy, say - so it stays the administrator's to edit,
    // the way any other value they wrote over the table's does.
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
    paintMachinery("auto");
    paintNote();
  } else if (existing) {
    // An entry naming no provider runs on a plane of your own. Nothing is
    // reset, because there is nothing to choose.
    state.place = places[0];
  } else {
    const picked = pick && places.find((x) => x.p && x.p.name === pick.p.name);
    choose(picked || places[0], pick && pick.m);
  }
  block.hidden = !state.place.p;

  // Where a model runs is settled when it is added. Moving it elsewhere is a
  // new model, so this is a row and not a choice.
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

// paintPresets keeps the line under each filled-in field true: which provider
// the value came from while it still holds, and what it replaced once it does
// not. It is the whole of the explanation a disabled field gets, so it is
// written for one: "Anthropic's endpoint" says why the box cannot be typed in.
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

// sameValue reports whether a field still holds what the provider's table put
// there. Numbers are compared as numbers, because 5 and 5.0 are one price.
function sameValue(f, now, def, m) {
  now = String(now).trim();
  if (f.same) return f.same(now, m);
  return f.numeric ? Number(now) === Number(def) : now === def;
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

function toMicros(v) {
  const n = parseFloat(v);
  return Number.isFinite(n) && n > 0 ? Math.round(n * 1e6) : 0;
}

// statedMicros is toMicros, or null for an empty field.
function statedMicros(v) {
  return String(v).trim() === "" ? null : toMicros(v);
}
