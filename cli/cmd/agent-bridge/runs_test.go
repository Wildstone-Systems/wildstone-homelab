package main

import (
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
)

func TestRunRegistrySetGetDelete(t *testing.T) {
	r := newRunRegistry()
	if _, ok := r.get("run-1"); ok {
		t.Fatal("get on an empty registry found something")
	}
	r.set("run-1", agentapi.Handle("task1.secret"))
	h, ok := r.get("run-1")
	if !ok || h != agentapi.Handle("task1.secret") {
		t.Fatalf("get = (%q, %v), want (task1.secret, true)", h, ok)
	}
	r.delete("run-1")
	if _, ok := r.get("run-1"); ok {
		t.Fatal("get found a deleted run")
	}
}

func TestRunRegistrySolePending(t *testing.T) {
	r := newRunRegistry()
	if _, ok := r.solePending(); ok {
		t.Fatal("solePending found something in an empty registry")
	}
	r.set("run-1", agentapi.Handle("task1.secret"))
	h, ok := r.solePending()
	if !ok || h != agentapi.Handle("task1.secret") {
		t.Fatalf("solePending = (%q, %v), want (task1.secret, true)", h, ok)
	}
	r.set("run-2", agentapi.Handle("task2.secret"))
	if _, ok := r.solePending(); ok {
		t.Fatal("solePending returned true with two runs in flight")
	}
}
