// Exercise the actual Homepage handlers with deferred network responses.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
const html = readFileSync(new URL("../../keystone/portal/web/assistant.html", import.meta.url), "utf8");
const start = html.indexOf("  function memoryRow(m) {");
const end = html.indexOf("  // Re-reads the open conversation", start);
function element(tag) {
  return {tag, children: [], handlers: {}, disabled: false,
    appendChild(child) {this.children.push(child); this.lastChild = child;},
    addEventListener(event, handler) {this.handlers[event] = handler;}};
}
for (const mode of ["success", "write-failure", "reload-failure", "inherited-reload-failure"]) {
  const success = mode !== "write-failure";
  const inputs = [], requests = [];
  let finish, refreshed = false;
  const context = vm.createContext({make: element,
    document: {createElement(tag) {const e = element(tag); inputs.push(e); return e;}, createTextNode: element},
    fetch(_url, options) {requests.push(JSON.parse(options.body)); return new Promise(resolve => {finish = resolve;});},
    loadMemory() {refreshed = true; return Promise.resolve(mode.endsWith("reload-failure") ? false : true);}, text() {}});
  vm.runInContext(html.slice(start, end), context);
  context.memoryRow({seq: 1, mine: true, privacy: {localOnly: mode === "inherited-reload-failure", trustedOnly: true}});
  inputs[0].checked = mode !== "inherited-reload-failure";
  inputs[0].handlers.change();
  assert.ok(inputs.every(input => input.disabled), "both controls must lock during the request");
  inputs[1].handlers.change();
  assert.equal(requests.length, 1, "a second event cannot overwrite the first restriction");
  assert.deepEqual(requests[0], {seq: 1, localOnly: mode !== "inherited-reload-failure"}, "only the changed flag is submitted");
  finish({ok: success, json: async () => ({localOnly:true,trustedOnly:true})});
  await new Promise(resolve => setImmediate(resolve));
  if (mode === "success") {
    assert.ok(refreshed);
    assert.ok(inputs.every(input => input.disabled), "lock survives until replacement rows arrive");
  } else {
    assert.ok(inputs.every(input => !input.disabled));
    assert.equal(inputs[1].checked, true, "unmodified inherited flag is retained");
    assert.equal(inputs[0].checked, mode.endsWith("reload-failure"), "reload failure keeps the saved write; failed writes restore the saved value");
  }
}
console.log("Homepage privacy serialization checks passed.");

const reloadStart = html.indexOf("  function loadMemory() {");
const reloadEnd = html.indexOf("  function memoryRow(m) {", reloadStart);
const reloadContext = vm.createContext({fetch() {return Promise.reject(new Error("offline"));}, text() {}});
vm.runInContext(html.slice(reloadStart, reloadEnd), reloadContext);
assert.equal(await reloadContext.loadMemory(), false, "actual reload reports network failure to its caller");

for(const status of [401,500]) {
 let clears=0;
 const context=vm.createContext({fetch:async()=>({ok:false,status,json:async()=>({error:"unavailable"})}),el:()=>({}),clear:()=>{clears++},text(){}});
 vm.runInContext(html.slice(reloadStart,reloadEnd),context);
 assert.equal(await context.loadMemory(),false,"HTTP reload failure reaches recovery");
 assert.equal(clears,0,"failed HTTP reload preserves the existing controls");
}
