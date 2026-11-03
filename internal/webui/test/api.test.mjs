// Unit tests for the control API client, with fetch standing in for the
// server.

import { test } from "node:test";
import assert from "node:assert/strict";

import { api, setCsrf, ApiError } from "../assets/api.js";

// serve answers each call with the session the cookie holds now: its token,
// and whose it is.
function serve(session) {
  const sent = [];
  globalThis.fetch = async (url, init) => {
    sent.push({ url, token: init.headers["X-CSRF-Token"] });
    if (url.endsWith("/v1/me")) {
      return Response.json({ user_id: session.user, csrf: session.csrf });
    }
    if (init.headers["X-CSRF-Token"] !== session.csrf) {
      return Response.json(
        { error: { code: "csrf", message: "missing its CSRF token" } },
        { status: 403 },
      );
    }
    return Response.json({ ok: true });
  };
  return sent;
}

test("a call refused for an old token is sent again with the new session's", async () => {
  setCsrf("old", "user_a");
  const sent = serve({ user: "user_a", csrf: "new" });
  assert.deepEqual(await api.signOut(), { ok: true });
  assert.deepEqual(
    sent.map((s) => s.token),
    ["old", undefined, "new"],
  );
});

test("a call is not sent again as another person", async () => {
  setCsrf("old", "user_a");
  const sent = serve({ user: "user_b", csrf: "new" });
  await assert.rejects(
    api.signOut(),
    (err) => err instanceof ApiError && err.code === "csrf",
  );
  assert.equal(sent.length, 2);
});
