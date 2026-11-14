// Package api holds the wire contract between the agent and the cloud control plane: what a
// node reports up (NodeStatus), the directives that come back (Directive and its kinds), and
// the paths both sides use. Defined once here so the two sides can never drift; everything in
// it that a node sends is the closed allowlist of what leaves the house (see CONTRIBUTING.md
// on widening it).
package api
