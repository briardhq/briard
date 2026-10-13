package cli

// defaultSock mirrors ConfigFromEnv's ADMIN_SOCK default on Windows (sock_linux.go says why the
// literal is written twice).
const defaultSock = `C:\ProgramData\briard\run\admin.sock`

// stateDir mirrors the agent's state directory on Windows (sock_linux.go says why).
const stateDir = `C:\ProgramData\briard\state`
