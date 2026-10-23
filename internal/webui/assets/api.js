// The control API client.
//
// Every mutating call carries the session's CSRF token. It comes from /v1/me,
// not a cookie, so a page that has not loaded its identity cannot change
// anything.

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

// request sends one call and decodes the answer. raw returns the response
// itself, for a body that streams or is a file. A refusal is an ApiError
// either way.
async function request(method, path, body, { raw, signal } = {}) {
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
      signal,
    });
  } catch (e) {
    if (e && e.name === "AbortError") throw e;
    throw new ApiError(0, "The control plane is unreachable.");
  }

  if (raw && res.ok) return res;
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

function query(params) {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params || {})) {
    if (v !== undefined && v !== null && v !== "") q.set(k, v);
  }
  const s = q.toString();
  return s ? "?" + s : "";
}

// path encodes each value put into it, so an id or a name is one segment.
const path = (strings, ...values) =>
  strings.reduce((s, part, i) => s + encodeURIComponent(values[i - 1]) + part);

// inOrg is one of an organisation's things, such as a model, by name.
const inOrg = (orgID, collection, name, rest = "") =>
  path`/v1/${collection}/${name}` + rest + query({ org_id: orgID });

export const api = {
  authConfig: () => get("/auth/config"),
  me: () => get("/v1/me"),
  // Moves this session to another organisation the person is in.
  switchOrg: (orgID) => request("POST", "/v1/me/org", { org_id: orgID }),
  localSignIn: (key) => request("POST", "/auth/local", { key }),
  signOut: () => request("POST", "/auth/logout"),
  // Signing in with a provider is a page the browser goes to, not a call.
  loginURL: (params) => url("/auth/login" + query(params)),

  // The sign-up screen, after a sign-in that matched no organisation.
  signupInfo: () => get("/auth/signup"),
  completeSignup: (body) => request("POST", "/auth/signup", body),

  // Passkeys. The /auth routes work before anybody has signed in, and take
  // a JSON body even when it is empty.
  passkeySignInOptions: (flow, next) =>
    request("POST", "/auth/passkey/options", {
      flow: flow || "",
      next: next || "",
    }),
  passkeySignIn: (flow, credential) =>
    request("POST", "/auth/passkey/sign-in", { flow, credential }),
  passkeySetupOptions: (token) =>
    request("POST", "/auth/passkey/setup/options", { token }),
  passkeySetup: (token, challengeID, name, credential) =>
    request("POST", "/auth/passkey/setup", {
      token,
      challenge_id: challengeID,
      name,
      credential,
    }),
  passkeys: (userID) => get("/v1/passkeys" + query({ user_id: userID })),
  ownPasskeyOptions: () => request("POST", "/v1/passkeys/options", {}),
  addPasskey: (challengeID, name, credential) =>
    request("POST", "/v1/passkeys", {
      challenge_id: challengeID,
      name,
      credential,
    }),
  deletePasskey: (id) => request("DELETE", path`/v1/passkeys/${id}`),
  passkeyLink: (userID) =>
    request("POST", path`/v1/users/${userID}/passkey-link`),

  orgs: () => get("/v1/orgs"),
  createOrg: (name, emailDomain) =>
    request("POST", "/v1/orgs", { name, email_domain: emailDomain || "" }),
  updateOrg: (id, name, emailDomain) =>
    request("PATCH", path`/v1/orgs/${id}`, { name, email_domain: emailDomain }),
  liftOrgLimit: (id) =>
    request("PATCH", path`/v1/orgs/${id}`, { limited: false }),
  deleteOrg: (id) => request("DELETE", path`/v1/orgs/${id}`),

  projects: (orgID) => get("/v1/projects" + query({ org_id: orgID })),
  createProject: (orgID, name, description) =>
    request("POST", "/v1/projects", { org_id: orgID, name, description }),
  // change holds name, description or both.
  updateProject: (id, change) =>
    request("PATCH", path`/v1/projects/${id}`, change),
  deleteProject: (id) => request("DELETE", path`/v1/projects/${id}`),

  users: (orgID) => get("/v1/users" + query({ org_id: orgID })),
  addUser: (orgID, email, role, signIn) =>
    request("POST", "/v1/users", {
      org_id: orgID,
      email,
      role,
      sign_in: signIn || "",
    }),
  setUserRole: (id, role) => request("PATCH", path`/v1/users/${id}`, { role }),
  disableUser: (id) => request("POST", path`/v1/users/${id}/disable`),
  enableUser: (id) => request("POST", path`/v1/users/${id}/enable`),

  setup: (orgID) => get("/v1/setup" + query({ org_id: orgID })),
  // The developer's own keys, the limits above them, and recent refusals.
  access: () => get("/v1/access"),

  keys: (orgID, { since } = {}) =>
    get("/v1/keys" + query({ org_id: orgID, since })),
  createKey: (payload) => request("POST", "/v1/keys", payload),
  renameKey: (id, name) => request("PATCH", path`/v1/keys/${id}`, { name }),
  revokeKey: (id) => request("DELETE", path`/v1/keys/${id}`),
  rotateKey: (id) => request("POST", path`/v1/keys/${id}/rotate`, {}),
  activateKey: (id) => request("POST", path`/v1/keys/${id}/activate`, {}),

  guardrails: (scope, id) => get(path`/v1/guardrails/${scope}/${id}`),
  putGuardrails: (scope, id, limits) =>
    request("PUT", path`/v1/guardrails/${scope}/${id}`, limits),

  // The organisation's filters. `stats` adds what each did to the traffic in
  // the window, which only the Filters screen shows.
  filters: (orgID, { stats, since } = {}) =>
    get(
      "/v1/filters" + query({ org_id: orgID, stats: stats ? "1" : "", since }),
    ),
  putFilter: (orgID, alias, filter) =>
    request("PUT", inOrg(orgID, "filters", alias), filter),
  deleteFilter: (orgID, alias) =>
    request("DELETE", inOrg(orgID, "filters", alias)),
  checkFilter: (orgID, alias) =>
    request("POST", inOrg(orgID, "filters", alias, "/check")),
  // What one filter has been doing to the organisation's own traffic.
  filterReport: (orgID, alias, since) =>
    get(path`/v1/filters/${alias}/report` + query({ org_id: orgID, since })),

  // The organisation's routers. `stats` adds the traffic each took in the
  // window, and how its route steps decided.
  routers: (orgID, { stats, since } = {}) =>
    get(
      "/v1/routers" + query({ org_id: orgID, stats: stats ? "1" : "", since }),
    ),
  putRouter: (orgID, alias, router) =>
    request("PUT", inOrg(orgID, "routers", alias), router),
  deleteRouter: (orgID, alias) =>
    request("DELETE", inOrg(orgID, "routers", alias)),
  // Puts sample prompts through each route step of the router.
  checkRouter: (orgID, alias) =>
    request("POST", inOrg(orgID, "routers", alias, "/check")),
  // How much went through one router, which models answered, and how its
  // route steps decided.
  routerReport: (orgID, alias, since) =>
    get(path`/v1/routers/${alias}/report` + query({ org_id: orgID, since })),

  // Sandbox classes, with what the driver can deliver and what the reader's
  // guardrail allows, so a screen offers only what is possible.
  sandboxClasses: (orgID) =>
    get("/v1/sandbox-classes" + query({ org_id: orgID })),
  deleteSandboxClass: (orgID, name) =>
    request("DELETE", inOrg(orgID, "sandbox-classes", name)),

  sandboxes: (orgID, { all } = {}) =>
    get("/v1/sandboxes" + query({ org_id: orgID, all: all ? "1" : "" })),
  createSandbox: (sandbox) => request("POST", "/v1/sandboxes", sandbox),
  // Terminate, not delete: the row outlives the machine.
  terminateSandbox: (id) => request("DELETE", path`/v1/sandboxes/${id}`),
  extendSandbox: (id, lifetime) =>
    request("POST", path`/v1/sandboxes/${id}/extend`, lifetime),
  suspendSandbox: (id) => request("POST", path`/v1/sandboxes/${id}/suspend`),
  resumeSandbox: (id) => request("POST", path`/v1/sandboxes/${id}/resume`),

  // The client catalogue. It comes from the control plane so the panel and
  // `keera connect` hand out the same configurations.
  connect: () => get("/v1/connect"),
  renderConnect: (models) => request("POST", "/v1/connect/render", { models }),
  // Which clients called with the reader's own keys in the window.
  clients: (since) => get("/v1/clients" + query({ since })),

  // The organisation's models. `stats` adds how fast each has been answering
  // in the window, which only the Models screen shows.
  models: (orgID, { stats, since } = {}) =>
    get(
      "/v1/models" + query({ org_id: orgID, stats: stats ? "1" : "", since }),
    ),
  providers: () => get("/v1/providers"),
  putModel: (orgID, alias, model) =>
    request("PUT", inOrg(orgID, "models", alias), model),
  deleteModel: (orgID, alias) =>
    request("DELETE", inOrg(orgID, "models", alias)),
  checkModel: (orgID, alias) =>
    request("POST", inOrg(orgID, "models", alias, "/check")),

  // The MCP servers the gateway stands in front of, and their tool calls.
  // toolCalls takes { org_id, since, server, tool, summary, limit }.
  mcpServers: (orgID) => get("/v1/mcp-servers" + query({ org_id: orgID })),
  putMCPServer: (orgID, alias, server) =>
    request("PUT", inOrg(orgID, "mcp-servers", alias), server),
  deleteMCPServer: (orgID, alias) =>
    request("DELETE", inOrg(orgID, "mcp-servers", alias)),
  toolCalls: (params) => get("/v1/tool-calls" + query(params)),

  playground: (keyID, payload, signal) =>
    request("POST", "/v1/playground/chat" + query({ key_id: keyID }), payload, {
      raw: true,
      signal,
    }),

  // The dashboard, or with a scope of { project_id }, { key_id } or { alias }
  // one entity's screen: the same report over fewer rows, so the two agree.
  overview: (orgID, since, scope) =>
    get("/v1/overview" + query({ org_id: orgID, since, ...scope })),

  // The same window as a map: clients, the models they reach, and the
  // traffic between each pair.
  trafficMap: (orgID, since) =>
    get("/v1/map" + query({ org_id: orgID, since })),
  usage: (orgID, groupBy, since) =>
    get("/v1/usage" + query({ org_id: orgID, group_by: groupBy, since })),
  audit: (params) => get("/v1/audit" + query(params)),
  // One month's bill for the deployment's own provider keys.
  billing: (orgID, month) =>
    get("/v1/billing" + query({ org_id: orgID, month })),
  // The credit an organisation pays in advance, its card and its payments.
  creditAccount: (orgID) =>
    get("/v1/billing/account" + query({ org_id: orgID })),
  updateCredit: (orgID, change) =>
    request("PATCH", "/v1/billing/account" + query({ org_id: orgID }), change),
  forgetCard: (orgID) =>
    request("DELETE", "/v1/billing/account/card" + query({ org_id: orgID })),
  // Starts a payment. The answer's url is PostFinance's payment page.
  topUp: (orgID, amountMicros, saveCard) =>
    request("POST", "/v1/billing/topups" + query({ org_id: orgID }), {
      amount_micros: amountMicros,
      save_card: saveCard,
    }),
  // Saves the receipt of a paid payment: PostFinance's invoice, as a PDF.
  receipt: async (id) => {
    const res = await request(
      "GET",
      path`/v1/billing/payments/${id}/receipt`,
      undefined,
      { raw: true },
    );
    const name = /filename="?([^";]+)/.exec(
      res.headers.get("Content-Disposition") || "",
    );
    save(
      URL.createObjectURL(await res.blob()),
      name ? name[1] : "keera-receipt.pdf",
    );
  },
  grantCredit: (orgID, amountMicros, note) =>
    request("POST", "/v1/billing/grants", {
      org_id: orgID,
      amount_micros: amountMicros,
      note,
    }),
  // The event log, narrowed to a project, key, model, person or status.
  requests: (params) => get("/v1/requests" + query(params)),

  // The same log grouped into sessions, one row per task. It takes the
  // narrowing of requests(), and `sort`.
  sessions: (params) => get("/v1/sessions" + query(params)),

  // One session, named by the id of any request in it.
  session: (orgID, id) =>
    get(path`/v1/sessions/${id}` + query({ org_id: orgID })),

  // The log as it is written, from `after` on. The caller must close the
  // EventSource: browsers allow few connections per origin. It carries no
  // CSRF token, since an EventSource sets no headers, and as a GET it changes
  // nothing.
  requestStream: (params) =>
    new EventSource(url("/v1/requests/stream" + query(params))),

  // download is a GET the browser saves instead of the panel parsing it. It
  // uses the session cookie, so no key ends up in a URL.
  download: (route, params) =>
    save(url(route) + query({ ...params, format: "csv" })),
};

// save makes the browser save href. name is for an href the server does not
// name, such as an object URL, which is released once saving has started.
function save(href, name) {
  const a = document.createElement("a");
  a.href = href;
  a.rel = "noopener";
  if (name) a.download = name;
  document.body.append(a);
  a.click();
  a.remove();
  if (name) setTimeout(() => URL.revokeObjectURL(href), 10_000);
}

/** chatTargets is what a client may name as its model: the organisation's
 *  enabled chat models, then its routers, as `keera connect` offers them.
 *  Subscription models are left out, because an ordinary key cannot reach
 *  them. Failing to read the routers still leaves the models. */
export async function chatTargets(orgID) {
  const [models, routers] = await Promise.all([
    api.models(orgID).then((r) => r.data || []),
    api
      .routers(orgID)
      .then((r) => r.data || [])
      .catch(() => []),
  ]);
  return models
    .filter((m) => m.enabled !== false && m.kind === "chat" && !m.subscription)
    .concat(
      // A router picks its model, so it has no context window of its own.
      routers.map((p) => ({
        alias: p.alias,
        kind: "chat",
        enabled: true,
        router: true,
        display_name: p.display_name,
        description: p.description,
      })),
    );
}
