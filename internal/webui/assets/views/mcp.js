// The MCP page: the tools an agent calls, behind the same keys and guardrails
// as the models. A server is catalogue like a model - shared by every tenant -
// so only an operator adds or changes one.

import { api } from "../api.js";
import {
  h,
  table,
  modal,
  confirm,
  toast,
  pill,
  num,
  ms,
  icon,
  icons,
  copyText,
  showError,
  field,
  aliasProblem,
  currentRange,
  rangePicker,
} from "../ui.js";
import { bytes, toolCallTable } from "./tools.js";

// mcpView is the MCP page: the servers the gateway stands in front of, the
// address a client is given for each, and - for an administrator - what each
// tool has been called for and the latest calls.
export async function mcpView(ctx) {
  const me = ctx.state.me;
  const canEdit = me.can_edit_catalogue;
  const admin = me.unrestricted || me.role === "admin";
  const since = currentRange();
  const [servers, connect, summary, calls] = await Promise.all([
    api.mcpServers().then((r) => r.data || []),
    api.connect().catch(() => ({})),
    admin
      ? api
          .toolCalls({ org_id: ctx.orgID, since, summary: true })
          .then((r) => r.data || [])
          .catch(() => [])
      : [],
    admin
      ? api
          .toolCalls({ org_id: ctx.orgID, since, limit: 100 })
          .then((r) => r.data || [])
          .catch(() => [])
      : [],
  ]);
  const base = (connect.gateway_url || "").replace(/\/$/, "");
  ctx.setSubtitle(`${servers.length} server${servers.length === 1 ? "" : "s"}`);

  const head = h(
    "div",
    { class: "detail-head" },
    h(
      "div",
      { class: "muted" },
      "Agents call a server's tools through the gateway with their Keera " +
        "key. They see only the tools their guardrails allow, filters check " +
        "each call, the server's credential stays on the gateway, and every " +
        "call is logged without its content.",
    ),
    h(
      "div",
      { class: "row", style: { flexWrap: "wrap" } },
      h("div", { style: { flex: 1 } }),
      admin ? rangePicker(ctx, since) : null,
      canEdit
        ? h(
            "button",
            { class: "btn btn-primary", onClick: () => editServer(ctx, null) },
            icon(icons.plus),
            "New MCP server",
          )
        : null,
    ),
  );

  const columns = [
    {
      label: "Server",
      sortKey: (m) => m.alias,
      cell: (m) =>
        h(
          "div",
          { class: "stack" },
          h("strong", { class: "mono" }, m.alias),
          m.description
            ? h(
                "span",
                { class: "faint", style: { fontSize: "11.5px" } },
                m.description,
              )
            : null,
        ),
    },
    {
      label: "Address for clients",
      cell: (m) =>
        h(
          "span",
          { class: "row-tight" },
          h("span", { class: "mono" }, `${base}/mcp/${m.alias}`),
          h(
            "button",
            {
              class: "btn btn-sm",
              title: "Copy",
              "aria-label": "Copy",
              onClick: () => copyText(`${base}/mcp/${m.alias}`),
            },
            icon(icons.copy),
          ),
        ),
    },
  ];
  if (admin) {
    columns.push({
      label: "Calls",
      num: true,
      shrink: true,
      sortKey: (m) => callsOf(summary, m.alias),
      cell: (m) =>
        h("span", { class: "nowrap" }, num(callsOf(summary, m.alias))),
    });
  }
  columns.push({
    label: "Status",
    shrink: true,
    cell: (m) =>
      m.enabled ? pill("Enabled", "good") : pill("Disabled", "warn"),
  });
  if (canEdit) {
    columns.push({
      label: "",
      shrink: true,
      cell: (m) =>
        h(
          "div",
          { class: "row-tight" },
          h(
            "button",
            { class: "btn btn-sm", onClick: () => editServer(ctx, m) },
            "Edit",
          ),
          // A server the catalogue file declares comes back on the next start.
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
                        "Clients lose access to its tools. To keep it and its " +
                        "call log, disable it instead.",
                      confirmLabel: "Remove server",
                      danger: true,
                      onConfirm: async () => {
                        await api.deleteMCPServer(m.alias);
                        toast("MCP server removed", "good");
                        ctx.reload();
                      },
                    }),
                },
                "Remove",
              ),
        ),
    });
  }

  const list = table(columns, servers, {
    emptyTitle: "No MCP servers yet",
    emptyBody: canEdit
      ? "Add one. Its tools use the same keys, allow-lists, filters and log " +
        "as models."
      : "An operator adds them.",
  });

  const wrap = h("div", {}, head, list);
  if (!admin || !servers.length) return wrap;

  wrap.append(
    section("Tools", "each tool's calls in this window"),
    table(
      [
        {
          label: "Tool",
          sortKey: (t) => `${t.server}/${t.tool}`,
          cell: (t) => h("span", { class: "mono" }, `${t.server}/${t.tool}`),
        },
        {
          label: "Calls",
          num: true,
          sortKey: (t) => t.calls,
          cell: (t) => num(t.calls),
        },
        {
          label: "Failed",
          num: true,
          sortKey: (t) => t.failed,
          cell: (t) => num(t.failed),
        },
        {
          label: "Not allowed",
          num: true,
          sortKey: (t) => t.denied,
          cell: (t) => num(t.denied),
        },
        {
          label: "Refused",
          num: true,
          sortKey: (t) => t.refused,
          cell: (t) => num(t.refused),
        },
        {
          label: "Average",
          num: true,
          sortKey: (t) => t.avg_ms,
          cell: (t) => ms(t.avg_ms),
        },
        {
          label: "Sent",
          num: true,
          sortKey: (t) => t.arg_bytes,
          cell: (t) => bytes(t.arg_bytes),
        },
        {
          label: "Back",
          num: true,
          sortKey: (t) => t.result_bytes,
          cell: (t) => bytes(t.result_bytes),
        },
      ],
      summary,
      {
        sortBy: "Calls",
        sortDir: "desc",
        emptyTitle: "No tool calls in this window",
        emptyBody:
          "Point a client at a server address above, with a Keera key.",
      },
    ),
    section("Latest calls", "newest first, at most 100"),
    toolCallTable(calls, {
      emptyBody: "Nothing has called a tool in this window.",
    }),
  );
  return wrap;
}

// callsOf is how many calls one server's tools had.
function callsOf(summary, alias) {
  return summary
    .filter((t) => t.server === alias)
    .reduce((n, t) => n + t.calls, 0);
}

// section is a heading between the page's tables.
function section(title, note) {
  return h(
    "div",
    {
      class: "section-head",
      style: { marginTop: "28px", marginBottom: "12px" },
    },
    h("h2", {}, title),
    h("div", { style: { flex: 1 } }),
    h("span", { class: "faint" }, note),
  );
}

// editServer adds a server, or changes one. A server the catalogue file
// declares is applied again on every start, so only its stored credential can
// change here - no file carries one.
function editServer(ctx, existing) {
  const m = existing || { enabled: true };
  const locked = !!m.managed;
  const input = (value, props = {}) =>
    h("input", {
      class: "input",
      value: value || "",
      disabled: locked,
      ...props,
    });

  const alias = input(m.alias, {
    class: "input mono",
    placeholder: "github",
    disabled: !!existing,
  });
  const url = input(m.url, {
    class: "input mono",
    placeholder: "https://mcp.example.ch/mcp",
  });
  const description = input(m.description, {
    placeholder: "Issues and pull requests",
  });
  const authHeader = input(m.auth_header, {
    class: "input mono",
    placeholder: "Authorization",
  });
  const apiKeyEnv = input(m.api_key_env, {
    class: "input mono",
    placeholder: "GITHUB_TOKEN",
  });

  // Write-only, as on a model: the control plane never sends a credential
  // back, so an empty field leaves the stored one alone.
  const canStore = ctx.state.me.can_store_credentials;
  const apiKey = h("input", {
    class: "input",
    type: "password",
    autocomplete: "off",
    placeholder: canStore
      ? m.has_api_key
        ? "•••••••• stored - type to replace"
        : "the server's token"
      : "set KEERA_SECRET_KEY to store a credential here",
    disabled: !canStore,
  });
  const clearKey =
    m.has_api_key && canStore ? h("input", { type: "checkbox" }) : null;
  const enabled = h("input", {
    type: "checkbox",
    checked: m.enabled !== false,
    disabled: locked,
  });
  const err = h("div");

  const body = h(
    "form",
    { onSubmit: (e) => e.preventDefault() },
    err,
    locked
      ? h(
          "div",
          { class: "banner", style: { marginBottom: "12px" } },
          "This server comes from the catalogue file, which is applied on " +
            "every start. Change it there. Only its credential can be set here.",
        )
      : null,
    field("Alias", alias, "Used in the address clients connect to."),
    field("Address", url, "The server's Streamable HTTP endpoint."),
    field("Description", description, "What the server is for, in a sentence."),
    field(
      "Credential",
      apiKey,
      h(
        "span",
        {},
        "The gateway sends it to the server. Clients never see it. Stored encrypted.",
        clearKey
          ? h(
              "label",
              { class: "row-tight", style: { marginTop: "6px" } },
              clearKey,
              "Remove the stored credential",
            )
          : null,
      ),
    ),
    field(
      "Credential variable",
      apiKeyEnv,
      "Or the name of a gateway environment variable that holds it.",
    ),
    field(
      "Header",
      authHeader,
      "The header for the credential. Empty sends it as a bearer token in " +
        "Authorization. Any other header gets the value as is.",
    ),
    h("label", { class: "row-tight" }, enabled, "Serve this server"),
  );

  modal({
    title: existing ? `Edit ${m.alias}` : "New MCP server",
    body,
    actions: (close) => [
      h("button", { class: "btn", onClick: close }, "Cancel"),
      h(
        "button",
        {
          class: "btn btn-primary",
          onClick: async (e) => {
            const name = alias.value.trim();
            const problem = existing ? "" : aliasProblem(name);
            if (problem) return showError(err, problem);
            if (!url.value.trim()) {
              return showError(err, "The server's address is required.");
            }
            // Absent unless the operator typed one or asked for the stored
            // one to go: sending "" on every save would wipe it.
            let credential;
            if (clearKey && clearKey.checked) credential = "";
            else if (apiKey.value) credential = apiKey.value;
            e.currentTarget.disabled = true;
            try {
              await api.putMCPServer(name, {
                url: url.value.trim(),
                description: description.value.trim(),
                auth_header: authHeader.value.trim(),
                api_key_env: apiKeyEnv.value.trim(),
                enabled: enabled.checked,
                ...(credential === undefined ? {} : { api_key: credential }),
              });
              close();
              toast(existing ? "MCP server saved" : "MCP server added", "good");
              ctx.reload();
            } catch (ex) {
              showError(err, ex.message);
              e.target.disabled = false;
            }
          },
        },
        !existing ? "Add server" : locked ? "Save credential" : "Save server",
      ),
    ],
  });
}
