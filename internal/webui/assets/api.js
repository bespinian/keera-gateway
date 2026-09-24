// The control API client.
//
// Every mutating call carries the session's CSRF token, which comes from
// /v1/me rather than a cookie - so a page that has not loaded its own identity
// cannot make a change at all.
//
// The panel is at the root, so every path below is relative to BASE and the
// three places that open a connection put it on. The paths here then read
// exactly as the routes do.

const BASE = "/control";

const url = (path) => BASE + path;

export class ApiError extends Error {
  constructor(status, message, code) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

let csrf = "";

export function setCsrf(token) {
  csrf = token || "";
}

async function request(method, path, body) {
  const headers = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (csrf && method !== "GET") headers["X-CSRF-Token"] = csrf;

  let res;
  try {
    res = await fetch(url(path), {
      method,
      headers,
      credentials: "same-origin",
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch (e) {
    throw new ApiError(0, "The control plane is unreachable.");
  }

  if (res.status === 204) return null;

  const text = await res.text();
  let payload = null;
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch {
      payload = null;
    }
  }
  if (!res.ok) {
    const err = payload && payload.error ? payload.error : {};
    throw new ApiError(
      res.status,
      err.message || `Request failed (${res.status}).`,
      err.code,
    );
  }
  return payload;
}

const get = (path) => request("GET", path);

// stream posts and hands back the raw response instead of a decoded body, for
// the one call whose body arrives token by token. A refusal still surfaces as
// an ApiError, so a caller handles it the same way it handles every other.
async function stream(path, body, signal) {
  const headers = { "Content-Type": "application/json" };
  if (csrf) headers["X-CSRF-Token"] = csrf;

  let res;
  try {
    res = await fetch(url(path), {
      method: "POST",
      headers,
      credentials: "same-origin",
      body: JSON.stringify(body),
      signal,
    });
  } catch (e) {
    if (e && e.name === "AbortError") throw e;
    throw new ApiError(0, "The control plane is unreachable.");
  }
  if (!res.ok) {
    let payload = null;
    try {
      payload = JSON.parse(await res.text());
    } catch {
      payload = null;
    }
    const err = payload && payload.error ? payload.error : {};
    throw new ApiError(
      res.status,
      err.message || `Request failed (${res.status}).`,
      err.code,
    );
  }
  return res;
}

function query(params) {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params || {})) {
    if (v !== undefined && v !== null && v !== "") q.set(k, v);
  }
  const s = q.toString();
  return s ? "?" + s : "";
}

export const api = {
  authConfig: () => get("/auth/config"),
  me: () => get("/v1/me"),
  localSignIn: (key) => request("POST", "/auth/local", { key }),
  signOut: () => request("POST", "/auth/logout"),

  orgs: () => get("/v1/orgs"),
  createOrg: (name, emailDomain) =>
    request("POST", "/v1/orgs", { name, email_domain: emailDomain || "" }),
  setOrgDomain: (id, emailDomain) =>
    request("PATCH", `/v1/orgs/${encodeURIComponent(id)}`, {
      email_domain: emailDomain,
    }),
  deleteOrg: (id) => request("DELETE", `/v1/orgs/${encodeURIComponent(id)}`),

  teams: (orgID) => get("/v1/teams" + query({ org_id: orgID })),
  createTeam: (orgID, name) =>
    request("POST", "/v1/teams", { org_id: orgID, name }),
  renameTeam: (id, name) =>
    request("PATCH", `/v1/teams/${encodeURIComponent(id)}`, { name }),
  deleteTeam: (id) => request("DELETE", `/v1/teams/${encodeURIComponent(id)}`),

  users: (orgID) => get("/v1/users" + query({ org_id: orgID })),
  inviteUser: (orgID, email, role) =>
    request("POST", "/v1/users", { org_id: orgID, email, role }),
  setUserRole: (id, role) =>
    request("PATCH", `/v1/users/${encodeURIComponent(id)}`, { role }),
  disableUser: (id) =>
    request("POST", `/v1/users/${encodeURIComponent(id)}/disable`),
  enableUser: (id) =>
    request("POST", `/v1/users/${encodeURIComponent(id)}/enable`),

  setup: (orgID) => get("/v1/setup" + query({ org_id: orgID })),
  // The one read shaped for the developer rather than the administrator: their
  // own keys, the limits above them, and what has been refused lately.
  access: () => get("/v1/access"),

  keys: (orgID, teamID) =>
    get("/v1/keys" + query({ org_id: orgID, team_id: teamID })),
  createKey: (payload) => request("POST", "/v1/keys", payload),
  revokeKey: (id) => request("DELETE", `/v1/keys/${encodeURIComponent(id)}`),

  guardrails: (scope, id) =>
    get(`/v1/guardrails/${encodeURIComponent(scope)}/${encodeURIComponent(id)}`),
  putGuardrails: (scope, id, limits) =>
    request(
      "PUT",
      `/v1/guardrails/${encodeURIComponent(scope)}/${encodeURIComponent(id)}`,
      limits,
    ),

  // The organisation's filters. `stats` asks for what each of them has done to
  // the traffic in the window as well, which the Filters screen wants and the
  // guardrails dialog does not - a filter's row is not worth much without its
  // traffic, and a checkbox in a dialog does not need an aggregate over the
  // usage log behind it.
  filters: (orgID, { stats, since } = {}) =>
    get(
      "/v1/filters" + query({ org_id: orgID, stats: stats ? "1" : "", since }),
    ),
  putFilter: (orgID, alias, filter) =>
    request(
      "PUT",
      `/v1/filters/${encodeURIComponent(alias)}` + query({ org_id: orgID }),
      filter,
    ),
  deleteFilter: (orgID, alias) =>
    request(
      "DELETE",
      `/v1/filters/${encodeURIComponent(alias)}` + query({ org_id: orgID }),
    ),
  checkFilter: (orgID, alias) =>
    request(
      "POST",
      `/v1/filters/${encodeURIComponent(alias)}/check` +
        query({ org_id: orgID }),
    ),
  // One filter's own screen. A check says what a filter does to three sample
  // segments; this says what it has been doing to the organisation's own
  // traffic, which is the only thing an instruction can actually be tuned
  // against.
  filterReport: (orgID, alias, since) =>
    get(
      `/v1/filters/${encodeURIComponent(alias)}/report` +
        query({ org_id: orgID, since }),
    ),

  // The organisation's routers. `stats` asks for where each of them has been
  // sending traffic as well, which is the only thing that distinguishes a
  // router doing its job from a router naming one destination on every
  // request - the traffic is identical in every other respect.
  routers: (orgID, { stats, since } = {}) =>
    get(
      "/v1/routers" + query({ org_id: orgID, stats: stats ? "1" : "", since }),
    ),
  putRouter: (orgID, alias, router) =>
    request(
      "PUT",
      `/v1/routers/${encodeURIComponent(alias)}` + query({ org_id: orgID }),
      router,
    ),
  deleteRouter: (orgID, alias) =>
    request(
      "DELETE",
      `/v1/routers/${encodeURIComponent(alias)}` + query({ org_id: orgID }),
    ),
  checkRouter: (orgID, alias) =>
    request(
      "POST",
      `/v1/routers/${encodeURIComponent(alias)}/check` +
        query({ org_id: orgID }),
    ),
  // One router's own screen: where it has actually been sending the requests
  // that named it, which is the number it is judged by.
  routerReport: (orgID, alias, since) =>
    get(
      `/v1/routers/${encodeURIComponent(alias)}/report` +
        query({ org_id: orgID, since }),
    ),

  // Sandboxes: the machines this gateway lends out.
  //
  // The catalogue read carries three things at once - the classes, what this
  // deployment's driver can actually deliver, and what the reader's own
  // guardrail allows - because a screen that offered a class the driver refuses
  // or the guardrail forbids would be offering something the reader finds out
  // about only after choosing.
  sandboxClasses: (orgID) =>
    get("/v1/sandbox-classes" + query({ org_id: orgID })),
  putSandboxClass: (name, cls) =>
    request("PUT", `/v1/sandbox-classes/${encodeURIComponent(name)}`, cls),
  deleteSandboxClass: (name) =>
    request("DELETE", `/v1/sandbox-classes/${encodeURIComponent(name)}`),

  sandboxes: (orgID, { all, team, cls, purpose } = {}) =>
    get(
      "/v1/sandboxes" +
        query({
          org_id: orgID,
          all: all ? "1" : "",
          team_id: team,
          class: cls,
          purpose,
        }),
    ),
  sandbox: (id) => get(`/v1/sandboxes/${encodeURIComponent(id)}`),
  createSandbox: (sandbox) => request("POST", "/v1/sandboxes", sandbox),
  // DELETE is the method; terminate is the word, because the row outlives the
  // machine and the panel says so everywhere else.
  terminateSandbox: (id) =>
    request("DELETE", `/v1/sandboxes/${encodeURIComponent(id)}`),
  extendSandbox: (id, ttl) =>
    request("POST", `/v1/sandboxes/${encodeURIComponent(id)}/extend`, { ttl }),
  suspendSandbox: (id) =>
    request("POST", `/v1/sandboxes/${encodeURIComponent(id)}/suspend`),
  resumeSandbox: (id) =>
    request("POST", `/v1/sandboxes/${encodeURIComponent(id)}/resume`),
  // What sandboxes have cost, grouped. The other half of `usage`: a task that
  // cost a franc in tokens and eleven minutes of a four-core machine is a
  // number a budget conversation can use, and the first half on its own is not.
  sandboxUsage: (orgID, { groupBy, since } = {}) =>
    get("/v1/sandbox-usage" + query({ org_id: orgID, group_by: groupBy, since })),

  // The client catalogue: what a developer puts where to point their editor
  // here. It comes from the control plane rather than from this bundle so that
  // the panel and `keera connect` cannot hand out configurations that differ.
  connect: () => get("/v1/connect"),

  models: () => get("/v1/models"),
  providers: () => get("/v1/providers"),
  putModel: (alias, model) =>
    request("PUT", `/v1/models/${encodeURIComponent(alias)}`, model),
  deleteModel: (alias) =>
    request("DELETE", `/v1/models/${encodeURIComponent(alias)}`),
  checkModel: (alias) =>
    request("POST", `/v1/models/${encodeURIComponent(alias)}/check`),

  // The MCP servers the gateway stands in front of, and their tool calls.
  // toolCalls takes { org_id, since, server, tool, summary }.
  mcpServers: () => get("/v1/mcp-servers"),
  putMCPServer: (alias, server) =>
    request("PUT", `/v1/mcp-servers/${encodeURIComponent(alias)}`, server),
  deleteMCPServer: (alias) =>
    request("DELETE", `/v1/mcp-servers/${encodeURIComponent(alias)}`),
  toolCalls: (params) => get("/v1/tool-calls" + query(params)),

  playground: (orgID, payload, signal) =>
    stream("/v1/playground/chat" + query({ org_id: orgID }), payload, signal),

  // The dashboard, and - with a scope of { team_id }, { key_id } or { alias } -
  // one team's, one key's or one model's own screen. The same report over
  // fewer rows, so that a number on an entity's page and its share of the
  // dashboard can never disagree.
  overview: (orgID, since, scope) =>
    get("/v1/overview" + query({ org_id: orgID, since, ...scope })),

  // The same window drawn rather than tabulated: the clients calling this
  // gateway, the models it forwards to, which of those are somebody else's
  // computer, and how much traffic ran between each pair.
  trafficMap: (orgID, since) =>
    get("/v1/map" + query({ org_id: orgID, since })),
  usage: (orgID, groupBy, since) =>
    get("/v1/usage" + query({ org_id: orgID, group_by: groupBy, since })),
  audit: (params) => get("/v1/audit" + query(params)),
  // The event log: every recorded request, narrowed to a team, a key, a model,
  // a person or a status - or to none of them, which is the Requests screen.
  requests: (params) => get("/v1/requests" + query(params)),

  // The same log grouped into the tasks its requests were made for: one row
  // per session rather than per call. It takes the narrowing requests() takes,
  // plus `sort` and `unhappy`, because what this screen is opened for is the
  // ranking - the task that cost four francs, the one that made four hundred
  // calls - and a log ordered by time buries both.
  sessions: (params) => get("/v1/sessions" + query(params)),

  // One task from beginning to end, named by the id of any request in it.
  // Any request rather than only the first: that is how a reader arrives, from
  // a row in the request log, and requiring the opening call would make the row
  // in front of them the one thing that could not be used to find it.
  session: (id, orgID) =>
    get(`/v1/sessions/${encodeURIComponent(id)}` + query({ org_id: orgID })),

  // The same log as it is written. The caller passes the same narrowing it
  // passed to requests() plus `after`, the newest row it already holds, and
  // gets an EventSource back - which it must close, because the browser allows
  // very few open connections per origin and this one never ends on its own.
  //
  // It rides the session cookie, exactly as every other read here does. There
  // is no CSRF token on it because there is nowhere to put one: an EventSource
  // sets no headers. That is safe for the same reason a GET is - it changes
  // nothing - and the route it opens is a read like any other.
  requestStream: (params) =>
    new EventSource(url("/v1/requests/stream" + query(params))),

  // download is a GET the browser saves rather than the panel parses. It rides
  // the session cookie like every other read, so an export needs no second
  // credential and leaks no key into a URL.
  download: (path, params) => {
    const a = document.createElement("a");
    a.href = url(path) + query({ ...params, format: "csv" });
    a.rel = "noopener";
    document.body.append(a);
    a.click();
    a.remove();
  },
};
