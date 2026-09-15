// Package runnerclient is a typed Go client for the Doppels Runner IPC
// protocol (RFC 001, docs/runner-protocol.md): JSON-RPC 2.0 over NDJSON
// framed Unix Domain Sockets.
//
// Design:
//
//   - One *Client owns exactly one net.Conn. There is no reconnect logic in
//     v1: if the connection drops, every in-flight and future Call returns
//     ErrClosed (after Close) or the underlying read/write error, and the
//     caller must Dial again and re-subscribe (RFC §10: "Desconexión =
//     unsubscribe implícito ... Reconexión = nuevo initialize +
//     re-subscripciones").
//   - Requests and responses are multiplexed over the single connection by
//     JSON-RPC id: a single background read loop goroutine (started by Dial,
//     stopped by Close) demultiplexes every incoming frame, delivering
//     responses to the Call goroutine that issued them and notifications to
//     the registered handler. Any number of goroutines may call Client.Call
//     concurrently.
//
// This package is intended to be reused, unmodified, by Doppels Desktop via
// FFI later in the Desktop-first Runner plan; keep it dependency-light and
// free of CLI-specific concerns.
package runnerclient
