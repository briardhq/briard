// Package arch holds executable architecture guards — run as
// `go test ./internal/arch/`. They turn the CONTRIBUTING.md invariants into
// failing tests: host seam discipline, failover-is-observe-only, and no
// force-promotion anywhere in the codebase. Two tree guards run beside them: no
// committed secret, and no pointer at a private document or tracker item.
package arch
