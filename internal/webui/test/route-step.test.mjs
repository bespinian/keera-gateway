// Unit tests for the route step's helpers.

import { test } from "node:test";
import assert from "node:assert/strict";

import { deciderHint } from "../assets/views/route-step.js";

const decider = { alias: "keera-decider", kind: "decision" };
const chat = { alias: "qwen", kind: "chat" };

test("a chosen decision model says what it cannot read", () => {
  const readers = [{ ...decider, max_context_tokens: 2048 }, chat];
  assert.equal(
    deciderHint(readers, "keera-decider", "the gate refuses anything longer"),
    "A decision model only picks an answer, so it is quick. " +
      "It reads at most 2.0k tokens; the gate refuses anything longer.",
  );
});

test("a decision model without a limit names none", () => {
  assert.equal(
    deciderHint([decider], "keera-decider", "unused"),
    "A decision model only picks an answer, so it is quick.",
  );
});

test("a chat model, or none yet, points to a decision model", () => {
  const readers = [chat, decider];
  const want = "A decision model such as keera-decider is quicker.";
  assert.equal(deciderHint(readers, "qwen", "unused"), want);
  assert.equal(deciderHint(readers, "", "unused"), want);
});

test("without a decision model, there is nothing to add", () => {
  assert.equal(deciderHint([chat], "qwen", "unused"), "");
});
