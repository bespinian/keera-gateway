// The request log: one row per recorded request, wherever it is being read.
//
// The Requests screen is this section and nothing else, and each team's,
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
} from "../ui.js";
import { outcomeMeaning, statusLabel, oneLine } from "../status.js";
import { flameChart } from "../flame.js";

const PAGE = 100;

// Whether the log is being watched, remembered like every other choice this
// section holds. It defaults to on: somebody who opened the request log opened
// it to find out what is happening, and the most common version of that
// question is about the next thirty seconds rather than the last hour.
const LIVE_KEY = "keera.requests.live";

// How long a row that has just arrived is marked as new. Long enough to catch
// the eye of somebody looking elsewhere on the screen, short enough that a log
// nobody is watching is not a wall of highlights when they come back.
const FRESH_MS = 4000;

// The lenses on the log, widest first. "All" leads because the first question
// is what has been happening rather than what went wrong: a log that opens on
// the failures cannot tell somebody that their agent's calls arrived and were
// served, which half the time is the answer.
const OUTCOMES = [
  { key: "", label: "All", of: (c) => c.total },
  { key: "ok", label: "Served", of: (c) => c.ok },
  { key: "failed", label: "Failed", of: (c) => c.failed },
  { key: "refused", label: "Refused", of: (c) => c.refused },
  { key: "interrupted", label: "Interrupted", of: (c) => c.interrupted },
];

// The outcome is remembered on its own, because it is the one lens both callers
// have and because the dashboard's failure count sets it directly.
const OUTCOME_KEY = "keera.requests.outcome";

// What the log can be narrowed by on a screen that carries the filters: the
// query parameter, the facet the counts arrive under, how a value is written
// for a reader, and - for the one filter that is asked in more than one size -
// how its list is drawn. They are one list so that reading them, drawing them
// and clearing them cannot fall out of step with each other.
const NARROWINGS = [
  { param: "alias", facet: "models", label: "Every model", aria: "Model" },
  {
    param: "team_id",
    facet: "teams",
    label: "Every team",
    aria: "Team",
    name: (v, names) => names.teams[v] || v,
  },
  {
    param: "key_id",
    facet: "keys",
    label: "Every key",
    aria: "Key",
    name: (v, names) => names.keys[v] || v,
  },
  {
    param: "user_id",
    facet: "users",
    label: "Everyone",
    aria: "Person",
    name: (v, names) => names.users[v] || v,
  },
  {
    param: "status",
    facet: "statuses",
    label: "Every status",
    aria: "Status",
    name: statusFilterLabel,
    options: statusOptions,
  },
];

// The status classes, offered above the exact statuses.
//
// The same reader asks this filter in two sizes within a minute: "which of
// these was the 402" is one code, and "did anything fail this afternoon" is
// all of 5XX at once, asked by somebody who does not know which code their
// backends answer with.
//
// The value is what the control plane reads as a class, and `holds` is the
// same division over the facet counts, so a class is offered with how many
// rows carry it exactly as an exact status is.
const STATUS_CLASSES = [
  { value: "5xx", label: "Server errors (5XX)", holds: (s) => s >= 500 },
  {
    value: "4xx",
    label: "Client errors (4XX)",
    holds: (s) => s >= 400 && s < 500,
  },
  { value: "2xx", label: "Answered (2XX)", holds: (s) => s >= 200 && s < 300 },
];

// statusFilterLabel names one status filter with no row beside it to read: a
// class by what it holds, an exact status by its number and what that means.
function statusFilterLabel(v) {
  const cls = STATUS_CLASSES.find((c) => c.value === v);
  return cls ? cls.label : `${v} - ${statusLabel(Number(v))}`;
}

// statusOptions draws the two sizes as two groups. A reader looking for "the
// server errors" should not have to read a list of codes to work out whether
// one of them is the whole class they meant, and a reader looking for the 402
// should not have to scroll past the classes to find it.
//
// A class is listed only when the window holds something in it, which the
// outcome lens above already narrows: under "Failed" there is nothing but 5XX,
// and offering 4XX there would be offering a filter that selects nothing.
function statusOptions(counts, selected) {
  const groups = STATUS_CLASSES.map((c) => ({
    ...c,
    count: counts.reduce(
      (n, f) => (c.holds(Number(f.value)) ? n + Number(f.count) : n),
      0,
    ),
  })).filter((c) => c.count > 0 || c.value === selected);

  const exact = counts.map((c) =>
    h(
      "option",
      { value: c.value, selected: c.value === selected },
      `${statusFilterLabel(c.value)} (${num(c.count)})`,
    ),
  );
  // A status that is selected and no longer occurs - one picked under "All" and
  // still selected under "Failed" - stays in the list and says so. A select
  // showing nothing selected is a filter nobody can see they have applied.
  if (
    selected &&
    !groups.some((c) => c.value === selected) &&
    !counts.some((c) => c.value === selected)
  ) {
    exact.unshift(
      h(
        "option",
        { value: selected, selected: true },
        `${statusFilterLabel(selected)} (0)`,
      ),
    );
  }

  return [
    groups.length
      ? h(
          "optgroup",
          { label: "Groups" },
          groups.map((c) =>
            h(
              "option",
              { value: c.value, selected: c.value === selected },
              `${c.label} (${num(c.count)})`,
            ),
          ),
        )
      : null,
    exact.length ? h("optgroup", { label: "Exact status" }, exact) : null,
  ].filter(Boolean);
}

const narrowKey = (param) => "keera.requests." + param;

function readNarrowing() {
  const out = {};
  for (const n of NARROWINGS)
    out[n.param] = sessionStorage.getItem(narrowKey(n.param)) || "";
  return out;
}

function clearNarrowing() {
  for (const n of NARROWINGS) sessionStorage.removeItem(narrowKey(n.param));
}

/** setOutcome is the dashboard's failure count, and the failure pill on an
 *  entity's chart, pointing the log at the rows they counted - on this screen
 *  or on the one they send the reader to. */
export function setOutcome(outcome) {
  sessionStorage.setItem(OUTCOME_KEY, outcome);
}

/** openFailed points this log at the failed rows and nothing else, for the
 *  dashboard's failure count.
 *
 *  It clears the narrowing where setOutcome leaves it alone. The count on that
 *  pill is over the whole organisation, and a model or a key still selected
 *  from somebody's last visit would put a smaller number under a pill that had
 *  just promised them a bigger one. */
export function openFailed() {
  setOutcome("failed");
  clearNarrowing();
}

/** currentOutcome is which lens the log is being read through, for a screen
 *  that says something about it around the section rather than inside it. */
export function currentOutcome() {
  return sessionStorage.getItem(OUTCOME_KEY) || "";
}

/** requestLog is the section itself.
 *
 *  `scope` is what the caller is already about ({ team_id }, { key_id },
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
  const outcome = sessionStorage.getItem(OUTCOME_KEY) || "";
  const narrowed = filters ? readNarrowing() : {};
  // The scope wins over the narrowing: a team's own screen is about that team
  // whatever was last picked on the dashboard.
  const params = {
    org_id: ctx.orgID,
    since,
    limit: PAGE,
    outcome,
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
    keys: res.key_aliases || {},
    teams: res.team_names || {},
    users: res.user_names || {},
  };
  const anyNarrowed = NARROWINGS.some((n) => narrowed[n.param]);

  const wrap = h("div", { style: { marginTop: "24px" } });

  // The counts are held rather than only drawn, because the stream below sends
  // fresh ones with every batch of rows. They are the control plane's own count
  // over the window each time and never a running total accumulated here: a
  // number beside "All" that drifted from the one a reload would show is worse
  // than a number that only changes when the reader asks for it.
  const shown = { ...counts };
  const tallies = new Map();
  const seg = h(
    "div",
    { class: "seg" },
    OUTCOMES.map((oc) => {
      const tally = h("span", { class: "faint" }, num(oc.of(counts)));
      const button = h(
        "button",
        {
          "aria-pressed": String(oc.key === outcome),
          disabled: oc.key !== outcome && oc.of(counts) === 0,
          onClick: () => {
            setOutcome(oc.key);
            ctx.reload();
          },
        },
        oc.label,
        " ",
        tally,
      );
      tallies.set(oc, { button, tally });
      return button;
    }),
  );

  function retally(next) {
    Object.assign(shown, next);
    for (const [oc, { button, tally }] of tallies) {
      tally.textContent = num(oc.of(shown));
      button.disabled = oc.key !== outcome && oc.of(shown) === 0;
    }
  }

  const watch = live ? liveControl() : null;
  wrap.append(
    h(
      "div",
      { class: "section-head" },
      title ? h("h2", {}, title) : null,
      lead,
      h("div", { style: { flex: 1 } }),
      seg,
      watch ? watch.button : null,
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

  // The narrowing keeps what it has when the outcome or the range changes,
  // because "now show me that team's failures" is the next thing somebody asks
  // rather than a mistake. A selection that matches nothing under the new
  // outcome says so below, and Clear is right beside it.
  if (filters) {
    wrap.append(
      h(
        "div",
        { class: "row", style: { margin: "12px 0", flexWrap: "wrap" } },
        NARROWINGS.map((n) =>
          narrowSelect(ctx, n, narrowed[n.param], facets[n.facet] || [], names),
        ),
        anyNarrowed
          ? h(
              "button",
              {
                class: "btn btn-quiet btn-sm",
                onClick: () => {
                  clearNarrowing();
                  ctx.reload();
                },
              },
              "Clear",
            )
          : null,
      ),
    );
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
    ? requestTable(ctx, rows, names, currency, hide)
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

  // Paging appends rather than replaces, so somebody reading backwards through
  // an incident keeps what they have already read on the screen.
  if (rows.length === PAGE) {
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
              requestTable(ctx, page, names, currency, hide),
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
      if (payload.outcomes) retally(payload.outcomes);
      // Labels travel only when a row named something this screen opened too
      // early to know about - a key issued a minute ago, whose first request is
      // exactly what somebody is watching for.
      Object.assign(names.keys, payload.key_aliases || {});
      Object.assign(names.teams, payload.team_names || {});
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
      // The batch arrives newest first and the table reads the same way, so
      // they go on in the other order to keep the newest at the top.
      for (const q of fresh.slice().reverse()) {
        q.fresh = true;
        rows.unshift(q);
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

// liveControl is the switch, and the one thing on the screen that says whether
// the log is being watched at all.
//
// It says which of three things is true, because they are three different
// situations for the reader: it is watching, they turned it off, or it is not
// connected and they should not read an unchanging screen as a quiet afternoon.
function liveControl() {
  const on = sessionStorage.getItem(LIVE_KEY) !== "off";
  const blip = h("span", { class: "blip" });
  const button = h(
    "button",
    {
      class: "btn btn-sm",
      "aria-pressed": String(on),
      "aria-label": "Watch for new requests",
    },
    blip,
    "Live",
  );
  let listener = null;

  const control = {
    on,
    button,
    state(what) {
      blip.dataset.state = what;
      button.title = {
        live: "Live: new requests appear here. Click to stop.",
        connecting: "Reconnecting to the request log. Click to stop watching.",
        paused: "Paused. Click to see new requests live.",
        stopped:
          "The stream stopped and could not reconnect. Click to try again, " +
          "or reload the page.",
      }[what];
    },
    onChange(fn) {
      listener = fn;
    },
  };
  button.addEventListener("click", () => {
    control.on = !control.on;
    sessionStorage.setItem(LIVE_KEY, control.on ? "on" : "off");
    button.setAttribute("aria-pressed", String(control.on));
    if (listener) listener(control.on);
  });
  return control;
}

// narrowSelect is one filter, listing only the values that occur in the window
// and how many rows each holds. A deployment with two hundred keys has four
// that made a request this week, and the other hundred and ninety-six are
// something to search through.
//
// A value that is selected but no longer occurs - a model picked under "all"
// and still selected under "failed" - is kept and marked, because a select
// that silently shows nothing is a filter nobody can see they have applied.
//
// A filter that groups its values draws its own list: the status one is the
// only such filter, and it owns both halves of what it offers.
function narrowSelect(ctx, n, selected, counts, names) {
  const label = (v) => (n.name ? n.name(v, names) : v);
  const options = n.options
    ? n.options(counts, selected)
    : counts.map((c) =>
        h(
          "option",
          { value: c.value, selected: c.value === selected },
          `${label(c.value)} (${num(c.count)})`,
        ),
      );
  if (!n.options && selected && !counts.some((c) => c.value === selected)) {
    options.unshift(
      h(
        "option",
        { value: selected, selected: true },
        `${label(selected)} (0)`,
      ),
    );
  }
  return h(
    "select",
    {
      class: "select",
      style: { width: "auto" },
      "aria-label": n.aria,
      disabled: options.length === 0,
      onChange: (e) => {
        sessionStorage.setItem(narrowKey(n.param), e.target.value);
        ctx.reload();
      },
    },
    h("option", { value: "" }, n.label),
    options,
  );
}

/** requestTable is a page of the log, and is exported for the one screen that
 *  holds request rows without having asked this section for them: a session,
 *  which is a task's own calls read from beginning to end. They are the same
 *  rows and so they are the same table - the same columns, the same outcome
 *  wording, the same dialog behind View.
 *
 *  `opts` is merged into the table's own options, which is how that screen asks
 *  for the one thing it needs differently: its rows are read oldest first,
 *  because a task is read forwards. */
export function requestTable(ctx, rows, names, currency, hide, opts = {}) {
  const columns = [
    {
      label: "When",
      shrink: true,
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
    hide.team
      ? null
      : {
          label: "Team",
          shrink: true,
          sortKey: (q) => names.teams[q.team_id] || q.team_id || null,
          cell: (q) =>
            q.team_id
              ? h(
                  "span",
                  { class: "muted nowrap" },
                  names.teams[q.team_id] || q.team_id,
                )
              : h("span", { class: "faint" }, "none"),
        },
    {
      label: "Tokens",
      num: true,
      shrink: true,
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
      `${names.keys[q.key_id] || q.key_id || ""} ${names.teams[q.team_id] || ""}`,
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
    ["Team", q.team_id ? names.teams[q.team_id] || q.team_id : "-"],
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
            title: "All calls the agent made for the same task",
            onClick: () => {
              close();
              ctx.navigate("/sessions/" + encodeURIComponent(q.id));
            },
          },
          "Open its session",
        )
    : null;

  // Where the time went, when the gateway that served this request recorded
  // it. It goes above the fields rather than below them because it is the
  // question the dialog is most often opened with - the row already said what
  // happened and what it cost, and what it could not say is which part of the
  // request was the wait.
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
