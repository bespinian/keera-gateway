// The MCP page: the tools an agent calls, behind the same keys and guardrails
// as the models. A server belongs to one organisation, and its administrators
// add and change it.

import { api } from "../api.js";
import {
  h,
  table,
  pill,
  num,
  ms,
  icons,
  iconButton,
  copyText,
  field,
  aliasProblem,
  takenProblem,
  currentRange,
  rangePicker,
  bytes,
  plural,
  isAdmin,
  rowActions,
  listHead,
  sectionHead,
  dash,
  formModal,
} from "../ui.js";
import { toolCallTable } from "./tools.js";

// mcpView lists the servers and the address a client uses for each. An
// administrator also sees the tool call counts and the latest calls.
export async function mcpView(ctx) {
  const canEdit = isAdmin(ctx);
  const since = currentRange();
  const [servers, connect, summary, calls] = await Promise.all([
    api.mcpServers(ctx.orgID).then((r) => r.data || []),
    api.connect().catch(() => ({})),
    canEdit
      ? api
          .toolCalls({ org_id: ctx.orgID, since, summary: "1" })
          .then((r) => r.data || [])
          .catch(() => [])
      : [],
    canEdit
      ? api
          .toolCalls({ org_id: ctx.orgID, since, limit: 100 })
          .then((r) => r.data || [])
          .catch(() => [])
      : [],
  ]);
  const base = (connect.gateway_url || "").replace(/\/$/, "");
  ctx.setSubtitle(`${servers.length} ${plural(servers.length, "server")}`);

  const head = listHead(
    null,
    canEdit ? [rangePicker(ctx, since)] : [],
    canEdit && {
      label: "Add MCP server",
      onClick: () => editServer(ctx, null),
    },
  );

  const columns = [
    {
      label: "Alias",
      sortKey: (m) => m.alias,
      cell: (m) =>
        h(
          "div",
          { class: "stack" },
          h("strong", { class: "mono" }, m.alias),
          m.description
            ? h("span", { class: "faint small" }, m.description)
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
          iconButton(icons.copy, "Copy", () =>
            copyText(`${base}/mcp/${m.alias}`),
          ),
        ),
    },
  ];
  if (canEdit) {
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
        rowActions(ctx, m.alias, {
          editTitle: "Edit this server",
          onEdit: () => editServer(ctx, m),
          deleteTitle: "Delete this server",
          body:
            "Clients lose access to its tools. Its call log is kept. " +
            "To pause it instead, disable it.",
          confirmLabel: "Delete server",
          remove: () => api.deleteMCPServer(m.org_id, m.alias),
        }),
    });
  }

  const list = table(columns, servers, {
    emptyTitle: "No MCP servers yet",
    emptyBody: canEdit
      ? "MCP servers hold tools your models and agents can use when " +
        "fulfilling your tasks."
      : "An administrator adds them.",
  });

  const wrap = h("div", {}, head, list);
  if (!canEdit || !servers.length) return wrap;

  const gap = { marginTop: "28px" };
  wrap.append(
    sectionHead("Tools", "each tool's calls in this period", gap),
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
          cell: (t) => countPill(t.failed, "bad"),
        },
        {
          label: "Not allowed",
          num: true,
          sortKey: (t) => t.denied,
          cell: (t) => countPill(t.denied, "warn"),
        },
        {
          label: "Refused",
          num: true,
          sortKey: (t) => t.refused,
          cell: (t) => countPill(t.refused, "warn"),
        },
        {
          label: "Median",
          num: true,
          sortKey: (t) => t.latency_median_ms,
          cell: (t) => ms(t.latency_median_ms),
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
        emptyTitle: "No tool calls in this period",
        emptyBody:
          "Point a client at a server address above, with a Keera key.",
      },
    ),
    sectionHead("Latest calls", "newest first, at most 100", gap),
    toolCallTable(calls, {
      emptyBody: "Nothing has called a tool in this period.",
    }),
  );
  return wrap;
}

// countPill fades out a zero so any other count stands out.
function countPill(n, tone) {
  return n ? pill(num(n), tone) : dash();
}

function callsOf(summary, alias) {
  return summary
    .filter((t) => t.server === alias)
    .reduce((n, t) => n + t.calls, 0);
}

function editServer(ctx, existing) {
  const m = existing || { enabled: true };
  // A new server belongs to the organisation in view.
  const orgID = existing ? existing.org_id : ctx.orgID;
  const input = (value, props = {}) =>
    h("input", {
      class: "input",
      value: value || "",
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
  // No placeholder: naming Authorization here sends the value without
  // "Bearer ", which is not what an empty field does.
  const authHeader = input(m.auth_header, { class: "input mono" });

  // Write-only: the control plane never sends a credential back, so an empty
  // field leaves the stored one alone.
  const apiKey = h("input", {
    class: "input",
    type: "password",
    autocomplete: "off",
    placeholder: m.has_api_key
      ? "•••••••• stored - type to replace"
      : "the server's token",
  });
  const clearKey = m.has_api_key ? h("input", { type: "checkbox" }) : null;
  const enabled = h("input", {
    type: "checkbox",
    checked: m.enabled !== false,
  });

  const body = h(
    "form",
    { onSubmit: (e) => e.preventDefault() },
    field("Alias", alias, "Used in the address clients connect to."),
    field("Endpoint", url, "The server's Streamable HTTP address."),
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
      "Header",
      authHeader,
      "The header for the credential. Empty sends it as a bearer token in " +
        "Authorization. A header named here, Authorization too, gets the value " +
        "as is.",
    ),
    h("label", { class: "row-tight" }, enabled, "Serve this server"),
  );

  formModal({
    title: existing ? `Edit ${m.alias}` : "Add MCP server",
    body,
    submitLabel: existing ? "Save" : "Add MCP server",
    done: existing ? "MCP server saved" : "MCP server added",
    ctx,
    submit: async () => {
      const name = alias.value.trim();
      const problem = existing ? "" : aliasProblem(name);
      if (problem) throw new Error(problem);
      if (!url.value.trim())
        throw new Error("The server's address is required.");
      // Sent only when typed or cleared: sending "" on every save would wipe
      // it.
      let credential;
      if (clearKey && clearKey.checked) credential = "";
      else if (apiKey.value) credential = apiKey.value;
      if (!existing) {
        const taken = takenProblem(
          (await api.mcpServers(orgID)).data,
          name,
          "MCP server",
        );
        if (taken) throw new Error(taken);
      }
      await api.putMCPServer(orgID, name, {
        url: url.value.trim(),
        description: description.value.trim(),
        auth_header: authHeader.value.trim(),
        enabled: enabled.checked,
        ...(credential === undefined ? {} : { api_key: credential }),
      });
    },
  });
}
