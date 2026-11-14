// Package arch holds executable architecture guards — run as
// `go test ./internal/arch/`. They turn the CONTRIBUTING.md invariants into
// failing tests: host seam discipline, failover-is-observe-only, and no
// force-promotion anywhere in the codebase; and they keep host-OS calls in the
// per-OS files of the packages that have a Windows arm. Three tree guards run
// beside them: no committed secret, no pointer at a private document or tracker
// item, and one word, "app", for what a household installs. Two build facts are
// held in step too: the guest image's input hash covers every package its
// firmware links, and the test rigs' promoter chain matches the product's.
package arch
