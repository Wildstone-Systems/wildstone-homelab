// TEST-ONLY. Loaded by TestMatterServerServesHomeAssistant through
// NODE_OPTIONS=--import, and only when the Docker host's kernel has no
// IPv6 at all (no /proc/net/if_inet6) — a CI sandbox, never a node.
//
// Matter is IPv6-first: matter.js binds its operational and mDNS
// sockets as udp6 on "::", and on a kernel without IPv6 that bind fails
// with EAFNOSUPPORT and the server exits before its WebSocket listens.
// This shim turns every udp6 socket into a placeholder that never binds
// and drops whatever is sent through it — matter.js opens a real udp4
// socket beside each one — so the server comes up IPv4-only. Nothing a real household
// needs survives that — Thread devices are IPv6-only — which is why a
// real node must pass the home-automation readiness probe's IPv6
// checks instead. What it leaves intact is everything the test judges:
// the WebSocket API Home Assistant speaks, the fabric in /data, and
// which addresses the WebSocket is bound to.
import dgram from 'node:dgram'
import { syncBuiltinESMExports } from 'node:module'

const create = dgram.createSocket
dgram.createSocket = function (opts, cb) {
  const o = typeof opts === 'string' ? { type: opts } : { ...opts }
  const v6 = o.type === 'udp6'
  if (v6) {
    o.type = 'udp4'
    delete o.ipv6Only
  }
  const s = create.call(this, o, cb)
  if (!v6) return s
  // What was the IPv6 socket becomes a placeholder that never binds:
  // matter.js opens a real IPv4 socket beside it on the same port (and
  // learns that port from this one when it asked for an ephemeral one),
  // so binding here would take the port the IPv4 socket needs.
  let port = 0
  s.bind = function (...args) {
    const cb = args.find((a) => typeof a === 'function')
    const req = typeof args[0] === 'object' && args[0] ? args[0].port : args[0]
    port = typeof req === 'number' && req > 0 ? req : 40000 + Math.floor(Math.random() * 20000)
    queueMicrotask(() => {
      this.emit('listening')
      if (cb) cb()
    })
    return this
  }
  s.address = () => ({ address: '::', family: 'IPv6', port })
  for (const m of ['setBroadcast', 'setMulticastTTL', 'setMulticastLoopback', 'setTTL', 'ref', 'unref']) {
    s[m] = () => s
  }
  const close = s.close
  s.close = function (cb) {
    try {
      return close.call(this, cb)
    } catch {
      if (cb) queueMicrotask(cb)
    }
  }
  for (const m of ['addMembership', 'dropMembership', 'setMulticastInterface', 'addSourceSpecificMembership', 'dropSourceSpecificMembership']) {
    s[m] = () => {}
  }
  s.send = function (...args) {
    const cb = typeof args[args.length - 1] === 'function' ? args[args.length - 1] : undefined
    if (cb) queueMicrotask(() => cb(null, 0))
  }
  return s
}
// matter.js imports node:dgram as a namespace; its bindings follow the
// CommonJS object only once they are synced.
syncBuiltinESMExports()
console.warn('wildstone test shim: no IPv6 on this Docker host; Matter UDP forced to IPv4 (TEST ONLY)')
