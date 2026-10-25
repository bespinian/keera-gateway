// Unit tests for the panel's pure helpers, run with `make check-js`. They use
// node:test, so the panel keeps having no build step and no dependencies.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import {
  aliasProblem,
  modelAliasProblem,
  sandboxNameProblem,
  compact,
  ms,
  bytes,
  duration,
  pct,
  plural,
  docsURL,
} from "../assets/ui.js";

// The same cases hold the server's rules, in names_test.go.
const cases = JSON.parse(
  readFileSync(new URL("../testdata/names.json", import.meta.url), "utf8"),
);

for (const [kind, problem] of [
  ["alias", aliasProblem],
  ["model", modelAliasProblem],
  ["sandbox", sandboxNameProblem],
]) {
  test(`the panel checks ${kind} names as the server does`, () => {
    for (const name of cases[kind].valid) {
      assert.equal(problem(name), "", `${JSON.stringify(name)} is refused`);
    }
    for (const name of cases[kind].invalid) {
      assert.notEqual(problem(name), "", `${JSON.stringify(name)} is accepted`);
    }
  });
}

test("docsURL sends administrators to manage and members to use", () => {
  const as = (can_admin_org) => ({ state: { me: { can_admin_org } } });
  assert.equal(docsURL(as(true)), "https://docs.keera.ch/manage");
  assert.equal(docsURL(as(false)), "https://docs.keera.ch/use");
});

test("compact shortens large counts", () => {
  assert.equal(compact(999), "999");
  assert.equal(compact(1500), "1.5k");
  assert.equal(compact(25_000), "25k");
  assert.equal(compact(2_500_000), "2.5M");
  assert.equal(compact(3e9), "3.0B");
  assert.equal(compact(undefined), "0");
});

test("time and size read in their unit", () => {
  assert.equal(ms(0), "-");
  assert.equal(ms(250), "250 ms");
  assert.equal(ms(1500), "1.5 s");
  assert.equal(bytes(512), "512 B");
  assert.equal(bytes(2048), "2.0 kB");
  assert.equal(duration(59_000), "59 s");
  assert.equal(duration(61_000), "1 min 1 s");
  assert.equal(duration(2 * 3600_000), "2 h");
});

test("a share too small for one decimal keeps three", () => {
  assert.equal(pct(1, 0), "-");
  assert.equal(pct(1, 4), "25.0%");
  assert.equal(pct(1, 10_000), "0.010%");
});

test("plural picks the word for the count", () => {
  assert.equal(plural(1, "key"), "key");
  assert.equal(plural(2, "key"), "keys");
  assert.equal(plural(0, "proxy", "proxies"), "proxies");
});
