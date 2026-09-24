// Sessions: the request log grouped into the tasks its requests were made for.
//
// Every other screen counts requests, and a request is not the unit anybody
// works in. A developer gives a coding agent one instruction and the agent
// makes thirty or forty calls carrying it out, so "four rappen a call" is a
// number nobody can act on and "one franc twenty a task, eleven minutes,
// forty-one calls" is what goes into a budget conversation.
//
// The screen leads with the totals and offers rankings rather than only time,
// because the row worth opening is never the most recent one: it is the task
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
} from "../ui.js";
import { outcomeMeaning, outcomeOf, oneLine } from "../status.js";
import { requestTable, showRequest } from "./requestlog.js";
import { toolCallTable } from "./tools.js";

const PAGE = 100;

// The rankings, and what each of them is for. They are the reason this screen
// exists rather than a nicety on top of it, so they sit next to the range
// picker rather than behind a menu.
const SORTS = [
  { key: "", label: "Recent", long: "Newest first" },
  { key: "cost", label: "Cost", long: "The tasks that cost the most" },
  {
    key: "requests",
    label: "Calls",
    long: "The tasks that made the most calls",
  },
  { key: "duration", label: "Time", long: "The tasks that ran the longest" },
];

const SORT_KEY = "keera.sessions.sort";
const UNHAPPY_KEY = "keera.sessions.unhappy";

export async function sessionsView(ctx) {
  const since = currentRange();
  const sort = sessionStorage.getItem(SORT_KEY) || "";
  const unhappy = sessionStorage.getItem(UNHAPPY_KEY) === "1";
  const range = RANGES.find((r) => r.since === since);
  ctx.setSubtitle(range ? range.long : "");

  const params = {
    org_id: ctx.orgID,
    since,
    limit: PAGE,
    sort,
    unhappy: unhappy ? "1" : "",
  };
  const res = await api.sessions(params);
  const rows = res.data || [];
  const totals = res.totals || {};
  const currency = res.currency || ctx.currency;
  const names = {
    keys: res.key_aliases || {},
    teams: res.team_names || {},
    users: res.user_names || {},
  };

  const wrap = h("div", {});

  wrap.append(
    h(
      "div",
      { class: "row", style: { marginBottom: "16px", flexWrap: "wrap" } },
      rangePicker(ctx, since),
      h(
        "div",
        { class: "seg" },
        SORTS.map((sc) =>
          h(
            "button",
            {
              "aria-pressed": String(sc.key === sort),
              title: sc.long,
              onClick: () => {
                sessionStorage.setItem(SORT_KEY, sc.key);
                ctx.reload();
              },
            },
            sc.label,
          ),
        ),
      ),
      h("div", { style: { flex: 1 } }),
      h(
        "button",
        {
          class: "btn btn-sm",
          "aria-pressed": String(unhappy),
          title:
            "Only tasks with a failure, a refusal or an interrupted answer",
          onClick: () => {
            sessionStorage.setItem(UNHAPPY_KEY, unhappy ? "0" : "1");
            ctx.reload();
          },
        },
        "Hit trouble",
      ),
      h(
        "button",
        {
          class: "btn btn-sm",
          title: "Download all matching tasks as CSV, one row per task",
          onClick: () => api.download("/v1/sessions", params),
        },
        icon(icons.download),
        "CSV",
      ),
    ),
  );

  wrap.append(totalTiles(totals, currency));

  if (!rows.length) {
    wrap.append(
      h(
        "div",
        { class: "card", style: { marginTop: "16px" } },
        unhappy
          ? empty(
              "Nothing hit trouble in this window",
              "Every task in this range succeeded. Turn off Hit trouble to " +
                "see them.",
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
      sessionTable(ctx, rows, names, currency),
    ),
  );

  // Paging appends rather than replaces, so somebody reading down a window
  // keeps what they have already read on the screen. It is offered only under
  // the default ordering: under a ranking the cursor would be a cursor over
  // another order, and "page two of the most expensive tasks" is not a question
  // anybody asks - whoever sorted by cost wanted the top of that list.
  if (rows.length === PAGE && sort === "") {
    let before = res.next_before;
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
            wrap.insertBefore(
              sessionTable(ctx, page, names, currency),
              moreWrap,
            );
            before = next.next_before;
            if (page.length < PAGE) {
              moreWrap.replaceChildren(
                h("div", { class: "hint" }, "That is the whole window."),
              );
              return;
            }
          } catch (ex) {
            moreWrap.append(
              h("div", { class: "banner banner-bad" }, ex.message),
            );
          } finally {
            more.disabled = false;
            more.textContent = `Load ${PAGE} more`;
          }
        },
      },
      `Load ${PAGE} more`,
    );
    const moreWrap = h(
      "div",
      { style: { marginTop: "12px", textAlign: "center" } },
      more,
    );
    wrap.append(moreWrap);
  }

  wrap.append(hint(res.gap_seconds));
  return wrap;
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
      "Tasks",
      compact(tasks),
      null,
      t.unhappy ? `${num(t.unhappy)} hit trouble` : "none hit trouble",
    ),
    stat(
      "Spend",
      money(t.cost_micros, ""),
      currency,
      tasks ? `${money(t.median_cost_micros, "")} median per task` : null,
    ),
    stat(
      "Calls per task",
      num(t.median_requests),
      null,
      tasks
        ? `longest ${num(t.longest_requests)} · ${compact(t.requests)} in all`
        : null,
    ),
    stat(
      "Time per task",
      duration(t.median_duration_ms),
      null,
      tasks
        ? `${compact(t.input_tokens + t.output_tokens)} tokens in all`
        : null,
    ),
  );
}

function sessionTable(ctx, rows, names, currency) {
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
                : "Grouped by a hash of the prompt that opened the task",
            ),
            a.stated ? pill("named") : null,
          ),
          h(
            "span",
            { class: "faint mono", style: { fontSize: "11px" } },
            (a.models || []).join(", ") || "no model",
          ),
        ),
    },
    {
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
        a.key_id
          ? h(
              "a",
              {
                class: "nowrap",
                href: "/keys/" + encodeURIComponent(a.key_id),
                title: names.teams[a.team_id]
                  ? "in " + names.teams[a.team_id]
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
      label: "Calls",
      num: true,
      shrink: true,
      sortKey: (a) => a.requests,
      sortDir: "desc",
      cell: (a) => h("span", { class: "nowrap" }, num(a.requests)),
    },
    {
      label: "Tokens",
      num: true,
      shrink: true,
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
            title: "All calls in this task, in order",
            onClick: () =>
              ctx.navigate("/sessions/" + encodeURIComponent(a.id)),
          },
          "Open",
        ),
    },
  ];

  return table(columns, rows, {
    search: (a) =>
      `${a.key} ${(a.models || []).join(" ")} ${a.last_error || ""} ` +
      `${names.users[a.user_id] || ""} ${names.keys[a.key_id] || a.key_id || ""} ` +
      `${names.teams[a.team_id] || ""}`,
    searchLabel: "these sessions",
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
      return gone(
        ctx,
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
    keys: res.key_aliases || {},
    teams: res.team_names || {},
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
    h(
      "a",
      { class: "crumb", href: "/sessions", onClick: go(ctx, "/sessions") },
      icon(icons.back),
      "Sessions",
    ),
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
          : pill("no key"),
        s.team_id
          ? h(
              "a",
              {
                class: "pill pill-button",
                href: "/teams/" + encodeURIComponent(s.team_id),
                title: "Open this team",
                onClick: go(ctx, "/teams/" + encodeURIComponent(s.team_id)),
              },
              names.teams[s.team_id] || s.team_id,
            )
          : pill("Organisation-wide"),
        s.user_id ? pill(names.users[s.user_id] || s.user_id) : null,
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
        "Calls",
        num(s.requests),
        null,
        s.ok === s.requests ? "all served" : `${num(s.ok)} served`,
      ),
      stat(
        "Took",
        duration(ran),
        null,
        s.requests > 1
          ? `${duration(ran / Math.max(1, s.requests - 1))} between calls on average`
          : "one call",
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
        `${money(Math.round(s.cost_micros / Math.max(1, s.requests)), "")} a call`,
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
            "each bar is one call, by what it cost",
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
        h("strong", {}, "How this task ended: "),
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
      h(
        "div",
        { class: "section-head" },
        h("h2", {}, "Calls"),
        h("div", { style: { flex: 1 } }),
        h("span", { class: "faint" }, "oldest first"),
      ),
      h(
        "div",
        { style: { marginTop: "12px" } },
        requestTable(
          ctx,
          requests,
          names,
          currency,
          { key: true, team: true },
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
        h(
          "div",
          { class: "section-head" },
          h("h2", {}, "Tool calls"),
          h("div", { style: { flex: 1 } }),
          h(
            "span",
            { class: "faint" },
            s.stated
              ? "named by the client with this session"
              : "this key's calls while the task ran",
          ),
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
          `Call ${i + 1} of ${requests.length} · ${q.alias || "no model"} · ` +
          `${outcomeMeaning(q)[0]} · ${money(q.cost_micros, currency)} · ` +
          `${ms(q.ttft_ms)} to the first token`,
        "aria-label": `Call ${i + 1}, ${outcomeOf(q)}`,
        onClick: () => showRequest(ctx, q, names, currency),
      });
    }),
  );
}

// gone is a session that is not there, said in the terms of why it might not
// be rather than as a failure to load a page.
function gone(ctx, title, body) {
  ctx.setTitle("Session", "");
  return h(
    "div",
    {},
    h(
      "div",
      { class: "detail-head" },
      h(
        "a",
        { class: "crumb", href: "/sessions", onClick: go(ctx, "/sessions") },
        icon(icons.back),
        "Sessions",
      ),
    ),
    h("div", { class: "card" }, empty(title, body)),
  );
}

// hint is what this screen is and what it cannot promise, which for a grouping
// that is inferred rather than stored is worth saying on the screen instead of
// in a document nobody opens.
function hint(gapSeconds, detail) {
  const gap = duration((gapSeconds || 0) * 1000);
  const how =
    `Calls belong to one task when they share a conversation and are less than ` +
    `${gap} apart. `;
  return h(
    "div",
    { class: "hint", style: { marginTop: "16px" } },
    detail
      ? how +
          "A conversation is matched by a hash of its opening prompt, or of " +
          "an id the client sent. The prompt is not stored and cannot be " +
          "recovered from the hash. If a client changes its opening prompt, a " +
          "new task starts."
      : how +
          "Grouping is computed each time you open this screen, so changing " +
          "the gap re-groups past tasks too.",
  );
}
