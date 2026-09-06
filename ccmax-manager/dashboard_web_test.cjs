const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

const source = readFileSync(join(__dirname, "web/app.js"), "utf8");
function loadFunctions(context, names) {
  vm.createContext(context);
  for (const name of names) {
    const start = source.search(new RegExp(`^(?:async )?function ${name}\\(`, "m"));
    assert.notEqual(start, -1);
    const end = source.indexOf("\n}\n", start) + 2;
    vm.runInContext(source.slice(start, end), context);
  }
  return context;
}

test("all time has no implicit month limit; custom timestamps are preserved", () => {
  const c = loadFunctions({ URLSearchParams, Date }, ["overviewFilterParams"]);
  assert.equal(c.overviewFilterParams({ preset: "all", from: "old" }).toString(), "");
  const params = c.overviewFilterParams({ preset: "custom", from: "2025-01-01T00:00", to: "2026-08-26T21:00" });
  assert.equal(params.get("from"), "2025-01-01T00:00");
  assert.equal(params.get("to"), "2026-08-26T21:00");
});

test("presets use the Shanghai calendar even at UTC month boundaries", () => {
  const c = loadFunctions({ URLSearchParams, Date }, ["overviewFilterParams", "overviewDate"]);
  const now = new Date("2026-08-31T18:30:00Z");
  assert.equal(c.overviewFilterParams({ preset: "today" }, now).get("from"), "2026-09-01T00:00");
  assert.equal(c.overviewFilterParams({ preset: "week" }, now).get("from"), "2026-08-26T00:00");
  assert.equal(c.overviewFilterParams({ preset: "month" }, now).get("from"), "2026-09-01T00:00");
  assert.match(c.overviewDate(now), /2026\/09\/01.*02:30/);
});

function overviewContext(api) {
  const nodes = new Map();
  const c = {
    URLSearchParams, Date, AbortController, api,
    state: { overviewRequestID: 0, overviewRange: { preset: "all" }, dashboard: null },
    canView: () => true, hydrateGroupControls() {}, populateSelects() {}, renderDashboard() {},
    $: (id) => {
      if (!nodes.has(id)) nodes.set(id, { setAttribute(name, value) { this[name] = value; } });
      return nodes.get(id);
    },
  };
  return loadFunctions(c, ["overviewFilterParams", "loadOverview"]);
}

test("late responses cannot replace the selected range", async () => {
  const requests = [];
  const signals = [];
  const c = overviewContext((path, options) => new Promise((resolve) => {
    signals.push(options.signal);
    requests.push(resolve);
  }));
  const first = c.loadOverview();
  const second = c.loadOverview();
  assert.equal(signals[0].aborted, true);
  assert.equal(signals[1].aborted, false);
  requests[1]({ id: "new", purposes: [], groups: [] });
  await second;
  requests[0]({ id: "old", purposes: [], groups: [] });
  await first;
  assert.equal(c.state.dashboard.id, "new");
  assert.equal(c.$("#overview-metrics")["aria-busy"], "false");
});

test("failed queries retain the last result and show an explicit error", async () => {
  const c = overviewContext(async () => { throw new Error("timeout"); });
  c.state.dashboard = { id: "previous" };
  await assert.rejects(c.loadOverview(), /timeout/);
  assert.equal(c.state.dashboard.id, "previous");
  assert.equal(c.$("#overview-error").hidden, false);
  assert.match(c.$("#overview-error").textContent, /timeout/);
  assert.equal(c.$("#overview-metrics")["aria-busy"], "false");
});
