// The filters and rankings the request log and the session list share.
// Readers move between the two mid-question, so the choices carry over.

import { h, num, remembered, remember } from "../ui.js";
import { statusLabel } from "../status.js";

export function namesFrom(res) {
  return {
    keys: res.key_names || {},
    projects: res.project_names || {},
    users: res.user_names || {},
  };
}

export function addNames(names, res) {
  for (const [kind, more] of Object.entries(namesFrom(res))) {
    Object.assign(names[kind], more);
  }
}

// "All" leads: often the answer is that the calls were served, which a log
// of failures cannot show.
//
// On a session list, a lens keeps the tasks with at least one such request,
// and "Success" the tasks where every request was.
const OUTCOMES = [
  { key: "", label: "All", of: (c) => c.total },
  { key: "ok", label: "Success", of: (c) => c.ok },
  { key: "failed", label: "Failed", of: (c) => c.failed },
  { key: "refused", label: "Refused", of: (c) => c.refused },
  { key: "interrupted", label: "Interrupted", of: (c) => c.interrupted },
];

// Remembered on its own: only an entity's screen offers the outcome. The
// Requests and Sessions screens filter by status instead.
const OUTCOME_KEY = "keera.log.outcome";

// One list, so reading, drawing and clearing cannot drift apart. A session's
// status is that of its last request.
const NARROWINGS = [
  {
    param: "status",
    facet: "statuses",
    label: "All statuses",
    aria: "Status",
    name: statusFilterLabel,
    options: statusOptions,
  },
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
];

// Status classes, above the exact statuses: people ask "did anything fail"
// without knowing the code. The control plane reads `value` as a class;
// `holds` splits the facet counts the same way.
const STATUS_CLASSES = [
  { value: "5xx", label: "Server errors (5xx)", holds: (s) => s >= 500 },
  {
    value: "4xx",
    label: "Client errors (4xx)",
    holds: (s) => s >= 400 && s < 500,
  },
  { value: "2xx", label: "Success (2xx)", holds: (s) => s >= 200 && s < 300 },
];

function statusFilterLabel(v) {
  const cls = STATUS_CLASSES.find((c) => c.value === v);
  return cls ? cls.label : `${v} - ${statusLabel(Number(v))}`;
}

// statusOptions lists a class only when the window has rows in it.
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
  // Kept with (0), as in narrowSelect.
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

/** narrowings leaves out the user filter for a member. */
function narrowings(ctx) {
  return NARROWINGS.filter(
    (n) => n.param !== "user_id" || ctx.state.me.can_admin_org,
  );
}

const narrowKey = (param) => "keera.log." + param;

export function readNarrowing(ctx) {
  const out = {};
  for (const n of narrowings(ctx))
    out[n.param] = remembered(narrowKey(n.param)) || "";
  return out;
}

function clearNarrowing() {
  for (const n of NARROWINGS) remember(narrowKey(n.param), null);
}

/** setOutcome points the log at the rows a failure pill counted. */
export function setOutcome(outcome) {
  remember(OUTCOME_KEY, outcome);
}

/** openFailed points the Requests screen at the dashboard's 5xx rows. It
 *  clears the other filters, which would show fewer rows than the count. */
export function openFailed() {
  clearNarrowing();
  remember(narrowKey("status"), "5xx");
}

export function currentOutcome() {
  return remembered(OUTCOME_KEY) || "";
}

/** outcomeSeg is the row of lenses with their counts. `retally` takes fresh
 *  counts from the stream, not a running total, so they cannot drift. */
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

/** narrowRow is the filters of a screen with no scope. Each keeps its value
 *  when another or the range changes, as combining them is the point. */
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

/** ranking is a list's ranking, remembered under key, in the form table()
 *  takes as `rank`. */
export function ranking(ctx, key) {
  return {
    by: remembered(key) || "",
    set: (by) => {
      remember(key, by);
      ctx.reload();
    },
  };
}

/** rankedNote goes under a full ranked page. Paging works only in time order,
 *  so the rest is reached by narrowing. */
export function rankedNote(count, what) {
  return h(
    "div",
    { class: "hint", style: { marginTop: "12px", textAlign: "center" } },
    `The top ${num(count)} ${what}. Narrow the filters or the period to see ` +
      "others, or sort by time to page through all of them.",
  );
}

// narrowSelect lists only the values in the window, with their counts, so
// unused keys do not bury the used ones. A selected value that no longer
// occurs is kept with (0), so the filter still shows as set. A filter with
// fewer than two values is left out, unless it is set.
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
        remember(narrowKey(n.param), e.target.value);
        ctx.reload();
      },
    },
    h("option", { value: "" }, n.label),
    options,
  );
}
