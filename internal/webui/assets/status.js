// What a status meant, in the words of whoever has to act on it.
//
// This lives on its own because two screens read the same event log and have to
// call the same thing by the same name. The failure log says "Budget spent" for
// a 402; a team's own request log showing "402" next to it, or worse a
// different sentence, would be the same row described two ways in one panel.
//
// The number is the record. These are the sentence next to it.

const STATUS = {
  400: ["Bad request", "The request could not be read."],
  402: [
    "Budget spent",
    "A budget that covers the key is used up for this period.",
  ],
  403: ["Not permitted", "A guardrail refused the key."],
  404: [
    "Model unavailable",
    "The key may not use this model, or no backend serves it.",
  ],
  408: ["Timed out", "The request did not complete in time."],
  413: ["Too large", "The prompt is over a size limit."],
  429: ["Rate limited", "Over a per-minute limit on requests or tokens."],
  500: ["Backend error", "The backend returned an error."],
  502: ["Backend unreachable", "No backend for the model could be reached."],
  503: [
    "Nothing serving",
    "The model has no backend, or a filter it needs cannot run.",
  ],
  504: ["Backend timed out", "The backend did not start answering in time."],
};

/** statusMeaning returns [label, why] for one recorded status. */
export function statusMeaning(status) {
  if (STATUS[status]) return STATUS[status];
  if (status >= 500) return ["Backend error", "The backend did not answer."];
  if (status >= 400)
    return ["Refused", "The gateway did not forward this request."];
  return ["Interrupted", "The answer started but did not finish."];
}

/** statusLabel names a status on its own, with no row beside it to read - the
 *  way a filter has to name one. It is not statusMeaning: that reads a status
 *  in a log that holds nothing but unhappy requests, where a 200 can only be an
 *  answer that stopped. In a log that holds every request, almost every 200 is
 *  one that was served, and a filter offering "200 - Interrupted" above four
 *  hundred perfectly good requests is a filter that lies about them. */
export function statusLabel(status) {
  return status < 400 ? "Served" : statusMeaning(status)[0];
}

export function statusTone(status) {
  if (status >= 500) return "bad";
  if (status >= 400) return "warn";
  return "";
}

/** outcomeOf is what happened to one row, as the request log groups them. A
 *  served request is the case the failure log has no name for, because it never
 *  holds one. */
export function outcomeOf(q) {
  if (q.status >= 500) return "failed";
  if (q.status >= 400) return "refused";
  if (q.error) return "interrupted";
  return "ok";
}

/** outcomeMeaning names what happened to one row, gives the pill its tone, and
 *  says it in a sentence: [label, tone, why].
 *
 *  Read from the row rather than from the status, because the status alone
 *  cannot tell the two 200s apart: a request that was answered and one whose
 *  stream stopped halfway both carry it, and only the message beside it says
 *  which happened. */
export function outcomeMeaning(q) {
  switch (outcomeOf(q)) {
    case "ok":
      return ["Served", "good", "The model answered in full."];
    case "interrupted":
      return ["Interrupted", "warn", "The answer started but did not finish."];
    default: {
      const [label, why] = statusMeaning(q.status);
      return [label, statusTone(q.status), why];
    }
  }
}

/** oneLine folds a message onto one row of a table. Backends answer with stack
 *  traces and with the request they were sent, and a table cell holding forty
 *  lines of one is a table nobody can scan. */
export function oneLine(text, max) {
  const flat = text.replace(/\s+/g, " ").trim();
  return flat.length > max ? flat.slice(0, max - 1) + "…" : flat;
}
