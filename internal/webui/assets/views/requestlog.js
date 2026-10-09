// The request log: one row per recorded request, wherever it is being read.
//
// The Requests screen is this section and nothing else, and each project's,
// key's and model's own screen ends in it. A chart says when something
// changed and only the log says what changed, so whoever learns to read it
// under one chart has learned to read it everywhere.
//
// The two callers differ in one thing. An entity's screen arrives already
// narrowed and offers no narrowing of its own; the Requests screen is narrowed
// to nothing, so it carries the filters. Everything else, down to the dialog
// behind a row's View button, is shared.

import { api } from "../api.js";
import {
  h,
  table,
  pill,
  empty,
  modal,
  go,
  compact,
  num,
  money,
  ms,
  ago,
  dateTime,
  icon,
  icons,
  liveControl,
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
} from "./logfilters.js";

const PAGE = 100;

// Whether the log is being watched, remembered like every other choice this
// section holds. It defaults to on: somebody who opened the request log opened
// it to find out what is happening, and the most common version of that
// question is about the next thirty seconds rather than the last hour.
const LIVE_KEY = "keera.requests.live";

// The ranking, remembered apart from the session list's, which offers others.
const SORT_KEY = "keera.requests.sort";

// How long a row that has just arrived is marked as new. Long enough to catch
// the eye of somebody looking elsewhere on the screen, short enough that a log
// nobody is watching is not a wall of highlights when they come back.
const FRESH_MS = 4000;

/** requestLog is the section itself.
 *
 *  `scope` is what the caller is already about ({ project_id }, { key_id },
 *  { alias }) and is fixed; `filters` asks for the narrowing controls, which
 *  only a screen with no scope of its own has any use for. `hide` drops the
 *  columns that would repeat the scope in every row.
 *
 *  `title` is the heading over the section, which the screen that is nothing
 *  but this section passes empty - its own page title already says Requests,
 *  and the same word twice reads as two things. `lead` is whatever that screen
 *  puts where the heading was, which is its range picker.
 *
 *  `live` watches the log while it is on the screen, so a request recorded now
 *  appears now. It is asked for rather than assumed, because it costs an open
 *  connection for as long as the screen is up.
 *
 *  It leads with the counts by outcome rather than with the rows: "four
 *  hundred requests, two of them refused" and "four hundred requests, three
 *  hundred of them refused" are the same first page of a table and completely
 *  different afternoons. */
export async function requestLog(
  ctx,
  {
    scope = {},
    since,
    hide = {},
    filters = false,
    title = "Requests",
    lead = null,
    live = false,
  },
) {
  const outcome = currentOutcome();
  const narrowed = filters ? readNarrowing(ctx, { status: true }) : {};
  const rank = ranking(ctx, SORT_KEY);
  // The scope wins over the narrowing: a project's own screen is about that project
  // whatever was last picked on the dashboard.
  const params = {
    org_id: ctx.orgID,
    since,
    limit: PAGE,
    outcome,
    sort: rank.by,
    ...narrowed,
    ...scope,
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
      h(
        "div",
        { class: "banner banner-bad" },
        err.message || "The request log could not be read.",
      ),
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
  const currency = res.currency || ctx.currency;
  const names = {
    keys: res.key_names || {},
    projects: res.project_names || {},
    users: res.user_names || {},
  };
  const anyNarrowed = Object.values(narrowed).some(Boolean);

  const wrap = h("div", { style: { marginTop: "24px" } });

  // The counts are held rather than only drawn, because the stream below sends
  // fresh ones with every batch of rows.
  const seg = outcomeSeg(ctx, outcome, counts);

  // New rows arrive in time order, so a ranked log is not watched: a request
  // that just arrived has no place in a list of the most expensive ones until
  // the control plane ranks it.
  const watch =
    live && rank.by === ""
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
      lead,
      h("div", { style: { flex: 1 } }),
      seg.el,
      watch ? watch.el : null,
      h(
        "button",
        {
          class: "btn btn-sm",
          title: "Download all matching requests as CSV",
          onClick: () => api.download("/v1/requests", params),
        },
        icon(icons.download),
        "CSV",
      ),
    ),
  );

  if (filters) {
    wrap.append(narrowRow(ctx, narrowed, facets, names, { status: true }));
  }

  // Both empty states still watch. An empty log is the screen most likely to be
  // sat in front of waiting - somebody has just pointed an editor here - and
  // "nothing yet" that stays "nothing yet" after the first request arrives is
  // the one thing this section must not say.
  if (!counts.total && !anyNarrowed) {
    wrap.append(
      h(
        "div",
        { class: "card" },
        empty(
          watch && watch.on
            ? "No requests yet in this window"
            : "No requests in this window",
          watch && watch.on
            ? "Nothing recorded in this range. New requests appear here as " +
                "they arrive."
            : "Nothing recorded in this range. Widen it, or check that a " +
                "client points at this gateway.",
        ),
      ),
    );
    attachLive(null);
    return wrap;
  }

  const log = rows.length
    ? requestTable(ctx, rows, names, currency, hide, { rank })
    : null;
  wrap.append(
    log ||
      h(
        "div",
        { class: "card" },
        empty(
          "Nothing matches this filter",
          filters && anyNarrowed
            ? "Nothing matches all the filters above. Clear some, pick a " +
                "different outcome, or widen the range."
            : "Pick a different outcome above, or widen the range.",
        ),
      ),
  );

  if (rows.length === PAGE && rank.by !== "") {
    wrap.append(rankedNote(PAGE, "requests"));
  }

  // Paging appends rather than replaces, so somebody reading backwards through
  // an incident keeps what they have already read on the screen.
  if (rows.length === PAGE && rank.by === "") {
    let before = res.next_before;
    const more = h(
      "button",
      {
        class: "btn",
        onClick: async () => {
          more.disabled = true;
          more.textContent = "Loading…";
          try {
            const next = await api.requests({ ...params, before });
            const page = next.data || [];
            wrap.insertBefore(
              requestTable(ctx, page, names, currency, hide, { rank }),
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
  attachLive(log);
  return wrap;

  /** attachLive opens the stream and keeps this section up to date from it.
   *
   *  Declared inside requestLog because everything it touches belongs to one
   *  rendering of the section: the rows array the table was built from, the
   *  counts beside it, the labels the rows are drawn with. `log` is that
   *  table, or null when there is nothing on screen to add a row to.
   *
   *  The declaration is hoisted to the top of its scope, which is what lets
   *  the empty-window path above call it before this line. */
  function attachLive(log) {
    if (!watch) return;

    // The stream takes the narrowing and not the paging: what it is for is the
    // rows past the newest one on the screen, and `after` is that row. A screen
    // with no rows sends nothing and the control plane starts it at the log's
    // current end, rather than replaying a window nobody asked to see again.
    const listen = {
      org_id: ctx.orgID,
      since,
      outcome,
      ...narrowed,
      ...scope,
      after: rows.length ? rows[0].id : 0,
    };

    let source = null;
    const close = () => {
      if (source) source.close();
      source = null;
    };
    ctx.onTeardown(close);

    const open = () => {
      close();
      watch.state("connecting");
      source = api.requestStream(listen);
      source.addEventListener("requests", (e) => {
        watch.state("live");
        arrived(JSON.parse(e.data));
      });
      source.addEventListener("open", () => watch.state("live"));
      // An EventSource retries a dropped connection on its own and gives up
      // only on a response it was refused - an expired session, a role that may
      // no longer read this. Both are worth saying, and they are different
      // sentences.
      source.addEventListener("error", () => {
        watch.state(
          source && source.readyState === EventSource.CLOSED
            ? "stopped"
            : "connecting",
        );
      });
    };

    function arrived(payload) {
      if (payload.next_after) listen.after = payload.next_after;
      if (payload.outcomes) seg.retally(payload.outcomes);
      // Labels travel only when a row named something this screen opened too
      // early to know about - a key issued a minute ago, whose first request is
      // exactly what somebody is watching for.
      Object.assign(names.keys, payload.key_names || {});
      Object.assign(names.projects, payload.project_names || {});
      Object.assign(names.users, payload.user_names || {});

      const fresh = payload.data || [];
      if (!fresh.length) return;
      if (!log) {
        // There is no table to put these in - the window was empty, or nothing
        // matched. Drawing the screen again is what turns it into the one this
        // reader now wants, and it happens once.
        ctx.reload();
        return;
      }
      // The table reads newest first. A row that was committed late is older
      // than some already shown, so each goes in where its id belongs.
      for (const q of fresh) {
        q.fresh = true;
        const at = rows.findIndex((r) => r.id < q.id);
        rows.splice(at < 0 ? rows.length : at, 0, q);
      }
      log.redraw();
      // The mark comes off after a moment, and the table is drawn again so that
      // it actually leaves. Left on, a row that arrived an hour ago would light
      // up again the next time the reader sorted a column.
      setTimeout(() => {
        for (const q of fresh) delete q.fresh;
        log.redraw();
      }, FRESH_MS);
    }

    watch.onChange((on) => (on ? open() : (close(), watch.state("paused"))));
    if (watch.on) open();
    else watch.state("paused");
  }
}

/** requestTable is a page of the log, and is exported for the one screen that
 *  holds request rows without having asked this section for them: a session,
 *  which is a task's own calls read from beginning to end. They are the same
 *  rows and so they are the same table - the same columns, the same outcome
 *  wording, the same dialog behind View.
 *
 *  `opts` is merged into the table's own options. The log passes its ranking
 *  there, because it holds one page of many. A session holds all its rows and
 *  sorts them in place, oldest first, because a task is read forwards. */
export function requestTable(ctx, rows, names, currency, hide, opts = {}) {
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
            q.alias
              ? h("span", { class: "mono" }, q.alias)
              : h("span", { class: "faint" }, "-"),
        },
    {
      // The outcome and, when there is one, what the client was told. The
      // message belongs next to the verdict rather than in a column of its own:
      // most rows in a request log have none, and an empty column six rows wide
      // is a column that pushes the numbers off the screen.
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
          q.error
            ? h(
                "span",
                {
                  class: "muted",
                  style: { fontSize: "11.5px" },
                  title: q.error,
                },
                oneLine(q.error, 90),
              )
            : null,
        );
      },
    },
    hide.key
      ? null
      : {
          // The key is a link, because "which key was that" is never the last
          // question: the next one is what else that key has been doing, and that
          // is one screen over rather than a filter somebody has to rebuild.
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
              : h("span", { class: "faint" }, "no key"),
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
              : h("span", { class: "faint" }, "none"),
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
              `${compact(q.input_tokens)} · ${compact(q.output_tokens)}`,
            )
          : h("span", { class: "faint" }, "-"),
    },
    {
      label: `Cost (${currency})`,
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
              money(q.cost_micros, ""),
              // An estimate is marked, because the number beside it will be
              // reconciled against an invoice by somebody who was not here.
              q.estimated ? h("span", { class: "faint" }, "~") : null,
            )
          : h("span", { class: "faint" }, "-"),
    },
    {
      // First token rather than total: for a coding agent that is the latency
      // a developer perceives, and it is what says a model has gone slow rather
      // than gone down. The total is on hover, and in the row's own dialog.
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
            onClick: () => showRequest(ctx, q, names, currency),
          },
          "View",
        ),
    },
  ].filter(Boolean);

  return table(columns, rows, {
    search: (q) =>
      `${q.alias} ${q.status} ${q.error || ""} ` +
      `${names.keys[q.key_id] || q.key_id || ""} ${names.projects[q.project_id] || ""}`,
    searchLabel: "these requests",
    emptyTitle: "Nothing here",
    // A row that arrived while the reader was looking at the screen. It is
    // marked rather than announced: the count above already changed, and what
    // this answers is the smaller question of which line is the new one.
    rowClass: (q) => (q.fresh ? "row-new" : null),
    ...opts,
  });
}

// showRequest is one request in full - the row expanded into every field the
// gateway recorded, with the message shown whole and selectable because what is
// done with it is pasting it into a message to somebody else.
//
// It also carries the way out of it. One call of forty says very little about
// why the forty were made, and the task this one belonged to is the next thing
// its reader wants - so the dialog offers it rather than leaving them to find
// the Sessions screen and rebuild a filter that would not have found this row
// anyway.
export function showRequest(ctx, q, names, currency) {
  const [label, tone, why] = outcomeMeaning(q);
  const recorded = [
    ["Model", q.alias || "-"],
    // The status the client was sent, said in the terms of what actually
    // happened - a stream that stopped halfway was still sent a 200, and so
    // was one that finished.
    ["Status", `${q.status} - ${label}`],
    [
      "Key",
      q.key_id ? `${names.keys[q.key_id] || q.key_id} (${q.key_id})` : "no key",
    ],
    [
      "Project",
      q.project_id ? names.projects[q.project_id] || q.project_id : "-",
    ],
    ["Who", q.user_id ? names.users[q.user_id] || q.user_id : "-"],
    ["When", dateTime(q.ts)],
    [
      "Input tokens",
      num(q.input_tokens) +
        // The cached share is what makes the cost reproducible: it is charged
        // at its own rate, so without it the money on this row cannot be
        // arrived at from the tokens on it.
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
    [
      "Cost",
      money(q.cost_micros, currency) + (q.estimated ? " (estimated)" : ""),
    ],
    ["First token", ms(q.ttft_ms)],
    ["Took", ms(q.latency_ms)],
    ["Streamed", q.stream ? "yes" : "no"],
    ["Client hung up", q.canceled ? "yes" : "no"],
  ];
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

  // Where the time went, when it was recorded. It goes above the fields because
  // it is the question the dialog is most often opened with - the row already
  // said what happened and what it cost, but not which part of the request was
  // the wait.
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
