package cli

// defaultSock mirrors ConfigFromEnv's ADMIN_SOCK default (agent/host/config.go, runDir per OS).
// The literal is written twice rather than shared, because the alternative — a const in
// shared/api — would put a deployment path in the closed wire-contract package. Keep the two in
// step; a mismatch surfaces as "is the agent running?", which is why that error names the path
// it tried.
const defaultSock = "/run/briard/admin.sock"
