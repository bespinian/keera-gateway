// A router's steps as data. No DOM here, so the guardrails dialog can use it
// without loading the editor.
//
// The checks mirror policy.Step.problem, so the editor shows problems before
// the control plane checks again on save.

const MAX_STEPS = 64;

export const KINDS = {
  filter: "Filter",
  route: "Route",
  rate_limit: "Rate limit",
  budget: "Budget",
  system_prompt: "System prompt",
  model: "Model",
};

/** limits reports whether a step counts requests, tokens or money. */
export function limits(kind) {
  return kind === "rate_limit" || kind === "budget";
}

/** passes reports whether a step hands the request on to one next step. */
export function passes(kind) {
  return kind === "filter" || kind === "system_prompt" || limits(kind);
}

/** steps lists every step in the order the gateway walks them. */
export function steps(start) {
  const out = [];
  const walk = (s) => {
    if (!s) return;
    out.push(s);
    walk(s.next);
    walk(s.exceeded);
    for (const dest of Object.keys(s.branches || {}).sort()) {
      walk(s.branches[dest]);
    }
  };
  walk(start);
  return out;
}

/** newID names a step. A limit's counters are kept under the id, so it
 *  never changes. */
export function newID(start) {
  const taken = new Set(steps(start).map((s) => s.id));
  for (;;) {
    const id = "s" + Math.random().toString(36).slice(2, 8);
    if (!taken.has(id)) return id;
  }
}

/** summary is the first path's steps, as one line for a table. */
export function summary(start) {
  const out = [];
  for (let s = start; s;) {
    out.push(s.kind === "rate_limit" ? "rate limit" : s.kind);
    if (s.kind === "route") {
      const n = Object.keys(s.branches || {}).length;
      if (n) out.push(`${n} ${n === 1 ? "branch" : "branches"}`);
      break;
    }
    s = s.next;
  }
  return out.join(" → ");
}

/** reaches lists every model the router may send to. Allowing the router
 *  allows all of them. */
export function reaches(router) {
  const out = [];
  const add = (alias) => {
    if (alias && !out.includes(alias)) out.push(alias);
  };
  for (const s of steps(router.start)) {
    if (s.kind === "model") add(s.ref);
    if (s.kind === "route") {
      for (const d of s.destinations || []) {
        if (!(s.branches || {})[d]) add(d);
      }
    }
  }
  return out;
}

/** usedBy lists the routers with a step that names filter ref. */
export function usedBy(routers, ref) {
  return (routers || []).filter((p) =>
    steps(p.start).some((s) => s.kind === "filter" && s.ref === ref),
  );
}

const ID_SHAPE = /^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$/;

/** problems maps each step id to what is wrong with it; "" is the whole
 *  router. An empty map means it can be saved. refs holds the organisation's
 *  models and filters. */
export function problems(start, refs) {
  const out = new Map();
  if (!start) {
    out.set("", "Add a first step. Every router ends at a model.");
    return out;
  }
  const all = steps(start);
  if (all.length > MAX_STEPS) {
    out.set("", `A router may have at most ${MAX_STEPS} steps.`);
  }
  const seen = new Set();
  for (const s of all) {
    const msg = !ID_SHAPE.test(s.id || "")
      ? "Its id is not usable."
      : seen.has(s.id)
        ? "Another step has the same id."
        : stepProblem(s, refs);
    seen.add(s.id);
    if (msg) out.set(s.id, msg);
  }
  return out;
}

function stepProblem(s, refs) {
  if (!KINDS[s.kind]) return "Unknown kind of step.";
  if (passes(s.kind) && !s.next) {
    return (
      "Nothing follows it. Every branch has to end at a model or a route " +
      "step."
    );
  }
  switch (s.kind) {
    case "filter":
      if (!s.ref) return "Choose a filter.";
      if (!refs.filters.some((f) => f.alias === s.ref)) {
        return `This organisation has no filter ${s.ref}.`;
      }
      return "";
    case "route":
      return routeProblem(s, refs);
    case "model":
      if (!s.ref) return "Choose a model.";
      return answererProblem(s.ref, refs);
    case "rate_limit":
      if (!(s.requests > 0) && !(s.tokens > 0)) {
        return "Set a number of requests or tokens, or both.";
      }
      return "";
    case "budget":
      return s.budget_micros > 0 ? "" : "Set an amount.";
    case "system_prompt":
      return (s.prompt || "").trim() ? "" : "Write the prompt.";
  }
  return "";
}

/** warnings flags a model filter that rewrites the request before a route
 *  step reads it. Pattern filters only take out secrets, so they don't count.
 *  Mirrors policy.Router.Warnings, in the same words. */
export function warnings(start, filters) {
  const out = [];
  const walk = (s, rewrites) => {
    if (!s) return;
    if (s.kind === "filter") {
      const f = (filters || []).find((x) => x.alias === s.ref);
      if (f && (f.mode || "rewrite") === "rewrite" && !f.shadow) {
        rewrites = rewrites.concat(s.ref);
      }
    }
    const reads =
      s.kind === "route" && (s.mode || "instruction") === "instruction";
    if (reads && rewrites.length) {
      const msg =
        `The route step '${s.id}' reads the request after the filter ` +
        `'${rewrites.join("', '")}' rewrote it, so it decides on the ` +
        "rewritten text, not what the client sent.";
      if (!out.includes(msg)) out.push(msg);
    }
    walk(s.next, rewrites);
    walk(s.exceeded, rewrites);
    for (const dest of Object.keys(s.branches || {}).sort()) {
      walk(s.branches[dest], rewrites);
    }
  };
  walk(start, []);
  return out;
}

/** clean drops what the gateway does not read, such as empty limits. */
export function clean(s) {
  if (!s) return undefined;
  const out = { id: s.id, kind: s.kind };
  if (s.ref) out.ref = s.ref;
  if (s.kind === "rate_limit") {
    if (s.requests > 0) out.requests = s.requests;
    if (s.tokens > 0) out.tokens = s.tokens;
    out.period = s.period || "minute";
  }
  if (s.kind === "system_prompt") out.prompt = (s.prompt || "").trim();
  if (s.kind === "budget") {
    out.budget_micros = s.budget_micros;
    out.period = s.period || "month";
  }
  if (passes(s.kind)) out.next = clean(s.next);
  if (limits(s.kind) && s.exceeded) out.exceeded = clean(s.exceeded);
  if (s.kind === "route") {
    out.mode = s.mode || "instruction";
    out.destinations = [...(s.destinations || [])];
    if (out.mode === "instruction") {
      out.model = s.model || "";
      out.prompt = (s.prompt || "").trim();
      if (s.fallback) out.fallback = s.fallback;
    }
    if (out.mode === "size" && Object.keys(s.ceilings || {}).length) {
      out.ceilings = { ...s.ceilings };
    }
    if (out.mode === "header") {
      out.header = (s.header || "").trim();
      out.values = { ...(s.values || {}) };
      if (s.fallback) out.fallback = s.fallback;
    }
    const branches = {};
    for (const [dest, b] of Object.entries(s.branches || {})) {
      if (b) branches[dest] = clean(b);
    }
    if (Object.keys(branches).length) out.branches = branches;
  }
  return out;
}

// answererProblem says why a model cannot answer through a router, or "".
function answererProblem(alias, refs) {
  const m = refs.models.find((x) => x.alias === alias);
  if (!m) return `This organisation has no model ${alias}.`;
  if (m.kind !== "chat") return `${alias} is not a chat model.`;
  if (m.subscription) {
    return `${alias} is paid by each person's own Claude subscription.`;
  }
  return "";
}

// routeProblem mirrors policy.Step.routeProblem, in shorter words.
function routeProblem(s, refs) {
  const mode = s.mode || "instruction";
  const dests = s.destinations || [];
  if (!dests.length) return "Choose at least one destination.";
  if (dests.length > 16) return "Choose at most 16 destinations.";
  if (mode === "instruction") {
    if (!s.model) return "Choose the deciding model.";
    if (!(s.prompt || "").trim()) return "Write the instruction.";
    if (s.fallback && !dests.includes(s.fallback)) {
      return `It falls back to ${s.fallback}, which is not a destination.`;
    }
  }
  for (const d of dests) {
    const why = answererProblem(d, refs);
    if (why) return why;
  }
  if (mode === "size") {
    const open = dests.filter((d) => !((s.ceilings || {})[d] > 0));
    if (open.length !== 1) {
      return "Leave the ceiling off exactly one destination.";
    }
  }
  if (mode === "header") {
    if (!(s.header || "").trim()) return "Name the header it reads.";
    const values = s.values || {};
    if (!Object.keys(values).length) return "Give a destination a value.";
    if (s.fallback && !dests.includes(s.fallback)) {
      return `It falls back to ${s.fallback}, which is not a destination.`;
    }
    for (const d of dests) {
      if (d !== s.fallback && !Object.values(values).includes(d)) {
        return `No value sends a request to ${d}, and it is not the fallback.`;
      }
    }
  }
  for (const dest of Object.keys(s.branches || {})) {
    if (!dests.includes(dest)) {
      return `${dest} is not one of its destinations any more.`;
    }
  }
  return "";
}
