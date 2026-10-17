// What a status meant, in the words of whoever has to act on it.
//
// Several screens read the same event log, so they share these words to call
// the same thing by the same name. The number is the record; these are the
// sentence next to it.

const STATUS = {
  400: ["Bad request", "The request could not be read."],
  402: [
    "Budget spent",
    "A budget that covers the key is used up for this period.",
  ],
  // Not only filters answer 403: a model a new organisation cannot use yet
  // does too, and the row keeps only the message, not the code.
  403: [
    "Refused",
    "A filter refused the request, or the key may not use this model yet.",
  ],
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

export function statusMeaning(status) {
  if (STATUS[status]) return STATUS[status];
  if (status >= 500) return ["Backend error", "The backend did not answer."];
  if (status >= 400)
    return ["Refused", "The gateway did not forward this request."];
  return ["Interrupted", "The answer started but did not finish."];
}

/** statusLabel names a status with no row beside it, as a filter must. It is
 *  not statusMeaning, which reads a log of only unhappy requests, where a 200
 *  can only be an answer that stopped. In a log of every request, almost every
 *  200 was served. */
export function statusLabel(status) {
  return status < 400 ? "Served" : statusMeaning(status)[0];
}

function statusTone(status) {
  if (status >= 500) return "bad";
  if (status >= 400) return "warn";
  return "";
}

/** outcomeOf is what happened to one row, as the request log groups them. */
export function outcomeOf(q) {
  if (q.status >= 500) return "failed";
  if (q.status >= 400) return "refused";
  if (q.error) return "interrupted";
  return "ok";
}

/** outcomeMeaning returns [label, tone, why] for one row.
 *
 *  It reads the row, not just the status: a request that was answered and one
 *  whose stream stopped halfway both carry 200, and only the message tells
 *  them apart. */
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

/** oneLine folds a message onto one table row. Backends answer with stack
 *  traces and echoed requests, and a forty-line cell makes a table unreadable. */
export function oneLine(text, max) {
  const flat = text.replace(/\s+/g, " ").trim();
  return flat.length > max ? flat.slice(0, max - 1) + "…" : flat;
}
