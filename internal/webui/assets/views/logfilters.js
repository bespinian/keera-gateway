// The filters and rankings the request log and the session list share.
//
// Both read the same table, one row per request or one row per task, and a
// reader moves between them in the middle of one question: "which of this
// project's tasks failed" turns into "and which call was it". So both are
// narrowed the same way, the choices carry over from one to the other, and
// both rank by clicking a column.

import { h, num } from "../ui.js";
import { statusLabel } from "../status.js";

// The lenses on the log, widest first. "All" leads because the first question
// is what has been happening rather than what went wrong: a log that opens on
// the failures cannot tell somebody that their agent's calls arrived and were
// served, which half the time is the answer.
//
// On the session list of a project, key or model, a lens keeps the tasks
// holding at least one such request, and "Served" the tasks where every
// request was.
const OUTCOMES = [
  { key: "", label: "All", of: (c) => c.total },
  { key: "ok", label: "Served", of: (c) => c.ok },
  { key: "failed", label: "Failed", of: (c) => c.failed },
  { key: "refused", label: "Refused", of: (c) => c.refused },
  { key: "interrupted", label: "Interrupted", of: (c) => c.interrupted },
];

// The outcome is remembered on its own. Only the log and the session list on
// a project's, key's or model's screen offer it; the Requests and Sessions
// screens filter by status instead.
const OUTCOME_KEY = "keera.log.outcome";

// What a log can be narrowed by on a screen that carries the filters: the
// query parameter, the facet the counts arrive under, how a value is written
// for a reader, and - for the one filter that is asked in more than one size -
// how its list is drawn. They are one list so that reading them, drawing them
// and clearing them cannot fall out of step with each other.
//
// On the session list, a session's status is the status of its last request,
// the one its "Ended" column shows.
const NARROWINGS = [
  { param: "alias", facet: "models", label: "All models", aria: "Model" },
  {
    param: "project_id",
    facet: "projects",
    label: "All projects",
    aria: "Project",
    name: (v, names) => names.projects[v] || v,
  },
  {
    param: "key_id",
    facet: "keys",
    label: "All keys",
    aria: "Key",
    name: (v, names) => names.keys[v] || v,
  },
  {
    param: "user_id",
    facet: "users",
    label: "All users",
    aria: "User",
    name: (v, names) => names.users[v] || v,
  },
  {
    param: "status",
    facet: "statuses",
    label: "All statuses",
    aria: "Status",
    name: statusFilterLabel,
    options: statusOptions,
  },
];

// The status classes, offered above the exact statuses.
//
// The same reader asks this filter in two sizes within a minute: "which of
// these was the 402" is one code, and "did anything fail this afternoon" is
// all of 5xx at once, asked by somebody who does not know which code their
// backends answer with.
//
// The value is what the control plane reads as a class, and `holds` is the
// same division over the facet counts, so a class is offered with how many
// rows carry it exactly as an exact status is.
const STATUS_CLASSES = [
  { value: "5xx", label: "Server errors (5xx)", holds: (s) => s >= 500 },
  {
    value: "4xx",
    label: "Client errors (4xx)",
    holds: (s) => s >= 400 && s < 500,
  },
  { value: "2xx", label: "Served (2xx)", holds: (s) => s >= 200 && s < 300 },
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
// A class is listed only when the window holds something in it: offering 4xx
// in a window without one would be offering a filter that selects nothing.
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
  // A status that is selected and no longer occurs - one picked in a wider
  // range - stays in the list and says so. A select
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

/** narrowings is what this reader may narrow this log by. A member reads only
 *  their own requests, so "All users" would offer one person: them. */
function narrowings(ctx) {
  return NARROWINGS.filter(
    (n) => n.param !== "user_id" || ctx.state.me.can_admin_org,
  );
}

const narrowKey = (param) => "keera.log." + param;

/** readNarrowing is what the reader last narrowed a log by, as query
 *  parameters. */
export function readNarrowing(ctx) {
  const out = {};
  for (const n of narrowings(ctx))
    out[n.param] = sessionStorage.getItem(narrowKey(n.param)) || "";
  return out;
}

function clearNarrowing() {
  for (const n of NARROWINGS) sessionStorage.removeItem(narrowKey(n.param));
}

/** setOutcome is the failure pill on an entity's chart, pointing the log
 *  below it at the rows it counted. */
export function setOutcome(outcome) {
  sessionStorage.setItem(OUTCOME_KEY, outcome);
}

/** openFailed points the Requests screen at the server errors and nothing
 *  else, for the dashboard's failure count, which counts the same 5xx rows.
 *
 *  It clears the rest of the narrowing. The count on that pill is over the
 *  whole organisation, and a model or a key still selected from somebody's
 *  last visit would put a smaller number under a pill that had just promised
 *  them a bigger one. */
export function openFailed() {
  clearNarrowing();
  sessionStorage.setItem(narrowKey("status"), "5xx");
}

/** currentOutcome is which lens the log is being read through. */
export function currentOutcome() {
  return sessionStorage.getItem(OUTCOME_KEY) || "";
}

/** outcomeSeg is the row of lenses, each with how many rows it holds. A lens
 *  that holds nothing is disabled, unless it is the one being read through.
 *
 *  `retally` takes fresh counts, for the request log's live stream. They are
 *  the control plane's own count each time and never a running total kept
 *  here: a number beside "All" that drifted from the one a reload would show
 *  is worse than one that only changes when the reader asks for it. */
export function outcomeSeg(ctx, outcome, counts) {
  const shown = { ...counts };
  const tallies = new Map();
  const el = h(
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
  const retally = (next) => {
    Object.assign(shown, next);
    for (const [oc, { button, tally }] of tallies) {
      tally.textContent = num(oc.of(shown));
      button.disabled = oc.key !== outcome && oc.of(shown) === 0;
    }
  };
  return { el, retally };
}

/** narrowRow is the filters a screen with no scope of its own carries, and the
 *  Clear beside them once any is set. `lead` is drawn before the filters.
 *
 *  Each filter keeps what it has when another one or the range changes,
 *  because "now show me that project's failures" is the next thing somebody
 *  asks rather than a mistake. A combination that matches nothing says so
 *  below, and Clear is right beside it. */
export function narrowRow(ctx, narrowed, facets, names, lead = null) {
  const list = narrowings(ctx);
  return h(
    "div",
    { class: "row", style: { margin: "12px 0", flexWrap: "wrap" } },
    lead,
    list.map((n) =>
      narrowSelect(ctx, n, narrowed[n.param], facets[n.facet] || [], names),
    ),
    list.some((n) => narrowed[n.param])
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
  );
}

/** ranking is how a paged list is ranked, remembered under key, in the form
 *  table() takes as `rank`. Each list has its own, because each offers
 *  different rankings. */
export function ranking(ctx, key) {
  return {
    by: sessionStorage.getItem(key) || "",
    set: (by) => {
      sessionStorage.setItem(key, by);
      ctx.reload();
    },
  };
}

/** rankedNote is said under a ranked list that filled its page. Paging works
 *  only in time order, so what is not shown is reached by narrowing. */
export function rankedNote(count, what) {
  return h(
    "div",
    { class: "hint", style: { marginTop: "12px", textAlign: "center" } },
    `The top ${num(count)} ${what}. Narrow the filters or the period to see ` +
      "others, or sort by time to page through all of them.",
  );
}

// narrowSelect is one filter, listing only the values that occur in the window
// and how many rows each holds. A deployment with two hundred keys has four
// that made a request this week, and the other hundred and ninety-six are
// something to search through.
//
// A value that is selected but no longer occurs - a model picked under one
// status and still selected under another - is kept and marked, because a select
// that silently shows nothing is a filter nobody can see they have applied.
//
// A filter with one value or none filters nothing, so it is left out, as on
// every other list. One that is set stays, so it can be seen and cleared.
//
// A filter that groups its values draws its own list: the status one is the
// only such filter, and it owns both halves of what it offers.
function narrowSelect(ctx, n, selected, counts, names) {
  if (!selected && counts.length < 2) return null;
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
      onChange: (e) => {
        sessionStorage.setItem(narrowKey(n.param), e.target.value);
        ctx.reload();
      },
    },
    h("option", { value: "" }, n.label),
    options,
  );
}
