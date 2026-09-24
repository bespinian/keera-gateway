// Requests: the whole event log, read across the deployment.
//
// This is where the dashboard's numbers came from. The charts say when
// something changed and this says what changed - which model, whose key, which
// person, and what the backend answered. It holds every request rather than
// only the ones that went wrong, because "my agent's calls never arrived" is
// answered by the rows that worked.
//
// The screen is the shared log section and nothing else, so the counts here
// and on a team's or a key's own page can never disagree.
//
// Administrator-only, like the audit log: the rows name other people's keys
// and carry text the inference plane wrote. A member reading their own traffic
// has it on My access.

import { h, RANGES, currentRange, rangePicker } from "../ui.js";
import { requestLog, currentOutcome } from "./requestlog.js";

export async function requestsView(ctx) {
  const since = currentRange();
  const range = RANGES.find((r) => r.since === since);
  ctx.setSubtitle(range ? range.long : "");

  const log = await requestLog(ctx, {
    since,
    filters: true,
    // The screen that is nothing but this log is the one worth watching: what
    // it is opened to find out is usually what is happening now, and a reader
    // reloading every few seconds to learn that is a reader polling the
    // deployment by hand.
    live: true,
    // The page title already says Requests. The heading's place goes to the
    // range picker, which is the one control the section does not own.
    title: "",
    lead: rangePicker(ctx, since),
  });
  // The section is written to sit under a chart. Here it is the screen.
  log.style.marginTop = "0";

  return h(
    "div",
    {},
    log,
    h(
      "div",
      { class: "hint", style: { marginTop: "16px" } },
      advice(currentOutcome()),
    ),
  );
}

// advice is what to do with what is on the screen. Each of these is a different
// person's problem, and the log is the only place that ever says whose.
function advice(outcome) {
  switch (outcome) {
    case "ok":
      return (
        "Forwarded and answered. Look for slow first tokens, or keys that " +
        "spend more than expected."
      );
    case "failed":
      return (
        "The request was forwarded, but the backend did not answer. Each row " +
        "shows the backend's own message. Share it with whoever runs that " +
        "endpoint. If only one model fails, the problem is its backend."
      );
    case "refused":
      return (
        "A guardrail stopped the request. Change the budget or rate limit on " +
        "the team or key, or tell the developer which model to use."
      );
    case "interrupted":
      return (
        "The answer stopped while streaming. Usually the client hung up, " +
        "which needs no action. If one model keeps doing this, its backend is " +
        "dropping connections."
      );
    default:
      return (
        "Every request: served, refused, interrupted or failed. With Live " +
        "on, new ones appear as they arrive. Open a row for its details, or " +
        "export the list as CSV."
      );
  }
}
