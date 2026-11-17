// Unit tests for the Billing screen's pure helpers.

import { test } from "node:test";
import assert from "node:assert/strict";

import { machineRows } from "../assets/views/billing.js";

test("machine rows are CPU, memory and disk, per organisation", () => {
  const rows = [
    { org_id: "org_a", service: "", backend_model: "gpt" },
    { org_id: "org_a", service: "agents", item: "CPU" },
    { org_id: "org_a", service: "agents", item: "Disk" },
    { org_id: "org_a", service: "agents", item: "Memory" },
    { org_id: "org_b", service: "agents", item: "Disk" },
    { org_id: "org_b", service: "agents", item: "CPU" },
  ];
  assert.deepEqual(
    machineRows(rows).map((r) => `${r.org_id} ${r.item}`),
    ["org_a CPU", "org_a Memory", "org_a Disk", "org_b CPU", "org_b Disk"],
  );
});
