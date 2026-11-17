// Unit tests for the try dialog's pure helpers.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  trialOutcome,
  decision,
  modelAnswer,
  promptSent,
  firstLine,
  pretty,
  inOrder,
} from "../assets/views/try.js";

test("a gate that lets a prompt through says allowed, a rewrite filter unchanged", () => {
  assert.equal(trialOutcome("gate", "pass").label, "Allowed");
  assert.equal(trialOutcome("rewrite", "pass").label, "Unchanged");
  assert.equal(trialOutcome("gate", "refuse").label, "Refused");
  assert.equal(trialOutcome("rewrite", "error").tone, "bad");
});

test("the answer shown is the decision's, not the one asking why", () => {
  const calls = [
    { purpose: "decide", answer: "A" },
    { purpose: "retry", answer: "keera-speed" },
    { purpose: "explain", answer: "Because it is short." },
  ];
  assert.equal(decision(calls).answer, "keera-speed");
  assert.equal(decision([]), null);
});

test("a failed try shows what the model wrote, or what came back instead", () => {
  assert.equal(
    modelAnswer({ calls: [{ sent: true, answer: "MAYBE" }] }),
    "MAYBE",
  );
  assert.equal(
    modelAnswer({
      calls: [{ sent: true, status: 502, response: "<html>bad gateway" }],
    }),
    "HTTP 502\n<html>bad gateway",
  );
  assert.equal(
    modelAnswer({ calls: [{ sent: false, error: "too large" }] }),
    "Not sent.",
  );
  assert.equal(modelAnswer({ calls: [] }), "No model was asked.");
});

test("the prompt sent is every message under its role", () => {
  const sent = promptSent({
    messages: [
      { role: "system", content: "Refuse client data." },
      { role: "user", content: '["hello"]' },
    ],
  });
  assert.equal(sent, '[system]\nRefuse client data.\n\n[user]\n["hello"]');
  // A decision model's request has no messages; the request is the prompt.
  assert.equal(promptSent({ request: { state: ["hi"] } }), '{\n  "state": [\n    "hi"\n  ]\n}');
});

test("a prompt is named by its first line, cut short", () => {
  assert.equal(firstLine("  rename tmp2\nin this file"), "rename tmp2");
  assert.equal(firstLine("x".repeat(100)).length, 80);
});

test("JSON is indented and anything else is left as it is", () => {
  assert.equal(pretty('{"a":1}'), '{\n  "a": 1\n}');
  assert.equal(pretty({ a: 1 }), '{\n  "a": 1\n}');
  assert.equal(pretty("<html>bad gateway"), "<html>bad gateway");
});

test("a router's results follow the order its instructions are numbered in", () => {
  const steps = [{ id: "b" }, { id: "a" }];
  const routes = [{ step: "a" }, { step: "b" }];
  assert.deepEqual(
    inOrder(routes, steps).map((rt) => rt.step),
    ["b", "a"],
  );
});
