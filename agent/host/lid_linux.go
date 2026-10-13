package host

// lidAction is unknown on Linux: the agent holds no sleep off there, so it says nothing about a lid.
func lidAction() string { return "" }
