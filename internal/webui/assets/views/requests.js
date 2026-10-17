// Requests: the log of every request, not only the failures, because "my
// agent's calls never arrived" is answered by the rows that worked.
//
// The screen is only the shared log section, so its counts always match a
// project's or a key's own page. An administrator reads the whole
// organisation's log, a member only the requests of their own keys.

import { h, currentRange, rangePicker, rangeOf } from "../ui.js";
import { requestLog } from "./requestlog.js";
import { readNarrowing } from "./logfilters.js";

export async function requestsView(ctx) {
  const since = currentRange();
  ctx.setSubtitle(rangeOf(since)?.long || "");

  const log = await requestLog(ctx, {
    since,
    filters: true,
    // People open this screen to see what is happening now, so it refreshes
    // itself instead of being reloaded by hand.
    live: true,
    // The page title already says Requests.
    title: "",
    trail: rangePicker(ctx, since),
  });
  // The section is written to sit under a chart. Here it is the screen.
  log.style.marginTop = "0";

  const tip = advice(readNarrowing(ctx).status);
  return h(
    "div",
    {},
    log,
    tip && h("div", { class: "hint", style: { marginTop: "16px" } }, tip),
  );
}

// advice says what to do next, by the class of the status filtered to.
function advice(status) {
  switch ((status || "").charAt(0)) {
    case "2":
      return (
        "Forwarded and answered. Look for slow first tokens, or keys that " +
        "spend more than expected. A row marked Interrupted stopped while " +
        "streaming: usually the client hung up, which needs no action."
      );
    case "5":
      return (
        "The request got a 5xx: the backend did not answer, or the gateway " +
        "had nowhere to send it. Each row shows the message. If only one " +
        "model fails, look at its backends."
      );
    case "4":
      return (
        "The request got a 4xx. Mostly a guardrail stopped it: change the " +
        "budget or rate limit on the project or key, or tell the developer which " +
        "model to use. A 400 or 413 is a request that could not be taken; " +
        "each row shows why."
      );
    default:
      return null;
  }
}
