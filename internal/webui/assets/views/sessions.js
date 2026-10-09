// Sessions: the request log grouped into the tasks its requests were made for.
//
// Every other screen counts requests, and a request is not the unit anybody
// works in. A developer gives a coding agent one instruction and the agent
// makes thirty or forty calls carrying it out, so "four rappen a call" is a
// number nobody can act on and "one franc twenty a task, eleven minutes,
// forty-one calls" is what goes into a budget conversation.
//
// The screen leads with the totals and ranks by any of its numbers, not only
// by time, because the row worth opening is never the most recent one: it is the task
// that cost four francs, or the one that made four hundred calls and finished
// nothing.
//
// A session is grouped, not stored. The gateway records a hash of the
// conversation on every request and this screen cuts those into runs wherever
// the agent went quiet for longer than the deployment's idle gap. Nothing
// about any prompt is kept.

import { api, ApiError } from "../api.js";
import {
  h,
  table,
  stat,
  pill,
  empty,
  rowLink,
  go,
  icon,
  icons,
  num,
  compact,
  money,
  ms,
  duration,
  ago,
  dateTime,
  RANGES,
  currentRange,
  rangePicker,
  crumb,
  gone,
  sectionHead,
  isAdmin,
} from "../ui.js";
import { outcomeMeaning, outcomeOf, oneLine } from "../status.js";
import { requestTable, showRequest } from "./requestlog.js";
import {
  currentOutcome,
  narrowRow,
  outcomeSeg,
  ranking,
  rankedNote,
  readNarrowing,
} from "./logfilters.js";
import { toolCallTable } from "./tools.js";

const PAGE = 100;

// The ranking, remembered apart from the request log's, which offers others.
const SORT_KEY = "keera.sessions.sort";

const NO_COUNTS = { total: 0, ok: 0, failed: 0, refused: 0, interrupted: 0 };

export async function sessionsView(ctx) {
  const since = currentRange();
  const outcome = currentOutcome();
  const narrowed = readNarrowing(ctx);
  const rank = ranking(ctx, SORT_KEY);
  const range = RANGES.find((r) => r.since === since);
  ctx.setSubtitle(range ? range.long : "");

  const params = {
    org_id: ctx.orgID,
    since,
    limit: PAGE,
    sort: rank.by,
    outcome,
    ...narrowed,
    facets: "1",
  };
  const res = await api.sessions(params);
  const rows = res.data || [];
  const totals = res.totals || {};
  const currency = res.currency || ctx.currency;
  const names = {
    keys: res.key_names || {},
    projects: res.project_names || {},
    users: res.user_names || {},
  };
  const anyNarrowed = Object.values(narrowed).some(Boolean);

  const wrap = h("div", {});

  wrap.append(
    h(
      "div",
      { class: "section-head" },
      rangePicker(ctx, since),
      h("div", { style: { flex: 1 } }),
      outcomeSeg(ctx, outcome, totals.outcomes || NO_COUNTS).el,
      csvButton(params),
    ),
    narrowRow(ctx, narrowed, res.filters || {}, names),
  );

  wrap.append(totalTiles(totals, currency));

  if (!rows.length) {
    wrap.append(
      h(
        "div",
        { class: "card", style: { marginTop: "16px" } },
        outcome || anyNarrowed
          ? empty(
              "No sessions match these filters",
              "Clear some filters, pick a different outcome, or widen the " +
                "range.",
            )
          : empty(
              "No sessions in this window",
              "A session is a conversation with a coding agent. Requests " +
                "without one, such as embeddings, are only in the request log. " +
                "Try a wider range.",
            ),
      ),
    );
    wrap.append(hint(res.gap_seconds));
    return wrap;
  }

  wrap.append(
    h(
      "div",
      { style: { marginTop: "16px" } },
      sessionTable(ctx, rows, names, currency, {}, rank),
    ),
  );
  wrap.append(
    more(params, res, rows, rank, (page) =>
      sessionTable(ctx, page, names, currency, {}, rank),
    ),
  );
  wrap.append(hint(res.gap_seconds));
  return wrap;
}

/** sessionLog is the session list for one project, key or model, drawn below
 *  that thing's charts. The range comes from the screen it sits on. Like the
 *  request log there, it takes the outcome and the ranking but no narrowing:
 *  the screen it sits on is already narrowed. */
export async function sessionLog(ctx, { scope = {}, since, hide = {} }) {
  const outcome = currentOutcome();
  const rank = ranking(ctx, SORT_KEY);
  const params = {
    org_id: ctx.orgID,
    since,
    limit: PAGE,
    sort: rank.by,
    outcome,
    ...scope,
  };

  let res;
  try {
    res = await api.sessions(params);
  } catch (err) {
    // A log that will not load must not take the charts above it with it.
    return h(
      "div",
      { style: { marginTop: "24px" } },
      h(
        "div",
        { class: "banner banner-bad" },
        err.message || "The sessions could not be read.",
      ),
    );
  }

  const rows = res.data || [];
  const counts = (res.totals && res.totals.outcomes) || NO_COUNTS;
  const currency = res.currency || ctx.currency;
  const names = {
    keys: res.key_names || {},
    projects: res.project_names || {},
    users: res.user_names || {},
  };

  const wrap = h(
    "div",
    { style: { marginTop: "24px" } },
    sectionHead(
      "Sessions",
      h(
        "div",
        { class: "row" },
        outcomeSeg(ctx, outcome, counts).el,
        csvButton(params),
      ),
    ),
  );

  if (!rows.length) {
    wrap.append(
      h(
        "div",
        { class: "card", style: { marginTop: "12px" } },
        outcome
          ? empty(
              "No sessions match this filter",
              "Pick a different outcome above, or widen the range.",
            )
          : empty(
              "No sessions in this window",
              "Requests without a conversation, such as embeddings, are not " +
                "grouped into sessions. Try a wider range.",
            ),
      ),
    );
    return wrap;
  }

  wrap.append(
    h(
      "div",
      { style: { marginTop: "12px" } },
      sessionTable(ctx, rows, names, currency, hide, rank),
    ),
  );
  wrap.append(
    more(params, res, rows, rank, (page) =>
      sessionTable(ctx, page, names, currency, hide, rank),
    ),
  );
  return wrap;
}

// more is what follows a full page: the pager in time order, and a note under
// a ranking. Paging appends rather than replaces, so somebody reading down a
// window keeps what they have already read on the screen. Under a ranking the
// cursor would be a cursor over another order, and whoever ranked by cost
// wanted the top of that list.
function more(params, res, rows, rank, render) {
  if (rows.length < PAGE) return null;
  if (rank.by !== "") return rankedNote(PAGE, "sessions");
  return pager(params, res, render);
}

function csvButton(params) {
  return h(
    "button",
    {
      class: "btn btn-sm",
      title: "Download the matching sessions as CSV, one row each",
      onClick: () => api.download("/v1/sessions", params),
    },
    icon(icons.download),
    "CSV",
  );
}

// pager loads the next page and draws it above itself.
function pager(params, res, render) {
  let before = res.next_before;
  const moreWrap = h("div", {
    style: { marginTop: "12px", textAlign: "center" },
  });
  const more = h(
    "button",
    {
      class: "btn",
      onClick: async () => {
        more.disabled = true;
        more.textContent = "Loading…";
        try {
          const next = await api.sessions({ ...params, before });
          const page = next.data || [];
          moreWrap.before(render(page));
          before = next.next_before;
          if (page.length < PAGE) {
            moreWrap.replaceChildren(
              h("div", { class: "hint" }, "That is the whole window."),
            );
            return;
          }
        } catch (ex) {
          moreWrap.append(h("div", { class: "banner banner-bad" }, ex.message));
        } finally {
          more.disabled = false;
          more.textContent = `Load ${PAGE} more`;
        }
      },
    },
    `Load ${PAGE} more`,
  );
  moreWrap.append(more);
  return moreWrap;
}

// totalTiles is what the window amounts to, per task.
//
// Every figure here is a median rather than a mean, and the maximum is written
// under it. A window holding one task that made four hundred calls has an
// average that describes none of the other thirty-nine, and the pair - the
// ordinary task, and how far the worst one went - is what says whether there is
// anything on this screen worth opening.
function totalTiles(t, currency) {
  const tasks = t.sessions || 0;
  return h(
    "div",
    { class: "grid grid-4" },
    stat(
      "Sessions",
      compact(tasks),
      null,
      t.unhappy ? `${num(t.unhappy)} with problems` : "none with problems",
    ),
    stat(
      "Spend",
      money(t.cost_micros, ""),
      currency,
      tasks ? `${money(t.median_cost_micros, "")} median per session` : null,
    ),
    stat(
      "Requests per session",
      num(t.median_requests),
      null,
      tasks
        ? `longest ${num(t.longest_requests)} · ${compact(t.requests)} in all`
        : null,
    ),
    stat(
      "Time per session",
      duration(t.median_duration_ms),
      null,
      tasks
        ? `${compact(t.input_tokens + t.output_tokens)} tokens in all`
        : null,
    ),
  );
}

function sessionTable(ctx, rows, names, currency, hide, rank) {
  const columns = [
    {
      // The conversation, as the gateway hashed it. It is the only name a
      // session has - anything else would mean keeping the prompt that started
      // it - and it carries the way in, as every other list here does.
      //
      // The models it used go underneath rather than in a column of their own:
      // almost every task uses one, and a narrow column with the same value on
      // every row pushes the numbers off the screen. A session the client named
      // itself is marked; an inferred one is not, because that is nearly all of
      // them.
      label: "Session",
      shrink: true,
      sortKey: (a) => a.key,
      cell: (a) =>
        h(
          "div",
          { class: "stack", style: { gap: "2px" } },
          h(
            "div",
            { class: "row-tight nowrap" },
            rowLink(
              ctx,
              "/sessions/" + encodeURIComponent(a.id),
              h("span", { class: "mono" }, a.key),
              a.stated
                ? "The client named this session itself"
                : "Grouped by a hash of the prompt that opened the session",
            ),
            a.stated ? pill("Named") : null,
          ),
          h(
            "span",
            { class: "faint mono", style: { fontSize: "11px" } },
            (a.models || []).join(", ") || "no model",
          ),
        ),
    },
    !hide.who && {
      // Whose task it was. The person where the key was attributed to one, and
      // the key itself where it was not - which is the shared key issued for a
      // pipeline, and is the answer either way. "Which developer's task cost
      // four francs" is the question this screen is opened with as often as
      // "which task", and it is one column.
      label: "Who",
      shrink: true,
      sortKey: (a) =>
        names.users[a.user_id] || names.keys[a.key_id] || a.key_id || null,
      cell: (a) =>
        // The person's own screen where there is one to open, which only an
        // administrator has.
        a.user_id && isAdmin(ctx)
          ? h(
              "a",
              {
                class: "nowrap",
                href: "/users/" + encodeURIComponent(a.user_id),
                onClick: go(ctx, "/users/" + encodeURIComponent(a.user_id)),
              },
              names.users[a.user_id] || a.user_id,
            )
          : a.key_id
            ? h(
                "a",
                {
                  class: "nowrap",
                  href: "/keys/" + encodeURIComponent(a.key_id),
                  title: names.projects[a.project_id]
                    ? "in " + names.projects[a.project_id]
                    : null,
                  onClick: go(ctx, "/keys/" + encodeURIComponent(a.key_id)),
                },
                names.users[a.user_id] || names.keys[a.key_id] || a.key_id,
              )
            : h("span", { class: "faint" }, "no key"),
    },
    {
      label: "Started",
      shrink: true,
      rank: "",
      sortKey: (a) => new Date(a.started_at).getTime(),
      sortDir: "desc",
      cell: (a) =>
        h(
          "span",
          { class: "muted nowrap", title: dateTime(a.started_at) },
          ago(a.started_at),
        ),
    },
    {
      label: "Took",
      num: true,
      shrink: true,
      rank: "duration",
      sortKey: (a) => new Date(a.ended_at) - new Date(a.started_at),
      sortDir: "desc",
      cell: (a) =>
        h(
          "span",
          { class: "nowrap muted" },
          duration(new Date(a.ended_at) - new Date(a.started_at)),
        ),
    },
    {
      label: "Requests",
      num: true,
      shrink: true,
      rank: "requests",
      sortKey: (a) => a.requests,
      sortDir: "desc",
      cell: (a) => h("span", { class: "nowrap" }, num(a.requests)),
    },
    {
      label: "Tokens",
      num: true,
      shrink: true,
      rank: "tokens",
      sortKey: (a) => a.input_tokens + a.output_tokens,
      sortDir: "desc",
      cell: (a) =>
        h(
          "span",
          { class: "nowrap muted" },
          `${compact(a.input_tokens)} · ${compact(a.output_tokens)}`,
        ),
    },
    {
      label: `Cost (${currency})`,
      num: true,
      shrink: true,
      rank: "cost",
      sortKey: (a) => a.cost_micros || null,
      sortDir: "desc",
      cell: (a) =>
        a.cost_micros
          ? h("span", { class: "nowrap" }, money(a.cost_micros, ""))
          : h("span", { class: "faint" }, "-"),
    },
    {
      // How the task ended, which is the first thing asked about one that
      // stopped short. It is here rather than only inside the session, because
      // that is what saves opening thirty of them to find the one that died.
      label: "Ended",
      cell: (a) => endedCell(a),
    },
    {
      label: "",
      shrink: true,
      cell: (a) =>
        h(
          "button",
          {
            class: "btn btn-sm",
            title: "All requests in this session, in order",
            onClick: () =>
              ctx.navigate("/sessions/" + encodeURIComponent(a.id)),
          },
          "Open",
        ),
    },
  ];

  return table(columns.filter(Boolean), rows, {
    search: (a) =>
      `${a.key} ${(a.models || []).join(" ")} ${a.last_error || ""} ` +
      `${names.users[a.user_id] || ""} ${names.keys[a.key_id] || a.key_id || ""} ` +
      `${names.projects[a.project_id] || ""}`,
    searchLabel: "these sessions",
    rank,
    emptyTitle: "Nothing here",
  });
}

// endedCell says how the last call of a task went, in the same words the
// request log uses for the same row. A task whose final call was refused is not
// a task that finished.
function endedCell(a) {
  const [label, tone, why] = outcomeMeaning({
    status: a.last_status,
    error: a.last_error,
  });
  const trouble = [
    a.failed ? `${num(a.failed)} failed` : null,
    a.refused ? `${num(a.refused)} refused` : null,
    a.interrupted ? `${num(a.interrupted)} interrupted` : null,
  ].filter(Boolean);
  return h(
    "div",
    { class: "stack", style: { gap: "2px" } },
    h(
      "div",
      { class: "row-tight nowrap" },
      pill(label, tone),
      trouble.length
        ? h(
            "span",
            { class: "faint", style: { fontSize: "11px" } },
            trouble.join(" · "),
          )
        : null,
    ),
    a.last_error
      ? h(
          "span",
          {
            class: "muted",
            style: { fontSize: "11.5px" },
            title: a.last_error,
          },
          oneLine(a.last_error, 70),
        )
      : h("span", { class: "faint", style: { fontSize: "11.5px" } }, why),
  );
}

/* ------------------------------------------------------------------ one task */

export async function sessionDetailView(ctx) {
  let res;
  try {
    res = await api.session(ctx.param, ctx.orgID);
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) {
      ctx.setTitle("Session", "");
      return gone(
        ctx,
        "/sessions",
        "Sessions",
        "This request is not part of a session",
        "Nothing was recorded under this id, the request has no " +
          "conversation (such as an embedding), or retention has deleted it.",
      );
    }
    throw err;
  }

  const s = res.session;
  const requests = res.requests || [];
  const currency = res.currency || ctx.currency;
  const names = {
    keys: res.key_names || {},
    projects: res.project_names || {},
    users: res.user_names || {},
  };
  const [label, tone] = outcomeMeaning({
    status: s.last_status,
    error: s.last_error,
  });
  const ran = new Date(s.ended_at) - new Date(s.started_at);

  ctx.setTitle("Session", s.key);

  const head = h(
    "div",
    { class: "detail-head" },
    crumb(ctx, "/sessions", "Sessions"),
    h(
      "div",
      { class: "row", style: { flexWrap: "wrap" } },
      h(
        "div",
        { class: "wrap-chips" },
        pill(label, tone),
        s.key_id
          ? h(
              "a",
              {
                class: "pill pill-button",
                href: "/keys/" + encodeURIComponent(s.key_id),
                title: "Open this key",
                onClick: go(ctx, "/keys/" + encodeURIComponent(s.key_id)),
              },
              names.keys[s.key_id] || s.key_id,
            )
          : pill("No key"),
        s.project_id
          ? h(
              "a",
              {
                class: "pill pill-button",
                href: "/projects/" + encodeURIComponent(s.project_id),
                title: "Open this project",
                onClick: go(
                  ctx,
                  "/projects/" + encodeURIComponent(s.project_id),
                ),
              },
              names.projects[s.project_id] || s.project_id,
            )
          : null,
        s.user_id && isAdmin(ctx)
          ? h(
              "a",
              {
                class: "pill pill-button",
                href: "/users/" + encodeURIComponent(s.user_id),
                title: "Open this user",
                onClick: go(ctx, "/users/" + encodeURIComponent(s.user_id)),
              },
              names.users[s.user_id] || s.user_id,
            )
          : s.user_id
            ? pill(names.users[s.user_id] || s.user_id)
            : null,
        (s.models || []).map((m) =>
          h(
            "a",
            {
              class: "pill pill-button",
              href: "/models/" + encodeURIComponent(m),
              onClick: go(ctx, "/models/" + encodeURIComponent(m)),
            },
            m,
          ),
        ),
        pill(
          s.stated ? "Named by the client" : "Inferred from the opening prompt",
        ),
      ),
      h("div", { style: { flex: 1 } }),
      h(
        "span",
        { class: "muted nowrap", title: dateTime(s.started_at) },
        dateTime(s.started_at),
      ),
    ),
  );

  const wrap = h("div", {}, head);

  wrap.append(
    h(
      "div",
      { class: "grid grid-4" },
      stat(
        "Requests",
        num(s.requests),
        null,
        s.ok === s.requests ? "all served" : `${num(s.ok)} served`,
      ),
      stat(
        "Took",
        duration(ran),
        null,
        s.requests > 1
          ? `${duration(ran / Math.max(1, s.requests - 1))} between requests on average`
          : "one request",
      ),
      stat(
        "Tokens",
        compact(s.input_tokens + s.output_tokens),
        null,
        `${compact(s.input_tokens)} in · ${compact(s.output_tokens)} out`,
      ),
      stat(
        "Cost",
        money(s.cost_micros, ""),
        currency,
        `${money(Math.round(s.cost_micros / Math.max(1, s.requests)), "")} a request`,
      ),
    ),
  );

  wrap.append(
    h(
      "div",
      { style: { marginTop: "16px" } },
      h(
        "div",
        { class: "card" },
        h(
          "div",
          { class: "card-head" },
          h("h2", {}, "Turns"),
          h("div", { class: "spacer" }),
          h(
            "span",
            { class: "faint", style: { fontSize: "11.5px" } },
            "each bar is one request, by what it cost",
          ),
        ),
        h(
          "div",
          { class: "card-body" },
          requests.length
            ? turnStrip(ctx, requests, names, currency)
            : empty("Nothing recorded", "Its requests are no longer stored."),
        ),
      ),
    ),
  );

  if (s.last_error) {
    wrap.append(
      h(
        "div",
        { class: "banner banner-warn", style: { marginTop: "16px" } },
        h("strong", {}, "How this session ended: "),
        s.last_error,
      ),
    );
  }

  // The same table the request log draws, because these are the same rows: the
  // same columns, the same wording for an outcome, the same dialog behind View.
  // The one thing that differs is the order - a task is read forwards.
  wrap.append(
    h(
      "div",
      { style: { marginTop: "24px" } },
      sectionHead("Requests", "oldest first"),
      h(
        "div",
        { style: { marginTop: "12px" } },
        requestTable(
          ctx,
          requests,
          names,
          currency,
          { key: true, project: true },
          { sortBy: "When", sortDir: "asc" },
        ),
      ),
    ),
  );

  // What the agent did between its calls, through the MCP servers. Drawn only
  // when it did something, since most deployments have no servers yet.
  const tools = res.tool_calls || [];
  if (tools.length) {
    wrap.append(
      h(
        "div",
        { style: { marginTop: "24px" } },
        sectionHead(
          "Tool calls",
          s.stated
            ? "named by the client with this session"
            : "this key's calls while the session ran",
        ),
        h(
          "div",
          { style: { marginTop: "12px" } },
          toolCallTable(tools, { sortDir: "asc" }),
        ),
      ),
    );
  }

  wrap.append(hint(res.gap_seconds, true));
  return wrap;
}

// turnStrip is the task at a glance: one bar per call, in the order they were
// made, sized by what each cost and coloured by how each went.
//
// Forty even bars is an agent working; a long tail of identical small ones is
// an agent looping; a red bar two thirds along with nothing sensible after it
// is where the task went wrong, and clicking it opens that call.
//
// Cost rather than latency, because cost is what the reader came for and a
// refused call has no latency worth drawing. A call that cost nothing still
// gets a bar tall enough to click.
function turnStrip(ctx, requests, names, currency) {
  const max = Math.max(...requests.map((q) => q.cost_micros || 0), 1);
  return h(
    "div",
    { class: "turns" },
    requests.map((q, i) => {
      const [, tone] = outcomeMeaning(q);
      const share = (q.cost_micros || 0) / max;
      return h("button", {
        class: "turn",
        dataset: { tone: tone || "", ...(q.cost_micros ? {} : { empty: "1" }) },
        style: { height: Math.max(6, Math.round(share * 60)) + "px" },
        title:
          `Request ${i + 1} of ${requests.length} · ${q.alias || "no model"} · ` +
          `${outcomeMeaning(q)[0]} · ${money(q.cost_micros, currency)} · ` +
          `${ms(q.ttft_ms)} to the first token`,
        "aria-label": `Request ${i + 1}, ${outcomeOf(q)}`,
        onClick: () => showRequest(ctx, q, names, currency),
      });
    }),
  );
}

// gone is a session that is not there, said in the terms of why it might not
// be rather than as a failure to load a page.
// hint is what this screen is and what it cannot promise, which for a grouping
// that is inferred rather than stored is worth saying on the screen instead of
// in a document nobody opens.
function hint(gapSeconds, detail) {
  const gap = duration((gapSeconds || 0) * 1000);
  const how =
    `Requests belong to one session when they share a conversation and are less than ` +
    `${gap} apart. `;
  return h(
    "div",
    { class: "hint", style: { marginTop: "16px" } },
    detail
      ? how +
          "A conversation is matched by a hash of its opening prompt, or of " +
          "an id the client sent. The prompt is not stored and cannot be " +
          "recovered from the hash. If a client changes its opening prompt, a " +
          "new session starts."
      : how +
          "Grouping is computed each time you open this screen, so changing " +
          "the gap re-groups past sessions too.",
  );
}
