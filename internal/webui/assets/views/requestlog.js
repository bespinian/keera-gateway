// The request log: one row per recorded request. It is the whole Requests
// screen, and each project's, key's and model's screen ends in it.
//
// An entity's screen is already narrowed and offers only the outcome buttons.
// The Requests screen carries the filters instead. Everything else is shared.

import { api } from "../api.js";
import {
  h,
  table,
  pill,
  empty,
  modal,
  go,
  num,
  money,
  amount,
  ms,
  ago,
  dateTime,
  tokens,
  liveControl,
  dash,
  remembered,
  remember,
  loadMore,
  banner,
  csvButton,
  followRequests,
  throttled,
} from "../ui.js";
import { outcomeMeaning, oneLine } from "../status.js";
import { flameChart } from "../flame.js";
import {
  currentOutcome,
  narrowRow,
  outcomeSeg,
  ranking,
  rankedNote,
  readNarrowing,
  namesFrom,
  addNames,
} from "./logfilters.js";

const PAGE = 100;

// Whether the log is watched live. On by default: people open the log to see
// what is happening now.
const LIVE_KEY = "keera.requests.live";

// Remembered apart from the session list's ranking, which offers others.
const SORT_KEY = "keera.requests.sort";

const ID_KEY = "keera.requests.id";

// How long a new row stays marked. Long enough to catch the eye, short enough
// that a log nobody watched is not a wall of highlights.
const FRESH_MS = 4000;

// How often a ranked log asks for its page again while live, at most.
const REFRESH_MS = 3000;

/** requestLog is the section itself.
 *
 *  `scope` is the fixed thing the caller is about ({ project_id }, { key_id },
 *  { alias }). `filters` shows the narrowing controls instead of the outcome
 *  buttons, for a screen with no scope. `hide` drops columns that would repeat
 *  the scope in every row. `title` is the heading, empty when the page title
 *  already says it. `trail` goes at the right end of the controls.
 *
 *  `live` shows new requests as they are recorded. It is opt-in because it
 *  holds a connection open while the screen is up.
 *
 *  The counts come before the rows: two refusals in four hundred and three
 *  hundred in four hundred can look the same on the first page. */
export async function requestLog(
  ctx,
  {
    scope = {},
    since,
    hide = {},
    filters = false,
    title = "Requests",
    trail = null,
    live = false,
  },
) {
  const outcome = filters ? "" : currentOutcome();
  const narrowed = filters ? readNarrowing(ctx) : {};
  const searchedID = filters ? remembered(ID_KEY) || "" : "";
  const rank = ranking(ctx, SORT_KEY);
  // The scope wins over the narrowing: a project's screen is about that
  // project, whatever was last picked on the dashboard.
  const params = {
    org_id: ctx.orgID,
    since,
    limit: PAGE,
    outcome,
    sort: rank.by,
    ...narrowed,
    ...scope,
    request_id: searchedID,
    facets: filters ? "1" : "",
  };

  let res;
  try {
    res = await api.requests(params);
  } catch (err) {
    // A log that will not load must not take the charts above it with it.
    return h(
      "div",
      { style: { marginTop: "24px" } },
      banner("bad", err.message || "The request log could not be read."),
    );
  }

  const rows = res.data || [];
  const counts = res.outcomes || {
    total: 0,
    ok: 0,
    failed: 0,
    refused: 0,
    interrupted: 0,
  };
  const facets = res.filters || {};
  const names = namesFrom(res);
  const anyNarrowed = Object.values(narrowed).some(Boolean) || !!searchedID;

  const wrap = h("div", { style: { marginTop: "24px" } });

  // Kept, not only drawn: the stream sends fresh counts with every batch.
  const seg = filters ? null : outcomeSeg(ctx, outcome, counts);

  const watch = live
    ? liveControl({
        key: LIVE_KEY,
        label: "Watch for new requests",
        titles: {
          live: "Live: new requests appear here. Click to stop.",
          connecting:
            "Reconnecting to the request log. Click to stop watching.",
          paused: "Paused. Click to see new requests live.",
          stopped:
            "The stream stopped and could not reconnect. Click to try again, " +
            "or reload the page.",
        },
      })
    : null;
  wrap.append(
    h(
      "div",
      { class: "section-head" },
      title ? h("h2", {}, title) : null,
      h("div", { class: "spacer" }),
      seg ? seg.el : null,
      watch ? watch.el : null,
      trail,
      csvButton("Download all matching requests as CSV", () =>
        api.download("/v1/requests", params),
      ),
    ),
  );

  if (filters) {
    wrap.append(
      narrowRow(ctx, narrowed, facets, names, idSearch(ctx, searchedID)),
    );
  }

  // Both empty states still watch: an empty log is where somebody waits for
  // their first request, and "nothing yet" must not stay after it arrives.
  if (!counts.total && !anyNarrowed) {
    wrap.append(
      h(
        "div",
        { class: "card" },
        empty(
          watch && watch.on
            ? "No requests yet in this period"
            : "No requests in this period",
          watch && watch.on
            ? "New requests appear here as they arrive."
            : "Pick a longer period, or check that a client points at this " +
                "gateway.",
        ),
      ),
    );
    attachLive(null);
    return wrap;
  }

  const log = rows.length
    ? requestTable(ctx, rows, names, hide, { rank })
    : null;
  wrap.append(
    log ||
      h(
        "div",
        { class: "card" },
        searchedID
          ? empty(
              "No request has this id",
              "Check the id. Every period is searched, but the other " +
                "filters still apply.",
            )
          : empty(
              "Nothing matches this filter",
              filters
                ? "Nothing matches all the filters above. Clear some, or pick a " +
                    "longer period."
                : "Pick a different outcome above, or a longer period.",
            ),
      ),
  );

  if (rows.length === PAGE && rank.by !== "") {
    wrap.append(rankedNote(PAGE, "requests"));
  }

  if (rows.length === PAGE && rank.by === "") {
    let before = res.next_before;
    wrap.append(
      loadMore(
        PAGE,
        async () => {
          const next = await api.requests({ ...params, before });
          const page = next.data || [];
          addNames(names, next);
          rows.push(...page);
          log.redraw();
          before = next.next_before;
          return page.length;
        },
        "That is the whole period.",
      ),
    );
  }
  attachLive(log);
  return wrap;

  /** attachLive keeps this section up to date from the stream. `log` is the
   *  table, or null when there is none to add rows to.
   *
   *  It is declared inside requestLog because it works on this rendering's
   *  rows, counts and labels. Hoisting lets the empty path above call it. */
  function attachLive(log) {
    if (!watch) return;
    const ranked = rank.by !== "";

    // The stream takes the narrowing but not the paging: it sends the rows
    // after the newest one shown. With no rows, `after` is 0 and the control
    // plane starts at the log's current end instead of replaying the window.
    const listen = {
      org_id: ctx.orgID,
      since,
      outcome,
      ...narrowed,
      ...scope,
      // A ranked page's first row is not the newest, so it starts at the end.
      after: rows.length && !ranked ? rows[0].id : 0,
    };

    // A ranked page is fetched again rather than added to, because the control
    // plane decides where a new row ranks. Throttled to one query per
    // REFRESH_MS.
    const rerank = throttled(async () => {
      const next = await api.requests({ ...params, facets: "" });
      addNames(names, next);
      rows.splice(0, rows.length, ...(next.data || []));
      log.redraw();
    }, REFRESH_MS);
    ctx.onTeardown(rerank.stop);

    function arrived(payload) {
      if (payload.next_after) listen.after = payload.next_after;
      if (seg && payload.outcomes) seg.retally(payload.outcomes);
      // Labels come only for things newer than this screen, such as a key
      // issued a minute ago.
      addNames(names, payload);

      const fresh = payload.data || [];
      if (!fresh.length) return;
      if (!log) {
        // No table to add to (empty period or no match): draw the screen
        // again, once.
        ctx.reload();
        return;
      }
      if (ranked) {
        rerank();
        return;
      }
      // Newest first. A row committed late is older than some already shown,
      // so each goes in where its id belongs.
      for (const q of fresh) {
        q.fresh = true;
        const at = rows.findIndex((r) => r.id < q.id);
        rows.splice(at < 0 ? rows.length : at, 0, q);
      }
      log.redraw();
      // Remove the mark and redraw. Left on, an old row would light up again
      // the next time a column is sorted.
      setTimeout(() => {
        for (const q of fresh) delete q.fresh;
        log.redraw();
      }, FRESH_MS);
    }

    followRequests(ctx, watch, () => api.requestStream(listen), {
      onRequests: arrived,
    });
  }
}

/** requestTable is a page of the log. A session's screen uses it too, so its
 *  rows look the same as here.
 *
 *  `opts` is merged into the table's options. The log passes its ranking,
 *  since it holds one page of many. A session holds all its rows and sorts
 *  them oldest first, because a task is read forwards. */
export function requestTable(ctx, rows, names, hide, opts = {}) {
  const columns = [
    {
      label: "When",
      shrink: true,
      rank: "",
      sortKey: (q) => new Date(q.ts).getTime(),
      sortDir: "desc",
      cell: (q) =>
        h("span", { class: "muted nowrap", title: dateTime(q.ts) }, ago(q.ts)),
    },
    hide.model
      ? null
      : {
          label: "Model",
          shrink: true,
          sortKey: (q) => q.alias,
          cell: (q) =>
            q.alias ? h("span", { class: "mono" }, q.alias) : dash(),
        },
    {
      // The message sits under the outcome, not in its own column: most rows
      // have none, and an empty column pushes the numbers off the screen.
      label: "Outcome",
      sortKey: (q) => q.status,
      cell: (q) => {
        const [label, tone] = outcomeMeaning(q);
        return h(
          "div",
          { class: "stack" },
          h(
            "div",
            { class: "row-tight nowrap" },
            pill(label, tone),
            h(
              "span",
              { class: "mono faint", style: { fontSize: "11px" } },
              String(q.status),
            ),
          ),
          q.error || q.note
            ? h(
                "span",
                {
                  class: "muted small",
                  title: q.error || q.note,
                },
                oneLine(q.error || q.note, 90),
              )
            : null,
        );
      },
    },
    hide.key
      ? null
      : {
          // A link, because the next question is what else that key did.
          label: "Key",
          shrink: true,
          sortKey: (q) => names.keys[q.key_id] || q.key_id || null,
          cell: (q) =>
            q.key_id
              ? h(
                  "a",
                  {
                    class: "nowrap",
                    href: "/keys/" + encodeURIComponent(q.key_id),
                    onClick: go(ctx, "/keys/" + encodeURIComponent(q.key_id)),
                  },
                  names.keys[q.key_id] || q.key_id,
                )
              : h("span", { class: "faint" }, "No key"),
        },
    hide.project
      ? null
      : {
          label: "Project",
          shrink: true,
          sortKey: (q) => names.projects[q.project_id] || q.project_id || null,
          cell: (q) =>
            q.project_id
              ? h(
                  "span",
                  { class: "muted nowrap" },
                  names.projects[q.project_id] || q.project_id,
                )
              : h("span", { class: "faint" }, "No project"),
        },
    {
      label: "Tokens",
      num: true,
      shrink: true,
      rank: "tokens",
      sortKey: (q) => q.input_tokens + q.output_tokens,
      sortDir: "desc",
      cell: (q) =>
        q.input_tokens + q.output_tokens
          ? h(
              "span",
              { class: "nowrap muted" },
              tokens(q.input_tokens, q.output_tokens),
            )
          : dash(),
    },
    {
      label: "Cost (CHF)",
      num: true,
      shrink: true,
      rank: "cost",
      sortKey: (q) => q.cost_micros || null,
      sortDir: "desc",
      cell: (q) =>
        q.cost_micros
          ? h(
              "span",
              { class: "nowrap" },
              amount(q.cost_micros),
              // Marked, because someone will check it against an invoice.
              q.estimated ? h("span", { class: "faint" }, "~") : null,
            )
          : dash(),
    },
    {
      // First token rather than total: that is the wait a developer feels, and
      // it shows a model gone slow rather than down. The total is on hover.
      label: "First token",
      num: true,
      shrink: true,
      rank: "ttft",
      sortKey: (q) => q.ttft_ms || null,
      sortDir: "desc",
      cell: (q) =>
        q.ttft_ms
          ? h(
              "span",
              { class: "nowrap muted", title: `${ms(q.latency_ms)} in total` },
              ms(q.ttft_ms),
            )
          : h(
              "span",
              { class: "faint", title: `${ms(q.latency_ms)} in total` },
              "-",
            ),
    },
    {
      label: "",
      shrink: true,
      cell: (q) =>
        h(
          "button",
          {
            class: "btn btn-sm",
            title: "Request details",
            onClick: () => showRequest(ctx, q, names),
          },
          "View",
        ),
    },
  ].filter(Boolean);

  return table(columns, rows, {
    emptyTitle: "Nothing here",
    // A row that arrived live is marked, so the reader can see which is new.
    rowClass: (q) => (q.fresh ? "row-new" : null),
    ...opts,
  });
}

// idSearch finds a request by the id it was answered with, or by the client's
// own. It searches every period: someone holding an id rarely knows the day.
function idSearch(ctx, value) {
  const apply = (e) => {
    const id = e.target.value.trim();
    if (id === value) return;
    remember(ID_KEY, id);
    ctx.reload();
  };
  return h("input", {
    class: "input toolbar-search",
    type: "search",
    placeholder: "Find by request id",
    "aria-label": "Find by request id",
    autocomplete: "off",
    spellcheck: "false",
    value,
    onChange: apply,
    // The clear button of a search box fires this, and no change event.
    onSearch: apply,
  });
}

// showRequest shows every recorded field of one request. The message is whole
// and selectable, because people paste it to somebody else. It links to the
// request's session, since one call says little about the task it was part of.
export function showRequest(ctx, q, names) {
  const [label, tone, why] = outcomeMeaning(q);
  const recorded = [
    ["Model", q.alias || "-"],
    // Said with what happened: a stream that stopped halfway was still sent
    // a 200.
    ["Status", `${q.status} - ${label}`],
    [
      "Key",
      q.key_id ? `${names.keys[q.key_id] || q.key_id} (${q.key_id})` : "No key",
    ],
    [
      "Project",
      q.project_id ? names.projects[q.project_id] || q.project_id : "-",
    ],
    ["Who", q.user_id ? names.users[q.user_id] || q.user_id : "-"],
    ["When", dateTime(q.ts)],
    // Rows from before v0.7.0 have no id.
    ["Request id", q.request_id || "-"],
    // Most clients send no id of their own, so the line would mostly say none.
    q.client_request_id ? ["Client request id", q.client_request_id] : null,
    [
      "Input tokens",
      num(q.input_tokens) +
        // Cached tokens have their own rate, so the cost cannot be worked out
        // without them.
        (q.cached_input_tokens
          ? ` (${num(q.cached_input_tokens)} from the provider's cache)`
          : "") +
        (q.cache_write_tokens
          ? ` (${num(q.cache_write_tokens)} written to it)`
          : "") +
        (q.estimated ? " (estimated)" : ""),
    ],
    [
      "Output tokens",
      num(q.output_tokens) + (q.estimated ? " (estimated)" : ""),
    ],
    ["Cost", money(q.cost_micros) + (q.estimated ? " (estimated)" : "")],
    ["First token", ms(q.ttft_ms)],
    ["Took", ms(q.latency_ms)],
    ["Streamed", q.stream ? "yes" : "no"],
    ["Client hung up", q.canceled ? "yes" : "no"],
  ].filter(Boolean);
  const session = q.session_key
    ? (close) =>
        h(
          "button",
          {
            class: "btn",
            title: "All requests in the same session",
            onClick: () => {
              close();
              ctx.navigate("/sessions/" + encodeURIComponent(q.id));
            },
          },
          "Open its session",
        )
    : null;

  // Where the time went goes first: it is what the dialog is most often
  // opened for.
  const steps = flameChart(q.spans, q.latency_ms);

  modal({
    title: label,
    subtitle: `${q.alias || "no model"} · ${dateTime(q.ts)}`,
    actions: session,
    wide: true,
    body: h(
      "div",
      { class: "stack", style: { gap: "16px" } },
      h(
        "div",
        { class: "row-tight" },
        pill(label, tone),
        h("span", { class: "muted" }, why),
      ),
      steps
        ? h(
            "div",
            { class: "stack", style: { gap: "8px" } },
            h(
              "div",
              { class: "flame-head" },
              h("div", { class: "stat-label" }, "Where the time went"),
              h(
                "span",
                { class: "muted", style: { fontSize: "12px" } },
                `${ms(q.latency_ms)} in total`,
              ),
            ),
            steps,
          )
        : null,
      q.error
        ? h(
            "div",
            { class: "stack", style: { gap: "4px" } },
            h("div", { class: "stat-label" }, "What the client was told"),
            h(
              "pre",
              {
                class: "key-value",
                style: {
                  whiteSpace: "pre-wrap",
                  userSelect: "text",
                  margin: 0,
                },
              },
              q.error,
            ),
          )
        : null,
      // A note is what went wrong on the way to an answer that was served all
      // the same, such as a router falling back.
      q.note
        ? h(
            "div",
            { class: "stack", style: { gap: "4px" } },
            h("div", { class: "stat-label" }, "On the way to the answer"),
            h(
              "pre",
              {
                class: "key-value",
                style: {
                  whiteSpace: "pre-wrap",
                  userSelect: "text",
                  margin: 0,
                },
              },
              q.note,
            ),
          )
        : null,
      h(
        "div",
        { class: "stack", style: { gap: "4px" } },
        recorded.map(([k, v]) =>
          h(
            "div",
            { class: "row" },
            h("span", { class: "stat-label", style: { minWidth: "140px" } }, k),
            h("span", { class: "muted", style: { fontSize: "12px" } }, v),
          ),
        ),
      ),
    ),
  });
}
