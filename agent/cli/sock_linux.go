package cli

// defaultSock mirrors ConfigFromEnv's ADMIN_SOCK default (agent/host/config.go, runDir per OS).
// The literal is written twice rather than shared, because the alternative — a const in
// shared/api — would put a deployment path in the closed wire-contract package. Keep the two in
// step; a mismatch surfaces as "is the agent running?", which is why that error names the path
// it tried.
const defaultSock = "/run/briard/admin.sock"

// stateDir mirrors the agent's state directory (agent/host/config_linux.go), the root of the
// records the read verbs open directly; the pins on the paths under it keep the two in step.
const stateDir = "/var/lib/briard"
