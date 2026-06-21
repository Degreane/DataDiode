// Package plugin hosts protocol-adapter plugins.
//
// Two mechanisms are planned (see ADR-0001):
//   - WASM via wazero — default, sandboxed, language-agnostic.
//   - Subprocess over stdio (go-plugin style) — for plugins needing native OS access.
//
// In-process dynamic loading is deliberately not supported.
package plugin
