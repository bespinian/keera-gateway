// Unit tests for the router tree helpers.

import { test } from "node:test";
import assert from "node:assert/strict";

import { drawnOrder } from "../assets/views/router-tree.js";

test("steps are listed column by column, as the editor draws them", () => {
  const route = (id, destinations, branches) => ({
    id,
    kind: "route",
    destinations,
    branches,
  });
  const start = {
    id: "f",
    kind: "filter",
    next: route("r1", ["zeta", "alpha"], {
      zeta: route("r2", ["a", "b"], {}),
      alpha: route("r3", ["c", "d"], { c: route("r4", ["e", "f"], {}) }),
    }),
  };
  assert.deepEqual(
    drawnOrder(start).map((s) => s.id),
    ["f", "r1", "r2", "r3", "r4"],
  );
  assert.deepEqual(drawnOrder(null), []);
});
